# AI SRE / Incident Investigation Agent

## Overview

This project is an AI-powered SRE / incident investigation system designed to investigate production-style incidents by interacting with observability and infrastructure tools.

The goal is not to build a generic chatbot that receives logs and asks an LLM what went wrong.

Instead, the system should behave more like an engineer performing an investigation:

1. Receive an alert or incident.
2. Decide what information is needed.
3. Query observability systems through explicit tools.
4. Gather and correlate evidence.
5. Form and refine hypotheses.
6. Identify the most likely root cause.
7. Produce a structured diagnosis with supporting evidence.
8. Recommend appropriate remediation.
9. Evaluate the diagnosis against known ground truth.

The project is intended to demonstrate production-quality backend, platform, SRE, and AI infrastructure engineering.

---

## Motivation

Most simple AI incident-analysis demos provide the model with all relevant logs, metrics, or traces directly inside the prompt.

That avoids one of the most important parts of real incident investigation: deciding what information to inspect.

This project instead treats the LLM as an investigation agent.

The model receives limited initial context and must retrieve additional information through constrained tools.

Conceptually:

```text
Alert / Incident
       |
       v
Investigation Agent
       |
       +--> Metrics
       +--> Logs
       +--> Traces
       +--> Service Information
       +--> Kubernetes
       +--> Deployment Metadata
       |
       v
Evidence
       |
       v
Hypothesis / Root Cause
       |
       v
Structured Diagnosis
       |
       v
Recommended Remediation
```

The system should demonstrate that an AI agent can perform bounded, observable, and measurable investigation rather than merely generate plausible explanations.

---

# Project Goals

The project should demonstrate the ability to design and ship production-quality systems involving:

- Go backend development
- distributed systems
- observability
- Prometheus
- OpenTelemetry
- Docker
- Kubernetes
- structured tool use by LLMs
- agent orchestration
- evaluation of AI systems
- reliability engineering
- security boundaries
- CI/CD
- infrastructure automation
- cost and latency measurement

The project should remain focused.

Technologies should only be introduced when they solve a concrete problem.

---

# Core Design Principles

## 1. Evidence-Driven Investigation

The agent must support its diagnosis using evidence collected through tools.

A diagnosis should not simply be a plausible explanation generated from the alert text.

The agent should identify observable signals supporting its conclusion.

Examples include:

- latency changes
- error-rate increases
- dependency failures
- resource saturation
- trace latency
- pod restarts
- deployment changes
- connection-pool exhaustion
- timeout patterns

---

## 2. Explicit Tool Use

The agent should interact with systems through explicit interfaces.

Examples:

```text
query_metrics()
query_logs()
query_traces()
inspect_service()
inspect_kubernetes()
inspect_recent_deployments()
```

The LLM should decide which tools to use during an investigation.

Tools should be constrained, validated, observable, and independently testable.

The agent should never receive unrestricted shell access.

---

## 3. Ground Truth Must Be Isolated from the Agent

Incident scenarios are controlled by the project and therefore have known root causes.

Ground-truth information exists for evaluation purposes only.

The investigation agent must never be able to access it.

Conceptually:

```text
Scenario
   |
   +--> Fault Injection
   |
   +--> Observable System
   |        |
   |        v
   |   Investigation Agent
   |        |
   |        v
   |     Diagnosis
   |
   +--> Ground Truth
            |
            v
         Evaluator
```

The scenario runner and evaluator may access ground truth.

The investigation agent may not.

This separation protects the integrity of the evaluation system.

---

## 4. Evaluation Is a First-Class Component

The project should not be evaluated only through successful demo runs.

Every reproducible incident should define an expected root cause.

Example:

```text
Scenario:
database connection pool exhausted

Expected:
DATABASE_CONNECTION_EXHAUSTION

Agent diagnosis:
DATABASE_CONNECTION_EXHAUSTION

Result:
PASS
```

The evaluation framework should eventually measure:

- root-cause diagnosis accuracy
- successful investigation rate
- evidence correctness
- evidence completeness
- number of tool calls
- unnecessary tool calls
- investigation latency
- LLM request count
- input token usage
- output token usage
- estimated LLM cost
- tool failures
- retries
- invalid diagnoses

This should allow agent changes, models, prompts, and investigation strategies to be compared quantitatively.

---

## 5. The Agent Itself Must Be Observable

The investigation system is itself a production-style application.

It should therefore expose observability about its own behavior.

Eventually an investigation should be inspectable as a trace such as:

```text
incident.investigation
|
+-- llm.request
|
+-- tool.query_metrics
|
+-- llm.request
|
+-- tool.inspect_service
|
+-- tool.query_traces
|
+-- llm.request
|
+-- diagnosis
```

Useful telemetry includes:

- investigation duration
- LLM request duration
- tool latency
- tool failures
- number of tool calls
- token usage
- estimated cost
- retries
- investigation failures

---

