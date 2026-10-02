// Package checkout implements the checkout-api demo service.
package checkout

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/ipekutku/ai-sre-agent/internal/httpserver"
	"github.com/ipekutku/ai-sre-agent/internal/services/inventory"
)

const defaultSKU = "demo-sku"

// Inventory is the dependency checkout-api uses to look up stock.
type Inventory interface {
	GetItem(ctx context.Context, sku string) (inventory.Item, error)
}

// Response is the response body of a successful GET /checkout.
type Response struct {
	Status string         `json:"status"`
	Item   inventory.Item `json:"item"`
}

type errorResponse struct {
	Error string `json:"error"`
}

// NewHandler returns the checkout-api HTTP handler.
func NewHandler(logger *slog.Logger, inv Inventory) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /checkout", func(w http.ResponseWriter, r *http.Request) {
		sku := r.URL.Query().Get("sku")
		if sku == "" {
			sku = defaultSKU
		}

		item, err := inv.GetItem(r.Context(), sku)
		if err != nil {
			logger.WarnContext(r.Context(), "inventory lookup failed", "sku", sku, "error", err)
			writeJSON(w, http.StatusBadGateway, errorResponse{Error: "inventory unavailable"})
			return
		}
		writeJSON(w, http.StatusOK, Response{Status: "ok", Item: item})
	})
	mux.Handle("GET /healthz", httpserver.HealthHandler())
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
