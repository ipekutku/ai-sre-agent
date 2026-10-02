# ai-sre-agent

An AI SRE / incident investigation agent written in Go.

The agent investigates incidents by gathering evidence through explicit, constrained tools
(metrics, service metadata, and later logs, traces, and infrastructure state), then produces a
structured, evidence-backed diagnosis. Diagnoses are scored against scenario ground truth that
the agent never sees.

## Status

**Milestone 0: Engineering Baseline.** The repository has a Go module, a Makefile, and CI.
There is no application functionality yet. See [docs/ROADMAP.md](docs/ROADMAP.md) for what comes next.

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
- [AGENTS.md](AGENTS.md): instructions for AI coding agents working in this repository

## License

[MIT](LICENSE)
