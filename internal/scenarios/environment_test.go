package scenarios

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
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
	tempoUp   atomic.Bool
	// traceStart is the start time (unix ns) of the one trace Tempo search
	// returns; 0 means no traces.
	traceStart atomic.Int64
	search     url.Values
	lokiUp     atomic.Bool
	// logEntries is whether Loki returns an entry; logQuery is the last query.
	logEntries atomic.Bool
	logQuery   url.Values
}

func (f *fakeEnv) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/healthz":
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	case r.URL.Path == "/checkout":
		f.checkouts.Add(1)
	case r.URL.Path == "/api/search":
		f.mu.Lock()
		f.search = r.URL.Query()
		f.mu.Unlock()
		if ns := f.traceStart.Load(); ns != 0 {
			fmt.Fprintf(w, `{"traces":[{"traceID":"abc","startTimeUnixNano":"%d"}]}`, ns)
			return
		}
		_, _ = w.Write([]byte(`{"traces":[]}`))
	// Loki is served under /loki (see newEnv) so its /ready differs from Tempo's.
	case r.URL.Path == "/loki/ready":
		if !f.lokiUp.Load() {
			http.Error(w, "Ingester not ready", http.StatusServiceUnavailable)
		}
	case r.URL.Path == "/loki/loki/api/v1/query_range":
		f.mu.Lock()
		f.logQuery = r.URL.Query()
		f.mu.Unlock()
		if f.logEntries.Load() {
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"streams","result":[{"stream":{},"values":[["1","request"]]}]}}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"streams","result":[]}}`))
	case r.URL.Path == "/ready":
		if !f.tempoUp.Load() {
			http.Error(w, "Ingester not ready", http.StatusServiceUnavailable)
		}
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
	return Environment{CheckoutURL: srv.URL, InventoryURL: srv.URL, InventoryAdminURL: srv.URL, PrometheusURL: srv.URL, TempoURL: srv.URL, LokiURL: srv.URL + "/loki", HTTP: srv.Client()}
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
	if err := env.ApplyFault(ctx, Fault{Service: "inventory-api", Type: "errors", ErrorPercent: 50}); err != nil {
		t.Fatal(err)
	}
	if err := env.ApplyFault(ctx, Fault{Service: "inventory-api", Type: "errors"}); err == nil {
		t.Error("invalid fault should be rejected before calling the API")
	}
	want := []string{`PUT {"latency_ms":800}`, "DELETE", `PUT {"error_percent":50}`}
	if !slices.Equal(f.faults, want) {
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
	ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := env.WaitReady(ctx); err == nil || !strings.Contains(err.Error(), "tempo") {
		t.Fatalf("err = %v, want tempo not ready", err)
	}

	f.tempoUp.Store(true)
	ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := env.WaitReady(ctx); err == nil || !strings.Contains(err.Error(), "loki") {
		t.Fatalf("err = %v, want loki not ready", err)
	}

	f.lokiUp.Store(true)
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

	trigger.TimeoutSeconds = 1
	for _, v := range []string{"0.02", "NaN"} {
		f.set(v)
		if _, err := env.WaitForTrigger(context.Background(), trigger); err == nil || !strings.Contains(err.Error(), "not met within 1s") {
			t.Errorf("value %s: err = %v, want timeout", v, err)
		}
	}
}

func TestWaitForTraces(t *testing.T) {
	f := &fakeEnv{}
	env := newEnv(t, f)
	since := time.Now()

	// No traces, then only a trace from before since: not caught up.
	for _, start := range []int64{0, since.Add(-time.Second).UnixNano()} {
		f.traceStart.Store(start)
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		err := env.WaitForTraces(ctx, "checkout-api", since)
		cancel()
		if err == nil || !strings.Contains(err.Error(), "no checkout-api trace") {
			t.Errorf("trace start %d: err = %v, want not searchable yet", start, err)
		}
	}

	go func() {
		time.Sleep(50 * time.Millisecond)
		f.traceStart.Store(since.Add(time.Second).UnixNano())
	}()
	if err := env.WaitForTraces(context.Background(), "checkout-api", since); err != nil {
		t.Fatalf("WaitForTraces: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if q := f.search.Get("q"); q != `{ resource.service.name = "checkout-api" }` {
		t.Errorf("q = %s", q)
	}
	if f.search.Get("start") != strconv.FormatInt(since.Unix(), 10) {
		t.Errorf("start = %s, want %d", f.search.Get("start"), since.Unix())
	}
}

func TestWaitForLogs(t *testing.T) {
	f := &fakeEnv{}
	env := newEnv(t, f)
	since := time.Now()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	err := env.WaitForLogs(ctx, "checkout-api", since)
	cancel()
	if err == nil || !strings.Contains(err.Error(), "no checkout-api log entry") {
		t.Errorf("err = %v, want not searchable yet", err)
	}

	go func() {
		time.Sleep(50 * time.Millisecond)
		f.logEntries.Store(true)
	}()
	if err := env.WaitForLogs(context.Background(), "checkout-api", since); err != nil {
		t.Fatalf("WaitForLogs: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if q := f.logQuery.Get("query"); q != `{service_name="checkout-api"}` {
		t.Errorf("query = %s", q)
	}
	// Only entries logged since the trigger count.
	if f.logQuery.Get("start") != strconv.FormatInt(since.UnixNano(), 10) {
		t.Errorf("start = %s, want %d", f.logQuery.Get("start"), since.UnixNano())
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
