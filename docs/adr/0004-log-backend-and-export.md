# ADR 0004: Logs exported over OTLP to Loki

- Status: Accepted
- Date: 2026-10-14

## Context

Milestone 2 adds structured, trace-correlated application logs, and a `query_logs` tool that
lets the agent read them. The roadmap allows a dedicated log backend only if the investigation
interface needs one. A query tool needs a store it can filter by service, severity, text, and
trace or request ID, so a backend is required. Services currently write JSON logs to stdout only.

Decisions needed:

1. **Backend.** It must run locally in Compose and have a query API that a narrow tool can call.
2. **Shipping path.** Logs can be pushed by the services, or collected from container output by
   an agent.
3. **Isolation.** The fault admin API logs fault changes (`component=fault-admin`). These logs
   are ground truth and the agent must never be able to read them (ADR 0001).

## Decision

- **Backend: Grafana Loki** (`grafana/loki:3.7.8`, single binary, filesystem storage inside the
  container, 24 h retention). It accepts OTLP natively and pairs with Tempo (ADR 0003).
- **Shipping: OTLP/HTTP straight from each service to Loki's `/otlp/v1/logs` endpoint**, through
  the OpenTelemetry Go logs SDK and the `otelslog` bridge. Each service's `slog` logger writes to
  stdout as before and also exports every record. Configuration uses the standard
  `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT` variable; without it, logs go to stdout only.
- **Correlation fields.** Every record logged with a request context has `request_id`,
  `trace_id`, and `span_id`. Exported records carry the trace context natively, and the service
  name comes from the `service.name` resource, which Loki turns into the `service_name` label.
  `request_id` and the other attributes become Loki structured metadata.
- **Request IDs.** Each request gets the caller's `X-Request-ID` if it is valid (1-64 characters
  from `[A-Za-z0-9._-]`), or a new random one. checkout-api forwards it to inventory-api, and
  both services write one access-log line per request.
- **Isolation by construction.** The fault-admin logger is a separate stdout-only logger, not a
  child of the exporting one, so its records are never exported. A test checks this.

## Consequences

- **Per-signal endpoints.** Traces and logs go to different backends, so Compose sets
  `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` and `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT` instead of the
  shared `OTEL_EXPORTER_OTLP_ENDPOINT`. Setting only the shared variable enables both signals at
  one endpoint, which suits a Collector but not Tempo or Loki alone.
- **No Docker socket access.** Collecting container output (Grafana Alloy, Promtail) would need
  the Docker socket, which is a broad privilege, and would pick up the fault-admin lines unless
  they were filtered out downstream. Pushing from the process keeps the exclusion in code where
  it can be tested.
- **No Collector.** As with traces (ADR 0003), there is one fewer container. One can be added
  later by changing only the endpoint variables.
- **Exporter maturity.** The logs API and SDK are stable (v1.47), but the OTLP log exporter
  (v0.23) and the `otelslog` bridge (v0.21) are still pre-1.0 and may change their API.
- **Telemetry errors stay local.** Export failures are logged with a stdout-only logger, so a
  failing backend cannot feed its own errors back into the export queue.
- **Duplicated access-log fields.** Request status and latency also appear in metrics and
  spans. Logs add the request ID, the error message, and a line the agent can search by text.
