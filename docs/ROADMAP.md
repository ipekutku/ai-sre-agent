# AI SRE Agent Roadmap

## Roadmap Philosophy

This project should be developed incrementally.

Every milestone must leave behind a working, demonstrable system.

A milestone should be completed, tested, documented, and preferably tagged before major new capabilities are introduced.

The project should avoid adding technologies simply because they are common in AI, SRE, or cloud stacks.

Each new component must solve a concrete requirement.

---

# Milestone 0 — Engineering Baseline

## Goal

Create a clean production-style repository before implementing application functionality.

## Deliverables

- Go module
- initial repository structure
- `AGENTS.md`
- `CLAUDE.md`
- `docs/PROJECT.md`
- `docs/ROADMAP.md`
- Makefile
- GitHub Actions CI
- formatting checks
- `go vet`
- automated tests
- basic README
- `.gitignore`
- license

## CI Baseline

Pull requests should run:

```text
gofmt verification
go vet ./...
go test ./...
```

Additional static analysis may be introduced if it provides clear value.

## Exit Criteria

The repository has a clean development workflow and CI passes from a fresh clone.

---

# Milestone 1 — Metrics-Based Incident Investigator

## Goal

Build the smallest complete version of the project.

The system must contain one reproducible distributed-system incident and an agent capable of diagnosing it through explicit tools.

This milestone should prove the complete architecture:

```text
Incident
   |
   v
Agent
   |
   v
Tools
   |
   v
Observable System
   |
   v
Diagnosis
   |
   v
Ground-Truth Evaluation
```

---

## 1.1 Application Environment

Create two small Go services.

### checkout-api

Endpoint:

```text
GET /checkout
```

The service calls `inventory-api`.

### inventory-api

Endpoint:

```text
GET /inventory
```

Normal responses should be fast.

---

## 1.2 Local Infrastructure

Run the environment using Docker Compose.

Initial components:

```text
checkout-api
inventory-api
Prometheus
```

A small load generator may be added if required for producing useful metrics.

Avoid Kubernetes at this stage.

---

## 1.3 Metrics

Instrument both services with Prometheus metrics.

Useful signals include:

```text
HTTP request count
HTTP request duration
HTTP response status
outbound dependency request duration
```

The metric model should make it possible to distinguish:

```text
checkout server latency

checkout -> inventory dependency latency

inventory server latency
```

---

## 1.4 Fault Injection

Implement deterministic fault injection in `inventory-api`.

First failure mode:

```text
downstream latency
```

Example behavior:

```text
normal inventory latency:
~20 ms

faulted inventory latency:
~800 ms
```

Fault injection must be reproducible and controllable.

It should not require editing source code between normal and incident runs.

---

## 1.5 First Alert

Create an incident representing high latency in the upstream service.

Example:

```json
{
  "incident_id": "inc-001",
  "alert": "HighCheckoutLatency",
  "service": "checkout-api",
  "severity": "warning",
  "description": "p95 request latency exceeded threshold"
}
```

The incident must not reveal the injected fault.

---

## 1.6 Investigation Tools

Implement two tools.

### query_metrics

Allows the agent to execute constrained Prometheus queries.

Responsibilities:

- validate requests
- query Prometheus
- enforce timeouts
- limit response size
- return structured results
- record tool latency
- surface errors clearly

### inspect_service

Returns service metadata such as:

```text
name
health/status
version
dependencies
```

It must not expose fault injection state or scenario ground truth.

---

## 1.7 LLM Boundary

Introduce a small provider-independent LLM client interface.

The agent should depend on this interface rather than provider-specific code.

Provider-specific implementation may initially support one LLM provider only.

Do not add provider routing or failover yet.

---

## 1.8 Investigation Loop

Implement the initial agent orchestration loop.

The loop should support:

```text
incident
   |
   v
LLM
   |
   +--> tool request
   |
   v
tool execution
   |
   v
tool result
   |
   v
LLM
   |
   +--> additional tool call
   |
   +--> final structured diagnosis
```

The application should control:

- maximum investigation duration
- maximum number of tool calls
- tool dispatch
- invalid tool requests
- context cancellation
- provider errors
- structured output parsing

Keep the orchestration explicit.

Do not introduce an agent framework.

---

## 1.9 Structured Diagnosis

Define a machine-readable diagnosis schema.

Minimum fields:

```text
incident_id
root_cause.code
root_cause.summary
confidence
evidence
recommended_actions
```

Expected first root-cause code:

```text
INVENTORY_DOWNSTREAM_LATENCY
```

---

## 1.10 Scenario Definition

Create a scenario directory.

```text
scenarios/
└── inventory-latency/
    ├── scenario.yaml
    └── ground-truth.yaml
```

The scenario runner may access both files.

The investigation agent must never access the ground-truth file.

---

## 1.11 Evaluation

Implement an evaluator comparing:

```text
expected root cause
        vs
agent root cause
```

Initial output can be simple:

```text
Scenario: inventory-latency

Expected:
INVENTORY_DOWNSTREAM_LATENCY

Actual:
INVENTORY_DOWNSTREAM_LATENCY

Result:
PASS
```

