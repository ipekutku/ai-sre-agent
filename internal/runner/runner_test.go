package runner

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ipekutku/ai-sre-agent/internal/agent"
	"github.com/ipekutku/ai-sre-agent/internal/incident"
	"github.com/ipekutku/ai-sre-agent/internal/llm"
	"github.com/ipekutku/ai-sre-agent/internal/scenarios"
	"github.com/ipekutku/ai-sre-agent/internal/tools"
)

type fakeEnvironment struct {
	t         *testing.T
	mu        sync.Mutex
	events    []string
	started   chan struct{}
	stopping  chan struct{}
	drained   chan struct{}
	drainGate <-chan struct{}
	onStep    func(context.Context, string) error
	clears    int
	fault     bool
	since     time.Time
}

func newEnvironment(t *testing.T) *fakeEnvironment {
	return &fakeEnvironment{t: t, started: make(chan struct{}), stopping: make(chan struct{}), drained: make(chan struct{})}
}

func (e *fakeEnvironment) event(name string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, name)
}

func (e *fakeEnvironment) step(ctx context.Context, name string) error {
	e.event(name)
	if e.onStep != nil {
		return e.onStep(ctx, name)
	}
	return ctx.Err()
}

func (e *fakeEnvironment) WaitReady(ctx context.Context) error {
	assertDeadline(e.t, ctx, 2*time.Minute)
	return e.step(ctx, "ready")
}

func (e *fakeEnvironment) ClearFault(ctx context.Context) error {
	e.clears++
	// Restoration has an independent 10s deadline, whereas the initial reset
	// uses the caller's context. The tests use an unbounded caller context.
	name := "reset"
	if _, ok := ctx.Deadline(); ok {
		name = "cleanup"
		assertDeadline(e.t, ctx, 10*time.Second)
		if ctx.Err() != nil {
			e.t.Errorf("cleanup received ended context: %v", ctx.Err())
		}
		select {
		case <-e.started:
			select {
			case <-e.drained:
			default:
				e.t.Error("restoration began before traffic drained")
			}
		default:
		}
	}
	if err := e.step(ctx, name); err != nil {
		return err
	}
	e.fault = false
	return nil
}

func (e *fakeEnvironment) GenerateTraffic(ctx context.Context, rps int) scenarios.TrafficStats {
	if rps != 20 {
		e.t.Errorf("traffic rate = %d, want 20", rps)
	}
	e.event("traffic")
	close(e.started)
	<-ctx.Done()
	close(e.stopping)
	if e.drainGate != nil {
		<-e.drainGate
	}
	e.event("drained")
	close(e.drained)
	return scenarios.TrafficStats{OK: 12, Failed: 3}
}

func (e *fakeEnvironment) ApplyFault(ctx context.Context, f scenarios.Fault) error {
	<-e.started
	if f.Service != "inventory-api" || f.LatencyMS != 800 {
		e.t.Errorf("fault = %+v", f)
	}
	e.fault = true // Even a failed apply may have mutated the environment.
	return e.step(ctx, "fault")
}

func (e *fakeEnvironment) WaitForTrigger(ctx context.Context, trigger scenarios.Trigger) (float64, error) {
	if trigger.Query != "test_trigger" {
		e.t.Errorf("trigger = %+v", trigger)
	}
	return 0.8, e.step(ctx, "trigger")
}

func (e *fakeEnvironment) WaitForTraces(ctx context.Context, service string, since time.Time) error {
	assertDeadline(e.t, ctx, 2*time.Minute)
	if service != "checkout-api" || since.IsZero() {
		e.t.Errorf("trace wait = %s, %v", service, since)
	}
	e.since = since
	return e.step(ctx, "traces")
}

func (e *fakeEnvironment) WaitForLogs(ctx context.Context, service string, since time.Time) error {
	assertDeadline(e.t, ctx, 2*time.Minute)
	if service != "checkout-api" || since != e.since {
		e.t.Errorf("log wait = %s, %v; trace time = %v", service, since, e.since)
	}
	return e.step(ctx, "logs")
}

func assertDeadline(t *testing.T, ctx context.Context, maximum time.Duration) {
	t.Helper()
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > maximum {
		t.Errorf("context missing deadline bounded by %s", maximum)
	}
}

type fakeLLM func(context.Context, llm.Request) (llm.Response, error)

func (f fakeLLM) Generate(ctx context.Context, req llm.Request) (llm.Response, error) {
	return f(ctx, req)
}

