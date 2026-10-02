package scenarios

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The repository's own scenarios must always load.
func TestLoadRepositoryScenario(t *testing.T) {
	s, err := Load(filepath.Join("..", "..", "scenarios", "inventory-latency"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s.ID != "inventory-latency" || s.Incident.IncidentID != "inc-001" || s.Incident.Service != "checkout-api" {
		t.Errorf("scenario = %+v", s)
	}
	if s.Fault != (Fault{Service: "inventory-api", Type: "latency", LatencyMS: 800}) {
		t.Errorf("fault = %+v", s.Fault)
	}
}

// The incident must describe the symptom only; it must not name the faulty
// service or the root-cause code.
func TestRepositoryIncidentDoesNotRevealCause(t *testing.T) {
	s, err := Load(filepath.Join("..", "..", "scenarios", "inventory-latency"))
	if err != nil {
		t.Fatal(err)
	}
	inc := strings.ToLower(s.Incident.Alert + " " + s.Incident.Service + " " + s.Incident.Description)
	for _, leak := range []string{"inventory", "downstream", "fault", "injected"} {
		if strings.Contains(inc, leak) {
			t.Errorf("incident mentions %q: %+v", leak, s.Incident)
		}
	}
}

const validYAML = `id: demo
description: test scenario
incident:
  incident_id: inc-1
  alert: HighLatency
  service: checkout-api
  severity: warning
  description: p95 latency high
fault:
  service: inventory-api
  type: latency
  latency_ms: 800
`

func writeScenario(t *testing.T, name, content string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestLoadRejects(t *testing.T) {
	tests := []struct {
		name    string
		dir     string
		content string
		wantErr string
	}{
		{"id mismatch", "other", validYAML, `does not match directory "other"`},
		{"unknown field", "demo", validYAML + "ground_truth: X\n", "field ground_truth not found"},
		{"bad id", "Demo_1", strings.Replace(validYAML, "id: demo", "id: Demo_1", 1), "kebab-case"},
		{"invalid incident", "demo", strings.Replace(validYAML, "severity: warning", "severity: sev1", 1), "incident: severity"},
		{"unsupported fault type", "demo", strings.Replace(validYAML, "type: latency", "type: errors", 1), "unsupported type"},
		{"unsupported fault service", "demo", strings.Replace(validYAML, "service: inventory-api", "service: checkout-api", 1), "does not support fault injection"},
		{"zero latency", "demo", strings.Replace(validYAML, "latency_ms: 800", "latency_ms: 0", 1), "latency_ms must be"},
		{"latency above cap", "demo", strings.Replace(validYAML, "latency_ms: 800", "latency_ms: 60000", 1), "latency_ms must be"},
		{"not yaml", "demo", "id: [", "parse"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(writeScenario(t, tt.dir, tt.content))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(t.TempDir()); err == nil {
		t.Fatal("expected error for missing scenario.yaml")
	}
}
