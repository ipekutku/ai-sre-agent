package anthropic

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ipekutku/ai-sre-agent/internal/llm"
)

func TestAttemptsArePerGenerationIncludingRetries(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/models/"+DefaultModel {
			_, _ = io.WriteString(w, `{"type":"model","id":"`+DefaultModel+`","display_name":"Sonnet","created_at":"2026-01-01T00:00:00Z"}`)
			return
		}
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "retry-") && r.Header.Get("X-Stainless-Retry-Count") == "0" {
			w.Header().Set("retry-after-ms", "1")
			w.WriteHeader(529)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`)
			return
		}
		_, _ = io.WriteString(w, toolUseResponse)
	}))
	defer srv.Close()
	c, err := New(Config{APIKey: "test-key", BaseURL: srv.URL, HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Reuse the same client concurrently: the model preflight and other
	// generations must not contribute to any individual attempt counter.
	var wg sync.WaitGroup
	for _, text := range []string{"retry-a", "ok-b", "retry-c", "ok-d"} {
		wg.Go(func() {
			resp, err := c.Generate(context.Background(), llm.Request{Messages: []llm.Message{llm.UserText(text)}})
			want := 1
			if strings.HasPrefix(text, "retry-") {
				want = 2
			}
			if err != nil || resp.Attempts != want || resp.StopReason != llm.StopToolUse {
				t.Errorf("%s: response = %+v, err = %v, want %d attempts", text, resp, err, want)
			}
		})
	}
	wg.Wait()
	if calls.Load() != 6 {
		t.Errorf("Messages requests = %d, want 6", calls.Load())
	}
}

type attemptTransport func(*http.Request) (*http.Response, error)

func (f attemptTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c cancelOnClose) Close() error {
	c.cancel()
	return c.ReadCloser.Close()
}

func TestAttemptsOnCancellationDuringBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	httpClient := &http.Client{Transport: attemptTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		body := io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`))
		delay := "1"
		if calls == 2 {
			// The SDK closes a retriable response just before waiting for the
			// next attempt. Cancel there, without timing-dependent sleeps.
			body = cancelOnClose{ReadCloser: body, cancel: cancel}
			delay = "60000"
		}
		return &http.Response{StatusCode: 529, Header: http.Header{"Content-Type": {"application/json"}, "Retry-After-Ms": {delay}}, Body: body, Request: r}, nil
	})}
	c, err := New(Config{APIKey: "test-key", BaseURL: "http://example.invalid", HTTPClient: httpClient, MaxRetries: 3})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	resp, err := c.Generate(ctx, llm.Request{Messages: []llm.Message{llm.UserText("hi")}})
	if !errors.Is(err, context.Canceled) || resp.Attempts != 2 || calls != 2 {
		t.Errorf("response = %+v, calls = %d, err = %v", resp, calls, err)
	}
	if time.Since(start) > time.Second {
		t.Error("cancellation waited through backoff")
	}
}