Also record basic investigation metrics where available:

```text
tool calls
investigation duration
LLM requests
token usage
estimated cost
```

---

## 1.12 End-to-End Command

Provide a simple developer command for running the scenario.

Target experience:

```bash
make eval
```

Conceptually it should:

```text
start environment
generate normal traffic
inject failure
generate incident
run investigation
evaluate diagnosis
print result
```

Exact implementation may evolve.

---

## Milestone 1 Exit Criteria

Milestone 1 is complete only when:

- checkout-api runs
- inventory-api runs
- Prometheus collects metrics
- normal traffic works
- downstream latency fault can be reproducibly enabled
- checkout latency visibly increases
- agent receives the incident
- agent can query metrics
- agent can inspect service dependencies
- agent identifies `INVENTORY_DOWNSTREAM_LATENCY`
- diagnosis includes supporting evidence
- evaluator compares result against ground truth
- the complete scenario is reproducible
- tests pass
- CI passes
- README explains how to run the demo
- architecture is documented

Once complete, tag:

```text
v0.1.0
```

Do not add Kubernetes, traces, more services, or many additional scenarios before completing this milestone.

---

# Milestone 2 — Multi-Signal Investigation

## Goal

Expand investigation beyond metrics.

Introduce logs and distributed traces while preserving the same tool-driven architecture.

---

## 2.1 OpenTelemetry

Instrument application services with OpenTelemetry.

Capture traces for:

```text
checkout request
checkout -> inventory HTTP call
inventory request
```

---

## 2.2 Trace Backend

Add an appropriate local trace backend.

The specific choice should be based on simplicity and integration quality.

Potential option:

```text
Grafana Tempo
```

---

## 2.3 Logs

Introduce structured application logs.

Logs should contain useful correlation fields such as:

```text
service
timestamp
request_id
trace_id
severity
```

A dedicated log backend should only be added if needed by the investigation interface.

---

## 2.4 New Agent Tools

Possible tools:

```text
query_logs()
query_traces()
```

The agent should decide when metrics are sufficient and when additional evidence is required.

---

## 2.5 Agent Observability

Instrument the investigation agent itself using OpenTelemetry.

An investigation should produce spans similar to:

```text
investigation
|
+-- llm.request
|
+-- tool.query_metrics
|
+-- llm.request
|
+-- tool.query_traces
|
+-- tool.query_logs
|
+-- llm.final_diagnosis
```

---

## 2.6 Additional Incidents

Add a small number of scenarios requiring different evidence.

Possible candidates:

- downstream HTTP failure
- dependency timeout
- HTTP 5xx spike

Keep the suite small.

Each new scenario must have explicit ground truth.

---

## Milestone 2 Exit Criteria

The system can investigate incidents using:

```text
metrics
logs
traces
service metadata
```

Agent investigations are themselves traceable.

Multiple incident classes pass through the same evaluation framework.

Target release:

```text
v0.2.0
```

---

# Milestone 3 — Evaluation Platform

## Goal

Turn evaluation into a substantial engineering component of the project.

This milestone is one of the project's primary differentiators.

---

## 3.1 Scenario Runner

Build a reusable scenario execution framework.

Responsibilities:

```text
start/reset environment
apply fault
generate traffic
wait for observable state
create incident
invoke agent
collect result
clean up
```

---

## 3.2 Scenario Catalog

Expand to a meaningful but controlled benchmark.

Possible incidents:

- downstream latency
- downstream 5xx failure
- dependency timeout
- database connection exhaustion
- cache failure
- CPU pressure
- memory pressure
- retry storm
- rate-limit exhaustion
- configuration error

Do not add scenarios that cannot be reproduced reliably.

---

## 3.3 Evaluation Metrics

Measure:

```text
root-cause accuracy
investigation success rate
evidence correctness
tool-call count
unnecessary tool calls
investigation duration
LLM request count
input tokens
output tokens
estimated cost
tool failures
retries
```

---

## 3.4 Evidence Evaluation

Move beyond checking only root-cause labels.

Evaluate whether the diagnosis contains evidence that actually supports the conclusion.

Possible approaches:

- deterministic evidence requirements per scenario
- structured evidence categories
- exact metric/tool references
- separate model-based evaluation only where deterministic evaluation is insufficient

Prefer deterministic evaluation where possible.

---

## 3.5 Regression Testing

Allow changes to be compared.

Example:

```text
baseline prompt
vs
new prompt
```

or:

```text
model A
vs
model B
```

Comparison should include:

```text
accuracy
latency
tool usage
cost
```

---

## 3.6 Reports

Produce machine-readable and human-readable evaluation results.

Possible artifacts:

```text
evaluation.json
evaluation.md
```

Eventually CI or scheduled workflows may publish benchmark results.

---

## Milestone 3 Exit Criteria

The repository contains a reproducible incident benchmark and can quantitatively evaluate agent behavior.

Target release:

```text
v0.3.0
```

At this point the project should already be considered a complete and strong portfolio project.

Everything after this milestone is optional expansion.

---

# Milestone 4 — Kubernetes Investigation

## Goal

