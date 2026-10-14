# ADR 0005: Structured trace and log queries with a service allowlist

- Status: Accepted
- Date: 2026-10-14

## Context

Milestone 2 gives the agent access to traces (Tempo, ADR 0003) and logs (Loki, ADR 0004). The
existing `query_metrics` tool accepts raw PromQL, which is safe because Prometheus holds only
service metrics.

Tempo and Loki are different. Once agent investigations are traced (roadmap §2.5), Tempo will
also hold the agent's own spans: LLM requests, tool calls, and diagnosis attempts. An agent that
can read those can read its own earlier reasoning and conclusions. Raw TraceQL or LogQL would let
the model ask for any `service.name`, and preventing that by inspecting arbitrary query strings
is fragile.

## Decision

- **Structured input.** `query_traces` (and, later, `query_logs`) takes structured parameters
  and builds the query language itself. The parameters for `query_traces` are: a service from
  the catalog, a minimum duration, an error status, a lookback window, and a limit for searches,
  or a trace ID for a single trace. Every value is validated (enum, integer range, literal, or a
  32-character hex ID) before it goes into a query, so there is nothing to escape or inject.
- **Catalog allowlist on output.** Responses are filtered by service: only spans whose
  `service.name` is in the runner's service catalog are returned. A trace root from any other
  service is hidden. The query restricts the search, and the filter guarantees the result.
- **One tool, two modes.** `query_traces` either searches (returning trace summaries with a few
  matching spans each) or gets one trace by ID (returning a flat span list). The two modes
  cannot be combined.
- **Attribute allowlist.** Spans carry only the HTTP method, response status code, route, server
  address, and error type. Peer addresses, user agents, and SDK details are dropped.

## Consequences

- The tools are deliberately inconsistent with `query_metrics`. Raw PromQL stays because it is
  expressive and Prometheus has nothing to hide. If Prometheus later receives agent metrics,
  this decision should be revisited.
- The model cannot express arbitrary TraceQL queries, such as structural queries or
  attribute filters beyond those exposed. New parameters can be added when a scenario needs
  them, each validated the same way.
- Isolation is testable: tests feed the tool Tempo responses that include agent spans and check
  that none reach the output.
- Tempo search does not return a trace's total span count or error status, so search results
  show the matching spans (with their status) instead. The model gets the full picture by
  fetching a trace by ID.
