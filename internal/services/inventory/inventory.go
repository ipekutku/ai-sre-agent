// Package inventory implements the inventory-api demo service.
package inventory

import (
	"encoding/json"
	"net/http"
)

const defaultSKU = "demo-sku"

// Item is the response body of GET /inventory.
type Item struct {
	SKU      string `json:"sku"`
	Quantity int    `json:"quantity"`
}

// NewHandler returns the inventory-api HTTP handler.
func NewHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /inventory", handleInventory)
	mux.HandleFunc("GET /healthz", handleHealthz)
	return mux
}

func handleInventory(w http.ResponseWriter, r *http.Request) {
	sku := r.URL.Query().Get("sku")
	if sku == "" {
		sku = defaultSKU
	}
	writeJSON(w, http.StatusOK, Item{SKU: sku, Quantity: 42})
}

func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
