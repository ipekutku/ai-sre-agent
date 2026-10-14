# Architecture

This document describes the system as implemented (Milestone 1, plus the parts of Milestone 2
merged so far). For goals and long-term
direction, see [PROJECT.md](PROJECT.md) and [ROADMAP.md](ROADMAP.md).

## Overview

```text
                      scenario-runner (cmd/scenario-runner)
                 reads scenario.yaml + ground-truth.yaml
        +-----------------+------------------+-------------------------+
        |                 |                  |                         |
   traffic (20 rps)   fault admin API    incident only            ground truth
        |             (inventory :9081)       |                         |
        v                 |                   v                         v
  +--------------+        |         +-------------------+         +-----------+
  | checkout-api |--HTTP--+-------->|      agent        |-------->| evaluator |
  |    :8080     |   inventory-api  |  (internal/agent) |diagnosis| PASS/FAIL |
  +------+-------+      :8081       +----+---------+----+         +-----------+
         |                 |             |         |
         |  /metrics       | /metrics    | LLM     | tools (internal/tools)
         v                 v             v         v
      +---------------------+    Claude API    query_metrics ----> Prometheus :9090
      |  Prometheus :9090   |                  query_traces -----> Tempo :3200
      +---------------------+                  query_logs -------> Loki :3100
                                               inspect_service --> /healthz + catalog

  checkout-api, inventory-api --OTLP/HTTP spans--> Tempo :3200
  checkout-api, inventory-api --OTLP/HTTP logs---> Loki  :3100
  agent (investigation only) ---OTLP/HTTP spans--> Tempo :4318 (service "ai-sre-agent")
  Grafana :3000 (people only) ---reads--> Tempo, Loki, Prometheus
```

## Components

| Component | Location | Responsibility |
|---|---|---|
| checkout-api | `cmd/checkout-api`, `internal/services/checkout` | `GET /checkout`; calls inventory-api |
| inventory-api | `cmd/inventory-api`, `internal/services/inventory` | `GET /inventory` with a fixed baseline delay; fault admin API on a separate port |
| Metrics | `internal/httpmetrics` | server and client (dependency) latency histograms, `/metrics` |
| Telemetry | `internal/telemetry` | OpenTelemetry setup (OTLP/HTTP export of spans and logs, each a no-op without an endpoint); server and client spans with W3C propagation; request IDs; access logs; the JSON logger with trace correlation |
| Environment | `deploy/` | Docker Compose: both services, Prometheus (5 s scrape), Tempo, Loki, and Grafana (provisioned datasources only); ports on 127.0.0.1 |
| Scenarios | `scenarios/<id>/`, `internal/scenarios` | incident, fault, alert trigger; environment control for the runner |
| Tools | `internal/tools` | `query_metrics`, `query_traces`, `query_logs`, `inspect_service`, and the registry (validation, timeouts, size limits, logging, a span per call) |
| LLM boundary | `internal/llm`, `internal/llm/anthropic` | provider-independent `Client`; Claude implementation |
| Agent | `internal/agent` | investigation loop, budgets, `submit_diagnosis` validation, stats, investigation and LLM spans |
| Diagnosis | `internal/diagnosis` | diagnosis schema and the closed root-cause code list |
| Evaluation | `internal/evaluation` | loads ground truth; compares diagnosis; report |
| Runner | `cmd/scenario-runner` | orchestrates one scenario end to end (`make eval`); sets up the agent's tracer |

## Investigation flow

1. The runner gives the agent an `incident.Incident` (alert name, service, severity, description).
2. The agent sends Claude a fixed system prompt, the incident, and five tools:
   `query_metrics`, `query_traces`, `query_logs`, `inspect_service`, `submit_diagnosis`.
3. Each `tool_use` is dispatched through the tool registry; every one gets a `tool_result`
   (failures are returned to the model as structured errors).
4. The loop repeats until Claude calls `submit_diagnosis` with a valid diagnosis, or a budget
   (LLM requests, tool calls, duration) is exhausted.
5. The conversation history is append-only and the system prompt and tool list never change
   within an investigation. This lets assistant turns, including reasoning blocks, be replayed
   exactly and keeps the prompt cache valid.

## Ground-truth isolation

The agent must infer the cause from observations only.

- The agent receives only the incident. `scenario.yaml`'s fault and trigger sections and
  `ground-truth.yaml` are read by the runner and evaluator only.
