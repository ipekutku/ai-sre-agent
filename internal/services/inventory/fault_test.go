package inventory

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLatencyFaultLifecycle(t *testing.T) {
	l := NewLatency(20 * time.Millisecond)
	if got := l.Current(); got != 20*time.Millisecond {
		t.Fatalf("baseline = %v, want 20ms", got)
	}
	if err := l.SetFault(800 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if got := l.Current(); got != 800*time.Millisecond {
		t.Fatalf("with fault = %v, want 800ms", got)
	}
	l.ClearFault()
	if got := l.Current(); got != 20*time.Millisecond {
		t.Fatalf("after clear = %v, want baseline 20ms", got)
	}
}

func TestLatencySetFaultValidation(t *testing.T) {
	l := NewLatency(0)
	for _, d := range []time.Duration{0, -time.Millisecond, MaxFaultLatency + 1} {
		if err := l.SetFault(d); err == nil {
			t.Errorf("SetFault(%v) succeeded, want error", d)
		}
	}
	if got := l.Current(); got != 0 {
		t.Errorf("rejected faults changed latency to %v", got)
	}
}

func TestLatencyConcurrentAccess(t *testing.T) {
	l := NewLatency(time.Millisecond)
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(2)
		go func() { defer wg.Done(); _ = l.SetFault(time.Duration(i+1) * time.Millisecond) }()
		go func() { defer wg.Done(); _ = l.Current() }()
	}
	wg.Wait()
}

func TestInventoryAppliesLatency(t *testing.T) {
	l := NewLatency(0)
	if err := l.SetFault(80 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	start := time.Now()
	NewHandler(l).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/inventory", nil))
	elapsed := time.Since(start)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if elapsed < 80*time.Millisecond {
		t.Errorf("request took %v, want at least the injected 80ms", elapsed)
	}
}

func TestInventoryCallerCancels(t *testing.T) {
	l := NewLatency(0)
	if err := l.SetFault(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/inventory", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	start := time.Now()
	NewHandler(l).ServeHTTP(rec, req)

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
	l := NewLatency(20 * time.Millisecond)
	h := NewAdminHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), l)

	if rec, st := adminDo(t, h, http.MethodGet, ""); rec.Code != http.StatusOK || st.LatencyMS != 0 {
		t.Fatalf("initial GET: status %d, state %+v; want 200 and no fault", rec.Code, st)
	}

	rec, st := adminDo(t, h, http.MethodPut, `{"latency_ms": 800}`)
	if rec.Code != http.StatusOK || st.LatencyMS != 800 {
		t.Fatalf("PUT: status %d, state %+v; want 200 and 800ms", rec.Code, st)
	}
	if got := l.Current(); got != 800*time.Millisecond {
		t.Errorf("latency after PUT = %v, want 800ms", got)
	}

	if rec, st := adminDo(t, h, http.MethodDelete, ""); rec.Code != http.StatusOK || st.LatencyMS != 0 {
		t.Fatalf("DELETE: status %d, state %+v; want 200 and no fault", rec.Code, st)
	}
	if got := l.Current(); got != 20*time.Millisecond {
		t.Errorf("latency after DELETE = %v, want baseline 20ms", got)
	}
}

func TestAdminAPIRejectsInvalidRequests(t *testing.T) {
	l := NewLatency(20 * time.Millisecond)
	h := NewAdminHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), l)

	for _, body := range []string{
		``,
		`not json`,
		`{"latency_ms": 0}`,
		`{"latency_ms": -5}`,
		`{"latency_ms": 60000}`,
		`{"latency_ms": 800, "type": "error"}`, // unknown field
		`{"latency_ms": 800, "padding": "` + strings.Repeat("x", 2048) + `"}`,
	} {
		rec, _ := adminDo(t, h, http.MethodPut, body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("PUT %.40q: status %d, want 400", body, rec.Code)
		}
	}
	if got := l.Current(); got != 20*time.Millisecond {
		t.Errorf("rejected requests changed latency to %v", got)
	}
}
