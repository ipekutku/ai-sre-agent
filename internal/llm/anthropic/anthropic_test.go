package anthropic

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ipekutku/ai-sre-agent/internal/llm"
)

// fakeAPI is a minimal Messages API: it records request bodies and replies
// with the configured status and body.
type fakeAPI struct {
	status int
	body   string
	calls  atomic.Int32
	bodies []map[string]any
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.calls.Add(1)
	if r.URL.Path != "/v1/messages" {
		http.NotFound(w, r)
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.bodies = append(f.bodies, body)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("retry-after-ms", "1")
	w.WriteHeader(f.status)
	_, _ = io.WriteString(w, f.body)
}

func newTestClient(t *testing.T, api *fakeAPI, cfg Config) *Client {
	t.Helper()
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	cfg.APIKey = "test-key"
	cfg.BaseURL = srv.URL
	cfg.HTTPClient = srv.Client()
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

const toolUseResponse = `{
  "id": "msg_1", "type": "message", "role": "assistant", "model": "claude-sonnet-5-5",
  "content": [
    {"type": "thinking", "thinking": "", "signature": "sig-abc"},
    {"type": "text", "text": "Checking latency."},
    {"type": "tool_use", "id": "toolu_1", "name": "query_metrics", "input": {"query": "up"}}
  ],
  "stop_reason": "tool_use", "stop_sequence": null,
  "usage": {"input_tokens": 1000, "output_tokens": 200, "cache_creation_input_tokens": 400, "cache_read_input_tokens": 3000}
}`

var testTool = llm.Tool{
	Name:        "query_metrics",
	Description: "Run PromQL",
	InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}`),
}

func TestGenerateRequestAndResponse(t *testing.T) {
	api := &fakeAPI{status: 200, body: toolUseResponse}
	c := newTestClient(t, api, Config{Effort: "high"})

	resp, err := c.Generate(context.Background(), llm.Request{
		System:   "You are an SRE.",
		Messages: []llm.Message{llm.UserText("Investigate inc-001")},
		Tools:    []llm.Tool{testTool},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Request shape.
	body := api.bodies[0]
	if body["model"] != DefaultModel || body["max_tokens"] != float64(16000) {
		t.Errorf("model/max_tokens = %v/%v", body["model"], body["max_tokens"])
	}
	if oc, _ := body["output_config"].(map[string]any); oc["effort"] != "high" {
		t.Errorf("output_config = %v, want effort high", body["output_config"])
	}
	if _, ok := body["thinking"]; ok {
		t.Errorf("thinking should be left to the model default, got %v", body["thinking"])
	}
	if body["cache_control"] == nil {
		t.Error("expected top-level cache_control")
	}
	system, _ := body["system"].([]any)
	if len(system) != 1 || system[0].(map[string]any)["text"] != "You are an SRE." {
		t.Errorf("system = %v", body["system"])
	}
	tools, _ := body["tools"].([]any)
	tool := tools[0].(map[string]any)
	schema := tool["input_schema"].(map[string]any)
	if tool["name"] != "query_metrics" || schema["type"] != "object" || schema["additionalProperties"] != false {
		t.Errorf("tool = %v", tool)
	}
	if req, _ := schema["required"].([]any); len(req) != 1 || req[0] != "query" {
		t.Errorf("required = %v", schema["required"])
	}

	// Response mapping.
	if resp.StopReason != llm.StopToolUse || resp.Model != "claude-sonnet-5-5" || resp.Latency <= 0 {
		t.Errorf("response = %+v", resp)
	}
	if resp.Text() != "Checking latency." {
		t.Errorf("text = %q", resp.Text())
	}
	uses := resp.ToolUses()
	if len(uses) != 1 || uses[0].ToolUseID != "toolu_1" || uses[0].ToolName != "query_metrics" {
		t.Fatalf("tool uses = %+v", uses)
	}
	var input map[string]string
	if err := json.Unmarshal(uses[0].ToolInput, &input); err != nil || input["query"] != "up" {
		t.Errorf("tool input = %s (%v)", uses[0].ToolInput, err)
	}

	// Usage and cost: (1000*2 + 200*10 + 3000*0.20 + 400*2.50) / 1e6 = 0.0056
	u := resp.Usage
	if u.InputTokens != 1000 || u.OutputTokens != 200 || u.CacheReadTokens != 3000 || u.CacheWriteTokens != 400 {
		t.Errorf("usage = %+v", u)
	}
	if !u.CostKnown || math.Abs(u.CostUSD-0.0056) > 1e-9 {
		t.Errorf("cost = %v (known %v), want 0.0056", u.CostUSD, u.CostKnown)
	}
}

// The assistant turn, including its thinking block, must be replayed
// byte-for-byte on the next request; dropping it would invalidate later
// thinking blocks.
func TestGenerateReplaysAssistantTurnVerbatim(t *testing.T) {
	api := &fakeAPI{status: 200, body: toolUseResponse}
	c := newTestClient(t, api, Config{})
	ctx := context.Background()

	history := []llm.Message{llm.UserText("Investigate inc-001")}
	resp, err := c.Generate(ctx, llm.Request{Messages: history, Tools: []llm.Tool{testTool}})
	if err != nil {
		t.Fatal(err)
	}
	history = append(history, resp.Message, llm.Message{
		Role:    llm.RoleUser,
		Content: []llm.Block{llm.ToolResult("toolu_1", `{"result_type":"vector"}`, false)},
	})
	if _, err := c.Generate(ctx, llm.Request{Messages: history, Tools: []llm.Tool{testTool}}); err != nil {
		t.Fatal(err)
	}

	msgs := api.bodies[1]["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("second request has %d messages, want 3", len(msgs))
	}
	assistant := msgs[1].(map[string]any)
	content := assistant["content"].([]any)
	first := content[0].(map[string]any)
	if assistant["role"] != "assistant" || first["type"] != "thinking" || first["signature"] != "sig-abc" {
		t.Errorf("assistant turn not replayed verbatim: %v", assistant)
	}
	if len(content) != 3 {
		t.Errorf("assistant turn has %d blocks, want 3 (thinking, text, tool_use)", len(content))
	}
	result := msgs[2].(map[string]any)["content"].([]any)[0].(map[string]any)
	if result["type"] != "tool_result" || result["tool_use_id"] != "toolu_1" {
		t.Errorf("tool result = %v", result)
	}
}

func TestGenerateErrors(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		body      string
		wantKind  llm.ErrorKind
		wantCalls int32
	}{
		{"bad request is not retried", 400, `{"type":"error","error":{"type":"invalid_request_error","message":"bad"}}`, llm.ErrInvalidRequest, 1},
		{"auth", 401, `{"type":"error","error":{"type":"authentication_error","message":"no"}}`, llm.ErrAuth, 1},
		{"rate limit is retried", 429, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`, llm.ErrRateLimited, 2},
		{"overloaded is retried", 529, `{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`, llm.ErrUnavailable, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := &fakeAPI{status: tt.status, body: tt.body}
			c := newTestClient(t, api, Config{MaxRetries: 1})
			_, err := c.Generate(context.Background(), llm.Request{Messages: []llm.Message{llm.UserText("hi")}})
			if llm.KindOf(err) != tt.wantKind {
				t.Fatalf("err = %v, want kind %s", err, tt.wantKind)
			}
			if got := api.calls.Load(); got != tt.wantCalls {
				t.Errorf("calls = %d, want %d", got, tt.wantCalls)
			}
			if strings.Contains(err.Error(), "test-key") {
				t.Error("error message leaks the API key")
			}
		})
	}
}