- `internal/evaluation` is the only package that reads `ground-truth.yaml`.
- `TestAgentCannotImportGroundTruth` fails if `internal/agent`, `internal/tools`, or
  `internal/llm` depend on `internal/scenarios` or `internal/evaluation`.
- The fault admin API is on a separate listener that is not instrumented or traced and is not in the
  agent's service catalog ([ADR 0001](adr/0001-fault-injection-admin-api.md)). Its logger writes
  to stdout only, so fault changes never reach Loki ([ADR 0004](adr/0004-log-backend-and-export.md)).
- `query_traces` builds TraceQL from validated, structured input and returns only spans from
  catalog services, so spans from any other source, including the agent's own (`ai-sre-agent`),
  never reach the model ([ADR 0005](adr/0005-structured-trace-and-log-queries.md)).
- Only the investigation is traced. The runner's traffic, fault injection, and waits use an
  uninstrumented HTTP client and create no spans, and neither the runner nor the tools propagate
  trace context to the services. Agent spans carry no prompts, model output, tool payloads, or
  diagnosis text, and nothing from the evaluation (expected code, PASS/FAIL).
- Grafana is for people. No tool can reach it.
- `query_logs` builds LogQL the same way, returns only entries from catalog services, and drops
  any entry marked `component=fault-admin`, both in the query and on the response, as a second
  line of defence behind the stdout-only admin logger.
- The system prompt and incident text are tested to contain no scenario details.

## Evaluation

A diagnosis passes if it is valid (known root-cause code, at least one piece of evidence),
refers to the scenario's incident, and its code matches the ground truth exactly. Codes come
from a fixed list ([ADR 0002](adr/0002-closed-root-cause-taxonomy.md)).

The runner also reports investigation duration, LLM requests, tool calls (total, failed, and
per tool), token usage, estimated cost, and the investigation's trace ID.

## Security boundaries

- Tools are read-only: PromQL query endpoints only, Tempo search and trace-by-ID endpoints only,
  the Loki `query_range` endpoint only, and health endpoints from a fixed catalog.
- All model-generated tool input is decoded strictly and validated; unknown tools are rejected.
- Tool output, Prometheus, Tempo, and Loki responses, and health responses are size-limited; every call has a timeout.
- The API key is read by the SDK from the environment and is never sent to the model or logged.
- Fault injection has no authentication and relies on binding to 127.0.0.1; it is a local demo
  mechanism only.
- Tempo's OTLP receiver is published on 127.0.0.1:4318 so the host-side runner can export spans;
  anything local can write spans to it.
- Grafana allows anonymous access as a Viewer that may use Explore (`viewers_can_edit`,
  deprecated in Grafana but the narrowest setting that allows Explore), with the login form
  disabled and read-only provisioned datasources. It relies on binding to 127.0.0.1.

## Observability

- Services: request and dependency latency histograms, Go runtime and process metrics, and
  traces of every request (one trace per checkout request, exported to Tempo; see
  [ADR 0003](adr/0003-tracing-backend-and-export.md)). Spans carry standard HTTP attributes
  only, never fault state.
- Service logs: JSON on stdout and exported to Loki
  ([ADR 0004](adr/0004-log-backend-and-export.md)). Each request gets a request ID (the
  caller's valid `X-Request-ID`, or a new one) that checkout-api forwards to inventory-api, and
  one access-log line (`method`, `route`, `status`, `duration_ms`). Records logged during a
  request carry `request_id`, `trace_id`, and `span_id`. Telemetry export failures are logged
  to stdout only.
- Agent logs: structured logs per LLM request (model, stop reason, latency, tokens) and per tool
  call (tool, input, duration, outcome), plus an investigation summary with the trace ID. They go
  to stderr only.
- Agent traces: when an OTLP traces endpoint is configured (`make eval` sets Tempo's), each
  investigation is one trace under the service name `ai-sre-agent`. The root `investigation`
  span records the incident, budgets, outcome (`diagnosed`, `budget_exhausted`, `timeout`,
  `model_stopped`, `cancelled`, `llm_error`), the accepted root-cause code, and totals. Its
  children are one `llm.request` span per model request (model, finish reason, tokens, cost, and
  error kind on failure, using GenAI semantic-convention names where they exist) and one
  `tool.<name>` span per tool call from the registry (tool, input and output sizes, error code;
  `tool.unknown` for names that are not registered, which keeps model-chosen names out of span
  names). The request whose diagnosis was accepted is named `llm.final_diagnosis`. LLM spans end
  when the response arrives, so tool calls are their siblings, and rejected diagnoses are
  `diagnosis.rejected` events on the root span.
