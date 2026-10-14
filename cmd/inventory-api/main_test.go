package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
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
	service, admin := handlers(logger, config{baseLatency: 0}, tp)

	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodPut, "/fault", strings.NewReader(`{"latency_ms": 5}`)),
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
	spans := rec.Ended()
	if len(spans) != 1 || spans[0].Name() != "GET /inventory" {
		t.Fatalf("service spans = %d, want one GET /inventory span", len(spans))
	}
	for _, a := range spans[0].Attributes() {
		if strings.Contains(strings.ToLower(string(a.Key)+a.Value.Emit()), "fault") {
			t.Errorf("span attribute %s=%s mentions the fault", a.Key, a.Value.Emit())
		}
	}
}
