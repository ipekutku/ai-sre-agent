package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/ipekutku/ai-sre-agent/internal/llm"
)

func newTracedAgent(t *testing.T, client llm.Client, cfg Config) (*Agent, *tracetest.SpanRecorder) {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	cfg.TracerProvider = sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	a, _ := newAgent(t, client, cfg)
	return a, rec
}

func attrs(s sdktrace.ReadOnlySpan) map[attribute.Key]attribute.Value {
	m := map[attribute.Key]attribute.Value{}
	for _, kv := range s.Attributes() {
		m[kv.Key] = kv.Value
	}
	return m
}

func spanNames(spans []sdktrace.ReadOnlySpan) []string {
	names := make([]string, len(spans))
	for i, s := range spans {
		names[i] = s.Name()
	}
	return names
}

func findSpan(t *testing.T, spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	for _, s := range spans {
		if s.Name() == name {
			return s
		}
	}
	t.Fatalf("no %q span in %v", name, spanNames(spans))
	return nil
}

// children returns the names of root's child spans in start order.
func children(spans []sdktrace.ReadOnlySpan, root sdktrace.ReadOnlySpan) []string {
	var kids []sdktrace.ReadOnlySpan
	for _, s := range spans {
		if s.Parent().SpanID() == root.SpanContext().SpanID() {
			kids = append(kids, s)
		}
	}
	sort.SliceStable(kids, func(i, j int) bool { return kids[i].StartTime().Before(kids[j].StartTime()) })
	return spanNames(kids)
}

func TestInvestigationTrace(t *testing.T) {
	client := &scriptedLLM{steps: []func(llm.Request) (llm.Response, error){
		toolCall(use("t1", "inspect_service", `{"service":"checkout-api"}`)),
		toolCall(use("t2", "execute_shell", `{"cmd":"cat ground-truth.yaml"}`)),
		toolCall(use("t3", submitToolName, strings.Replace(validSubmission, "INVENTORY_DOWNSTREAM_LATENCY", "SLOW_STUFF", 1))),
		toolCall(use("t4", submitToolName, validSubmission)),
	}}
	a, rec := newTracedAgent(t, client, Config{})

	res, err := a.Investigate(context.Background(), testIncident)
	if err != nil {
		t.Fatal(err)
	}
	spans := rec.Ended()
	root := findSpan(t, spans, investigationSpan)

	// One trace, rooted at the investigation, with the roadmap's shape.
	if root.Parent().IsValid() {
		t.Error("investigation span has a parent")
	}
	if res.Stats.TraceID == "" || res.Stats.TraceID != root.SpanContext().TraceID().String() {
		t.Errorf("Stats.TraceID = %q, want %s", res.Stats.TraceID, root.SpanContext().TraceID())
	}
	for _, s := range spans {
		if s.SpanContext().TraceID() != root.SpanContext().TraceID() {
			t.Errorf("span %s is in another trace", s.Name())
		}
	}
	want := []string{"llm.request", "tool.inspect_service", "llm.request", "tool.unknown", "llm.request", "llm.final_diagnosis"}
	if got := children(spans, root); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("children = %v, want %v", got, want)
	}

	ra := attrs(root)
	for k, v := range map[attribute.Key]string{
		"incident.id": "inc-001", "incident.alert": "HighCheckoutLatency", "incident.service": "checkout-api",
		"investigation.outcome": outcomeDiagnosed, "diagnosis.root_cause_code": "INVENTORY_DOWNSTREAM_LATENCY",
	} {
		if ra[k].AsString() != v {
			t.Errorf("investigation %s = %q, want %q", k, ra[k].AsString(), v)
		}
	}
	for k, v := range map[attribute.Key]int64{
		"investigation.llm_requests": 4, "investigation.tool_calls": 2, "investigation.failed_tool_calls": 1,
		"investigation.max_tool_calls": 15, "gen_ai.usage.input_tokens": 400, "gen_ai.usage.output_tokens": 40,
	} {
		if ra[k].AsInt64() != v {
			t.Errorf("investigation %s = %d, want %d", k, ra[k].AsInt64(), v)
		}
	}
	if root.Status().Code == codes.Error {
		t.Errorf("investigation status = %+v", root.Status())
	}
	if ev := root.Events(); len(ev) != 1 || ev[0].Name != rejectedEvent {
		t.Errorf("investigation events = %+v, want one %s", ev, rejectedEvent)
	}

	final := attrs(findSpan(t, spans, finalDiagnosisSpan))
	if final["gen_ai.response.model"].AsString() != "test-model" || final["llm.request_number"].AsInt64() != 4 ||
		final["gen_ai.usage.input_tokens"].AsInt64() != 100 || final["llm.cost_usd"].AsFloat64() != 0.001 {
		t.Errorf("final diagnosis span attributes = %v", final)
	}
	if fr := final["gen_ai.response.finish_reasons"].AsStringSlice(); len(fr) != 1 || fr[0] != "tool_use" {
		t.Errorf("finish reasons = %v", fr)
	}

	// The failed tool call is an error span that does not name the unknown tool.
	unknown := findSpan(t, spans, "tool.unknown")
	if unknown.Status().Code != codes.Error || attrs(unknown)["tool.error_code"].AsString() != "unknown_tool" {
		t.Errorf("unknown tool span: status %+v, attributes %v", unknown.Status(), attrs(unknown))
	}

	// Spans carry sizes and codes only: no prompts, model output, tool input
	// or output, or diagnosis text.
	dump := dumpSpans(spans)
	for _, leaked := range []string{
		"execute_shell", "ground-truth", `"service":"checkout-api"`, "echo", "SLOW_STUFF",
		"inventory-api is slow", "988ms", "Investigate inventory-api", systemPrompt[:40],
		testIncident.Description,
	} {
		if strings.Contains(dump, leaked) {
			t.Errorf("spans contain %q", leaked)
		}
	}
}

