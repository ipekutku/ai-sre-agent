// Package inventory implements the inventory-api demo service.
package inventory

import (
	"encoding/json"
	"net/http"

	"github.com/ipekutku/ai-sre-agent/internal/httpserver"
)

const defaultSKU = "demo-sku"

// statusClientClosedRequest is the non-standard status (popularised by nginx)
// recorded when the caller disconnects before the response is ready.
const statusClientClosedRequest = 499

// Item is the response body of GET /inventory.
type Item struct {
	SKU      string `json:"sku"`
	Quantity int    `json:"quantity"`
}

// NewHandler returns the inventory-api HTTP handler. Each GET /inventory
// waits for latency.Current() before responding.
func NewHandler(latency *Latency) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /inventory", func(w http.ResponseWriter, r *http.Request) {
		if !sleep(r.Context(), latency.Current()) {
			// The caller gave up (e.g. its timeout fired). Record that
			// instead of letting metrics count it as a success.
			w.WriteHeader(statusClientClosedRequest)
			return
		}
		sku := r.URL.Query().Get("sku")
		if sku == "" {
			sku = defaultSKU
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
