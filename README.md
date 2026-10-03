# ai-sre-agent

[![CI](https://github.com/ipekutku/ai-sre-agent/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/ipekutku/ai-sre-agent/actions/workflows/ci.yml)

An AI SRE / incident investigation agent written in Go.

The agent investigates incidents by gathering evidence through explicit, constrained tools
(metrics, service metadata, and later logs, traces, and infrastructure state), then produces a
structured, evidence-backed diagnosis. Diagnoses are scored against scenario ground truth that
the agent never sees.

## Status

**Milestone 1: Metrics-Based Incident Investigator** (in progress).

Done:

- `checkout-api` and `inventory-api` run under Docker Compose, instrumented with Prometheus metrics
- deterministic latency fault injection in `inventory-api`
- the `inventory-latency` scenario, its ground truth, the diagnosis schema, and the evaluator
- the `query_metrics` and `inspect_service` investigation tools
- a provider-independent LLM client interface, implemented for Claude (default `claude-sonnet-5-5`)
- the investigation loop (`internal/agent`): tool dispatch, budgets, timeout, and a validated
  diagnosis chosen from a fixed list of root-cause codes ([ADR 0002](docs/adr/0002-closed-root-cause-taxonomy.md))
- `make eval`: the scenario end to end, from fault injection to PASS/FAIL

Remaining before `v0.1.0`: a successful `make eval` run against the real API.
See [docs/ROADMAP.md](docs/ROADMAP.md) for the full plan.

## Running the demo

Requires Docker with the Compose plugin, Go, and an Anthropic API key.

```bash
export ANTHROPIC_API_KEY=...   # from console.anthropic.com; each run costs a few cents
make eval
```

`make eval` starts the environment (`make up`) and runs
[`cmd/scenario-runner`](cmd/scenario-runner), which:

1. waits until the services and Prometheus are ready, and clears any leftover fault
2. sends steady `GET /checkout` traffic (20 rps) and records a 60 s baseline
3. injects the scenario's fault (inventory-api responds in 800 ms)
4. waits until the alert condition holds in Prometheus (checkout-api p95 > 500 ms)
5. gives the agent the incident only; the agent investigates through its tools
6. compares the diagnosis with `ground-truth.yaml`, prints the report, and clears the fault

Example output shape:

```text
Scenario: inventory-latency

Expected:
INVENTORY_DOWNSTREAM_LATENCY

Actual:
INVENTORY_DOWNSTREAM_LATENCY

Result:
PASS

Investigation:
  models:          [claude-sonnet-5-5]
  duration:        ...
  llm requests:    ...
  tool calls:      ... (0 failed)
  tokens:          ...
  estimated cost:  $...
```

The exit code is 0 on PASS, 1 on FAIL, 2 on a setup error. Runner options go in `EVAL_ARGS`,
e.g. `make eval EVAL_ARGS="-effort high -baseline 30s"` (see `go run ./cmd/scenario-runner -h`).
Without credentials the runner stops immediately. `make down` stops the environment.

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

## Investigation tools

The agent can only observe the system through these tools ([`internal/tools`](internal/tools)):

| Tool | Does | Limits |
|---|---|---|
| `query_metrics` | read-only PromQL (instant, or a range over the last N minutes) against a fixed Prometheus URL | query ≤ 1000 chars, range ≤ 60 min, ≤ 20 series, ≤ 30 points/series, 2 MiB response cap |
| `inspect_service` | name, health status, version (from `GET /healthz`), and dependencies (from a fixed catalog) | catalog services only; never sees admin/fault endpoints |

All calls go through a registry that enforces a per-call timeout and output-size limit, logs each
call with its latency, and returns failures as structured errors (`unknown_tool`, `invalid_input`,
`timeout`, `output_too_large`, `execution_failed`) the model can react to.

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

- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md): how the system is built today
- [docs/PROJECT.md](docs/PROJECT.md): goals and design principles
- [docs/adr/](docs/adr/): architecture decision records
- [docs/ROADMAP.md](docs/ROADMAP.md): milestones and exit criteria

## License

[MIT](LICENSE)
