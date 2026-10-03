// Command scenario-runner runs one incident scenario end to end against the
// local environment (make up) and evaluates the agent's diagnosis:
//
//	wait for environment -> generate traffic -> baseline -> inject fault ->
//	wait for alert condition -> investigate -> evaluate -> clean up
//
// The runner reads both scenario.yaml and ground-truth.yaml. The agent only
// receives the scenario's incident.
//
// Requires Anthropic API credentials (ANTHROPIC_API_KEY) for the investigation.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ipekutku/ai-sre-agent/internal/agent"
	"github.com/ipekutku/ai-sre-agent/internal/diagnosis"
	"github.com/ipekutku/ai-sre-agent/internal/evaluation"
	"github.com/ipekutku/ai-sre-agent/internal/llm/anthropic"
	"github.com/ipekutku/ai-sre-agent/internal/scenarios"
	"github.com/ipekutku/ai-sre-agent/internal/tools"
)

type options struct {
	scenarioDir    string
	checkoutURL    string
	inventoryURL   string
	adminURL       string
	prometheusURL  string
	model          string
	effort         string
	rps            int
	baseline       time.Duration
	maxLLMRequests int
	maxToolCalls   int
	maxDuration    time.Duration
}

func main() {
	var o options
	flag.StringVar(&o.scenarioDir, "scenario", "scenarios/inventory-latency", "scenario directory")
	flag.StringVar(&o.checkoutURL, "checkout-url", "http://127.0.0.1:8080", "checkout-api base URL")
	flag.StringVar(&o.inventoryURL, "inventory-url", "http://127.0.0.1:8081", "inventory-api base URL")
	flag.StringVar(&o.adminURL, "inventory-admin-url", "http://127.0.0.1:9081", "inventory-api fault admin URL (runner only)")
	flag.StringVar(&o.prometheusURL, "prometheus-url", "http://127.0.0.1:9090", "Prometheus base URL")
	flag.StringVar(&o.model, "model", anthropic.DefaultModel, "Claude model ID")
	flag.StringVar(&o.effort, "effort", "medium", "effort: low, medium, high, xhigh, max")
	flag.IntVar(&o.rps, "rps", 20, "checkout requests per second during the scenario")
	flag.DurationVar(&o.baseline, "baseline", 60*time.Second, "normal-traffic period before the fault")
	flag.IntVar(&o.maxLLMRequests, "max-llm-requests", 20, "investigation LLM request budget")
	flag.IntVar(&o.maxToolCalls, "max-tool-calls", 15, "investigation tool-call budget")
	flag.DurationVar(&o.maxDuration, "max-duration", 5*time.Minute, "investigation time limit")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	pass, err := run(ctx, logger, os.Stdout, o)
	if err != nil {
		fmt.Fprintln(os.Stderr, "scenario-runner:", err)
		os.Exit(2)
	}
	if !pass {
		os.Exit(1)
	}
}

