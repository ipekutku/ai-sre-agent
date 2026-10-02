# ai-sre-agent

[![CI](https://github.com/ipekutku/ai-sre-agent/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/ipekutku/ai-sre-agent/actions/workflows/ci.yml)

An AI SRE / incident investigation agent written in Go.

The agent investigates incidents by gathering evidence through explicit, constrained tools
(metrics, service metadata, and later logs, traces, and infrastructure state), then produces a
structured, evidence-backed diagnosis. Diagnoses are scored against scenario ground truth that
the agent never sees.

## Status

**Milestone 1: Metrics-Based Incident Investigator** (in progress). The two demo services exist;
metrics, Docker Compose, fault injection, and the agent are not built yet.
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
