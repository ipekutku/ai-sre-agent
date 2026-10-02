package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	// maxHealthBodyBytes bounds how much of a /healthz response is read.
	maxHealthBodyBytes = 4 << 10
	// healthProbeTimeout bounds the health probe; a slower service is
	// reported as unreachable.
	healthProbeTimeout = 2 * time.Second
)

// ServiceEntry is a service the agent is allowed to inspect.
type ServiceEntry struct {
	Name string
	// BaseURL is the service's public API (where /healthz is served).
	// It must never point at an admin or fault-injection endpoint.
	BaseURL      string
	Dependencies []string
}

// InspectService is the inspect_service tool: metadata about a service from
// a fixed catalog, plus its live health and version from GET /healthz.
type InspectService struct {
	catalog    map[string]ServiceEntry
	httpClient *http.Client
}

// NewInspectService returns an inspect_service tool for the given catalog.
func NewInspectService(catalog []ServiceEntry, httpClient *http.Client) (*InspectService, error) {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	m := make(map[string]ServiceEntry, len(catalog))
	for _, s := range catalog {
		if s.Name == "" {
			return nil, fmt.Errorf("catalog entry with empty name")
		}
		if _, dup := m[s.Name]; dup {
			return nil, fmt.Errorf("duplicate catalog entry %q", s.Name)
		}
		u, err := url.Parse(s.BaseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("service %q: base URL must be an absolute http(s) URL, got %q", s.Name, s.BaseURL)
		}
		s.BaseURL = strings.TrimRight(s.BaseURL, "/")
		m[s.Name] = s
	}
	return &InspectService{catalog: m, httpClient: httpClient}, nil
}

func (*InspectService) Name() string { return "inspect_service" }

func (s *InspectService) Description() string {
	return "Return metadata for a service: its health status (from its health endpoint), version, and the " +
		"services it depends on. Known services: " + strings.Join(s.names(), ", ") + "."
}

func (s *InspectService) InputSchema() json.RawMessage {
	names, _ := json.Marshal(s.names())
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "service": {"type": "string", "enum": ` + string(names) + `}
  },
  "required": ["service"],
  "additionalProperties": false
}`)
}

type inspectServiceInput struct {
	Service string `json:"service"`
}

// ServiceInfo is the output of inspect_service.
type ServiceInfo struct {
	Name string `json:"name"`
	// Status is "healthy", "unhealthy" (health endpoint returned an error),
	// or "unreachable" (no response).
	Status       string   `json:"status"`
	Version      string   `json:"version"`
	Dependencies []string `json:"dependencies"`
}

func (s *InspectService) Call(ctx context.Context, raw json.RawMessage) (any, error) {
	var in inspectServiceInput
	if err := decodeInput(raw, &in); err != nil {
		return nil, err
	}
	entry, ok := s.catalog[in.Service]
	if !ok {
		return nil, InvalidInput("unknown service %q; known services: %s", in.Service, strings.Join(s.names(), ", "))
	}

	info := ServiceInfo{
		Name:         entry.Name,
		Dependencies: append([]string{}, entry.Dependencies...),
	}
	info.Status, info.Version = s.health(ctx, entry.BaseURL)
	return info, nil
}

// health probes GET /healthz. Failures are reported as a status, not an
// error: an unhealthy service is a valid finding.
func (s *InspectService) health(ctx context.Context, baseURL string) (status, version string) {
	ctx, cancel := context.WithTimeout(ctx, healthProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/healthz", nil)
	if err != nil {
		return "unreachable", "unknown"
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "unreachable", "unknown"
	}
	defer resp.Body.Close()

	var body struct {
		Status  string `json:"status"`
		Version string `json:"version"`
	}
	decodeErr := json.NewDecoder(io.LimitReader(resp.Body, maxHealthBodyBytes)).Decode(&body)
	version = "unknown"
	if decodeErr == nil && body.Version != "" {
		version = body.Version
	}
	if resp.StatusCode != http.StatusOK || decodeErr != nil || body.Status != "ok" {
		return "unhealthy", version
	}
	return "healthy", version
}

func (s *InspectService) names() []string {
	names := make([]string, 0, len(s.catalog))
	for n := range s.catalog {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
