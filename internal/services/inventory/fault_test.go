package inventory

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestFaultLifecycle(t *testing.T) {
	f := NewFaults(20 * time.Millisecond)
	if d, fail := f.Next(); d != 20*time.Millisecond || fail {
		t.Fatalf("baseline = (%v, %v), want (20ms, false)", d, fail)
	}
	if err := f.Set(FaultState{LatencyMS: 800, ErrorPercent: 100}); err != nil {
		t.Fatal(err)
	}
	if d, fail := f.Next(); d != 800*time.Millisecond || !fail {
		t.Fatalf("with fault = (%v, %v), want (800ms, true)", d, fail)
	}
	if st := f.State(); st != (FaultState{LatencyMS: 800, ErrorPercent: 100}) {
		t.Errorf("state = %+v", st)
	}
	f.Clear()
	if d, fail := f.Next(); d != 20*time.Millisecond || fail {
		t.Fatalf("after clear = (%v, %v), want baseline (20ms, false)", d, fail)
	}
	if st := f.State(); st != (FaultState{}) {
		t.Errorf("state after clear = %+v, want zero", st)
	}
}

// Set replaces the whole fault, so an errors-only fault drops an earlier
// latency fault.
func TestFaultSetReplaces(t *testing.T) {
	f := NewFaults(20 * time.Millisecond)
	if err := f.Set(FaultState{LatencyMS: 800}); err != nil {
		t.Fatal(err)
	}
	if err := f.Set(FaultState{ErrorPercent: 100}); err != nil {
		t.Fatal(err)
	}
	if d, fail := f.Next(); d != 20*time.Millisecond || !fail {
		t.Errorf("Next = (%v, %v), want (20ms baseline, true)", d, fail)
	}
}

func TestFaultSetValidation(t *testing.T) {
	f := NewFaults(0)
	for _, st := range []FaultState{
		{},
		{LatencyMS: -1},
		{LatencyMS: MaxFaultLatency.Milliseconds() + 1},
		{LatencyMS: 1 << 62}, // would overflow time.Duration
		{ErrorPercent: -1},
		{ErrorPercent: 101},
		{LatencyMS: 800, ErrorPercent: 101},
	} {
		if err := f.Set(st); err == nil {
			t.Errorf("Set(%+v) succeeded, want error", st)
		}
	}
	if st := f.State(); st != (FaultState{}) {
		t.Errorf("rejected faults changed state to %+v", st)
	}
}

// The error schedule is exact: p failures in every window of 100 requests,
// spread evenly rather than bunched at the start of the window.
func TestErrorScheduleIsExact(t *testing.T) {
	for _, p := range []int{1, 7, 25, 50, 99, 100} {
		f := NewFaults(0)
		if err := f.Set(FaultState{ErrorPercent: p}); err != nil {
			t.Fatal(err)
		}
		fails := make([]bool, 300)
		for i := range fails {
			_, fails[i] = f.Next()
		}
		// Every 100 consecutive requests, not just aligned windows.
		for start := 0; start+100 <= len(fails); start++ {
			n := 0
			for _, failed := range fails[start : start+100] {
				if failed {
					n++
				}
			}
			if n != p {
				t.Fatalf("p=%d: requests %d-%d had %d failures, want %d", p, start, start+99, n, p)
			}
		}
		// Evenly spread: no run of consecutive failures longer than needed.
		maxRun, run := 0, 0
		for _, failed := range fails {
			if failed {
				run++
				maxRun = max(maxRun, run)
			} else {
				run = 0
			}
		}
		if p <= 50 && maxRun > 1 {
			t.Errorf("p=%d: %d consecutive failures, want them spread out", p, maxRun)
		}
	}
}

// Setting a fault restarts the schedule, so the same setting always produces
// the same sequence of failures.
func TestErrorScheduleRestartsOnSet(t *testing.T) {
	f := NewFaults(0)
	sequence := func() []bool {
		if err := f.Set(FaultState{ErrorPercent: 30}); err != nil {
			t.Fatal(err)
		}
		out := make([]bool, 10)
		for i := range out {
			_, out[i] = f.Next()
		}
		return out
	}
	first := sequence()
	_, _ = f.Next() // move the schedule on
	if second := sequence(); !slices.Equal(first, second) {
		t.Errorf("sequence after re-Set = %v, want %v", second, first)
	}
}

// Concurrent requests still get exactly p failures per 100 (run with -race).
func TestErrorScheduleConcurrent(t *testing.T) {
	f := NewFaults(0)
	if err := f.Set(FaultState{ErrorPercent: 40}); err != nil {
		t.Fatal(err)
	}
	var failed atomic.Int64
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				if _, fail := f.Next(); fail {
					failed.Add(1)
				}
				_ = f.State()
			}
		}()
	}
	wg.Wait()
	if got := failed.Load(); got != 400 {
		t.Errorf("failures over 1000 concurrent requests = %d, want 400", got)
	}
}

