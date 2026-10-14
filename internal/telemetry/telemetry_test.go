package telemetry

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/ipekutku/ai-sre-agent/internal/httpmetrics"
	"github.com/ipekutku/ai-sre-agent/internal/services/checkout"
	"github.com/ipekutku/ai-sre-agent/internal/services/inventory"
)

func newRecorder() (*tracetest.SpanRecorder, *sdktrace.TracerProvider) {
	rec := tracetest.NewSpanRecorder()
	return rec, sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
}

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestEnabled(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want bool
	}{
		{"no endpoint", map[string]string{}, false},
		{"endpoint", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://tempo:4318"}, true},
		{"traces endpoint", map[string]string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://tempo:4318/v1/traces"}, true},
		{"disabled", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://tempo:4318", "OTEL_SDK_DISABLED": "true"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Enabled(env(tt.env)); got != tt.want {
				t.Errorf("Enabled = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSetupWithoutEndpointIsNoop(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	tp, shutdown, err := Setup(context.Background(), logger, "test-service", env(nil))
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	defer func() {
		if err := Shutdown(shutdown); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	}()

	_, span := tp.Tracer("test").Start(context.Background(), "op")
	defer span.End()
	if span.IsRecording() {
		t.Error("span is recording without an OTLP endpoint")
	}
}

func TestSetupWithEndpoint(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// Nothing listens here; export failures must not surface as errors.
	tp, shutdown, err := Setup(context.Background(), logger, "test-service",
		env(map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:1"}))
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	_, span := tp.Tracer("test").Start(context.Background(), "op")
	if !span.IsRecording() {
		t.Error("span is not recording with an OTLP endpoint configured")
	}
	span.End()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = shutdown(ctx) // the failed flush is reported to the error handler
}

// TestCheckoutInventoryTrace checks the trace one checkout request produces
// when the services are wired as in cmd/: a checkout server span, a client
// span for the inventory call, and an inventory server span, all in one
// trace with the expected parents.
func TestCheckoutInventoryTrace(t *testing.T) {
	rec, tp := newRecorder()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	inventorySrv := httptest.NewServer(Handler(tp,
		httpmetrics.Handler(httpmetrics.NewRegistry(), inventory.NewHandler(inventory.NewLatency(0))), "/metrics"))
	defer inventorySrv.Close()

	reg := httpmetrics.NewRegistry()
	client := checkout.NewInventoryClient(inventorySrv.URL, &http.Client{
		Timeout:   2 * time.Second,
		Transport: Transport(tp, "inventory-api", httpmetrics.NewClient(reg).Transport("inventory-api", nil)),
	})
	checkoutSrv := httptest.NewServer(Handler(tp, httpmetrics.Handler(reg, checkout.NewHandler(logger, client)), "/metrics"))
	defer checkoutSrv.Close()

	get(t, checkoutSrv.URL+"/checkout?sku=abc")

	spans := waitForSpans(t, rec, 3)
	if len(spans) != 3 {
		t.Fatalf("got %d spans %v, want 3", len(spans), names(spans))
	}
	byName := map[string]sdktrace.ReadOnlySpan{}
	for _, s := range spans {
		byName[s.Name()] = s
	}
	checkoutSpan, clientSpan, inventorySpan := byName["GET /checkout"], byName["GET inventory-api"], byName["GET /inventory"]
	if checkoutSpan == nil || clientSpan == nil || inventorySpan == nil {
		t.Fatalf("span names = %v, want GET /checkout, GET inventory-api, GET /inventory", names(spans))
	}

	if got := checkoutSpan.SpanKind(); got != trace.SpanKindServer {
		t.Errorf("checkout span kind = %v, want server", got)
	}
	if got := clientSpan.SpanKind(); got != trace.SpanKindClient {
		t.Errorf("client span kind = %v, want client", got)
	}
	if got := inventorySpan.SpanKind(); got != trace.SpanKindServer {
		t.Errorf("inventory span kind = %v, want server", got)
	}

	traceID := checkoutSpan.SpanContext().TraceID()
	for _, s := range spans {
		if s.SpanContext().TraceID() != traceID {
			t.Errorf("span %q is in trace %s, want %s", s.Name(), s.SpanContext().TraceID(), traceID)
		}
	}
	if checkoutSpan.Parent().IsValid() {
		t.Errorf("checkout span has parent %s, want a root span", checkoutSpan.Parent().SpanID())
	}
	if clientSpan.Parent().SpanID() != checkoutSpan.SpanContext().SpanID() {
		t.Error("client span is not a child of the checkout span")
	}
	if !inventorySpan.Parent().IsRemote() || inventorySpan.Parent().SpanID() != clientSpan.SpanContext().SpanID() {
		t.Error("inventory span is not a remote child of the client span")
	}
}

func TestHandlerSkipsUntracedPaths(t *testing.T) {
	rec, tp := newRecorder()
	srv := httptest.NewServer(Handler(tp, httpmetrics.Handler(httpmetrics.NewRegistry(), testApp()), "/metrics"))
	defer srv.Close()

	get(t, srv.URL+"/metrics")
	if n := len(rec.Ended()); n != 0 {
		t.Errorf("scrape produced %d spans, want 0", n)
	}
	get(t, srv.URL+"/items/1")
	if got := names(waitForSpans(t, rec, 1)); len(got) != 1 || got[0] != "GET /items/{id}" {
		t.Errorf("spans = %v, want [GET /items/{id}]", got)
	}
}

// Tracing wraps the metrics handler, so the metrics route label must still
// come from the matched pattern.
func TestHandlerPreservesMetricRoutes(t *testing.T) {
	_, tp := newRecorder()
	srv := httptest.NewServer(Handler(tp, httpmetrics.Handler(httpmetrics.NewRegistry(), testApp()), "/metrics"))
	defer srv.Close()

	get(t, srv.URL+"/items/1")
	metrics := get(t, srv.URL+"/metrics")
	if !strings.Contains(metrics, `route="/items/{id}"`) {
		t.Errorf("metrics have no series for route /items/{id}:\n%s", metrics)
	}
}

func TestTransportPropagatesWithoutServerSpan(t *testing.T) {
	_, tp := newRecorder()
	var traceparent string
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		traceparent = r.Header.Get("traceparent")
	}))
	defer srv.Close()

	ctx, span := tp.Tracer("test").Start(context.Background(), "parent")
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err := (&http.Client{Transport: Transport(tp, "peer", nil)}).Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp.Body.Close()
	span.End()

	if !strings.Contains(traceparent, span.SpanContext().TraceID().String()) {
		t.Errorf("traceparent = %q, want trace ID %s", traceparent, span.SpanContext().TraceID())
	}
}

func testApp() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /items/{id}", func(http.ResponseWriter, *http.Request) {})
	return mux
}

func get(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return string(b)
}

// waitForSpans returns the ended spans once there are at least n. A server
// span ends just after the response is sent, so it can lag the client.
func waitForSpans(t *testing.T, rec *tracetest.SpanRecorder, n int) []sdktrace.ReadOnlySpan {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		spans := rec.Ended()
		if len(spans) >= n || time.Now().After(deadline) {
			return spans
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func names(spans []sdktrace.ReadOnlySpan) []string {
	out := make([]string, len(spans))
	for i, s := range spans {
		out[i] = s.Name()
	}
	return out
}
