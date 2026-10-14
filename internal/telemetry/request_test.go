package telemetry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/ipekutku/ai-sre-agent/internal/httpmetrics"
	"github.com/ipekutku/ai-sre-agent/internal/services/checkout"
)

func TestValidRequestID(t *testing.T) {
	tests := []struct {
		id   string
		want bool
	}{
		{"", false},
		{"abc-123_x.y", true},
		{"0123456789abcdef0123456789abcdef", true},
		{strings.Repeat("a", 64), true},
		{strings.Repeat("a", 65), false},
		{"has space", false},
		{"line\nbreak", false},
		{`quote"`, false},
		{"ünïcode", false},
	}
	for _, tt := range tests {
		if got := validRequestID(tt.id); got != tt.want {
			t.Errorf("validRequestID(%q) = %v, want %v", tt.id, got, tt.want)
		}
	}
}

var generatedID = regexp.MustCompile(`^[0-9a-f]{32}$`)

func TestHandlerRequestID(t *testing.T) {
	tests := []struct {
		name     string
		incoming string
		keep     bool
	}{
		{"none", "", false},
		{"valid", "client-req-42", true},
		{"invalid", "bad id\nwith newline", false},
		{"too long", strings.Repeat("a", 65), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var seen string
			app := http.NewServeMux()
			app.HandleFunc("GET /x", func(_ http.ResponseWriter, r *http.Request) { seen = RequestID(r.Context()) })
			_, tp := newRecorder()
			h := Handler(tp, discardLogger(), app)

			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			if tt.incoming != "" {
				req.Header.Set(RequestIDHeader, tt.incoming)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)

			if tt.keep && seen != tt.incoming {
				t.Errorf("request ID = %q, want the caller's %q", seen, tt.incoming)
			}
			if !tt.keep && !generatedID.MatchString(seen) {
				t.Errorf("request ID = %q, want a generated 32-hex ID", seen)
			}
			if got := w.Header().Get(RequestIDHeader); got != seen {
				t.Errorf("response %s = %q, want %q", RequestIDHeader, got, seen)
			}
		})
	}
}

func TestHandlerAccessLog(t *testing.T) {
	rec, tp := newRecorder()
	var buf syncBuffer
	app := http.NewServeMux()
	app.HandleFunc("GET /items/{id}", func(http.ResponseWriter, *http.Request) {})
	app.HandleFunc("GET /fail", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) })
	h := Handler(tp, NewLogger(&buf, "test-service", nil), httpmetrics.Handler(httpmetrics.NewRegistry(), app), "/metrics")

	for _, path := range []string{"/items/7", "/fail", "/metrics", "/nope"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
	}

	lines := buf.lines(t)
	if len(lines) != 3 {
		t.Fatalf("got %d access-log lines, want 3 (none for /metrics):\n%s", len(lines), buf.String())
	}
	spans := rec.Ended()
	if len(spans) != 3 {
		t.Fatalf("got %d spans, want 3", len(spans))
	}
	want := []struct {
		level, route string
		status       float64
	}{
		{"INFO", "/items/{id}", 200},
		{"ERROR", "/fail", 502},
		{"INFO", "unmatched", 404},
	}
	for i, w := range want {
		l := lines[i]
		if l["msg"] != "request" || l["method"] != "GET" || l["level"] != w.level || l["route"] != w.route || l["status"] != w.status {
			t.Errorf("line %d = %v, want level=%s route=%s status=%v", i, l, w.level, w.route, w.status)
		}
		if d, ok := l["duration_ms"].(float64); !ok || d < 0 {
			t.Errorf("line %d duration_ms = %v", i, l["duration_ms"])
		}
		if !generatedID.MatchString(str(l["request_id"])) {
			t.Errorf("line %d request_id = %v", i, l["request_id"])
		}
		// The access log is written inside the server span.
		if sc := spans[i].SpanContext(); l["trace_id"] != sc.TraceID().String() || l["span_id"] != sc.SpanID().String() {
			t.Errorf("line %d trace/span = %v/%v, want %s/%s", i, l["trace_id"], l["span_id"], sc.TraceID(), sc.SpanID())
		}
	}
}

