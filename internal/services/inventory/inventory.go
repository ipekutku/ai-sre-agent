// Package inventory implements the inventory-api demo service.
package inventory

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/ipekutku/ai-sre-agent/internal/httpserver"
)

const defaultSKU = "demo-sku"

// statusClientClosedRequest is the non-standard status (popularised by nginx)
// recorded when the caller disconnects before the response is ready.
const statusClientClosedRequest = 499

// errInternal is what a failed request logs and returns. Like any internal
// server error, it does not say why the request failed.
var errInternal = errors.New("internal error")

// Item is the response body of GET /inventory.
type Item struct {
	SKU      string `json:"sku"`
	Quantity int    `json:"quantity"`
}

type errorResponse struct {
	Error string `json:"error"`
}

// NewHandler returns the inventory-api HTTP handler. Each GET /inventory
// waits for the delay faults.Next returns, then responds 500 if the request
// is scheduled to fail, or 200 otherwise.
func NewHandler(logger *slog.Logger, faults *Faults) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /inventory", func(w http.ResponseWriter, r *http.Request) {
		delay, fail := faults.Next()
		if !sleep(r.Context(), delay) {
			// The caller gave up (e.g. its timeout fired). Record that
			// instead of letting metrics count it as a success.
			w.WriteHeader(statusClientClosedRequest)
			return
		}
		sku := r.URL.Query().Get("sku")
		if sku == "" {
			sku = defaultSKU
		}
		if fail {
			logger.ErrorContext(r.Context(), "inventory request failed", "sku", sku, "error", errInternal)
			writeJSON(w, http.StatusInternalServerError, errorResponse{Error: errInternal.Error()})
			return
		}
		writeJSON(w, http.StatusOK, Item{SKU: sku, Quantity: 42})
	})
	mux.Handle("GET /healthz", httpserver.HealthHandler())
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
