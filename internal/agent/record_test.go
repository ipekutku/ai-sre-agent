package agent

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ipekutku/ai-sre-agent/internal/llm"
	"github.com/ipekutku/ai-sre-agent/internal/tools"
)

func TestCanonicalInput(t *testing.T) {
	for _, tt := range []struct {
		name, a, b string
		equal      bool
	}{
		{"nested keys", ` {"b":2,"a":{"d":4,"c":3}} `, `{"a":{"c":3,"d":4},"b":2}`, true},
		{"escaped strings", `{"service":"\u0061"}`, `{"service":"a"}`, true},
		{"array objects", `[{"b":2,"a":1}]`, `[{"a":1,"b":2}]`, true},
		{"array order", `[1,2]`, `[2,1]`, false},
		{"large integers", `{"n":9007199254740992}`, `{"n":9007199254740993}`, false},
		{"precise decimals", `{"n":0.12345678901234567890}`, `{"n":0.12345678901234567891}`, false},
		{"huge exponent", ` {"n":1e999} `, `{"n":1e999}`, true},
		{"numeric spelling", `{"n":1}`, `{"n":1.0}`, false},
		{"signed zero", `-0`, `0`, false},
		{"same malformed input", `{"a":`, `{"a":`, true},
		{"malformed whitespace", `{"a":`, ` {"a":`, false},
		{"trailing data", `{} {}`, `{}`, false},
		// Duplicate object keys collapse to the last value, matching encoding/json.
		{"duplicate keys", `{"a":1,"a":2}`, `{"a":2}`, true},
		{"escaped unicode", `{"s":"\u00e9"}`, `{"s":"é"}`, true},
		{"null object", `null`, `{}`, false},
		{"blank object", `   `, `{}`, false},
		{"empty input self", ``, ``, true},
		{"empty object absent", ``, `{}`, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := canonicalInput(json.RawMessage(tt.a)) == canonicalInput(json.RawMessage(tt.b)); got != tt.equal {
				t.Errorf("canonical equality = %v, want %v", got, tt.equal)
			}
		})
	}
}

func TestInvestigationRecordsDispatchedCalls(t *testing.T) {
	client := &scriptedLLM{steps: []func(llm.Request) (llm.Response, error){
		toolCall(use("1", "inspect_service", `{"b":2,"a":1}`)),
		toolCall(use("2", "inspect_service", ` {"a":1,"b":2} `),
			use("3", "inspect_service", `{"n":9007199254740992}`),
			use("4", "inspect_service", `{"n":9007199254740993}`)),
		toolCall(use("5", submitToolName, validSubmission), use("6", "inspect_service", `{}`)),
	}}
	a, tool := newAgent(t, client, Config{})
	start := time.Now()
	res, err := a.Investigate(context.Background(), testIncident)
	end := time.Now()
	if err != nil {
		t.Fatal(err)
	}
	if tool.calls != 4 || res.Stats.ToolCalls != 4 || len(res.ToolCalls) != 4 || res.Stats.DuplicateToolCalls != 1 {
		t.Fatalf("calls = %d, stats = %+v, record = %+v", tool.calls, res.Stats, res.ToolCalls)
	}
	for i, call := range res.ToolCalls {
		if call.Seq != i+1 || call.Tool != "inspect_service" || call.ErrorCode != "" || call.Duration <= 0 {
			t.Errorf("call %d = %+v", i, call)
		}
		if call.StartedAt.Before(start) || call.StartedAt.After(end) || call.StartedAt.Location() != time.UTC {
			t.Errorf("call timestamp = %v, want UTC within investigation", call.StartedAt)
		}
		if i > 0 && call.StartedAt.Before(res.ToolCalls[i-1].StartedAt) {
			t.Error("call timestamps out of order")
		}
	}
	if string(res.ToolCalls[1].Input) != ` {"a":1,"b":2} ` {
		t.Errorf("record must retain raw input, got %s", res.ToolCalls[1].Input)
	}
	// Output sent to the model is unchanged: no sequence or record metadata.
	if got := client.requests[1].Messages[2].Content[0].Text; got != string(res.ToolCalls[0].Output) {
		t.Errorf("model output = %s, recorded output = %s", got, res.ToolCalls[0].Output)
	}
	// All repeated calls execute normally; each returns its own output.
	if !strings.Contains(string(res.ToolCalls[1].Output), `"a":1`) {
		t.Errorf("duplicate output = %s", res.ToolCalls[1].Output)
	}
}

type recordTool struct {
	call func(context.Context, json.RawMessage) (any, error)
}

