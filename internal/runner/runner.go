// Package runner executes a scenario against the observable environment and
// collects its investigation result. It does not load or evaluate ground truth.
// Only the incident crosses the boundary into the investigator.
package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ipekutku/ai-sre-agent/internal/agent"
	"github.com/ipekutku/ai-sre-agent/internal/incident"
	"github.com/ipekutku/ai-sre-agent/internal/scenarios"
)

// Environment provides the runner's environment controls. These operations must
// use uninstrumented I/O so fault details cannot reach the agent's tools.
// GenerateTraffic must stop, drain in-flight requests, and return within a
// bounded time after ctx ends: cleanup waits for it without its own timeout.
type Environment interface {
	WaitReady(context.Context) error
	ClearFault(context.Context) error
	GenerateTraffic(context.Context, int) scenarios.TrafficStats
	ApplyFault(context.Context, scenarios.Fault) error
	WaitForTrigger(context.Context, scenarios.Trigger) (float64, error)
	WaitForTraces(context.Context, string, time.Time) error
	WaitForLogs(context.Context, string, time.Time) error
}

// Investigator receives only the incident, never the scenario or ground truth.
type Investigator interface {
	Investigate(context.Context, incident.Incident) (agent.Result, error)
}

// Config controls traffic and the baseline period. The runner fixes the other
// timeouts: readiness (2m), backend catch-up (2m shared), and fault restoration (10s).
type Config struct {
	RPS      int
	Baseline time.Duration
}

// Timings records elapsed wall time for each attempted step, including failures.
// Cleanup includes stopping and draining traffic before restoring the fault.
// Unattempted steps have zero duration.
type Timings struct {
	Ready, FaultReset, Baseline, Fault, TriggerWait, BackendWait, Investigation, Cleanup time.Duration
}

// RunRecord contains observations and outcomes only, with no ground truth.
type RunRecord struct {
	ScenarioID           string
	Incident             incident.Incident
	Timings              Timings
	TriggerValue         float64
	Traffic              scenarios.TrafficStats
	Result               agent.Result
	InvestigationStarted bool
	InvestigationError   error
	CleanupError         error
	// Error is the lifecycle error returned by Run, including cleanup failure.
	// Investigation failures are kept separately unless caused by cancellation.
	Error error
}

// Runner executes one scenario at a time with its configured dependencies.
type Runner struct {
	env          Environment
	investigator Investigator
	logger       *slog.Logger
	cfg          Config
}

// New constructs a runner; config and scenario validation precede environment I/O.
func New(env Environment, investigator Investigator, logger *slog.Logger, cfg Config) *Runner {
	return &Runner{env: env, investigator: investigator, logger: logger, cfg: cfg}
}

