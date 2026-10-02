package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ipekutku/ai-sre-agent/internal/httpserver"
)

func newInspectService(t *testing.T, catalog ...ServiceEntry) *InspectService {
	t.Helper()
	s, err := NewInspectService(catalog, &http.Client{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func callIS(t *testing.T, s *InspectService, input string) (ServiceInfo, error) {
	t.Helper()
	out, err := s.Call(context.Background(), json.RawMessage(input))
	if err != nil {
		return ServiceInfo{}, err
	}
	return out.(ServiceInfo), nil
}

func TestInspectServiceHealthy(t *testing.T) {
	// The real health handler used by the services.
	srv := httptest.NewServer(httpserver.HealthHandler())
	defer srv.Close()

	s := newInspectService(t, ServiceEntry{Name: "checkout-api", BaseURL: srv.URL + "/", Dependencies: []string{"inventory-api"}})
	info, err := callIS(t, s, `{"service":"checkout-api"}`)
	if err != nil {
		t.Fatal(err)
	}
	want := ServiceInfo{Name: "checkout-api", Status: "healthy", Version: "dev", Dependencies: []string{"inventory-api"}}
	if info.Name != want.Name || info.Status != want.Status || info.Version != want.Version || !slices.Equal(info.Dependencies, want.Dependencies) {
		t.Errorf("info = %+v, want %+v", info, want)
	}
}

func TestInspectServiceStatuses(t *testing.T) {
	tests := []struct {
		name        string
		handler     http.HandlerFunc
		wantStatus  string
		wantVersion string
	}{
		{"500 with version", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(500)
			_, _ = w.Write([]byte(`{"status":"degraded","version":"1.2.3"}`))
		}, "unhealthy", "1.2.3"},
		{"200 but not json", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`ok`))
		}, "unhealthy", "unknown"},
		{"200 with non-ok status", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"status":"starting","version":"1.0.0"}`))
		}, "unhealthy", "1.0.0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(tt.handler)
			defer srv.Close()
			info, err := callIS(t, newInspectService(t, ServiceEntry{Name: "svc", BaseURL: srv.URL}), `{"service":"svc"}`)
			if err != nil {
				t.Fatal(err)
			}
			if info.Status != tt.wantStatus || info.Version != tt.wantVersion {
				t.Errorf("status/version = %s/%s, want %s/%s", info.Status, info.Version, tt.wantStatus, tt.wantVersion)
			}
		})
	}
}

func TestInspectServiceUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()

	info, err := callIS(t, newInspectService(t, ServiceEntry{Name: "svc", BaseURL: url}), `{"service":"svc"}`)
	if err != nil {
		t.Fatalf("an unreachable service is a finding, not a tool error: %v", err)
	}
	if info.Status != "unreachable" || info.Version != "unknown" {
		t.Errorf("info = %+v", info)
	}
}

func TestInspectServiceInvalidInput(t *testing.T) {
	s := newInspectService(t,
		ServiceEntry{Name: "checkout-api", BaseURL: "http://127.0.0.1:1"},
		ServiceEntry{Name: "inventory-api", BaseURL: "http://127.0.0.1:1"},
	)
	for _, in := range []string{`{}`, `{"service":"payments-api"}`, `{"service":"checkout-api","admin":true}`} {
		_, err := callIS(t, s, in)
		assertToolError(t, err, ErrInvalidInput)
	}
	_, err := callIS(t, s, `{"service":"nope"}`)
	if err == nil || !strings.Contains(err.Error(), "checkout-api, inventory-api") {
		t.Errorf("unknown-service error should list known services, got %v", err)
	}
}

func TestInspectServiceSchemaListsCatalog(t *testing.T) {
	s := newInspectService(t,
		ServiceEntry{Name: "inventory-api", BaseURL: "http://x"},
		ServiceEntry{Name: "checkout-api", BaseURL: "http://y"},
	)
	var schema struct {
		Properties struct {
			Service struct {
				Enum []string `json:"enum"`
			} `json:"service"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(s.InputSchema(), &schema); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	if !slices.Equal(schema.Properties.Service.Enum, []string{"checkout-api", "inventory-api"}) {
		t.Errorf("enum = %v", schema.Properties.Service.Enum)
	}
}

func TestNewInspectServiceValidatesCatalog(t *testing.T) {
	for _, catalog := range [][]ServiceEntry{
		{{Name: "", BaseURL: "http://x"}},
		{{Name: "a", BaseURL: "x:8080"}},
		{{Name: "a", BaseURL: "http://x"}, {Name: "a", BaseURL: "http://y"}},
	} {
		if _, err := NewInspectService(catalog, nil); err == nil {
			t.Errorf("NewInspectService(%+v) succeeded", catalog)
		}
	}
}

// Both tools' schemas must be valid JSON objects for the LLM API.
func TestSchemasAreValidJSON(t *testing.T) {
	q, _ := NewQueryMetrics("http://prometheus:9090", nil)
	s := newInspectService(t, ServiceEntry{Name: "a", BaseURL: "http://x"})
	for _, tool := range []Tool{q, s} {
		var v map[string]any
		if err := json.Unmarshal(tool.InputSchema(), &v); err != nil || v["type"] != "object" {
			t.Errorf("%s schema invalid: %v", tool.Name(), err)
		}
	}
}
