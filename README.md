# ai-sre-agent

[![CI](https://github.com/ipekutku/ai-sre-agent/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/ipekutku/ai-sre-agent/actions/workflows/ci.yml)

An AI SRE / incident investigation agent written in Go.

The agent investigates incidents by gathering evidence through explicit, constrained tools
(metrics, service metadata, and later logs, traces, and infrastructure state), then produces a
structured, evidence-backed diagnosis. Diagnoses are scored against scenario ground truth that
the agent never sees.

## Status

**Milestone 1: Metrics-Based Incident Investigator** (in progress). The two demo services run
under Docker Compose and Prometheus scrapes their metrics. Fault injection and the agent are not built yet.
See [docs/ROADMAP.md](docs/ROADMAP.md) for the full plan.

## Demo services

```text
GET /checkout  ->  checkout-api  --HTTP-->  inventory-api  <-  GET /inventory
```

| Service | Endpoints | Default address |
|---|---|---|
| `checkout-api` | `GET /checkout?sku=...`, `GET /healthz` | `:8080` |
| `inventory-api` | `GET /inventory?sku=...`, `GET /healthz` | `:8081` |

`checkout-api` returns `502` if its call to `inventory-api` fails or times out.
`inventory-api` responds after a fixed baseline delay (`BASE_LATENCY`, 20 ms by default).

Run them locally in two terminals:

```bash
go run ./cmd/inventory-api
go run ./cmd/checkout-api
curl 'localhost:8080/checkout?sku=abc'
```

Configuration is via environment variables:

| Variable | Service | Default |
|---|---|---|
| `ADDR` | both | `:8080` / `:8081` |
| `INVENTORY_URL` | checkout-api | `http://localhost:8081` |
| `INVENTORY_TIMEOUT` | checkout-api | `2s` |
| `ADMIN_ADDR` | inventory-api | `127.0.0.1:9081` |
| `BASE_LATENCY` | inventory-api | `20ms` |

## Local environment

Requires Docker with the Compose plugin.

```bash
make up     # build and start checkout-api, inventory-api, and Prometheus
make down   # stop and remove the containers
```

| Component | URL |
|---|---|
| checkout-api | http://localhost:8080/checkout |
| inventory-api | http://localhost:8081/inventory |
| Prometheus | http://localhost:9090 |

| inventory-api fault admin | http://localhost:9081/fault |

Ports are bound to `127.0.0.1` only. Prometheus scrapes every 5s
([`deploy/prometheus/prometheus.yml`](deploy/prometheus/prometheus.yml)).

## Fault injection

`inventory-api` serves a fault-injection admin API on a separate port (`ADMIN_ADDR`). It is not
instrumented, so fault state never appears in metrics. See
[ADR 0001](docs/adr/0001-fault-injection-admin-api.md) for why.

```bash
make fault-inventory-latency                        # inventory-api now responds in 800 ms
make fault-inventory-latency FAULT_LATENCY_MS=1500  # custom latency (max 10000)
make fault-clear                                    # back to the 20 ms baseline
curl localhost:9081/fault                           # current state: {"latency_ms": 0} = no fault
```

The API is `GET`, `PUT {"latency_ms": n}`, and `DELETE` on `/fault`. Delays are fixed (no
jitter), so the same setting always produces the same latency.

## Scenarios

Each incident scenario lives in [`scenarios/<id>/`](scenarios/):

| File | Contents | Read by |
|---|---|---|
| `scenario.yaml` | the incident to raise and the fault to inject | scenario runner (`internal/scenarios`) |
| `ground-truth.yaml` | the expected root-cause code | evaluator only (`internal/evaluation`) |

The investigation agent receives only the scenario's `incident` (alert, service, severity,
description), which describes the symptom and never the cause. The evaluator passes a diagnosis
only if it is valid (including at least one piece of evidence), refers to the scenario's incident,
and reports the expected root-cause code.

## Metrics

Each service exposes `GET /metrics`. Prometheus sets the `job` label to the service name.

| Metric | Labels | Measures |
|---|---|---|
| `http_server_request_duration_seconds` | `route`, `method`, `status` | inbound requests handled by the service |
| `http_client_request_duration_seconds` | `peer`, `method`, `status` | outbound calls to a dependency, as seen by the caller (`status="error"` if no response) |
| `process_cpu_seconds_total`, `go_*` | | process and Go runtime |

`route` is the matched route pattern (`unmatched` for unknown paths). Request count and status
come from the histograms' `_count` series.

These separate the three latencies an investigation needs to tell apart:

```promql
# checkout-api server latency (p95)
histogram_quantile(0.95, sum by (le) (rate(http_server_request_duration_seconds_bucket{job="checkout-api", route="/checkout"}[1m])))

# checkout-api -> inventory-api dependency latency (p95)
histogram_quantile(0.95, sum by (le) (rate(http_client_request_duration_seconds_bucket{job="checkout-api", peer="inventory-api"}[1m])))

# inventory-api server latency (p95)
histogram_quantile(0.95, sum by (le) (rate(http_server_request_duration_seconds_bucket{job="inventory-api", route="/inventory"}[1m])))
```

## Requirements

- Go (version pinned in [`go.mod`](go.mod))
- `make`

## Development

```bash
make fmt    # format Go code in place
make vet    # go vet ./...
make test   # go test ./...
make ci     # gofmt check + vet + test (what CI runs)
```

CI ([`.github/workflows/ci.yml`](.github/workflows/ci.yml)) runs `make ci` on every pull request and on pushes to `main`.

## Documentation

- [docs/PROJECT.md](docs/PROJECT.md): architecture, goals, and design principles
- [docs/ROADMAP.md](docs/ROADMAP.md): milestones and exit criteria

## License

[MIT](LICENSE)
