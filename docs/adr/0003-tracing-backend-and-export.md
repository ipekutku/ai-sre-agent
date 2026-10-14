# ADR 0003: Tracing with OpenTelemetry, exported directly to Tempo

- Status: Accepted
- Date: 2026-10-14

## Context

Milestone 2 adds distributed traces so investigations can use more than metrics. Traces must
cover the checkout request, the checkout -> inventory call, and the inventory request. Later,
the agent will query them through a tool (`query_traces`), and the agent's own investigations
will be traced too.

Decisions needed:

1. **Instrumentation.** The roadmap specifies OpenTelemetry. The only real choice is between
   the contrib `otelhttp` package and hand-written middleware.
2. **Trace backend.** It must run locally in Compose, accept OTLP, and have a query API a
   narrow tool can call. Candidates: Grafana Tempo and Jaeger.
3. **Export path.** Processes can export straight to the backend, or through an OpenTelemetry
   Collector.

## Decision

- **Instrumentation: `otelhttp`**, wrapped by `internal/telemetry` (`Handler`, `Transport`).
  The tracer provider is passed explicitly rather than through OpenTelemetry's global state,
  which keeps tests isolated.
- **Backend: Grafana Tempo** (`grafana/tempo:3.1.0`, single binary, local storage inside the
  container). Its TraceQL search (`/api/search`) can filter by service, duration, and status in
  one request, which a structured `query_traces` tool can build safely. Fetching a single trace
  is `/api/v2/traces/<id>`.
- **Export: OTLP/HTTP straight from each process to Tempo**, with no Collector. Endpoint
  configuration uses the standard `OTEL_EXPORTER_OTLP_ENDPOINT` variable. Without it, tracing is
  a no-op, so services run unchanged outside Compose and in tests.

## Consequences

- **Handler order matters.** `otelhttp` passes a copy of the request to the next handler, so a
  wrapper outside it cannot see the route pattern the inner `ServeMux` sets. The tracing handler
  therefore wraps the metrics handler, and `/metrics` is excluded from tracing explicitly. A test
  guards the metrics route labels.
- **Fault state stays out of traces.** The fault admin listener is not wrapped (ADR 0001), and
  spans carry only standard HTTP attributes. Tests check both.
- **No Collector.** There is one fewer container and config file. Batching, retries, and
  filtering happen in the SDK. If several telemetry pipelines later need routing or filtering,
  a Collector can be added without changing service code, because only the endpoint changes.
- **Ingestion lag.** Spans are batched for up to 5 s by the SDK, and Tempo makes them searchable
  about 20-30 s after the request. Anything that queries traces right after a fault (the scenario
  runner, the agent) must allow for this.
- **Binary size.** The OTLP/HTTP exporter still links parts of `google.golang.org/grpc` through
  its shared config package. Service binaries grow by about 9 MB (unstripped). Accepted; the
  alternative is a hand-written OTLP exporter.
- **Jaeger not chosen.** It has a built-in UI and a simple HTTP API, but its search API is less
  expressive than TraceQL. Tempo also matches the roadmap and pairs with Loki if a log backend is
  added. Viewing traces without a UI means using the Tempo API until a UI is added.
- **Trusting incoming trace context.** Services accept `traceparent` from any caller. This is
  fine for a local demo whose only external caller is the scenario runner, which sends none.

## Notes

- 2026-10-14: Logs are now exported to Loki (ADR 0004). Because traces and logs go to different
  backends, Compose sets the per-signal `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` instead of
  `OTEL_EXPORTER_OTLP_ENDPOINT`.
- 2026-10-15: The agent's investigations are now traced too (service name `ai-sre-agent`). The
  scenario runner runs on the host, so Compose also publishes Tempo's OTLP receiver on
  `127.0.0.1:4318`, and `make eval` points `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` at it. Still no
  Collector. Grafana (`grafana/grafana:13.2.4`, provisioned datasources, no dashboards) was added
  as the trace UI, so viewing traces no longer needs the raw Tempo API.