## 6. Production Engineering Over Framework Magic

The agent orchestration loop should initially be implemented directly in the project.

Agent frameworks such as LangChain, CrewAI, or AutoGen should not be introduced unless they solve a concrete future requirement.

The application should explicitly control:

- tool dispatch
- context propagation
- tool-call limits
- timeouts
- retries
- output validation
- LLM failures
- tool failures
- token accounting
- investigation duration
- structured diagnosis validation

This keeps important production behavior visible and understandable.

---

## 7. Secure and Constrained Agent Capabilities

An AI SRE agent should not automatically receive arbitrary infrastructure access.

Tools should provide narrow capabilities.

Examples:

```text
query_metrics(promql)
inspect_service(service)
```

rather than:

```text
execute_shell(command)
```

Tool implementations should eventually include:

- input validation
- request timeouts
- output-size limits
- known endpoints
- authentication boundaries
- read-only access where appropriate
- investigation-level tool budgets
- context cancellation
- structured audit logs

Future Kubernetes access should initially be read-only.

---

# Initial Architecture

The first version should remain deliberately small.

```text
                         +--------------------+
                         | Incident / Alert   |
                         +---------+----------+
                                   |
                                   v
                         +--------------------+
                         | Investigation      |
                         | Agent              |
                         +---------+----------+
                                   |
                       +-----------+-----------+
                       |                       |
                       v                       v
              +----------------+      +----------------+
              | query_metrics  |      | inspect_service|
              +-------+--------+      +-------+--------+
                      |                       |
                      v                       v
              +----------------+      +----------------+
              | Prometheus     |      | Service        |
              |                |      | Metadata       |
              +----------------+      +----------------+

                                   |
                                   v
                         +--------------------+
                         | Structured         |
                         | Diagnosis          |
                         +---------+----------+
                                   |
                                   v
                         +--------------------+
                         | Evaluator          |
                         +---------+----------+
                                   |
                                   v
                         Ground Truth Comparison
```

---

# Initial Application Environment

Milestone 1 contains two small services.

```text
Load Generator
      |
      v
+-------------------+
| checkout-api      |
|                   |
| GET /checkout     |
+---------+---------+
          |
          | HTTP
          v
+-------------------+
| inventory-api     |
|                   |
| GET /inventory    |
+-------------------+
```

Both services should expose Prometheus metrics.

The application intentionally includes a mechanism for reproducible fault injection.

---

# First Incident Scenario

The first scenario is a downstream latency failure.

Normal behavior:

```text
checkout-api
      |
      v
inventory-api

inventory latency: low
checkout latency: low
```

Injected incident:

```text
inventory-api response delay increases significantly
```

Observable result:

```text
checkout-api request latency increases

checkout-api CPU remains normal

inventory-api server latency increases

checkout-api outbound request latency to inventory-api increases
```

The alert should identify elevated latency in `checkout-api`.

The alert should not reveal the root cause.

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

Expected ground-truth root cause:

```text
INVENTORY_DOWNSTREAM_LATENCY
```

The investigation agent must infer this through observable evidence.

---

# Initial Agent Tools

Milestone 1 exposes only two investigation tools.

## query_metrics

Queries Prometheus using PromQL.

Conceptually:

```go
type MetricsTool interface {
    Query(ctx context.Context, promQL string) (MetricResult, error)
}
```

The agent can use this tool to inspect:

- request latency
- request counts
- error rates
- outbound dependency latency

---

## inspect_service

Returns limited service metadata.

Conceptually:

```go
type ServiceInspector interface {
    Inspect(ctx context.Context, service string) (ServiceInfo, error)
}
```

Example result:

```json
{
  "name": "checkout-api",
  "status": "healthy",
  "version": "1.0.0",
  "dependencies": [
    "inventory-api"
  ]
}
```

This tool must not reveal injected faults or ground-truth information.

---

# Structured Diagnosis

Agent output should be machine-readable.

Example:

```json
{
  "incident_id": "inc-001",
  "root_cause": {
    "code": "INVENTORY_DOWNSTREAM_LATENCY",
    "summary": "checkout-api latency is caused by elevated response latency from inventory-api"
  },
  "confidence": 0.94,
  "evidence": [
    {
      "source": "prometheus",
      "observation": "checkout-api p95 latency increased significantly"
    },
    {
      "source": "prometheus",
      "observation": "checkout-api outbound requests to inventory-api show matching latency"
    },
    {
      "source": "prometheus",
      "observation": "inventory-api server latency increased during the incident"
    }
  ],
  "recommended_actions": [
    "Investigate the cause of elevated latency in inventory-api",
    "Check recent inventory-api changes if the latency correlates with a deployment"
  ]
}
```

Structured output allows deterministic evaluation and downstream processing.

---

# Scenario Design

Scenarios should eventually be represented as reproducible configuration.

Example:

```text
scenarios/
└── inventory-latency/
    ├── scenario.yaml
    └── ground-truth.yaml
```

