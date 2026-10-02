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

Ports are bound to `127.0.0.1` only. Prometheus scrapes every 5s
([`deploy/prometheus/prometheus.yml`](deploy/prometheus/prometheus.yml)).

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
