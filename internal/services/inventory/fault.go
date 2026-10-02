package inventory

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"
)

// MaxFaultLatency bounds the latency that can be injected.
const MaxFaultLatency = 10 * time.Second

// maxFaultBodyBytes bounds the size of a fault request body.
const maxFaultBodyBytes = 1 << 10

// Latency holds the response delay inventory-api applies to GET /inventory:
// a fixed baseline, optionally replaced by an injected fault latency.
// It is safe for concurrent use.
type Latency struct {
	base  time.Duration
	fault atomic.Int64 // injected latency in nanoseconds; 0 means no fault
}

// NewLatency returns a Latency with the given baseline and no active fault.
func NewLatency(base time.Duration) *Latency {
	return &Latency{base: base}
}

// Current returns the delay to apply to the next request.
func (l *Latency) Current() time.Duration {
	if f := l.fault.Load(); f > 0 {
		return time.Duration(f)
	}
	return l.base
}

// SetFault replaces the baseline with d until ClearFault is called.
func (l *Latency) SetFault(d time.Duration) error {
	if d <= 0 || d > MaxFaultLatency {
		return fmt.Errorf("fault latency must be in (0, %s], got %s", MaxFaultLatency, d)
	}
	l.fault.Store(int64(d))
	return nil
}

// ClearFault restores the baseline latency.
func (l *Latency) ClearFault() { l.fault.Store(0) }

// FaultState is the request and response body of the fault admin API.
// LatencyMS is 0 when no fault is active.
type FaultState struct {
	LatencyMS int64 `json:"latency_ms"`
}

func (l *Latency) state() FaultState {
	return FaultState{LatencyMS: time.Duration(l.fault.Load()).Milliseconds()}
}

// NewAdminHandler returns the fault-injection admin API for l:
//
//	GET    /fault  current fault state
//	PUT    /fault  set {"latency_ms": n}
//	DELETE /fault  clear the fault
//
// It must be served on a separate listener from the service API so it is
// neither instrumented nor reachable through the service port.
func NewAdminHandler(logger *slog.Logger, l *Latency) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /fault", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, l.state())
	})
	mux.HandleFunc("PUT /fault", func(w http.ResponseWriter, r *http.Request) {
		var req FaultState
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxFaultBodyBytes))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, adminError{Error: "invalid JSON body: " + err.Error()})
			return
		}
		if err := l.SetFault(time.Duration(req.LatencyMS) * time.Millisecond); err != nil {
			writeJSON(w, http.StatusBadRequest, adminError{Error: err.Error()})
			return
		}
		logger.Info("fault injection set", "latency_ms", req.LatencyMS)
		writeJSON(w, http.StatusOK, l.state())
	})
	mux.HandleFunc("DELETE /fault", func(w http.ResponseWriter, _ *http.Request) {
		l.ClearFault()
		logger.Info("fault injection cleared")
		writeJSON(w, http.StatusOK, l.state())
	})
	return mux
}

type adminError struct {
	Error string `json:"error"`
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
