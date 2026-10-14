package inventory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// MaxFaultLatency bounds the latency that can be injected.
const MaxFaultLatency = 10 * time.Second

// maxFaultBodyBytes bounds the size of a fault request body.
const maxFaultBodyBytes = 1 << 10

// errorWindow is the number of consecutive requests over which an error
// percentage is applied exactly.
const errorWindow = 100

// Faults holds how inventory-api answers GET /inventory: a fixed baseline
// delay, optionally replaced by an injected latency, and an optional
// percentage of requests that fail.
//
// Failures follow a counter-based schedule instead of a random one: with
// error percent p, exactly p of every 100 consecutive requests fail, spread
// evenly, so a given configuration always produces the same error rate.
// It is safe for concurrent use.
type Faults struct {
	base time.Duration

	mu           sync.Mutex
	latency      time.Duration // injected latency; 0 means the baseline applies
	errorPercent int
	requests     uint64 // requests seen since the fault was set
}

// NewFaults returns Faults with the given baseline delay and no active fault.
func NewFaults(base time.Duration) *Faults {
	return &Faults{base: base}
}

// Next returns the delay to apply to the next request and whether that
// request should fail. Each call advances the error schedule.
func (f *Faults) Next() (delay time.Duration, fail bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delay = f.base
	if f.latency > 0 {
		delay = f.latency
	}
	if f.errorPercent > 0 {
		// Request i of each window fails when floor(i*p/100) steps up, which
		// spreads exactly p failures evenly over any 100 consecutive requests.
		i, p := f.requests%errorWindow, uint64(f.errorPercent)
		fail = (i+1)*p/errorWindow > i*p/errorWindow
		f.requests++
	}
	return delay, fail
}

// Set replaces the active fault with st and restarts the error schedule.
// Either field may be zero, but not both.
func (f *Faults) Set(st FaultState) error {
	var errs []error
	// Compared in milliseconds so a huge value cannot overflow the conversion.
	if st.LatencyMS < 0 || st.LatencyMS > MaxFaultLatency.Milliseconds() {
		errs = append(errs, fmt.Errorf("latency_ms must be in [0, %d], got %d", MaxFaultLatency.Milliseconds(), st.LatencyMS))
	}
	if st.ErrorPercent < 0 || st.ErrorPercent > 100 {
		errs = append(errs, fmt.Errorf("error_percent must be in [0, 100], got %d", st.ErrorPercent))
	}
	if st.LatencyMS == 0 && st.ErrorPercent == 0 {
		errs = append(errs, errors.New("set latency_ms, error_percent, or both; use DELETE to clear the fault"))
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.latency, f.errorPercent, f.requests = time.Duration(st.LatencyMS)*time.Millisecond, st.ErrorPercent, 0
	return nil
}

// Clear restores the baseline behaviour.
func (f *Faults) Clear() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.latency, f.errorPercent, f.requests = 0, 0, 0
}

// FaultState is the request and response body of the fault admin API.
// A zero field means that part of the fault is inactive.
type FaultState struct {
	LatencyMS    int64 `json:"latency_ms"`
	ErrorPercent int   `json:"error_percent"`
}

// State returns the active fault.
func (f *Faults) State() FaultState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return FaultState{LatencyMS: f.latency.Milliseconds(), ErrorPercent: f.errorPercent}
}

// NewAdminHandler returns the fault-injection admin API for f:
//
//	GET    /fault  current fault state
//	PUT    /fault  replace it with {"latency_ms": n, "error_percent": p} (either or both)
//	DELETE /fault  clear the fault
//
// It must be served on a separate listener from the service API so it is
// neither instrumented nor reachable through the service port.
func NewAdminHandler(logger *slog.Logger, f *Faults) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /fault", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, f.State())
	})
	mux.HandleFunc("PUT /fault", func(w http.ResponseWriter, r *http.Request) {
		var req FaultState
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxFaultBodyBytes))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid JSON body: " + err.Error()})
			return
		}
		if err := f.Set(req); err != nil {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
			return
		}
		logger.Info("fault injection set", "latency_ms", req.LatencyMS, "error_percent", req.ErrorPercent)
		writeJSON(w, http.StatusOK, f.State())
	})
	mux.HandleFunc("DELETE /fault", func(w http.ResponseWriter, _ *http.Request) {
		f.Clear()
		logger.Info("fault injection cleared")
		writeJSON(w, http.StatusOK, f.State())
	})
	return mux
}

// sleep waits for d and reports whether it completed before ctx was done.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
