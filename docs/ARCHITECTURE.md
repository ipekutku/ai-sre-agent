# Architecture

This document describes the system as implemented (Milestone 1). For goals and long-term
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
      |  Prometheus :9090   |                  inspect_service --> /healthz + catalog
      +---------------------+
```

## Components

| Component | Location | Responsibility |
|---|---|---|
| checkout-api | `cmd/checkout-api`, `internal/services/checkout` | `GET /checkout`; calls inventory-api |
| inventory-api | `cmd/inventory-api`, `internal/services/inventory` | `GET /inventory` with a fixed baseline delay; fault admin API on a separate port |
| Metrics | `internal/httpmetrics` | server and client (dependency) latency histograms, `/metrics` |
| Environment | `deploy/` | Docker Compose: both services and Prometheus (5 s scrape), ports on 127.0.0.1 |
| Scenarios | `scenarios/<id>/`, `internal/scenarios` | incident, fault, alert trigger; environment control for the runner |
| Tools | `internal/tools` | `query_metrics`, `inspect_service`, and the registry (validation, timeouts, size limits, logging) |
| LLM boundary | `internal/llm`, `internal/llm/anthropic` | provider-independent `Client`; Claude implementation |
| Agent | `internal/agent` | investigation loop, budgets, `submit_diagnosis` validation, stats |
| Diagnosis | `internal/diagnosis` | diagnosis schema and the closed root-cause code list |
| Evaluation | `internal/evaluation` | loads ground truth; compares diagnosis; report |
| Runner | `cmd/scenario-runner` | orchestrates one scenario end to end (`make eval`) |

## Investigation flow

1. The runner gives the agent an `incident.Incident` (alert name, service, severity, description).
2. The agent sends Claude a fixed system prompt, the incident, and three tools:
   `query_metrics`, `inspect_service`, `submit_diagnosis`.
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
- The fault admin API is on a separate listener that is not instrumented and is not in the
  agent's service catalog ([ADR 0001](adr/0001-fault-injection-admin-api.md)).
- The system prompt and incident text are tested to contain no scenario details.

## Evaluation

A diagnosis passes if it is valid (known root-cause code, at least one piece of evidence),
refers to the scenario's incident, and its code matches the ground truth exactly. Codes come
from a fixed list ([ADR 0002](adr/0002-closed-root-cause-taxonomy.md)).

The runner also reports investigation duration, LLM requests, tool calls (and failures),
token usage, and estimated cost.

## Security boundaries

- Tools are read-only: PromQL query endpoints only, and health endpoints from a fixed catalog.
- All model-generated tool input is decoded strictly and validated; unknown tools are rejected.
- Tool output, Prometheus responses, and health responses are size-limited; every call has a timeout.
- The API key is read by the SDK from the environment and is never sent to the model or logged.
- Fault injection has no authentication and relies on binding to 127.0.0.1; it is a local demo
  mechanism only.

## Observability

- Services: request and dependency latency histograms, Go runtime and process metrics.
- Agent: structured logs per LLM request (model, stop reason, latency, tokens) and per tool call
  (tool, input, duration, outcome), plus an investigation summary. Agent tracing is Milestone 2.