// Run executes readiness, fault reset, traffic, baseline, fault injection, trigger
// and backend waits, investigation, and cleanup. Non-cancellation investigation
// errors belong to the result (a diagnosis FAIL), not infrastructure errors.
// Every return after the environment is ready attempts fault restoration using
// an independent bounded context; nothing is restored if readiness fails. Traffic is stopped and drained before restoration.
// Cleanup errors are returned even if the investigation produced a diagnosis;
// errors.Join preserves an earlier lifecycle error as well.
func (r *Runner) Run(ctx context.Context, sc scenarios.Scenario) (record RunRecord, err error) {
	record.ScenarioID, record.Incident = sc.ID, sc.Incident
	defer func() { record.Error = err }()
	if err := sc.Validate(); err != nil {
		return record, fmt.Errorf("invalid scenario: %w", err)
	}
	if r.cfg.RPS <= 0 || r.cfg.RPS > int(time.Second) || r.cfg.Baseline < 0 {
		return record, fmt.Errorf("runner requires rps in [1, %d] and a non-negative baseline", int(time.Second))
	}
	if err := ctx.Err(); err != nil {
		return record, err
	}

	var ready bool
	var stopTraffic context.CancelFunc
	var trafficDone chan scenarios.TrafficStats
	defer func() {
		if ready {
			record.CleanupError = r.cleanup(stopTraffic, trafficDone, &record)
			err = errors.Join(err, record.CleanupError)
		}
		// Polling backends may return their last observation error instead of
		// ctx.Err(). Preserve cancellation even then, or during cleanup itself.
		if ctx.Err() != nil && !errors.Is(err, ctx.Err()) {
			err = errors.Join(err, ctx.Err())
		}
	}()

	if err := r.waitReady(ctx, &record); err != nil {
		return record, err
	}
	ready = true
	if err := timed(&record.Timings.FaultReset, func() error { return r.env.ClearFault(ctx) }); err != nil {
		return record, fmt.Errorf("reset fault: %w", err)
	}

	trafficCtx, cancel := context.WithCancel(ctx)
	stopTraffic = cancel
	trafficDone = make(chan scenarios.TrafficStats, 1)
	go func() { trafficDone <- r.env.GenerateTraffic(trafficCtx, r.cfg.RPS) }()

	if err := r.baseline(ctx, &record); err != nil {
		return record, err
	}
	if err := r.applyFault(ctx, sc, &record); err != nil {
		return record, err
	}
	if err := timed(&record.Timings.TriggerWait, func() error {
		var triggerErr error
		record.TriggerValue, triggerErr = r.env.WaitForTrigger(ctx, sc.Trigger)
		return triggerErr
	}); err != nil {
		return record, err
	}
	if err := r.waitBackends(ctx, &record); err != nil {
		return record, err
	}

	record.InvestigationStarted = true
	_ = timed(&record.Timings.Investigation, func() error {
		record.Result, record.InvestigationError = r.investigator.Investigate(ctx, record.Incident)
		return record.InvestigationError
	})
	if ctx.Err() != nil {
		return record, ctx.Err()
	}
	if errors.Is(record.InvestigationError, context.Canceled) {
		return record, record.InvestigationError
	}
	return record, nil
}

func (r *Runner) waitReady(ctx context.Context, record *RunRecord) error {
	return timed(&record.Timings.Ready, func() error {
		r.logger.Info("waiting for environment")
		readyCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		if err := r.env.WaitReady(readyCtx); err != nil {
			return fmt.Errorf("environment not ready (did you run make up?): %w", err)
		}
		return nil
	})
}

func (r *Runner) baseline(ctx context.Context, record *RunRecord) error {
	return timed(&record.Timings.Baseline, func() error {
		r.logger.Info("collecting baseline", "duration", r.cfg.Baseline, "rps", r.cfg.RPS)
		timer := time.NewTimer(r.cfg.Baseline)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return ctx.Err()
		}
	})
}

func (r *Runner) applyFault(ctx context.Context, sc scenarios.Scenario, record *RunRecord) error {
	return timed(&record.Timings.Fault, func() error {
		r.logger.Info("injecting fault", "service", sc.Fault.Service, "type", sc.Fault.Type)
		if err := r.env.ApplyFault(ctx, sc.Fault); err != nil {
			return fmt.Errorf("inject fault: %w", err)
		}
		return nil
	})
}

func (r *Runner) waitBackends(ctx context.Context, record *RunRecord) error {
	return timed(&record.Timings.BackendWait, func() error {
		triggered := time.Now()
		r.logger.Info("alert condition met; waiting for traces and logs to be searchable", "value", record.TriggerValue)
		waitCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		if err := r.env.WaitForTraces(waitCtx, record.Incident.Service, triggered); err != nil {
			return fmt.Errorf("traces not searchable in tempo: %w", err)
		}
		traceWait := time.Since(triggered)
		if err := r.env.WaitForLogs(waitCtx, record.Incident.Service, triggered); err != nil {
			return fmt.Errorf("logs not searchable in loki: %w", err)
		}
		r.logger.Info("raising incident", "incident_id", record.Incident.IncidentID,
			"trace_wait", traceWait.Round(100*time.Millisecond),
			"total_wait", time.Since(triggered).Round(100*time.Millisecond))
		return nil
	})
}

func (r *Runner) cleanup(stop context.CancelFunc, done <-chan scenarios.TrafficStats, record *RunRecord) error {
	return timed(&record.Timings.Cleanup, func() error {
		if stop != nil {
			stop()
			record.Traffic = <-done
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := r.env.ClearFault(cleanupCtx); err != nil {
			return fmt.Errorf("cleanup: clear fault: %w", err)
		}
		return nil
	})
}

func timed(duration *time.Duration, f func() error) error {
	start := time.Now()
	defer func() { *duration = time.Since(start) }()
	return f()
}