func TestInventoryAppliesLatency(t *testing.T) {
	f := NewFaults(0)
	if err := f.Set(FaultState{LatencyMS: 80}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	start := time.Now()
	NewHandler(discardLogger(), f).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/inventory", nil))
	elapsed := time.Since(start)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if elapsed < 80*time.Millisecond {
		t.Errorf("request took %v, want at least the injected 80ms", elapsed)
	}
}

// A failed request returns 500 after the configured delay and is logged as an
// ordinary server error: nothing in the response or log says it was injected.
func TestInventoryFailsScheduledRequests(t *testing.T) {
	f := NewFaults(0)
	if err := f.Set(FaultState{LatencyMS: 50, ErrorPercent: 100}); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	rec := httptest.NewRecorder()
	start := time.Now()
	NewHandler(logger, f).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/inventory?sku=abc-1", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Errorf("failed request took %v, want at least the 50ms delay", elapsed)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var line struct {
		Level, Msg, SKU, Error string
	}
	if err := json.Unmarshal(logs.Bytes(), &line); err != nil {
		t.Fatalf("decode log line %q: %v", logs.String(), err)
	}
	if line.Level != "ERROR" || line.Msg != "inventory request failed" || line.SKU != "abc-1" || line.Error == "" {
		t.Errorf("log line = %s", logs.String())
	}
	for _, text := range []string{rec.Body.String(), logs.String()} {
		lower := strings.ToLower(text)
		for _, leak := range []string{"fault", "inject", "error_percent", "schedule"} {
			if strings.Contains(lower, leak) {
				t.Errorf("%q mentions %q", text, leak)
			}
		}
	}
}

func TestInventoryErrorPercent(t *testing.T) {
	f := NewFaults(0)
	if err := f.Set(FaultState{ErrorPercent: 25}); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(discardLogger(), f)
	counts := map[int]int{}
	for range 200 {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/inventory", nil))
		counts[rec.Code]++
	}
	if counts[http.StatusInternalServerError] != 50 || counts[http.StatusOK] != 150 {
		t.Errorf("status counts = %v, want 50x500 and 150x200", counts)
	}
}

func TestInventoryCallerCancels(t *testing.T) {
	f := NewFaults(0)
	if err := f.Set(FaultState{LatencyMS: 5000}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/inventory", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	start := time.Now()
	NewHandler(discardLogger(), f).ServeHTTP(rec, req)

	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("handler took %v after caller cancelled, want it to stop early", elapsed)
	}
	if rec.Code != statusClientClosedRequest {
		t.Errorf("status = %d, want %d", rec.Code, statusClientClosedRequest)
	}
}

func adminDo(t *testing.T, h http.Handler, method, body string) (*httptest.ResponseRecorder, FaultState) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, "/fault", strings.NewReader(body)))
	var st FaultState
	if rec.Code == http.StatusOK {
		if err := json.NewDecoder(rec.Body).Decode(&st); err != nil {
			t.Fatalf("decode %s /fault: %v", method, err)
		}
	}
	return rec, st
}

func TestAdminAPI(t *testing.T) {
	f := NewFaults(20 * time.Millisecond)
	h := NewAdminHandler(discardLogger(), f)

	if rec, st := adminDo(t, h, http.MethodGet, ""); rec.Code != http.StatusOK || st != (FaultState{}) {
		t.Fatalf("initial GET: status %d, state %+v; want 200 and no fault", rec.Code, st)
	}

	for _, tt := range []struct {
		body string
		want FaultState
	}{
		{`{"latency_ms": 800}`, FaultState{LatencyMS: 800}},
		{`{"error_percent": 50}`, FaultState{ErrorPercent: 50}},
		{`{"latency_ms": 300, "error_percent": 10}`, FaultState{LatencyMS: 300, ErrorPercent: 10}},
	} {
		rec, st := adminDo(t, h, http.MethodPut, tt.body)
		if rec.Code != http.StatusOK || st != tt.want {
			t.Fatalf("PUT %s: status %d, state %+v; want 200 and %+v", tt.body, rec.Code, st, tt.want)
		}
		if _, st := adminDo(t, h, http.MethodGet, ""); st != tt.want {
			t.Errorf("GET after PUT %s = %+v, want %+v", tt.body, st, tt.want)
		}
	}

	if rec, st := adminDo(t, h, http.MethodDelete, ""); rec.Code != http.StatusOK || st != (FaultState{}) {
		t.Fatalf("DELETE: status %d, state %+v; want 200 and no fault", rec.Code, st)
	}
	if d, fail := f.Next(); d != 20*time.Millisecond || fail {
		t.Errorf("after DELETE = (%v, %v), want baseline (20ms, false)", d, fail)
	}
}

func TestAdminAPIRejectsInvalidRequests(t *testing.T) {
	f := NewFaults(20 * time.Millisecond)
	h := NewAdminHandler(discardLogger(), f)
	if err := f.Set(FaultState{LatencyMS: 300}); err != nil {
		t.Fatal(err)
	}

	for _, body := range []string{
		``,
		`not json`,
		`{}`,
		`{"latency_ms": 0}`,
		`{"latency_ms": -5}`,
		`{"latency_ms": 60000}`,
		`{"error_percent": 0}`,
		`{"error_percent": -1}`,
		`{"error_percent": 101}`,
		`{"error_percent": 12.5}`,
		`{"latency_ms": 800, "error_percent": 150}`,
		`{"latency_ms": 800, "type": "error"}`, // unknown field
		`{"latency_ms": 800, "padding": "` + strings.Repeat("x", 2048) + `"}`,
	} {
		rec, _ := adminDo(t, h, http.MethodPut, body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("PUT %.40q: status %d, want 400", body, rec.Code)
		}
	}
	if st := f.State(); st != (FaultState{LatencyMS: 300}) {
		t.Errorf("rejected requests changed the fault to %+v", st)
	}
}