func (*recordTool) Name() string                 { return "query_metrics" }
func (*recordTool) Description() string          { return "fixture" }
func (*recordTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (s *recordTool) Call(ctx context.Context, in json.RawMessage) (any, error) {
	return s.call(ctx, in)
}

func TestInvestigationRecordsFailuresAndExcludesUndispatched(t *testing.T) {
	fixture := &recordTool{call: func(_ context.Context, in json.RawMessage) (any, error) {
		if !json.Valid(in) {
			return nil, tools.InvalidInput("invalid JSON")
		}
		return nil, errors.New("backend failed")
	}}
	client := &scriptedLLM{steps: []func(llm.Request) (llm.Response, error){
		toolCall(use("1", "query_metrics", `nope`), use("2", "query_metrics", `nope`),
			use("3", "unknown", `{}`), use("4", "query_metrics", `{}`),
			use("5", "unknown", `{}`), use("6", submitToolName, `{}`)),
	}}
	cfg := Config{MaxToolCalls: 4, MaxLLMRequests: 1}
	a := New(client, mustRegistry(t, cfg, fixture), discardLogger(), cfg)
	res, err := a.Investigate(context.Background(), testIncident)
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("err = %v, want budget exhausted", err)
	}
	s := res.Stats
	if s.ToolCalls != 4 || s.FailedToolCalls != 4 || s.InvalidToolCalls != 3 || s.DuplicateToolCalls != 1 || s.RejectedDiagnoses != 1 {
		t.Errorf("stats = %+v", s)
	}
	if !reflect.DeepEqual(s.ToolCallsByName, map[string]int{"query_metrics": 3, "unknown": 1}) {
		t.Errorf("per-tool counts = %v", s.ToolCallsByName)
	}
	want := []tools.ErrorCode{tools.ErrInvalidInput, tools.ErrInvalidInput, tools.ErrUnknownTool, tools.ErrExecutionFailed}
	if len(res.ToolCalls) != len(want) {
		t.Fatalf("records = %+v", res.ToolCalls)
	}
	for i, call := range res.ToolCalls {
		if call.Seq != i+1 || call.ErrorCode != want[i] || call.Output != nil {
			t.Errorf("record %d = %+v", i, call)
		}
	}
}

func TestToolCallPayloadOwnership(t *testing.T) {
	input := json.RawMessage(`{"n":1}`)
	output := json.RawMessage(`{"n":2}`)
	fixture := &recordTool{call: func(_ context.Context, in json.RawMessage) (any, error) {
		in[5] = '9' // A tool cannot rewrite the pre-dispatch input snapshot.
		return output, nil
	}}
	client := &scriptedLLM{steps: []func(llm.Request) (llm.Response, error){
		toolCall(llm.Block{Type: llm.BlockToolUse, ToolName: "query_metrics", ToolInput: input}),
		toolCall(use("2", submitToolName, validSubmission)),
	}}
	a := New(client, mustRegistry(t, Config{}, fixture), discardLogger(), Config{})
	res, err := a.Investigate(context.Background(), testIncident)
	if err != nil {
		t.Fatal(err)
	}
	input[5], output[5] = '8', '8'
	if string(res.ToolCalls[0].Input) != `{"n":1}` || string(res.ToolCalls[0].Output) != `{"n":2}` {
		t.Errorf("mutated record = %+v", res.ToolCalls[0])
	}
	// Registry-result storage is copied even if a future registry reuses buffers.
	var record Result
	registryOutput := json.RawMessage(`{"ok":true}`)
	record.recordToolCall("query_metrics", nil, time.Now(), tools.Result{Output: registryOutput}, map[toolCallKey]struct{}{})
	registryOutput[0] = 'x'
	if string(record.ToolCalls[0].Output) != `{"ok":true}` {
		t.Error("record aliases registry output")
	}
}

func TestToolCallRecordSurvivesInvestigationFailure(t *testing.T) {
	for _, failure := range []string{"budget", "llm error", "cancelled", "timeout", "model stopped"} {
		t.Run(failure, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cfg := Config{}
			if failure == "budget" {
				cfg.MaxLLMRequests = 1
			}
			if failure == "timeout" {
				cfg.MaxDuration = 20 * time.Millisecond
			}
			client := llmFunc(func(ctx context.Context, req llm.Request) (llm.Response, error) {
				if len(req.Messages) == 1 {
					return toolCall(use("1", "inspect_service", `{}`))(req)
				}
				switch failure {
				case "cancelled":
					cancel()
					return llm.Response{}, ctx.Err()
				case "timeout":
					<-ctx.Done()
					return llm.Response{}, ctx.Err()
				case "model stopped":
					return textReply("", llm.StopRefusal)(req)
				default:
					return llm.Response{}, errors.New("provider failed")
				}
			})
			a, _ := newAgent(t, client, cfg)
			res, err := a.Investigate(ctx, testIncident)
			if err == nil || len(res.ToolCalls) != 1 || res.ToolCalls[0].Seq != 1 || res.Stats.ToolCalls != 1 {
				t.Errorf("result = %+v, err = %v", res, err)
			}
		})
	}
}

func TestToolCallRecordIncludesCancellationDuringDispatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fixture := &recordTool{call: func(ctx context.Context, _ json.RawMessage) (any, error) {
		cancel()
		return nil, ctx.Err()
	}}
	client := llmFunc(func(ctx context.Context, req llm.Request) (llm.Response, error) {
		if ctx.Err() != nil {
			return llm.Response{}, ctx.Err()
		}
		return toolCall(use("1", "query_metrics", `{}`))(req)
	})
	a := New(client, mustRegistry(t, Config{}, fixture), discardLogger(), Config{})
	res, err := a.Investigate(ctx, testIncident)
	if !errors.Is(err, context.Canceled) || len(res.ToolCalls) != 1 || res.ToolCalls[0].ErrorCode != tools.ErrExecutionFailed {
		t.Errorf("result = %+v, err = %v", res, err)
	}
}

