package scenarios

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Environment is the running demo system as seen by the scenario runner.
// Unlike the agent, the runner may use the fault-injection admin API.
type Environment struct {
	CheckoutURL       string
	InventoryURL      string
	InventoryAdminURL string
	PrometheusURL     string
	HTTP              *http.Client
}

// pollInterval matches the Prometheus scrape interval. Tests shorten it.
var pollInterval = 5 * time.Second

// WaitReady blocks until both services are healthy, the fault API answers,
// and Prometheus is scraping both services.
func (e Environment) WaitReady(ctx context.Context) error {
	checks := []struct {
		name  string
		check func(context.Context) error
	}{
		{"checkout-api", func(ctx context.Context) error { return e.get(ctx, e.CheckoutURL+"/healthz") }},
		{"inventory-api", func(ctx context.Context) error { return e.get(ctx, e.InventoryURL+"/healthz") }},
		{"inventory-api fault API", func(ctx context.Context) error { return e.get(ctx, e.InventoryAdminURL+"/fault") }},
		{"prometheus targets", func(ctx context.Context) error {
			v, err := e.QueryValue(ctx, `count(up{job=~"checkout-api|inventory-api"} == 1)`)
			if err != nil {
				return err
			}
			if v < 2 {
				return fmt.Errorf("%v of 2 targets up", v)
			}
			return nil
		}},
	}
	for _, c := range checks {
		if err := poll(ctx, func() error { return c.check(ctx) }); err != nil {
			return fmt.Errorf("waiting for %s: %w", c.name, err)
		}
	}
	return nil
}

// ApplyFault injects f through inventory-api's admin API.
func (e Environment) ApplyFault(ctx context.Context, f Fault) error {
	if err := f.Validate(); err != nil {
		return err
	}
	body := fmt.Sprintf(`{"latency_ms": %d}`, f.LatencyMS)
	return e.do(ctx, http.MethodPut, e.InventoryAdminURL+"/fault", body)
}

// ClearFault restores normal behaviour.
func (e Environment) ClearFault(ctx context.Context) error {
	return e.do(ctx, http.MethodDelete, e.InventoryAdminURL+"/fault", "")
}

// TrafficStats counts generated requests.
type TrafficStats struct {
	OK, Failed int64
}

// GenerateTraffic sends GET /checkout at rps requests per second until ctx is
// cancelled, then waits for in-flight requests and returns the counts.
// Requests are issued on a fixed schedule, so slow responses do not reduce
// the request rate.
func (e Environment) GenerateTraffic(ctx context.Context, rps int) TrafficStats {
	var ok, failed atomic.Int64
	var wg sync.WaitGroup
	inflight := make(chan struct{}, 200)
	ticker := time.NewTicker(time.Second / time.Duration(rps))
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return TrafficStats{OK: ok.Load(), Failed: failed.Load()}
		case <-ticker.C:
			select {
			case inflight <- struct{}{}:
			default:
				failed.Add(1) // too many in flight; count as failed rather than block
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-inflight }()
				// Detached from ctx so stopping traffic does not abort requests mid-flight.
				reqCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := e.get(reqCtx, e.CheckoutURL+"/checkout"); err != nil {
					failed.Add(1)
				} else {
					ok.Add(1)
				}
			}()
		}
	}
}

// WaitForTrigger polls t.Query until its value exceeds t.Above, returning the
// value that fired, or an error after t.TimeoutSeconds.
func (e Environment) WaitForTrigger(ctx context.Context, t Trigger) (float64, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(t.TimeoutSeconds)*time.Second)
	defer cancel()
	var fired float64
	err := poll(ctx, func() error {
		v, err := e.QueryValue(ctx, t.Query)
		if err != nil {
			return err
		}
		if v <= t.Above {
			return fmt.Errorf("value %v not above %v", v, t.Above)
		}
		fired = v
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("alert condition not met within %ds: %w", t.TimeoutSeconds, err)
	}
	return fired, nil
}

// QueryValue runs an instant PromQL query and returns the first value.
func (e Environment) QueryValue(ctx context.Context, query string) (float64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.PrometheusURL+"/api/v1/query",
		strings.NewReader(url.Values{"query": {query}}.Encode()))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := e.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	var pr struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			Result []struct {
				Value [2]any `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&pr); err != nil {
		return 0, fmt.Errorf("decode prometheus response: %w", err)
	}
	if pr.Status != "success" {
		return 0, fmt.Errorf("prometheus: %s", pr.Error)
	}
	if len(pr.Data.Result) == 0 {
		return 0, fmt.Errorf("query returned no data")
	}
	s, _ := pr.Data.Result[0].Value[1].(string)
	return strconv.ParseFloat(s, 64)
}

func (e Environment) get(ctx context.Context, target string) error {
	return e.do(ctx, http.MethodGet, target, "")
}

func (e Environment) do(ctx context.Context, method, target, body string) error {
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewBufferString(body))
	if err != nil {
		return err
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := e.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s %s: HTTP %d", method, target, resp.StatusCode)
	}
	return nil
}

// poll calls f every pollInterval until it succeeds or ctx is done, returning
// f's last error on timeout.
func poll(ctx context.Context, f func() error) error {
	for {
		err := f()
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(pollInterval):
		}
	}
}
