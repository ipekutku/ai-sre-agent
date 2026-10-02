package checkout

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ipekutku/ai-sre-agent/internal/services/inventory"
)

func TestInventoryClientAgainstRealHandler(t *testing.T) {
	srv := httptest.NewServer(inventory.NewHandler())
	defer srv.Close()

	// Trailing slash must not produce a "//inventory" path.
	c := NewInventoryClient(srv.URL+"/", srv.Client())
	item, err := c.GetItem(context.Background(), "abc 1&x=y")
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if item.SKU != "abc 1&x=y" {
		t.Errorf("sku = %q, want the query-escaped value round-tripped", item.SKU)
	}
}

func TestInventoryClientErrors(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		wantErr string
	}{
		{
			name: "non-200 status",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			},
			wantErr: "status 500",
		},
		{
			name: "invalid body",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("not json"))
			},
			wantErr: "decode inventory response",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(tt.handler)
			defer srv.Close()

			_, err := NewInventoryClient(srv.URL, srv.Client()).GetItem(context.Background(), "x")
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestInventoryClientTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	hc := srv.Client()
	hc.Timeout = 50 * time.Millisecond
	start := time.Now()
	_, err := NewInventoryClient(srv.URL, hc).GetItem(context.Background(), "x")
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("GetItem took %v, want it bounded by the client timeout", elapsed)
	}
}

func TestInventoryClientContextCancelled(t *testing.T) {
	srv := httptest.NewServer(inventory.NewHandler())
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := NewInventoryClient(srv.URL, srv.Client()).GetItem(ctx, "x")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
