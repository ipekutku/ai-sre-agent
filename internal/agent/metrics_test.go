package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	"github.com/ipekutku/ai-sre-agent/internal/llm"
)

func TestInvestigationMetricsAndTelemetry(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "success"
		if fail {
			name = "failed generation with partial response"
		}
		t.Run(name, func(t *testing.T) {
			providerErr := &llm.Error{Kind: llm.ErrUnavailable, Message: "unavailable"}
			client := &scriptedLLM{steps: []func(llm.Request) (llm.Response, error){
				func(req llm.Request) (llm.Response, error) {
					resp, err := toolCall(use("1", "inspect_service", `{}`), use("2", "inspect_service", `{}`),
						use("3", "unknown", `{}`), use("4", submitToolName, `{}`))(req)
					resp.Attempts = 2
					return resp, err
				},
				func(req llm.Request) (llm.Response, error) {
					resp, _ := toolCall(use("5", "inspect_service", `{}`), use("6", submitToolName, validSubmission))(req)
					resp.Attempts = 3
					if fail {
						return resp, providerErr
					}
					return resp, nil
				},
			}}
			a, rec := newTracedAgent(t, client, Config{})
			var logs bytes.Buffer
			a.logger = slog.New(slog.NewJSONHandler(&logs, nil))
			res, err := a.Investigate(context.Background(), testIncident)
			if (!fail && err != nil) || (fail && !errors.Is(err, providerErr)) {
				t.Fatalf("err = %v, want failure %v", err, fail)
			}
			if res.Stats.LLMRequests != 2 || res.Stats.LLMRetries != 3 {
				t.Errorf("stats = %+v", res.Stats)
			}
			if fail && (res.Diagnosis.RootCause.Code != "" || res.Stats.ToolCalls != 3 || len(res.ToolCalls) != 3 || res.Stats.Usage.InputTokens != 100) {
				t.Errorf("partial response treated as success: %+v", res)
			}
			root := findSpan(t, rec.Ended(), investigationSpan)
			rootAttrs := attrs(root)
			for key, want := range map[attribute.Key]int{
				"investigation.rejected_diagnoses":   res.Stats.RejectedDiagnoses,
				"investigation.duplicate_tool_calls": res.Stats.DuplicateToolCalls,
				"investigation.invalid_tool_calls":   res.Stats.InvalidToolCalls,
				"investigation.llm_retries":          3,
			} {
				if got := rootAttrs[key].AsInt64(); got != int64(want) {
					t.Errorf("%s = %d, want %d", key, got, want)
				}
			}
			for _, span := range rec.Ended() {
				if span.Name() != llmRequestSpan && span.Name() != finalDiagnosisSpan {
					continue
				}
				a := attrs(span)
				n := a["llm.request_number"].AsInt64()
				if a["llm.attempts"].AsInt64() != n+1 || a["llm.retries"].AsInt64() != n {
					t.Errorf("request %d attributes = %v", n, a)
				}
				if fail && n == 2 && span.Status().Code != codes.Error {
					t.Error("failed request span lacks error status")
				}
			}
			var requestLogs, summaries int
			for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
				var entry map[string]any
				if err := json.Unmarshal([]byte(line), &entry); err != nil {
					t.Fatal(err)
				}
				switch entry["msg"] {
				case "llm request", "llm request failed":
					requestLogs++
					n := entry["request"].(float64)
					if entry["attempts"] != n+1 || entry["retries"] != n {
						t.Errorf("request log metadata = %v", entry)
					}
				case "investigation completed", "investigation failed":
					summaries++
					if entry["llm_retries"] != float64(3) || entry["invalid_tool_calls"] != float64(1) || entry["rejected_diagnoses"] != float64(1) {
						t.Errorf("summary metadata = %v", entry)
					}
				}
			}
			if requestLogs != 2 || summaries != 1 {
				t.Errorf("request logs = %d, summaries = %d", requestLogs, summaries)
			}
		})
	}
}

// A generation cancelled during SDK backoff still contributes its attempts to
// the investigation totals and to the request span.
func TestCanceledGenerationRetriesReachStats(t *testing.T) {
	client := llmFunc(func(context.Context, llm.Request) (llm.Response, error) {
		return llm.Response{Attempts: 2, Latency: time.Millisecond}, context.Canceled
	})
	a, rec := newTracedAgent(t, client, Config{})
	res, err := a.Investigate(context.Background(), testIncident)
	if !errors.Is(err, context.Canceled) || res.Stats.LLMRequests != 1 || res.Stats.LLMRetries != 1 {
		t.Fatalf("result = %+v, err = %v", res, err)
	}
	requestAttrs := attrs(findSpan(t, rec.Ended(), llmRequestSpan))
	if requestAttrs["llm.attempts"].AsInt64() != 2 || requestAttrs["llm.retries"].AsInt64() != 1 {
		t.Errorf("request attributes = %v, want 2 attempts and 1 retry", requestAttrs)
	}
}

func TestZeroAttemptsDoesNotCountRetry(t *testing.T) {
	client := llmFunc(func(context.Context, llm.Request) (llm.Response, error) {
		return llm.Response{}, &llm.Error{Kind: llm.ErrInvalidRequest, Message: "invalid request"}
	})
	a, rec := newTracedAgent(t, client, Config{})
	res, err := a.Investigate(context.Background(), testIncident)
	if err == nil || res.Stats.LLMRequests != 1 || res.Stats.LLMRetries != 0 {
		t.Errorf("result = %+v, err = %v", res, err)
	}
	requestAttrs := attrs(findSpan(t, rec.Ended(), llmRequestSpan))
	for _, key := range []attribute.Key{"llm.attempts", "llm.retries"} {
		if value, ok := requestAttrs[key]; !ok || value.AsInt64() != 0 {
			t.Errorf("%s = %v (present %v), want explicit zero", key, value, ok)
		}
	}
}