func TestTransportPropagatesRequestID(t *testing.T) {
	_, tp := newRecorder()
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get(RequestIDHeader))
	}))
	defer srv.Close()
	client := &http.Client{Transport: Transport(tp, "peer", nil)}

	for _, ctx := range []context.Context{withRequestID(context.Background(), "req-1"), context.Background()} {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		resp.Body.Close()
		if req.Header.Get(RequestIDHeader) != "" {
			t.Error("transport modified the caller's request")
		}
	}
	if len(got) != 2 || got[0] != "req-1" || got[1] != "" {
		t.Errorf("received request IDs = %q, want [req-1, \"\"]", got)
	}
}

// TestCheckoutLogsCorrelate checks the logs of one failing checkout request
// when the services are wired as in cmd/: checkout's warning and both
// services' access logs share the request ID and trace ID, locally and in
// exported records.
func TestCheckoutLogsCorrelate(t *testing.T) {
	rec, tp := newRecorder()
	exp, lp := newLogRecorder()
	var invBuf, coBuf syncBuffer

	failing := http.NewServeMux()
	failing.HandleFunc("GET /inventory", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) })
	inventorySrv := httptest.NewServer(Handler(tp, NewLogger(&invBuf, "inventory-api", nil),
		httpmetrics.Handler(httpmetrics.NewRegistry(), failing), "/metrics"))
	defer inventorySrv.Close()

	coLogger := NewLogger(&coBuf, "checkout-api", lp)
	client := checkout.NewInventoryClient(inventorySrv.URL, &http.Client{
		Timeout:   2 * time.Second,
		Transport: Transport(tp, "inventory-api", nil),
	})
	checkoutSrv := httptest.NewServer(Handler(tp, coLogger, checkout.NewHandler(coLogger, client)))
	defer checkoutSrv.Close()

	req, _ := http.NewRequest(http.MethodGet, checkoutSrv.URL+"/checkout?sku=abc", nil)
	req.Header.Set(RequestIDHeader, "req-e2e")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("checkout status = %d, want 502", resp.StatusCode)
	}

	spans := waitForSpans(t, rec, 3)
	var checkoutSpan sdktrace.ReadOnlySpan
	for _, s := range spans {
		if s.Name() == "GET /checkout" {
			checkoutSpan = s
		}
	}
	if checkoutSpan == nil {
		t.Fatalf("no checkout span in %v", names(spans))
	}
	traceID := checkoutSpan.SpanContext().TraceID().String()

	coLines := coBuf.lines(t)
	if len(coLines) != 2 {
		t.Fatalf("checkout logged %d lines, want warning + access log:\n%s", len(coLines), coBuf.String())
	}
	warning, access := coLines[0], coLines[1]
	if warning["msg"] != "inventory lookup failed" || warning["level"] != "WARN" {
		t.Errorf("first checkout line = %v, want the inventory lookup warning", warning)
	}
	if access["msg"] != "request" || access["status"] != float64(502) || access["level"] != "ERROR" {
		t.Errorf("second checkout line = %v, want a 502 error access log", access)
	}
	invLines := waitForLines(t, &invBuf, 1)
	if len(invLines) != 1 || invLines[0]["status"] != float64(500) {
		t.Fatalf("inventory lines = %v, want one 500 access log", invLines)
	}
	for _, l := range []map[string]any{warning, access, invLines[0]} {
		if l["request_id"] != "req-e2e" || l["trace_id"] != traceID {
			t.Errorf("line %q from %v: request_id=%v trace_id=%v, want req-e2e and %s",
				l["msg"], l["service"], l["request_id"], l["trace_id"], traceID)
		}
	}

	records := exp.Records()
	if len(records) != 2 {
		t.Fatalf("exported %d checkout records, want 2", len(records))
	}
	for _, r := range records {
		if r.TraceID().String() != traceID || attributes(r)["request_id"] != "req-e2e" {
			t.Errorf("exported %q: trace=%s request_id=%q, want %s and req-e2e",
				r.Body().AsString(), r.TraceID(), attributes(r)["request_id"], traceID)
		}
	}
}

// waitForLines returns the buffer's JSON lines once there are at least n. A
// server logs its access line just before the response is sent, but the
// caller may still see the response first.
func waitForLines(t *testing.T, b *syncBuffer, n int) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		lines := b.lines(t)
		if len(lines) >= n || time.Now().After(deadline) {
			return lines
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