func TestReusedAgentStartsNewRecord(t *testing.T) {
	client := llmFunc(func(_ context.Context, req llm.Request) (llm.Response, error) {
		if len(req.Messages) == 1 {
			return toolCall(use("1", "inspect_service", `{}`))(req)
		}
		return toolCall(use("2", submitToolName, validSubmission))(req)
	})
	a, _ := newAgent(t, client, Config{})
	for range 2 {
		res, err := a.Investigate(context.Background(), testIncident)
		if err != nil || len(res.ToolCalls) != 1 || res.ToolCalls[0].Seq != 1 || res.Stats.DuplicateToolCalls != 0 {
			t.Fatalf("result = %+v, err = %v", res, err)
		}
	}
}

// Two identical calls in one assistant message are one repetition, and a
// registry failure that is neither invalid input nor an unknown tool is
// recorded without being counted as invalid.
func TestToolCallRecordCapturesRegistryFailures(t *testing.T) {
	steps := func() *scriptedLLM {
		return &scriptedLLM{steps: []func(llm.Request) (llm.Response, error){
			toolCall(use("1", "query_metrics", `{}`)),
			toolCall(use("2", submitToolName, validSubmission)),
		}}
	}
	for _, tt := range []struct {
		name string
		opts tools.Options
		call func(context.Context, json.RawMessage) (any, error)
		want tools.ErrorCode
	}{
		{"call timeout", tools.Options{CallTimeout: 20 * time.Millisecond},
			func(ctx context.Context, _ json.RawMessage) (any, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			}, tools.ErrTimeout},
		{"output too large", tools.Options{MaxOutputBytes: 16},
			func(context.Context, json.RawMessage) (any, error) { return strings.Repeat("x", 64), nil },
			tools.ErrOutputTooLarge},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fixture := &recordTool{call: tt.call}
			a := New(steps(), mustRegistryOpts(t, Config{}, tt.opts, fixture), discardLogger(), Config{})
			res, err := a.Investigate(context.Background(), testIncident)
			if err != nil {
				t.Fatal(err)
			}
			if len(res.ToolCalls) != 1 {
				t.Fatalf("records = %+v", res.ToolCalls)
			}
			if call := res.ToolCalls[0]; call.ErrorCode != tt.want || call.Output != nil || call.Duration <= 0 {
				t.Errorf("record = %+v, want %s with no output", call, tt.want)
			}
			if s := res.Stats; s.ToolCalls != 1 || s.FailedToolCalls != 1 || s.InvalidToolCalls != 0 {
				t.Errorf("stats = %+v", s)
			}
		})
	}
}

func TestDuplicatesWithinOneMessage(t *testing.T) {
	client := &scriptedLLM{steps: []func(llm.Request) (llm.Response, error){
		toolCall(use("1", "inspect_service", `{"a":1}`), use("2", "inspect_service", `{"a":1}`)),
		toolCall(use("3", submitToolName, validSubmission)),
	}}
	a, tool := newAgent(t, client, Config{})
	res, err := a.Investigate(context.Background(), testIncident)
	if err != nil {
		t.Fatal(err)
	}
	if tool.calls != 2 || res.Stats.ToolCalls != 2 || res.Stats.DuplicateToolCalls != 1 || len(res.ToolCalls) != 2 {
		t.Errorf("calls = %d, stats = %+v, record = %+v", tool.calls, res.Stats, res.ToolCalls)
	}
}

// Duplicate identity uses the input snapshot taken before dispatch, so a tool
// that rewrites its argument cannot make two identical calls look distinct.
func TestDuplicateIdentityUsesPreDispatchInput(t *testing.T) {
	calls := 0
	fixture := &recordTool{call: func(_ context.Context, in json.RawMessage) (any, error) {
		calls++
		if calls == 1 {
			in[5] = '9' // a post-dispatch snapshot would read {"n":9}
		}
		return json.RawMessage(`{"ok":true}`), nil
	}}
	client := &scriptedLLM{steps: []func(llm.Request) (llm.Response, error){
		toolCall(use("1", "query_metrics", `{"n":1}`), use("2", "query_metrics", `{"n":1}`)),
		toolCall(use("3", submitToolName, validSubmission)),
	}}
	a := New(client, mustRegistry(t, Config{}, fixture), discardLogger(), Config{})
	res, err := a.Investigate(context.Background(), testIncident)
	if err != nil {
		t.Fatal(err)
	}
	if res.Stats.DuplicateToolCalls != 1 || len(res.ToolCalls) != 2 {
		t.Fatalf("stats = %+v, record = %+v", res.Stats, res.ToolCalls)
	}
	for i, call := range res.ToolCalls {
		if string(call.Input) != `{"n":1}` {
			t.Errorf("call %d input = %s, want the pre-dispatch bytes", i, call.Input)
		}
	}
}