func TestGenerateTransportErrors(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	c, err := New(Config{APIKey: "k", BaseURL: url, MaxRetries: -1})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Generate(context.Background(), llm.Request{Messages: []llm.Message{llm.UserText("hi")}})
	if llm.KindOf(err) != llm.ErrTransport {
		t.Errorf("unreachable API: err = %v, want transport", err)
	}

	// A cancelled context returns promptly.
	// The handler also returns when the test ends, so Close never waits on it
	// (the server does not always notice the client hanging up).
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer slow.Close()
	defer close(release) // runs before slow.Close
	c, _ = New(Config{APIKey: "k", BaseURL: slow.URL, MaxRetries: -1})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = c.Generate(ctx, llm.Request{Messages: []llm.Message{llm.UserText("hi")}})
	if llm.KindOf(err) != llm.ErrTransport || time.Since(start) > 5*time.Second {
		t.Errorf("cancelled: err = %v after %v", err, time.Since(start))
	}
}

func TestGenerateRejectsInvalidRequests(t *testing.T) {
	api := &fakeAPI{status: 200, body: toolUseResponse}
	c := newTestClient(t, api, Config{})
	for name, req := range map[string]llm.Request{
		"bad role":   {Messages: []llm.Message{{Role: "system", Content: []llm.Block{{Type: llm.BlockText, Text: "x"}}}}},
		"bad block":  {Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.Block{{Type: "image"}}}}},
		"bad schema": {Messages: []llm.Message{llm.UserText("x")}, Tools: []llm.Tool{{Name: "t", InputSchema: json.RawMessage(`nope`)}}},
	} {
		if _, err := c.Generate(context.Background(), req); llm.KindOf(err) != llm.ErrInvalidRequest {
			t.Errorf("%s: err = %v, want invalid_request", name, err)
		}
	}
	if api.calls.Load() != 0 {
		t.Errorf("invalid requests reached the API %d times", api.calls.Load())
	}
}

