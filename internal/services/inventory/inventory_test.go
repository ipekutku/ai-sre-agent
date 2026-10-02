package inventory

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestInventory(t *testing.T) {
	tests := []struct {
		name    string
		target  string
		wantSKU string
	}{
		{name: "default sku", target: "/inventory", wantSKU: defaultSKU},
		{name: "explicit sku", target: "/inventory?sku=abc-1", wantSKU: "abc-1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			NewHandler(NewLatency(0)).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tt.target, nil))

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
			var item Item
			if err := json.NewDecoder(rec.Body).Decode(&item); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if item.SKU != tt.wantSKU {
				t.Errorf("sku = %q, want %q", item.SKU, tt.wantSKU)
			}
		})
	}
}

func TestRoutes(t *testing.T) {
	tests := []struct {
		method, target string
		want           int
	}{
		{http.MethodGet, "/healthz", http.StatusOK},
		{http.MethodPost, "/inventory", http.StatusMethodNotAllowed},
		{http.MethodGet, "/unknown", http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.target, func(t *testing.T) {
			rec := httptest.NewRecorder()
			NewHandler(NewLatency(0)).ServeHTTP(rec, httptest.NewRequest(tt.method, tt.target, nil))
			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d", rec.Code, tt.want)
			}
		})
	}
}