// The LLM span covers the model request only; the tool calls its response
// asked for run after it ends.
func TestLLMSpanExcludesToolTime(t *testing.T) {
	client := &scriptedLLM{steps: []func(llm.Request) (llm.Response, error){
		toolCall(use("t1", "inspect_service", `{}`)),
		toolCall(use("t2", submitToolName, validSubmission)),
	}}
	a, rec := newTracedAgent(t, client, Config{})
	slow := &sleepTool{d: 30 * time.Millisecond}
	a.tools = mustRegistry(t, a.cfg, slow)

	if _, err := a.Investigate(context.Background(), testIncident); err != nil {
		t.Fatal(err)
	}
	spans := rec.Ended()
	req, tool := findSpan(t, spans, llmRequestSpan), findSpan(t, spans, "tool.inspect_service")
	if req.EndTime().After(tool.StartTime()) {
		t.Errorf("llm.request ends at %v, after the tool call starts at %v", req.EndTime(), tool.StartTime())
	}
	if req.EndTime().Sub(req.StartTime()) >= slow.d {
		t.Errorf("llm.request lasted %v, which includes tool time", req.EndTime().Sub(req.StartTime()))
	}
}

func TestInvestigationTraceFailures(t *testing.T) {
	looping := make([]func(llm.Request) (llm.Response, error), 3)
	for i := range looping {
		looping[i] = toolCall(use("t", "inspect_service", `{}`))
	}
	tests := []struct {
		name        string
		steps       []func(llm.Request) (llm.Response, error)
		cfg         Config
		wantOutcome string
		wantLast    string
	}{
		{"request budget", looping, Config{MaxLLMRequests: 3}, outcomeBudgetExhausted, "tool.inspect_service"},
		{"refusal", []func(llm.Request) (llm.Response, error){textReply("", llm.StopRefusal)}, Config{}, outcomeModelStopped, llmRequestSpan},
		{"llm error", []func(llm.Request) (llm.Response, error){func(llm.Request) (llm.Response, error) {
			return llm.Response{}, &llm.Error{Kind: llm.ErrAuth, StatusCode: 401, Message: "authentication_error"}
		}}, Config{}, outcomeLLMError, llmRequestSpan},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, rec := newTracedAgent(t, &scriptedLLM{steps: tt.steps}, tt.cfg)
			res, err := a.Investigate(context.Background(), testIncident)
			if err == nil {
				t.Fatal("expected error")
			}
			spans := rec.Ended()
			root := findSpan(t, spans, investigationSpan)
			if got := attrs(root)["investigation.outcome"].AsString(); got != tt.wantOutcome {
				t.Errorf("outcome = %q, want %q", got, tt.wantOutcome)
			}
			if root.Status().Code != codes.Error {
				t.Errorf("investigation status = %+v, want error", root.Status())
			}
			if _, ok := attrs(root)["diagnosis.root_cause_code"]; ok {
				t.Error("failed investigation has a root-cause code")
			}
			if got := attrs(root)["investigation.llm_requests"].AsInt64(); got != int64(res.Stats.LLMRequests) {
				t.Errorf("llm_requests = %d, stats %d", got, res.Stats.LLMRequests)
			}
			kids := children(spans, root)
			if len(kids) == 0 || kids[len(kids)-1] != tt.wantLast {
				t.Errorf("children = %v, want last %q", kids, tt.wantLast)
			}
			for _, k := range kids {
				if k == finalDiagnosisSpan {
					t.Errorf("failed investigation has a %s span", finalDiagnosisSpan)
				}
			}
		})
	}

	// A failed LLM request records the error kind.
	a, rec := newTracedAgent(t, &scriptedLLM{steps: tests[2].steps}, Config{})
	_, _ = a.Investigate(context.Background(), testIncident)
	req := findSpan(t, rec.Ended(), llmRequestSpan)
	if req.Status().Code != codes.Error || attrs(req)["llm.error_kind"].AsString() != "auth" {
		t.Errorf("llm.request status %+v, attributes %v", req.Status(), attrs(req))
	}
}

