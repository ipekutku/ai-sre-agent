package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ipekutku/ai-sre-agent/internal/incident"
	"github.com/ipekutku/ai-sre-agent/internal/llm"
	"github.com/ipekutku/ai-sre-agent/internal/tools"
)

var testIncident = incident.Incident{
	IncidentID:  "inc-001",
	Alert:       "HighCheckoutLatency",
	Service:     "checkout-api",
	Severity:    "warning",
	Description: "p95 request latency exceeded threshold",
}

const validSubmission = `{
  "root_cause": {"code": "INVENTORY_DOWNSTREAM_LATENCY", "summary": "inventory-api is slow"},
  "confidence": 0.9,
  "evidence": [{"source": "prometheus", "observation": "inventory-api p95 rose from 24ms to 988ms"}],
  "recommended_actions": ["Investigate inventory-api"]
}`

// scriptedLLM returns one scripted step per Generate call and records requests.
type scriptedLLM struct {
	steps    []func(req llm.Request) (llm.Response, error)
	requests []llm.Request
}

func (s *scriptedLLM) Generate(ctx context.Context, req llm.Request) (llm.Response, error) {
	// Copy the slice header: the loop appends to req.Messages afterwards.
	req.Messages = append([]llm.Message(nil), req.Messages...)
	s.requests = append(s.requests, req)
	i := len(s.requests) - 1
	if i >= len(s.steps) {
		return llm.Response{}, errors.New("script exhausted")
	}
	return s.steps[i](req)
}

func toolCall(uses ...llm.Block) func(llm.Request) (llm.Response, error) {
	return func(llm.Request) (llm.Response, error) {
		return llm.Response{
			Message:    llm.Message{Role: llm.RoleAssistant, Content: uses, ProviderState: "opaque"},
			StopReason: llm.StopToolUse,
			Model:      "test-model",
			Usage:      llm.Usage{InputTokens: 100, OutputTokens: 10, CostUSD: 0.001, CostKnown: true},
		}, nil
	}
}

func use(id, name, input string) llm.Block {
	return llm.Block{Type: llm.BlockToolUse, ToolUseID: id, ToolName: name, ToolInput: json.RawMessage(input)}
}

func textReply(text string, stop llm.StopReason) func(llm.Request) (llm.Response, error) {
	return func(llm.Request) (llm.Response, error) {
		return llm.Response{
			Message:    llm.Message{Role: llm.RoleAssistant, Content: []llm.Block{{Type: llm.BlockText, Text: text}}},
			StopReason: stop, Model: "test-model",
			Usage: llm.Usage{InputTokens: 50, OutputTokens: 5, CostUSD: 0.0005, CostKnown: true},
		}, nil
	}
}

// echoTool counts its calls and returns its input.
type echoTool struct{ calls int }

func (e *echoTool) Name() string                 { return "inspect_service" }
func (e *echoTool) Description() string          { return "echo" }
func (e *echoTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (e *echoTool) Call(_ context.Context, in json.RawMessage) (any, error) {
	e.calls++
	return map[string]json.RawMessage{"echo": in}, nil
}

func newAgent(t *testing.T, client llm.Client, cfg Config) (*Agent, *echoTool) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	tool := &echoTool{}
	reg, err := tools.NewRegistry(logger, tools.Options{}, tool)
	if err != nil {
		t.Fatal(err)
	}
	return New(client, reg, logger, cfg), tool
}

