# ADR 0007: Environment reset by recreating containers

- Status: Accepted
- Date: 2026-10-18

## Context

Scenario runs share one environment. Prometheus, Tempo and Loki keep every earlier run's data,
and the agent's range queries can reach it. A real case from Milestone 2: an
`inventory-latency` diagnosis quoted the 2 s timeouts of the `inventory-timeout` run before it.
A 300 s cooldown between runs separated the spikes in time but did not hide them, and made a
three-scenario suite take about 15 minutes. Milestone 3 needs runs that cannot see each other
before it can report benchmark numbers.

Options:

1. **Recreate the containers before each run.** All backend data lives in the container
   filesystem (Tempo, Loki) or an anonymous volume (Prometheus), and fault state is in service
   memory.
2. **Clamp every tool's time range to the start of the run.** No container work, but it is a
   hidden limit on the agent that the incident does not explain, and it resets neither service
   state nor backend internals.
3. **Keep cooldowns.** Does not fix contamination.

## Decision

- **Recreate.** Before readiness, the runner runs
  `docker compose -f <file> up --detach --no-build --force-recreate --renew-anon-volumes
  inventory-api checkout-api prometheus tempo loki` (`scenarios.ComposeReset`).
- **Fixed command, runner only.** The argument list is built in code and run without a shell.
  The only input is the compose file path, an operator flag (`-compose-file`). Nothing from a
  scenario file or the model reaches it, and the agent's tools have no Docker access.
- **No rebuild.** `--no-build` keeps a series of runs on the images `make up` built, so the code
  under test cannot change partway through a suite.
- **Grafana is left running.** It stores no telemetry, and keeping it avoids reloading the UI.
- **On by default**, with `-reset=false` for fast manual loops that accept shared history.
- **Failure handling.** A reset has a 2 min timeout. Failure or cancellation is a setup error
  (exit 2); no fault has been injected, so fault restoration is skipped, as for a readiness
  failure.

## Consequences

- Runs no longer need a cooldown; `COOLDOWN` defaults to 0 and only matters with
  `-reset=false`.
- Each run pays for the recreation and readiness wait: about 11 s and 25 s (measured
  2026-10-26), which replaces a 300 s cooldown. Prometheus only has the run's own
  baseline as history, so the baseline length bounds what "compared with earlier" can mean.
- An investigation's trace in Tempo, and the services' telemetry, last only until the next run
  starts. Results that must outlive a run need to be written by the runner.
- The runner needs the Docker CLI and access to the Docker daemon, which `make up` already
  requires. This is the host runner's privilege, not the agent's, and is unrelated to ADR 0004's
  rejection of Docker socket access for log collection inside the environment.
- The new backends start within about 0.3 s of the new services, after the old containers have
  stopped. Measured 2026-10-26: the only Loki lines from before the reset returned were the
  recreated services' startup logs; no earlier run's telemetry reached the new backends.
- Cancelling a reset kills the compose CLI and can leave containers stopped; the next reset
  recreates them.
