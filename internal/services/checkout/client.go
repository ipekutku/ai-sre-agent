package checkout

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/ipekutku/ai-sre-agent/internal/services/inventory"
)

// maxInventoryResponseBytes bounds how much of an inventory response is read.
const maxInventoryResponseBytes = 1 << 20

// InventoryClient calls inventory-api over HTTP.
type InventoryClient struct {
	baseURL    string
	httpClient *http.Client
}

// NewInventoryClient returns a client for the inventory-api at baseURL.
// The httpClient's Timeout bounds each call.
func NewInventoryClient(baseURL string, httpClient *http.Client) *InventoryClient {
	return &InventoryClient{baseURL: strings.TrimRight(baseURL, "/"), httpClient: httpClient}
}

// GetItem fetches inventory for sku.
func (c *InventoryClient) GetItem(ctx context.Context, sku string) (inventory.Item, error) {
	u := c.baseURL + "/inventory?" + url.Values{"sku": {sku}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return inventory.Item{}, fmt.Errorf("build inventory request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return inventory.Item{}, fmt.Errorf("call inventory-api: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return inventory.Item{}, fmt.Errorf("inventory-api returned status %d", resp.StatusCode)
	}

	var item inventory.Item
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxInventoryResponseBytes)).Decode(&item); err != nil {
		return inventory.Item{}, fmt.Errorf("decode inventory response: %w", err)
	}
	return item, nil
}
