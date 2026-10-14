# ADR 0001: Fault injection through a separate runtime admin API

- Status: Accepted
- Date: 2026-10-02

## Context

Milestone 1 needs deterministic, reproducible fault injection in `inventory-api`
(normal latency ~20 ms, faulted ~800 ms) that can be switched on without editing source code.

The fault state is ground truth. The investigation agent must not be able to observe it
directly; it should only see the *effects* (latency) through its tools.

Options considered:

1. **Environment variable at startup**, faults applied by restarting the container with
   different settings.
2. **Runtime admin endpoint on the service's own port.**
3. **Runtime admin endpoint on a separate listener.**

## Decision

Option 3. `inventory-api` applies a fixed baseline delay (`BASE_LATENCY`, default 20 ms) and
exposes a small admin API on a separate listener (`ADMIN_ADDR`, `:9081` in Compose):

```text
GET    /fault                        current state
PUT    /fault  {"latency_ms": 800}   replace the baseline delay with 800 ms
DELETE /fault                        restore the baseline
```

Delays are fixed (no jitter), so a given configuration always produces the same latency.

## Consequences

- **No restart artefacts.** Restarting the service (option 1) would reset counters, change
  `process_start_time_seconds`, and drop in-flight requests. Those are extra signals that do
  not exist in a real downstream-latency incident and could act as a clue.
- **Fault state stays out of agent-visible data.** The admin listener is not wrapped by
  `httpmetrics`, so fault requests never appear in `/metrics`, and the service port does not
  serve the admin routes. Option 2 would have put `/fault` traffic into the scraped metrics.
- **Fault changes are logged** (`component=fault-admin`) for auditability. When a log query
  tool is added (Milestone 2), these entries must be excluded from what the agent can read,
  or written to a separate stream.
- **Access control is by network placement only.** The admin API has no authentication. It
  defaults to `127.0.0.1:9081` outside containers and Compose publishes it on `127.0.0.1`
  only. This is acceptable for a local demo environment; it must not be exposed elsewhere.
- **One fault type for now.** The API only supports latency. Other fault types (errors,
  timeouts) will extend this API when the scenarios that need them are added.

## Notes

- 2026-10-14: Service logs are now exported to Loki (ADR 0004). The fault-admin logger is a
  separate logger that writes to stdout only, so fault changes are still logged for auditing
  but never reach the log backend. A test in `cmd/inventory-api` checks this.
- 2026-10-15: The API now supports HTTP errors, as anticipated above:
  `PUT /fault {"latency_ms": n, "error_percent": p}`, with either field or both, replaces the
  whole fault. Errors are deterministic like delays. A counter-based schedule, restarted on each
  `PUT`, makes exactly `p` of every 100 consecutive requests fail, spread evenly, with no RNG. A
  failed request waits for the configured delay, then returns `500` and logs a generic server
  error. Neither the response nor the log mentions injection, and the tests that check the admin
  API produces no spans and no exported logs now run with an error fault active.