func newTestRunner(t *testing.T, env *fakeEnvironment) (*Runner, scenarios.Scenario) {
	t.Helper()
	sc := scenarios.Scenario{
		ID: "test-scenario",
		Incident: incident.Incident{IncidentID: "inc-test", Alert: "HighCheckoutLatency",
			Service: "checkout-api", Severity: "warning", Description: "p95 above threshold"},
		Fault:   scenarios.Fault{Service: "inventory-api", Type: "latency", LatencyMS: 800},
		Trigger: scenarios.Trigger{Query: "test_trigger", Above: 0.5, TimeoutSeconds: 120},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	registry, err := tools.NewRegistry(logger, tools.Options{})
	if err != nil {
		t.Fatal(err)
	}
	client := fakeLLM(func(ctx context.Context, req llm.Request) (llm.Response, error) {
		if len(req.Messages) != 1 || !strings.Contains(req.Messages[0].Content[0].Text, sc.Incident.IncidentID) {
			t.Errorf("initial messages = %+v", req.Messages)
		}
		for _, secret := range []string{sc.ID, sc.Trigger.Query, "latency_ms", "800"} {
			if strings.Contains(req.Messages[0].Content[0].Text, secret) {
				t.Errorf("scenario detail %q reached investigator", secret)
			}
		}
		if err := env.step(ctx, "investigate"); err != nil {
			return llm.Response{}, err
		}
		return llm.Response{
			StopReason: llm.StopToolUse, Model: "test-model",
			Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.Block{{
				Type: llm.BlockToolUse, ToolUseID: "submit", ToolName: "submit_diagnosis",
				ToolInput: json.RawMessage(`{"root_cause":{"code":"INVENTORY_DOWNSTREAM_LATENCY","summary":"slow dependency"},"confidence":0.9,"evidence":[{"source":"prometheus","observation":"elevated latency"}],"recommended_actions":[]}`),
			}}},
		}, nil
	})
	return New(env, agent.New(client, registry, logger, agent.Config{}), logger, Config{RPS: 20, Baseline: time.Millisecond}), sc
}

func TestRunLifecycle(t *testing.T) {
	env := newEnvironment(t)
	r, sc := newTestRunner(t, env)
	record, err := r.Run(context.Background(), sc)
	if err != nil || record.Error != nil || record.CleanupError != nil || record.InvestigationError != nil {
		t.Fatalf("Run errors: %v, %+v", err, record)
	}
	want := []string{"ready", "reset", "traffic", "fault", "trigger", "traces", "logs", "investigate", "drained", "cleanup"}
	if !reflect.DeepEqual(env.events, want) {
		t.Errorf("steps = %v, want %v", env.events, want)
	}
	if env.fault || record.ScenarioID != sc.ID || record.Incident != sc.Incident || record.TriggerValue != 0.8 {
		t.Errorf("record = %+v, fault active = %v", record, env.fault)
	}
	if !record.InvestigationStarted || record.Result.Diagnosis.IncidentID != sc.Incident.IncidentID || record.Result.Stats.LLMRequests != 1 {
		t.Errorf("investigation not retained: %+v", record)
	}
	if record.Traffic != (scenarios.TrafficStats{OK: 12, Failed: 3}) {
		t.Errorf("traffic = %+v", record.Traffic)
	}
	tm := record.Timings
	for name, duration := range map[string]time.Duration{
		"ready": tm.Ready, "reset": tm.FaultReset, "baseline": tm.Baseline, "fault": tm.Fault,
		"trigger": tm.TriggerWait, "backends": tm.BackendWait, "investigation": tm.Investigation, "cleanup": tm.Cleanup,
	} {
		if duration <= 0 {
			t.Errorf("%s timing = %s", name, duration)
		}
	}
	if tm.Baseline < r.cfg.Baseline {
		t.Errorf("baseline = %s, want at least %s", tm.Baseline, r.cfg.Baseline)
	}
}

func TestRunRestoresAfterFailures(t *testing.T) {
	stepErr := errors.New("step failed")
	for _, stage := range []string{"ready", "reset", "fault", "trigger", "traces", "logs", "investigate"} {
		t.Run(stage, func(t *testing.T) {
			env := newEnvironment(t)
			env.onStep = func(_ context.Context, name string) error {
				if name == stage {
					return stepErr
				}
				return nil
			}
			r, sc := newTestRunner(t, env)
			record, err := r.Run(context.Background(), sc)
			if stage == "investigate" {
				if err != nil || !errors.Is(record.InvestigationError, stepErr) || record.Result.Stats.LLMRequests != 1 {
					t.Fatalf("investigation failure lost: %v, %+v", err, record)
				}
			} else if !errors.Is(err, stepErr) || !errors.Is(record.Error, stepErr) || record.InvestigationStarted {
				t.Fatalf("setup failure lost: %v, %+v", err, record)
			}
			if stage == "ready" {
				// Nothing was injected, so an unreachable environment is not touched again.
				if !reflect.DeepEqual(env.events, []string{"ready"}) || record.CleanupError != nil || record.Timings.Cleanup != 0 {
					t.Errorf("restoration attempted after readiness failure: events=%v, record=%+v", env.events, record)
				}
			} else if env.fault || env.events[len(env.events)-1] != "cleanup" || record.Timings.Cleanup <= 0 {
				t.Errorf("not restored: fault=%v, events=%v, record=%+v", env.fault, env.events, record)
			}
			if stage != "ready" && stage != "reset" && record.Traffic.OK != 12 {
				t.Errorf("traffic not drained: %+v", record.Traffic)
			}
		})
	}
}