func TestNewValidatesEffort(t *testing.T) {
	if _, err := New(Config{Effort: "extreme"}); err == nil {
		t.Error("expected error for unknown effort")
	}
}

func TestUnknownModelCost(t *testing.T) {
	api := &fakeAPI{status: 200, body: strings.Replace(toolUseResponse, `"model": "claude-sonnet-5-5"`, `"model": "claude-future-9"`, 1)}
	c := newTestClient(t, api, Config{Model: "claude-future-9"})
	resp, err := c.Generate(context.Background(), llm.Request{Messages: []llm.Message{llm.UserText("hi")}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Usage.CostKnown || resp.Usage.InputTokens != 1000 {
		t.Errorf("usage = %+v, want tokens but unknown cost", resp.Usage)
	}
}

func TestCheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/models/"+DefaultModel {
			_, _ = io.WriteString(w, `{"type":"model","id":"`+DefaultModel+`","display_name":"Sonnet","created_at":"2026-01-01T00:00:00Z"}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"not_found_error","message":"model not found"}}`)
	}))
	defer srv.Close()

	ok, _ := New(Config{APIKey: "k", BaseURL: srv.URL, MaxRetries: -1})
	if err := ok.Check(context.Background()); err != nil {
		t.Errorf("valid model: %v", err)
	}
	bad, _ := New(Config{APIKey: "k", BaseURL: srv.URL, MaxRetries: -1, Model: "claude-nope"})
	if err := bad.Check(context.Background()); llm.KindOf(err) != llm.ErrInvalidRequest {
		t.Errorf("unknown model: err = %v, want invalid_request", err)
	}

	// No credentials anywhere: reported as auth, without a network call.
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	none, _ := New(Config{BaseURL: srv.URL, MaxRetries: -1})
	if err := none.Check(context.Background()); llm.KindOf(err) != llm.ErrAuth {
		t.Errorf("no credentials: err = %v, want auth", err)
	}
}

// Regression: the API's error message must be surfaced; the type alone
// ("invalid_request_error") did not explain a failing run.
func TestErrorIncludesAPIMessage(t *testing.T) {
	api := &fakeAPI{status: 400, body: `{"type":"error","error":{"type":"invalid_request_error","message":"Your credit balance is too low"}}`}
	c := newTestClient(t, api, Config{MaxRetries: -1})
	_, err := c.Generate(context.Background(), llm.Request{Messages: []llm.Message{llm.UserText("hi")}})
	if err == nil || !strings.Contains(err.Error(), "invalid_request_error: Your credit balance is too low") {
		t.Errorf("err = %v, want the API message included", err)
	}
}

func TestWorkspaceHeader(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("anthropic-workspace-id"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, toolUseResponse)
	}))
	defer srv.Close()

	for _, ws := range []string{"wrkspc_123", ""} {
		c, _ := New(Config{APIKey: "k", BaseURL: srv.URL, MaxRetries: -1, WorkspaceID: ws})
		if _, err := c.Generate(context.Background(), llm.Request{Messages: []llm.Message{llm.UserText("hi")}}); err != nil {
			t.Fatal(err)
		}
	}
	if len(got) != 2 || got[0] != "wrkspc_123" || got[1] != "" {
		t.Errorf("anthropic-workspace-id headers = %q, want [wrkspc_123 \"\"]", got)
	}
}