func TestInvestigateHappyPath(t *testing.T) {
	client := &scriptedLLM{steps: []func(llm.Request) (llm.Response, error){
		toolCall(use("t1", "inspect_service", `{"service":"checkout-api"}`)),
		toolCall(use("t2", submitToolName, validSubmission)),
	}}
	a, tool := newAgent(t, client, Config{})

	res, err := a.Investigate(context.Background(), testIncident)
	if err != nil {
		t.Fatal(err)
	}
	d := res.Diagnosis
	if d.IncidentID != "inc-001" || d.RootCause.Code != "INVENTORY_DOWNSTREAM_LATENCY" || len(d.Evidence) != 1 {
		t.Errorf("diagnosis = %+v", d)
	}
	s := res.Stats
	if s.LLMRequests != 2 || s.ToolCalls != 1 || s.FailedToolCalls != 0 || tool.calls != 1 {
		t.Errorf("stats = %+v, tool calls %d", s, tool.calls)
	}
	if s.Usage.InputTokens != 200 || !s.Usage.CostKnown || s.Duration <= 0 || !reflect.DeepEqual(s.Models, []string{"test-model"}) {
		t.Errorf("stats = %+v", s)
	}

	// The first request contains only the incident; no ground truth.
	first := client.requests[0]
	if len(first.Messages) != 1 || !strings.Contains(first.Messages[0].Content[0].Text, `"incident_id": "inc-001"`) {
		t.Errorf("first request messages = %+v", first.Messages)
	}
	// Tools: registry tools plus submit_diagnosis.
	if len(first.Tools) != 2 || first.Tools[0].Name != "inspect_service" || first.Tools[1].Name != submitToolName {
		t.Errorf("tools = %+v", first.Tools)
	}

	// Append-only history; identical system prompt and tools.
	second := client.requests[1]
	if second.System != first.System || !reflect.DeepEqual(second.Tools, first.Tools) {
		t.Error("system prompt or tools changed between requests")
	}
	if !reflect.DeepEqual(second.Messages[:1], first.Messages) || len(second.Messages) != 3 {
		t.Fatalf("history not append-only: %+v", second.Messages)
	}
	if second.Messages[1].ProviderState != "opaque" {
		t.Error("assistant turn was not appended unchanged")
	}
	result := second.Messages[2].Content[0]
	if result.Type != llm.BlockToolResult || result.ToolUseID != "t1" || result.IsError ||
		!strings.Contains(result.Text, `"service":"checkout-api"`) {
		t.Errorf("tool result = %+v", result)
	}
}

func TestInvestigateRejectsInvalidSubmission(t *testing.T) {
	client := &scriptedLLM{steps: []func(llm.Request) (llm.Response, error){
		toolCall(use("t1", submitToolName, strings.Replace(validSubmission, "INVENTORY_DOWNSTREAM_LATENCY", "SLOW_STUFF", 1))),
		toolCall(use("t2", submitToolName, `{"root_cause":{"code":"UNKNOWN","summary":"s"},"confidence":0.1,"evidence":[],"recommended_actions":[]}`)),
		toolCall(use("t3", submitToolName, validSubmission)),
	}}
	a, _ := newAgent(t, client, Config{})

	res, err := a.Investigate(context.Background(), testIncident)
	if err != nil {
		t.Fatal(err)
	}
	if res.Diagnosis.RootCause.Code != "INVENTORY_DOWNSTREAM_LATENCY" || res.Stats.LLMRequests != 3 {
		t.Errorf("result = %+v", res)
	}
	for i, want := range []string{"not a known root-cause code", "at least one evidence"} {
		r := client.requests[i+1].Messages[len(client.requests[i+1].Messages)-1].Content[0]
		if !r.IsError || !strings.Contains(r.Text, want) {
			t.Errorf("feedback %d = %+v, want error containing %q", i, r, want)
		}
	}
}

func TestInvestigateNudgesWhenModelAnswersInText(t *testing.T) {
	client := &scriptedLLM{steps: []func(llm.Request) (llm.Response, error){
		textReply("I think it's inventory.", llm.StopEndTurn),
		toolCall(use("t1", submitToolName, validSubmission)),
	}}
	a, _ := newAgent(t, client, Config{})

	res, err := a.Investigate(context.Background(), testIncident)
	if err != nil {
		t.Fatal(err)
	}
	nudge := client.requests[1].Messages[2]
	if nudge.Role != llm.RoleUser || !strings.Contains(nudge.Content[0].Text, submitToolName) {
		t.Errorf("nudge = %+v", nudge)
	}
	if res.Stats.Usage.InputTokens != 150 {
		t.Errorf("usage = %+v", res.Stats.Usage)
	}
}

func TestInvestigateToolBudget(t *testing.T) {
	client := &scriptedLLM{steps: []func(llm.Request) (llm.Response, error){
		toolCall(use("t1", "inspect_service", `{}`), use("t2", "inspect_service", `{}`), use("t3", "inspect_service", `{}`)),
		toolCall(use("t4", submitToolName, validSubmission)),
	}}
	a, tool := newAgent(t, client, Config{MaxToolCalls: 2})

	res, err := a.Investigate(context.Background(), testIncident)
	if err != nil {
		t.Fatal(err)
	}
	if tool.calls != 2 || res.Stats.ToolCalls != 2 {
		t.Errorf("tool executed %d times (stats %d), want 2", tool.calls, res.Stats.ToolCalls)
	}
	results := client.requests[1].Messages[2].Content
	if len(results) != 3 {
		t.Fatalf("every tool_use needs a tool_result, got %d", len(results))
	}
	if !results[2].IsError || !strings.Contains(results[2].Text, "budget_exhausted") {
		t.Errorf("third result = %+v", results[2])
	}
}