Possible scenario definition:

```yaml
id: inventory-latency

alert:
  name: HighCheckoutLatency
  service: checkout-api

fault:
  service: inventory-api
  type: latency
  latency_ms: 800
```

Ground truth:

```yaml
expected_root_cause:
  code: INVENTORY_DOWNSTREAM_LATENCY

required_evidence:
  - checkout latency elevated
  - checkout to inventory latency elevated
  - inventory latency elevated
```

The agent must never receive `ground-truth.yaml`.

---

# Evaluation Architecture

```text
Scenario Runner
      |
      +--> start environment
      |
      +--> generate traffic
      |
      +--> inject fault
      |
      +--> generate incident
                     |
                     v
             Investigation Agent
                     |
                     v
                diagnosis.json
                     |
                     v
                 Evaluator
                /         \
               /           \
      ground truth       diagnosis
               \           /
                \         /
                    v
                eval.json
```

Evaluation should eventually support running multiple scenarios and producing aggregate results.

Example future report:

```text
Scenarios:                  20
Diagnosis accuracy:         85%
Evidence correctness:       90%
Median tool calls:          6
Median investigation time:  7.8s
Average investigation cost: $0.012
```

---

# LLM Provider Boundary

The investigation agent should depend on a small internal LLM interface rather than a specific provider throughout the codebase.

Conceptually:

```go
type Client interface {
    Generate(
        ctx context.Context,
        req Request,
    ) (Response, error)
}
```

Initially:

```text
AI SRE Agent
     |
     v
LLM Provider
```

Later:

```text
AI SRE Agent
     |
     v
LLM Client Interface
     |
     +--> Direct Provider
     |
     +--> LLM Gateway
```

This allows the separate LLM Gateway portfolio project to be integrated later without making it a dependency of early development.

---

# MCP

MCP is intentionally not part of the initial system.

Direct Go tool interfaces are simpler and sufficient for the first milestones.

MCP may become appropriate later if observability or infrastructure tools need to be exposed to multiple independent AI clients.

It should only be introduced when it solves an actual interoperability problem.

---

# Technology Strategy

Initial technology choices:

| Area | Technology |
|---|---|
| Primary language | Go |
| Service runtime | Go |
| Agent runtime | Go |
| Metrics | Prometheus |
| Local environment | Docker Compose |
| CI | GitHub Actions |
| LLM | Provider behind internal interface |
| Configuration | YAML where appropriate |
| Tests | Go testing |
| Build automation | Makefile |

Technologies intentionally deferred:

- Kubernetes
- OpenShift
- AWS
- Terraform
- Helm
- PostgreSQL
- Redis
- frontend
- MCP
- complex agent frameworks

These may be introduced when later milestones require them.

---

# Repository Direction

Expected repository structure:

```text
ai-sre-agent/
|
├── cmd/
|   ├── agent/
|   ├── checkout-api/
|   ├── inventory-api/
|   └── scenario-runner/
|
├── internal/
|   ├── agent/
|   ├── llm/
|   ├── tools/
|   ├── evaluation/
|   └── scenarios/
|
├── scenarios/
|
├── deploy/
|   ├── docker-compose.yml
|   └── prometheus/
|
├── docs/
|   ├── PROJECT.md
|   ├── ROADMAP.md
|   └── adr/
|
├── .github/
|   └── workflows/
|
├── AGENTS.md
├── CLAUDE.md
├── Makefile
├── go.mod
└── README.md
```

The exact structure may evolve as implementation reveals better boundaries.

---

# Engineering Quality

The repository should eventually include:

- strong README
- architecture documentation
- automated formatting
- static analysis
- unit tests
- integration tests
- CI
- reproducible local environment
- deterministic fault injection
- structured configuration
- structured logs
- observability
- evaluation reports
- architecture decision records where useful
- security documentation
- measurable results

Code should favor readability and explicit behavior over unnecessary abstraction.

---

# AI-Assisted Development Philosophy

AI coding tools are expected to be used heavily during development.

They are development accelerators, not owners of architectural decisions.

Human responsibility remains with:

- architecture
- requirements
- scope
- code review
- testing
- evaluation design
- security
- reliability
- production quality
- acceptance of generated code

Development tasks should therefore be given to coding agents in bounded increments with clear acceptance criteria.

---

# Success Criteria

The project succeeds if it demonstrates more than an LLM producing plausible incident explanations.

A successful system should be able to show that:

1. Incidents are reproducible.
2. Root causes are known independently from the agent.
3. The agent must gather information through tools.
4. Investigations produce observable evidence.
5. Diagnoses are structured and machine-evaluable.
6. Agent performance can be measured.
7. Changes to prompts, models, tools, or orchestration can be evaluated objectively.
8. The system is engineered with production concerns such as reliability, observability, security, testing, and cost in mind.

The objective is to build an engineered AI system, not an agent demo.