func run(ctx context.Context, logger *slog.Logger, out io.Writer, o options) (bool, error) {
	sc, err := scenarios.Load(o.scenarioDir)
	if err != nil {
		return false, err
	}
	gt, err := evaluation.LoadGroundTruth(o.scenarioDir)
	if err != nil {
		return false, err
	}
	llmClient, err := anthropic.New(anthropic.Config{
		Model:       o.model,
		Effort:      o.effort,
		WorkspaceID: os.Getenv("ANTHROPIC_WORKSPACE_ID"), // only needed for keys not scoped to a workspace
	})
	if err != nil {
		return false, err
	}
	// Fail fast on missing credentials or a bad model ID (no tokens used).
	checkCtx, cancelCheck := context.WithTimeout(ctx, 30*time.Second)
	err = llmClient.Check(checkCtx)
	cancelCheck()
	if err != nil {
		return false, fmt.Errorf("anthropic API check failed (is ANTHROPIC_API_KEY set?): %w", err)
	}

	hc := &http.Client{Timeout: 10 * time.Second}
	env := scenarios.Environment{
		CheckoutURL: o.checkoutURL, InventoryURL: o.inventoryURL,
		InventoryAdminURL: o.adminURL, PrometheusURL: o.prometheusURL, HTTP: hc,
	}

	// The agent's view of the system: metrics and the service catalog.
	// The fault admin URL is deliberately absent.
	qm, err := tools.NewQueryMetrics(o.prometheusURL, hc)
	if err != nil {
		return false, err
	}
	is, err := tools.NewInspectService([]tools.ServiceEntry{
		{Name: "checkout-api", BaseURL: o.checkoutURL, Dependencies: []string{"inventory-api"}},
		{Name: "inventory-api", BaseURL: o.inventoryURL},
	}, hc)
	if err != nil {
		return false, err
	}
	registry, err := tools.NewRegistry(logger, tools.Options{}, qm, is)
	if err != nil {
		return false, err
	}
	investigator := agent.New(llmClient, registry, logger, agent.Config{
		MaxLLMRequests: o.maxLLMRequests, MaxToolCalls: o.maxToolCalls, MaxDuration: o.maxDuration,
	})

	// 1. Environment ready, no leftover fault.
	logger.Info("waiting for environment")
	readyCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	err = env.WaitReady(readyCtx)
	cancel()
	if err != nil {
		return false, fmt.Errorf("environment not ready (did you run make up?): %w", err)
	}
	if err := env.ClearFault(ctx); err != nil {
		return false, fmt.Errorf("reset fault: %w", err)
	}
	// Always leave the environment healthy, even on failure or Ctrl-C.
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := env.ClearFault(cleanupCtx); err != nil {
			logger.Error("failed to clear fault", "error", err)
		}
	}()

	// 2. Traffic for the whole scenario.
	trafficCtx, stopTraffic := context.WithCancel(ctx)
	trafficDone := make(chan scenarios.TrafficStats, 1)
	go func() { trafficDone <- env.GenerateTraffic(trafficCtx, o.rps) }()
	defer stopTraffic()

	// 3. Baseline.
	logger.Info("collecting baseline", "duration", o.baseline, "rps", o.rps)
	if err := sleep(ctx, o.baseline); err != nil {
		return false, err
	}

	// 4. Fault, then wait until the alert condition is observable.
	logger.Info("injecting fault", "service", sc.Fault.Service, "type", sc.Fault.Type)
	if err := env.ApplyFault(ctx, sc.Fault); err != nil {
		return false, fmt.Errorf("inject fault: %w", err)
	}
	value, err := env.WaitForTrigger(ctx, sc.Trigger)
	if err != nil {
		return false, err
	}
	logger.Info("alert condition met; raising incident", "value", value, "incident_id", sc.Incident.IncidentID)

	// 5. Investigate. The agent receives only the incident.
	result, invErr := investigator.Investigate(ctx, sc.Incident)

	stopTraffic()
	traffic := <-trafficDone

	// 6. Evaluate and report.
	ev := evaluation.Evaluate(sc, gt, result.Diagnosis)
	if invErr != nil {
		ev.Pass = false
		ev.Actual = ""
		ev.Reason = "investigation failed: " + invErr.Error()
	}
	if err := evaluation.WriteReport(out, ev); err != nil {
		return false, err
	}
	writeDetails(out, result, traffic)
	if errors.Is(invErr, context.Canceled) {
		return false, invErr
	}
	return ev.Pass, nil
}

func writeDetails(w io.Writer, r agent.Result, t scenarios.TrafficStats) {
	s := r.Stats
	cost := "unknown"
	if s.Usage.CostKnown {
		cost = fmt.Sprintf("$%.4f", s.Usage.CostUSD)
	}
	fmt.Fprintf(w, "\nInvestigation:\n")
	fmt.Fprintf(w, "  models:          %v\n", s.Models)
	fmt.Fprintf(w, "  duration:        %s\n", s.Duration.Round(100*time.Millisecond))
	fmt.Fprintf(w, "  llm requests:    %d\n", s.LLMRequests)
	fmt.Fprintf(w, "  tool calls:      %d (%d failed)\n", s.ToolCalls, s.FailedToolCalls)
	fmt.Fprintf(w, "  tokens:          %d in, %d out, %d cache read, %d cache write\n",
		s.Usage.InputTokens, s.Usage.OutputTokens, s.Usage.CacheReadTokens, s.Usage.CacheWriteTokens)
	fmt.Fprintf(w, "  estimated cost:  %s\n", cost)
	fmt.Fprintf(w, "  traffic:         %d ok, %d failed\n", t.OK, t.Failed)

	d := r.Diagnosis
	if d.RootCause.Code == "" {
		return
	}
	fmt.Fprintf(w, "\nDiagnosis (confidence %.2f):\n  %s\n", d.Confidence, d.RootCause.Summary)
	writeList(w, "Evidence", d.Evidence)
	if len(d.RecommendedActions) > 0 {
		fmt.Fprintf(w, "\nRecommended actions:\n")
		for _, a := range d.RecommendedActions {
			fmt.Fprintf(w, "  - %s\n", a)
		}
	}
}

func writeList(w io.Writer, title string, ev []diagnosis.Evidence) {
	fmt.Fprintf(w, "\n%s:\n", title)
	for _, e := range ev {
		fmt.Fprintf(w, "  - [%s] %s\n", e.Source, e.Observation)
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}