func TestInvestigateUnknownToolIsReportedToModel(t *testing.T) {
	client := &scriptedLLM{steps: []func(llm.Request) (llm.Response, error){
		toolCall(use("t1", "execute_shell", `{"cmd":"cat ground-truth.yaml"}`)),
		toolCall(use("t2", submitToolName, validSubmission)),
	}}
	a, _ := newAgent(t, client, Config{})

	res, err := a.Investigate(context.Background(), testIncident)
	if err != nil {
		t.Fatal(err)
	}
	r := client.requests[1].Messages[2].Content[0]
	if !r.IsError || !strings.Contains(r.Text, "unknown_tool") || res.Stats.FailedToolCalls != 1 {
		t.Errorf("result = %+v, stats = %+v", r, res.Stats)
	}
}

func TestInvestigateFailures(t *testing.T) {
	looping := make([]func(llm.Request) (llm.Response, error), 10)
	for i := range looping {
		looping[i] = toolCall(use("t", "inspect_service", `{}`))
	}
	tests := []struct {
		name    string
		steps   []func(llm.Request) (llm.Response, error)
		cfg     Config
		wantErr error
		wantMsg string
	}{
		{"request budget", looping, Config{MaxLLMRequests: 3}, ErrBudgetExhausted, ""},
		{"refusal", []func(llm.Request) (llm.Response, error){textReply("", llm.StopRefusal)}, Config{}, ErrModelStopped, "refusal"},
		{"truncated", []func(llm.Request) (llm.Response, error){textReply("", llm.StopMaxTokens)}, Config{}, ErrModelStopped, "max_tokens"},
		{"llm error", []func(llm.Request) (llm.Response, error){func(llm.Request) (llm.Response, error) {
			return llm.Response{}, &llm.Error{Kind: llm.ErrAuth, StatusCode: 401, Message: "authentication_error"}
		}}, Config{}, nil, "llm auth"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, _ := newAgent(t, &scriptedLLM{steps: tt.steps}, tt.cfg)
			res, err := a.Investigate(context.Background(), testIncident)
			if err == nil {
				t.Fatal("expected error")
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Errorf("err = %v, want %v", err, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("err = %v, want containing %q", err, tt.wantMsg)
			}
			if res.Stats.LLMRequests == 0 || res.Stats.Duration <= 0 {
				t.Errorf("stats must be populated on failure: %+v", res.Stats)
			}
		})
	}
}

func TestInvestigateTimeout(t *testing.T) {
	// The model never answers; only the investigation deadline ends the call.
	client := llmFunc(func(ctx context.Context, _ llm.Request) (llm.Response, error) {
		<-ctx.Done()
		return llm.Response{}, &llm.Error{Kind: llm.ErrTransport, Message: "cancelled", Err: ctx.Err()}
	})
	a, _ := newAgent(t, client, Config{MaxDuration: 50 * time.Millisecond})

	start := time.Now()
	_, err := a.Investigate(context.Background(), testIncident)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("timeout not enforced: took %v", time.Since(start))
	}
}

type llmFunc func(context.Context, llm.Request) (llm.Response, error)

func (f llmFunc) Generate(ctx context.Context, req llm.Request) (llm.Response, error) {
	return f(ctx, req)
}

func TestInvestigateRejectsInvalidIncident(t *testing.T) {
	a, _ := newAgent(t, &scriptedLLM{}, Config{})
	if _, err := a.Investigate(context.Background(), incident.Incident{}); err == nil {
		t.Fatal("expected error for invalid incident")
	}
}

// The prompt and tool schema must offer every code and stay scenario-neutral.
func TestPromptIsScenarioNeutral(t *testing.T) {
	for _, banned := range []string{"inc-001", "800", "fault", "injected", "ground truth", "ground-truth"} {
		if strings.Contains(strings.ToLower(systemPrompt), banned) {
			t.Errorf("system prompt mentions %q", banned)
		}
	}
	var schema struct {
		Properties struct {
			RootCause struct {
				Properties struct {
					Code struct {
						Enum []string `json:"enum"`
					} `json:"code"`
				} `json:"properties"`
			} `json:"root_cause"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(submitTool.InputSchema, &schema); err != nil {
		t.Fatal(err)
	}
	if len(schema.Properties.RootCause.Properties.Code.Enum) != 8 {
		t.Errorf("submit_diagnosis enum = %v, want all 8 codes", schema.Properties.RootCause.Properties.Code.Enum)
	}
}
