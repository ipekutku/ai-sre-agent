package httpmetrics

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func testApp() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ok", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /fail", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})
	mux.HandleFunc("GET /items/{id}", func(w http.ResponseWriter, _ *http.Request) {})
	return mux
}

func serve(h http.Handler, method, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return rec
}

func TestServerMetricsLabels(t *testing.T) {
	reg := NewRegistry()
	h := Handler(reg, testApp())

	serve(h, http.MethodGet, "/ok")
	serve(h, http.MethodGet, "/ok")
	serve(h, http.MethodGet, "/fail")
	serve(h, http.MethodGet, "/items/1")
	serve(h, http.MethodGet, "/items/2")
	serve(h, http.MethodGet, "/no/such/path")
	serve(h, http.MethodGet, "/another/unknown")

	// Durations are timing-dependent, so assert sample counts per label set.
	tests := []struct {
		route, status string
		want          int
	}{
		{"/ok", "200", 2},
		{"/fail", "502", 1},
		{"/items/{id}", "200", 2}, // route uses the pattern, not the raw path
		{unmatchedRoute, "404", 2},
	}
	for _, tt := range tests {
		got := histogramCount(t, reg, "http_server_request_duration_seconds",
			map[string]string{"route": tt.route, "method": "GET", "status": tt.status})
		if got != tt.want {
			t.Errorf("count{route=%q,status=%q} = %d, want %d", tt.route, tt.status, got, tt.want)
		}
	}
}

func TestMetricsEndpoint(t *testing.T) {
	reg := NewRegistry()
	h := Handler(reg, testApp())
	serve(h, http.MethodGet, "/ok")

	rec := serve(h, http.MethodGet, "/metrics")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, name := range []string{
		"http_server_request_duration_seconds_bucket",
		"process_cpu_seconds_total",
		"go_goroutines",
	} {
		if !strings.Contains(body, name) {
			t.Errorf("/metrics output missing %s", name)
		}
	}
	if strings.Contains(body, `route="/metrics"`) {
		t.Error("scrapes of /metrics should not be recorded as server requests")
	}
}

func TestClientTransport(t *testing.T) {
	srv := httptest.NewServer(testApp())
	defer srv.Close()

	reg := prometheus.NewRegistry()
	c := NewClient(reg)
	hc := &http.Client{Transport: c.Transport("dep", srv.Client().Transport)}

	for _, path := range []string{"/ok", "/fail"} {
		resp, err := hc.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	failing := &http.Client{Transport: c.Transport("dep", roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("connection refused")
	}))}
	if _, err := failing.Get("http://dep.invalid/ok"); err == nil {
		t.Fatal("expected transport error")
	}

	for status, want := range map[string]int{"200": 1, "502": 1, "error": 1} {
		got := histogramCount(t, reg, "http_client_request_duration_seconds",
			map[string]string{"peer": "dep", "method": "GET", "status": status})
		if got != want {
			t.Errorf("count{status=%q} = %d, want %d", status, got, want)
		}
	}
}

// histogramCount returns the sample count of the histogram series with exactly
// the given labels, or 0 if it does not exist.
func histogramCount(t *testing.T, g prometheus.Gatherer, name string, labels map[string]string) int {
	t.Helper()
	mfs, err := g.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
	metrics:
		for _, m := range mf.GetMetric() {
			if len(m.GetLabel()) != len(labels) {
				continue
			}
			for _, lp := range m.GetLabel() {
				if labels[lp.GetName()] != lp.GetValue() {
					continue metrics
				}
			}
			return int(m.GetHistogram().GetSampleCount())
		}
	}
	return 0
}