func TestInvestigationTraceTimeout(t *testing.T) {
	client := llmFunc(func(ctx context.Context, _ llm.Request) (llm.Response, error) {
		<-ctx.Done()
		return llm.Response{}, &llm.Error{Kind: llm.ErrTransport, Message: "cancelled", Err: ctx.Err()}
	})
	a, rec := newTracedAgent(t, client, Config{MaxDuration: 20 * time.Millisecond})
	if _, err := a.Investigate(context.Background(), testIncident); err == nil {
		t.Fatal("expected error")
	}
	root := findSpan(t, rec.Ended(), investigationSpan)
	if got := attrs(root)["investigation.outcome"].AsString(); got != outcomeTimeout {
		t.Errorf("outcome = %q, want %q", got, outcomeTimeout)
	}
}

func TestInvestigationWithoutTracing(t *testing.T) {
	client := &scriptedLLM{steps: []func(llm.Request) (llm.Response, error){
		toolCall(use("t1", submitToolName, validSubmission)),
	}}
	a, _ := newAgent(t, client, Config{})
	res, err := a.Investigate(context.Background(), testIncident)
	if err != nil {
		t.Fatal(err)
	}
	if res.Stats.TraceID != "" {
		t.Errorf("TraceID = %q, want empty without a tracer provider", res.Stats.TraceID)
	}
}

// dumpSpans renders every span's name, attributes, events, and status.
func dumpSpans(spans []sdktrace.ReadOnlySpan) string {
	var b strings.Builder
	for _, s := range spans {
		fmt.Fprintf(&b, "%s %s\n", s.Name(), s.Status().Description)
		for _, kv := range s.Attributes() {
			fmt.Fprintf(&b, "  %s=%s\n", kv.Key, kv.Value.Emit())
		}
		for _, e := range s.Events() {
			fmt.Fprintf(&b, "  event %s\n", e.Name)
			for _, kv := range e.Attributes {
				fmt.Fprintf(&b, "    %s=%s\n", kv.Key, kv.Value.Emit())
			}
		}
	}
	return b.String()
}

type sleepTool struct{ d time.Duration }

func (s *sleepTool) Name() string                 { return "inspect_service" }
func (s *sleepTool) Description() string          { return "sleeps" }
func (s *sleepTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (s *sleepTool) Call(context.Context, json.RawMessage) (any, error) {
	time.Sleep(s.d)
	return map[string]bool{"ok": true}, nil
}