Move the simulated production environment to a Kubernetes-style runtime and allow the agent to investigate infrastructure-level failures.

Use a lightweight local Kubernetes environment.

Potential options:

```text
kind
k3d
```

Choose based on simplicity.

---

## 4.1 Kubernetes Deployment

Deploy the application services to the local cluster.

Introduce:

- Deployments
- Services
- ConfigMaps
- resource limits
- readiness checks
- liveness checks

Helm may be introduced if it meaningfully improves reproducibility.

---

## 4.2 Kubernetes Tool

Add a constrained, read-only investigation interface.

Possible operations:

```text
inspect_pods
inspect_deployment
inspect_events
inspect_resource_usage
inspect_config
```

Avoid unrestricted Kubernetes access.

---

## 4.3 Kubernetes Scenarios

Potential incidents:

- pod crash loop
- OOM kill
- CPU throttling
- failed readiness probe
- bad configuration
- unavailable replicas

---

## Milestone 4 Exit Criteria

The agent can correlate application-level observability with Kubernetes state.

Target release:

```text
v0.4.0
```

---

# Milestone 5 — Deployment and Change Intelligence

## Goal

Allow investigations to consider recent system changes.

Many real incidents are correlated with deployments or configuration changes.

---

## 5.1 Deployment Metadata

Expose deployment information to the agent.

Potential fields:

```text
service
version
deployment timestamp
commit SHA
image version
configuration version
```

---

## 5.2 Git Metadata

Add a constrained interface for inspecting relevant Git history.

Possible tool:

```text
inspect_recent_changes(service)
```

The agent should not receive unrestricted repository access unless justified.

---

## 5.3 Bad Deployment Scenario

Create a scenario where a known deployment introduces the incident.

The agent should correlate:

```text
incident start time
deployment time
changed service
observable failure
```

---

## Milestone 5 Exit Criteria

The agent can use deployment/change information as supporting evidence without assuming every incident is caused by a recent deployment.

Target release:

```text
v0.5.0
```

---

# Milestone 6 — Platform Integrations

## Goal

Introduce architecture only when the project has become complex enough to justify it.

Potential additions include:

- MCP
- integration with the separate LLM Gateway project
- richer model routing
- persistent investigation history

---

## 6.1 LLM Gateway Integration

Replace or supplement the direct provider implementation with the separate LLM Gateway project.

Potential capabilities:

```text
provider routing
model selection
retry/failover
centralized token accounting
cost tracking
LLM observability
rate limiting
```

Project 2 must remain independently runnable.

The gateway should be an optional integration.

---

## 6.2 MCP

Consider exposing investigation tools through MCP if multiple clients or agents need to consume the same capabilities.

Possible MCP-exposed tools:

```text
Prometheus
logs
traces
Kubernetes
deployment metadata
Git metadata
```

MCP should not replace simple internal interfaces unless interoperability becomes valuable.

---

## 6.3 Investigation Persistence

Add persistent storage only if useful.

Potential reasons:

- historical investigation comparison
- evaluation history
- audit trails
- debugging
- model comparison

PostgreSQL may be appropriate at this stage.

---

# Milestone 7 — Cloud Deployment

## Goal

Demonstrate cloud and infrastructure-as-code capabilities if doing so adds portfolio value beyond the existing local/Kubernetes environment.

This milestone is optional.

Potential technologies:

```text
AWS
Terraform
Kubernetes
Helm
GitHub Actions
```

Possible architecture:

```text
AWS
|
+-- managed/container runtime
|
+-- Prometheus-compatible metrics
|
+-- tracing/logging
|
+-- AI SRE Agent
```

Cloud infrastructure should remain cost-conscious.

Do not build a large cloud platform solely for portfolio optics.

---

# Milestone 8 — Advanced Investigation Research

Optional future experimentation may include:

- parallel hypothesis exploration
- multiple specialized investigation agents
- investigation memory
- dynamic tool selection
- planner/executor separation
- confidence calibration
- automated remediation proposals
- human approval workflows
- safe remediation execution
- model comparison
- prompt optimization
- incident summarization
- postmortem generation

These should only be explored after the core evaluation system is mature.

---

# Explicit Non-Goals for Early Development

Before `v0.1.0`, do not add:

- Kubernetes
- AWS
- Terraform
- PostgreSQL
- Redis
- frontend
- MCP
- multiple models
- multiple LLM providers
- agent frameworks
- complex microservice architecture
- automated remediation
- large incident catalog

Before `v0.2.0`, avoid expanding the system unless the first metrics-based investigation works reliably.

---

# Development Rule

When considering a new technology or feature, ask:

```text
Does this help us investigate incidents better,
evaluate the agent better,
or make the system meaningfully more production-quality?
```

If the answer is no, defer it.

---

# Target Portfolio Story

The final project should make it possible to demonstrate:

```text
production-style observable services
        +
reproducible incidents
        +
LLM tool use
        +
autonomous investigation
        +
structured root-cause diagnosis
        +
ground-truth evaluation
        +
agent observability
        +
backend/platform engineering
```

The strength of the project should come from measurable engineering quality rather than the number of technologies used.
