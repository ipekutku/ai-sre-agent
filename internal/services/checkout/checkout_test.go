package checkout

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ipekutku/ai-sre-agent/internal/services/inventory"
)

type fakeInventory struct {
	item    inventory.Item
	err     error
	gotSKU  string
	gotCtx  context.Context
	callCnt int
}

func (f *fakeInventory) GetItem(ctx context.Context, sku string) (inventory.Item, error) {
	f.callCnt++
	f.gotSKU = sku
	f.gotCtx = ctx
	return f.item, f.err
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestCheckoutSuccess(t *testing.T) {
	inv := &fakeInventory{item: inventory.Item{SKU: "abc-1", Quantity: 7}}
	rec := httptest.NewRecorder()
	NewHandler(discardLogger(), inv).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/checkout?sku=abc-1", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if inv.gotSKU != "abc-1" {
		t.Errorf("inventory called with sku %q, want %q", inv.gotSKU, "abc-1")
	}
	var resp Response
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if resp.Status != "ok" || resp.Item != inv.item {
		t.Errorf("response = %+v, want status ok and item %+v", resp, inv.item)
	}
}

func TestCheckoutDefaultSKU(t *testing.T) {
	inv := &fakeInventory{}
	rec := httptest.NewRecorder()
	NewHandler(discardLogger(), inv).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/checkout", nil))

	if inv.gotSKU != defaultSKU {
		t.Errorf("inventory called with sku %q, want %q", inv.gotSKU, defaultSKU)
	}
}

func TestCheckoutPropagatesRequestContext(t *testing.T) {
	type ctxKey struct{}
	inv := &fakeInventory{}
	req := httptest.NewRequest(http.MethodGet, "/checkout", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxKey{}, "marker"))

	NewHandler(discardLogger(), inv).ServeHTTP(httptest.NewRecorder(), req)

	if inv.gotCtx == nil || inv.gotCtx.Value(ctxKey{}) != "marker" {
		t.Error("inventory call did not receive the request context")
	}
}

func TestCheckoutInventoryFailure(t *testing.T) {
	inv := &fakeInventory{err: errors.New("boom")}
	rec := httptest.NewRecorder()
	NewHandler(discardLogger(), inv).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/checkout", nil))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
	var body errorResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Error == "" || body.Error == "boom" {
		t.Errorf("error body = %q, want a generic message that does not leak the internal error", body.Error)
	}
}

func TestRoutes(t *testing.T) {
	tests := []struct {
		method, target string
		want           int
	}{
		{http.MethodGet, "/healthz", http.StatusOK},
		{http.MethodPost, "/checkout", http.StatusMethodNotAllowed},
		{http.MethodGet, "/unknown", http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.target, func(t *testing.T) {
			inv := &fakeInventory{}
			rec := httptest.NewRecorder()
			NewHandler(discardLogger(), inv).ServeHTTP(rec, httptest.NewRequest(tt.method, tt.target, nil))
			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d", rec.Code, tt.want)
			}
			if inv.callCnt != 0 {
				t.Errorf("inventory called %d times, want 0", inv.callCnt)
			}
		})
	}
}
