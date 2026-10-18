// Command scenario-runner runs one incident scenario end to end against the
// local environment (make up) and evaluates the agent's diagnosis:
//
//	wait for environment -> generate traffic -> baseline -> inject fault ->
//	wait for alert condition -> wait for traces and logs -> investigate -> clean up -> evaluate
//
// The CLI loads the scenario and ground truth, wires internal/runner, and
// evaluates its RunRecord after execution. The agent receives only the incident.
//
// The investigation is traced (service name "ai-sre-agent") when an OTLP
// traces endpoint is configured through the standard OTEL_EXPORTER_OTLP_*
// environment variables; make eval points it at Tempo. Only the investigation
// is traced: the runner's own steps (traffic, fault injection, waits) use an
// uninstrumented HTTP client and create no spans, so fault details never reach
// the trace backend.
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
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/ipekutku/ai-sre-agent/internal/agent"
	"github.com/ipekutku/ai-sre-agent/internal/diagnosis"
	"github.com/ipekutku/ai-sre-agent/internal/evaluation"
	"github.com/ipekutku/ai-sre-agent/internal/llm/anthropic"
	"github.com/ipekutku/ai-sre-agent/internal/runner"
	"github.com/ipekutku/ai-sre-agent/internal/scenarios"
	"github.com/ipekutku/ai-sre-agent/internal/telemetry"
	"github.com/ipekutku/ai-sre-agent/internal/tools"
)

// agentServiceName is the service name of the agent's spans. It is not in the
// tools' service catalog, so the agent can never read its own spans.
const agentServiceName = "ai-sre-agent"

type options struct {
	scenarioDir    string
	checkoutURL    string
	inventoryURL   string
	adminURL       string
	prometheusURL  string
	tempoURL       string
	lokiURL        string
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
	flag.StringVar(&o.tempoURL, "tempo-url", "http://127.0.0.1:3200", "Tempo base URL")
	flag.StringVar(&o.lokiURL, "loki-url", "http://127.0.0.1:3100", "Loki base URL")
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

	// The runner's logger writes to stderr only, so a failing trace backend
	// cannot feed its own errors back into the export queue.
	providers, shutdownTelemetry, err := telemetry.Setup(ctx, logger, agentServiceName, os.Getenv)
	if err != nil {
		return false, err
	}
	defer func() {
		if err := telemetry.Shutdown(shutdownTelemetry); err != nil {
			logger.Warn("flushing agent spans failed", "error", err)
		}
	}()

	// Neither the runner nor the tools trace their HTTP requests, so no
	// trace context is sent to the services or backends.
	hc := &http.Client{Timeout: 10 * time.Second}
	env := scenarios.Environment{
		CheckoutURL: o.checkoutURL, InventoryURL: o.inventoryURL,
		InventoryAdminURL: o.adminURL, PrometheusURL: o.prometheusURL, TempoURL: o.tempoURL, LokiURL: o.lokiURL, HTTP: hc,
	}

	// The agent's view of the system: metrics, traces, logs, and the service
	// catalog. The fault admin URL is deliberately absent.
	catalog := []tools.ServiceEntry{
		{Name: "checkout-api", BaseURL: o.checkoutURL, Dependencies: []string{"inventory-api"}},
		{Name: "inventory-api", BaseURL: o.inventoryURL},
	}
	services := make([]string, len(catalog))
	for i, s := range catalog {
		services[i] = s.Name
	}
	qm, err := tools.NewQueryMetrics(o.prometheusURL, hc)
	if err != nil {
		return false, err
	}
	qt, err := tools.NewQueryTraces(o.tempoURL, services, hc)
	if err != nil {
		return false, err
	}
	ql, err := tools.NewQueryLogs(o.lokiURL, services, hc)
	if err != nil {
		return false, err
	}
	is, err := tools.NewInspectService(catalog, hc)
	if err != nil {
		return false, err
	}
	registry, err := tools.NewRegistry(logger, tools.Options{TracerProvider: providers.Tracer}, qm, qt, ql, is)
	if err != nil {
		return false, err
	}
	investigator := agent.New(llmClient, registry, logger, agent.Config{
		MaxLLMRequests: o.maxLLMRequests, MaxToolCalls: o.maxToolCalls, MaxDuration: o.maxDuration,
		TracerProvider: providers.Tracer,
	})

	execution := runner.New(env, investigator, logger, runner.Config{RPS: o.rps, Baseline: o.baseline})
	record, runErr := execution.Run(ctx, sc)
	return report(out, sc, gt, record, runErr)
}

// report evaluates only after execution and cleanup. An investigation's result
// remains visible even if cleanup fails, but a lifecycle error always exits 2.
func report(out io.Writer, sc scenarios.Scenario, gt evaluation.GroundTruth, record runner.RunRecord, runErr error) (bool, error) {
	if !record.InvestigationStarted {
		return false, runErr
	}
	ev := evaluation.Evaluate(sc, gt, record.Result.Diagnosis)
	if record.InvestigationError != nil {
		ev.Pass = false
		ev.Actual = ""
		ev.Reason = "investigation failed: " + record.InvestigationError.Error()
	}
	if err := evaluation.WriteReport(out, ev); err != nil {
		return false, errors.Join(runErr, err)
	}
	writeDetails(out, record.Result, record.Traffic)
	if runErr != nil {
		return false, runErr
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
	fmt.Fprintf(w, "  llm retries:     %d\n", s.LLMRetries)
	fmt.Fprintf(w, "  tool calls:      %d (%d failed)%s\n", s.ToolCalls, s.FailedToolCalls, toolBreakdown(s.ToolCallsByName))
	fmt.Fprintf(w, "  duplicate calls: %d\n", s.DuplicateToolCalls)
	fmt.Fprintf(w, "  invalid calls:   %d\n", s.InvalidToolCalls)
	fmt.Fprintf(w, "  rejected diagnoses: %d\n", s.RejectedDiagnoses)
	fmt.Fprintf(w, "  tokens:          %d in, %d out, %d cache read, %d cache write\n",
		s.Usage.InputTokens, s.Usage.OutputTokens, s.Usage.CacheReadTokens, s.Usage.CacheWriteTokens)
	fmt.Fprintf(w, "  estimated cost:  %s\n", cost)
	fmt.Fprintf(w, "  traffic:         %d ok, %d failed\n", t.OK, t.Failed)
	if s.TraceID != "" {
		fmt.Fprintf(w, "  trace id:        %s\n", s.TraceID)
	}

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

// toolBreakdown formats per-tool call counts as ": a=1, b=2", sorted by name.
func toolBreakdown(counts map[string]int) string {
	if len(counts) == 0 {
		return ""
	}
	names := make([]string, 0, len(counts))
	for n := range counts {
		names = append(names, n)
	}
	sort.Strings(names)
	parts := make([]string, len(names))
	for i, n := range names {
		parts[i] = fmt.Sprintf("%s=%d", n, counts[n])
	}
	return ": " + strings.Join(parts, ", ")
}

func writeList(w io.Writer, title string, ev []diagnosis.Evidence) {
	fmt.Fprintf(w, "\n%s:\n", title)
	for _, e := range ev {
		fmt.Fprintf(w, "  - [%s] %s\n", e.Source, e.Observation)
	}
}