func TestRunPreservesCleanupAndOriginalErrors(t *testing.T) {
	cleanupErr, originalErr := errors.New("restore failed"), errors.New("original failed")
	for _, stage := range []string{"", "reset", "fault", "investigate"} {
		t.Run("prior_"+stage, func(t *testing.T) {
			env := newEnvironment(t)
			env.onStep = func(_ context.Context, name string) error {
				if name == "cleanup" {
					return cleanupErr
				}
				if name == stage {
					return originalErr
				}
				return nil
			}
			r, sc := newTestRunner(t, env)
			record, err := r.Run(context.Background(), sc)
			if !errors.Is(err, cleanupErr) || !errors.Is(record.Error, cleanupErr) || !errors.Is(record.CleanupError, cleanupErr) {
				t.Fatalf("cleanup failure lost: %v, %+v", err, record)
			}
			switch stage {
			case "":
				if record.Result.Diagnosis.RootCause.Code == "" {
					t.Error("cleanup error hid successful diagnosis")
				}
			case "investigate":
				if !errors.Is(record.InvestigationError, originalErr) || record.Result.Stats.LLMRequests != 1 {
					t.Error("cleanup error hid investigation failure")
				}
			default:
				if !errors.Is(err, originalErr) || !errors.Is(record.Error, originalErr) {
					t.Error("cleanup error hid setup failure")
				}
			}
		})
	}
}

func TestRunCancellation(t *testing.T) {
	for _, stage := range []string{"ready", "reset", "fault", "trigger", "traces", "logs", "investigate"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			env := newEnvironment(t)
			env.onStep = func(stepCtx context.Context, name string) error {
				if name == stage {
					cancel()
					if stage == "reset" {
						return nil // Let baseline observe cancellation.
					}
				}
				return stepCtx.Err()
			}
			r, sc := newTestRunner(t, env)
			if stage == "reset" {
				r.cfg.Baseline = time.Hour
			}
			record, err := r.Run(ctx, sc)
			if !errors.Is(err, context.Canceled) || !errors.Is(record.Error, context.Canceled) {
				t.Fatalf("cancellation lost: %v, %+v", err, record)
			}
			wantLast := "cleanup"
			if stage == "ready" {
				wantLast = "ready" // Nothing to restore before the environment is ready.
			}
			if record.CleanupError != nil || env.fault || env.events[len(env.events)-1] != wantLast {
				t.Errorf("cleanup after cancellation failed: %+v, events=%v", record, env.events)
			}
			if stage == "investigate" && (!record.InvestigationStarted || !errors.Is(record.InvestigationError, context.Canceled)) {
				t.Errorf("investigation cancellation lost: %+v", record)
			}
		})
	}
}

func TestRunWaitsForTrafficDrain(t *testing.T) {
	env := newEnvironment(t)
	gate := make(chan struct{})
	env.drainGate = gate
	r, sc := newTestRunner(t, env)
	done := make(chan RunRecord, 1)
	go func() {
		record, _ := r.Run(context.Background(), sc)
		done <- record
	}()
	<-env.stopping
	select {
	case <-done:
		t.Fatal("Run returned before in-flight traffic drained")
	default:
	}
	close(gate)
	record := <-done
	if record.Error != nil || record.Traffic.OK != 12 {
		t.Errorf("drained result = %+v", record)
	}
}

func TestRunPreservesCancellationAndCleanupFailure(t *testing.T) {
	for _, stage := range []string{"trigger", "investigate", "cleanup"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			stepErr, cleanupErr := errors.New("last observation"), errors.New("restore failed")
			env := newEnvironment(t)
			env.onStep = func(_ context.Context, name string) error {
				if name == stage {
					cancel()
					if stage != "cleanup" {
						// Real polling may return its last observation, not ctx.Err().
						return stepErr
					}
				}
				if name == "cleanup" {
					return cleanupErr
				}
				return nil
			}
			r, sc := newTestRunner(t, env)
			record, err := r.Run(ctx, sc)
			if !errors.Is(err, context.Canceled) || !errors.Is(err, cleanupErr) || !errors.Is(record.CleanupError, cleanupErr) {
				t.Errorf("cancellation or cleanup failure lost: %v, %+v", err, record)
			}
			switch stage {
			case "trigger":
				if !errors.Is(err, stepErr) {
					t.Error("polling error lost")
				}
			case "investigate":
				if !errors.Is(record.InvestigationError, stepErr) {
					t.Error("investigation error lost")
				}
			case "cleanup":
				if record.Result.Diagnosis.RootCause.Code == "" {
					t.Error("diagnosis lost on cancellation during cleanup")
				}
			}
		})
	}
}

func TestRunValidatesBeforeIO(t *testing.T) {
	for _, invalid := range []string{"scenario", "rps", "baseline", "cancelled"} {
		t.Run(invalid, func(t *testing.T) {
			env := newEnvironment(t)
			r, sc := newTestRunner(t, env)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch invalid {
			case "scenario":
				sc.ID = ""
			case "rps":
				r.cfg.RPS = 0
			case "baseline":
				r.cfg.Baseline = -time.Second
			case "cancelled":
				cancel()
			}
			record, err := r.Run(ctx, sc)
			if err == nil || record.Error == nil || len(env.events) != 0 {
				t.Errorf("invalid run performed I/O: %v, %+v, events=%v", err, record, env.events)
			}
		})
	}
}
