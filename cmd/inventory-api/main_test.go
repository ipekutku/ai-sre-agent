package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

func TestLoadConfig(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		want    config
		wantErr bool
	}{
		{
			name: "defaults",
			env:  map[string]string{},
			want: config{addr: ":8081", adminAddr: "127.0.0.1:9081", baseLatency: 20 * time.Millisecond},
		},
		{
			name: "overrides",
			env:  map[string]string{"ADDR": ":9000", "ADMIN_ADDR": ":9001", "BASE_LATENCY": "0s"},
			want: config{addr: ":9000", adminAddr: ":9001", baseLatency: 0},
		},
		{name: "bad latency", env: map[string]string{"BASE_LATENCY": "fast"}, wantErr: true},
		{name: "negative latency", env: map[string]string{"BASE_LATENCY": "-1ms"}, wantErr: true},
		{name: "latency above max", env: map[string]string{"BASE_LATENCY": "1m"}, wantErr: true},
		{name: "same addresses", env: map[string]string{"ADDR": ":9000", "ADMIN_ADDR": ":9000"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := loadConfig(func(k string) string { return tt.env[k] })
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got config %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("loadConfig: %v", err)
			}
			if got != tt.want {
				t.Errorf("config = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// The fault admin API must never produce spans, and service spans must not
// reveal fault state, even while a fault is active.
func TestAdminAPIIsNotTraced(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	service, admin := handlers(logger, logger, config{baseLatency: 0}, tp)

	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodPut, "/fault", strings.NewReader(`{"latency_ms": 5, "error_percent": 100}`)),
		httptest.NewRequest(http.MethodGet, "/fault", nil),
	} {
		w := httptest.NewRecorder()
		admin.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s /fault: status %d", req.Method, w.Code)
		}
	}
	if n := len(rec.Ended()); n != 0 {
		t.Fatalf("admin API produced %d spans, want 0", n)
	}

	w := httptest.NewRecorder()
	service.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/inventory", nil))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("service status = %d, want 500 from the error fault", w.Code)
	}
	spans := rec.Ended()
	if len(spans) != 1 || spans[0].Name() != "GET /inventory" {
		t.Fatalf("service spans = %d, want one GET /inventory span", len(spans))
	}
	text := spans[0].Status().Description
	for _, a := range spans[0].Attributes() {
		text += " " + string(a.Key) + "=" + a.Value.Emit()
	}
	for _, e := range spans[0].Events() {
		text += " " + e.Name
	}
	for _, leak := range []string{"fault", "inject"} {
		if strings.Contains(strings.ToLower(text), leak) {
			t.Errorf("span mentions %q: %s", leak, text)
		}
	}
}

// Fault-admin logs are ground truth: they are written locally for auditing
// but must never be exported to the log backend the agent can query.
func TestAdminLogsAreNotExported(t *testing.T) {
	exp := &logRecorder{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(exp)))
	var local bytes.Buffer
	serviceLogger, adminLogger := loggers(&local, lp)
	service, admin := handlers(serviceLogger, adminLogger, config{baseLatency: 0}, tracenoop.NewTracerProvider())

	adminDo := func(method, body string) {
		t.Helper()
		w := httptest.NewRecorder()
		admin.ServeHTTP(w, httptest.NewRequest(method, "/fault", strings.NewReader(body)))
		if w.Code != http.StatusOK {
			t.Fatalf("%s /fault: status %d", method, w.Code)
		}
	}
	// One request fails under the error fault, one succeeds after clearing it.
	adminDo(http.MethodPut, `{"latency_ms": 5, "error_percent": 100}`)
	service.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/inventory", nil))
	adminDo(http.MethodDelete, "")
	service.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/inventory", nil))

	if !strings.Contains(local.String(), `"component":"fault-admin"`) {
		t.Errorf("fault changes were not logged locally:\n%s", local.String())
	}
	records := exp.records
	if len(records) == 0 {
		t.Fatal("service requests exported no records; the exporter is not wired")
	}
	var sawFailure bool
	for _, r := range records {
		body := r.Body().AsString()
		sawFailure = sawFailure || body == "inventory request failed"
		text := strings.ToLower(body)
		r.WalkAttributes(func(kv attribute.KeyValue) bool {
			text += " " + strings.ToLower(string(kv.Key)+"="+kv.Value.Emit())
			return true
		})
		for _, leak := range []string{"fault", "inject"} {
			if strings.Contains(text, leak) {
				t.Errorf("exported record mentions %q: %s", leak, text)
			}
		}
	}
	if !sawFailure {
		t.Error("the failed request's error log was not exported")
	}
}

// logRecorder is an in-memory log exporter, used with a SimpleProcessor.
type logRecorder struct {
	records []sdklog.Record
}

func (e *logRecorder) Export(_ context.Context, records []sdklog.Record) error {
	for _, r := range records {
		e.records = append(e.records, r.Clone())
	}
	return nil
}

func (e *logRecorder) Shutdown(context.Context) error   { return nil }
func (e *logRecorder) ForceFlush(context.Context) error { return nil }
