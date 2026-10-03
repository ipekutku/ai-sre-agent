package scenarios

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func init() { pollInterval = 10 * time.Millisecond }

// fakeEnv serves the endpoints the runner uses. promValue is returned for
// every Prometheus query.
type fakeEnv struct {
	mu        sync.Mutex
	promValue string
	faults    []string // "PUT <body>" / "DELETE"
	checkouts atomic.Int64
}

func (f *fakeEnv) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/healthz":
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	case r.URL.Path == "/checkout":
		f.checkouts.Add(1)
	case r.URL.Path == "/fault":
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		if r.Method != http.MethodGet {
			f.faults = append(f.faults, strings.TrimSpace(r.Method+" "+string(body)))
		}
		f.mu.Unlock()
	case r.URL.Path == "/api/v1/query":
		f.mu.Lock()
		v := f.promValue
		f.mu.Unlock()
		if v == "" {
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1,"` + v + `"]}]}}`))
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeEnv) set(v string) { f.mu.Lock(); f.promValue = v; f.mu.Unlock() }

func newEnv(t *testing.T, f *fakeEnv) Environment {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return Environment{CheckoutURL: srv.URL, InventoryURL: srv.URL, InventoryAdminURL: srv.URL, PrometheusURL: srv.URL, HTTP: srv.Client()}
}

func TestFaultLifecycle(t *testing.T) {
	f := &fakeEnv{}
	env := newEnv(t, f)
	ctx := context.Background()

	if err := env.ApplyFault(ctx, Fault{Service: "inventory-api", Type: "latency", LatencyMS: 800}); err != nil {
		t.Fatal(err)
	}
	if err := env.ClearFault(ctx); err != nil {
		t.Fatal(err)
	}
	if err := env.ApplyFault(ctx, Fault{Service: "inventory-api", Type: "errors"}); err == nil {
		t.Error("unsupported fault should be rejected before calling the API")
	}
	want := []string{`PUT {"latency_ms": 800}`, "DELETE"}
	if len(f.faults) != 2 || f.faults[0] != want[0] || f.faults[1] != want[1] {
		t.Errorf("fault API calls = %q, want %q", f.faults, want)
	}
}

func TestWaitReady(t *testing.T) {
	f := &fakeEnv{}
	env := newEnv(t, f)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := env.WaitReady(ctx); err == nil || !strings.Contains(err.Error(), "prometheus targets") {
		t.Fatalf("err = %v, want prometheus targets not ready", err)
	}

	f.set("2")
	if err := env.WaitReady(context.Background()); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
}

func TestWaitForTrigger(t *testing.T) {
	f := &fakeEnv{}
	f.set("0.02")
	env := newEnv(t, f)
	trigger := Trigger{Query: "x", Above: 0.5, TimeoutSeconds: 5}

	go func() {
		time.Sleep(50 * time.Millisecond)
		f.set("0.98")
	}()
	v, err := env.WaitForTrigger(context.Background(), trigger)
	if err != nil || v != 0.98 {
		t.Fatalf("WaitForTrigger = %v, %v; want 0.98", v, err)
	}

	f.set("0.02")
	trigger.TimeoutSeconds = 1
	if _, err := env.WaitForTrigger(context.Background(), trigger); err == nil || !strings.Contains(err.Error(), "not met within 1s") {
		t.Errorf("err = %v, want timeout", err)
	}
}

func TestGenerateTraffic(t *testing.T) {
	f := &fakeEnv{}
	env := newEnv(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	stats := env.GenerateTraffic(ctx, 40)
	// ~20 requests expected at 40 rps for 0.5s; allow scheduling slack.
	if stats.OK < 10 || stats.OK > 25 || stats.Failed != 0 {
		t.Errorf("stats = %+v, want ~20 ok", stats)
	}
	if f.checkouts.Load() != stats.OK {
		t.Errorf("server saw %d requests, stats say %d", f.checkouts.Load(), stats.OK)
	}
}

func TestQueryValueErrors(t *testing.T) {
	f := &fakeEnv{}
	env := newEnv(t, f)
	if _, err := env.QueryValue(context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "no data") {
		t.Errorf("err = %v, want no data", err)
	}
	env.PrometheusURL += "/nope"
	if _, err := env.QueryValue(context.Background(), "x"); err == nil {
		t.Error("expected error for non-Prometheus response")
	}
}
