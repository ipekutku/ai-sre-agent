# ADR 0002: Closed root-cause taxonomy

- Status: Accepted
- Date: 2026-10-03

## Context

The evaluator compares the agent's root cause with each scenario's ground truth. For the
result to be trustworthy:

1. a correct diagnosis must not fail because of wording,
2. passing must require investigation, not guessing,
3. results must be comparable across runs, prompts, and models.

Options considered:

- **Free-form codes.** The agent invents a label (e.g. `INVENTORY_SLOW_RESPONSES`).
  Realistic, but exact matching fails correct answers with different wording, which makes
  scores noisy and understated. Fixing that needs fuzzy matching or an LLM judge, which the
  roadmap defers to Milestone 3.
- **Closed taxonomy.** The agent must choose from a fixed list.

## Decision

Use a closed taxonomy: `diagnosis.Codes` (8 codes). The agent sees it in its system prompt and
as an `enum` in the `submit_diagnosis` tool schema. Diagnoses and ground-truth files with codes
outside the list are rejected.

The list must stay scenario-neutral. It covers plausible causes for the demo system, and each
wrong answer can be ruled out only with specific evidence (e.g. `CHECKOUT_INVENTORY_NETWORK_LATENCY`
versus `INVENTORY_DOWNSTREAM_LATENCY` differ only in inventory-api's own server latency).

## Consequences

- Scoring is deterministic exact matching.
- The agent sees the correct answer among the options, like a multiple-choice exam; real incidents
  have no such menu. A blind guess passes about 1 time in 8, so repeated runs and evidence
  evaluation (Milestone 3) are needed before drawing strong conclusions.
- New scenarios whose cause is not in the list require adding a code, which changes what the agent
  sees for every scenario. Re-run all scenarios after such a change.
- Revisit in Milestone 3, when evidence evaluation may make free-form or hierarchical codes viable.
