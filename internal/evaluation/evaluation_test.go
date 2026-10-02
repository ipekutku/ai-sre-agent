package evaluation

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ipekutku/ai-sre-agent/internal/diagnosis"
	"github.com/ipekutku/ai-sre-agent/internal/scenarios"
)

var repoScenarioDir = filepath.Join("..", "..", "scenarios", "inventory-latency")

func loadRepoScenario(t *testing.T) (scenarios.Scenario, GroundTruth) {
	t.Helper()
	s, err := scenarios.Load(repoScenarioDir)
	if err != nil {
		t.Fatalf("load scenario: %v", err)
	}
	gt, err := LoadGroundTruth(repoScenarioDir)
	if err != nil {
		t.Fatalf("load ground truth: %v", err)
	}
	return s, gt
}

func correctDiagnosis() diagnosis.Diagnosis {
	return diagnosis.Diagnosis{
		IncidentID: "inc-001",
		RootCause: diagnosis.RootCause{
			Code:    "INVENTORY_DOWNSTREAM_LATENCY",
			Summary: "checkout-api latency is caused by inventory-api latency",
		},
		Confidence: 0.9,
		Evidence: []diagnosis.Evidence{
			{Source: "prometheus", Observation: "inventory-api p95 latency rose from ~25ms to ~990ms"},
		},
	}
}

func TestLoadRepositoryGroundTruth(t *testing.T) {
	_, gt := loadRepoScenario(t)
	if gt.ExpectedRootCause.Code != "INVENTORY_DOWNSTREAM_LATENCY" {
		t.Errorf("expected code = %q", gt.ExpectedRootCause.Code)
	}
}

func TestEvaluate(t *testing.T) {
	s, gt := loadRepoScenario(t)

	tests := []struct {
		name       string
		mutate     func(*diagnosis.Diagnosis)
		wantPass   bool
		wantActual string
		wantReason string
	}{
		{
			name:       "correct",
			mutate:     func(*diagnosis.Diagnosis) {},
			wantPass:   true,
			wantActual: "INVENTORY_DOWNSTREAM_LATENCY",
		},
		{
			name:       "wrong root cause",
			mutate:     func(d *diagnosis.Diagnosis) { d.RootCause.Code = "CHECKOUT_CPU_SATURATION" },
			wantActual: "CHECKOUT_CPU_SATURATION",
			wantReason: "does not match ground truth",
		},
		{
			name:       "wrong incident",
			mutate:     func(d *diagnosis.Diagnosis) { d.IncidentID = "inc-999" },
			wantActual: "INVENTORY_DOWNSTREAM_LATENCY",
			wantReason: `incident "inc-999"`,
		},
		{
			name:       "correct code but no evidence",
			mutate:     func(d *diagnosis.Diagnosis) { d.Evidence = nil },
			wantReason: "invalid diagnosis",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := correctDiagnosis()
			tt.mutate(&d)
			r := Evaluate(s, gt, d)

			if r.Pass != tt.wantPass || r.Actual != tt.wantActual || !strings.Contains(r.Reason, tt.wantReason) {
				t.Errorf("result = %+v, want pass=%v actual=%q reason containing %q",
					r, tt.wantPass, tt.wantActual, tt.wantReason)
			}
			if r.ScenarioID != "inventory-latency" || r.Expected != "INVENTORY_DOWNSTREAM_LATENCY" {
				t.Errorf("result identity = %+v", r)
			}
			if tt.wantPass && r.Reason != "" {
				t.Errorf("passing result has reason %q", r.Reason)
			}
		})
	}
}

func TestWriteReport(t *testing.T) {
	tests := []struct {
		name string
		r    Result
		want string
	}{
		{
			name: "pass",
			r:    Result{ScenarioID: "inventory-latency", Expected: "A", Actual: "A", Pass: true},
			want: "Scenario: inventory-latency\n\nExpected:\nA\n\nActual:\nA\n\nResult:\nPASS\n",
		},
		{
			name: "fail without diagnosis",
			r:    Result{ScenarioID: "inventory-latency", Expected: "A", Reason: "invalid diagnosis: x"},
			want: "Scenario: inventory-latency\n\nExpected:\nA\n\nActual:\n(none)\n\nResult:\nFAIL\n\nReason:\ninvalid diagnosis: x\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var b strings.Builder
			if err := WriteReport(&b, tt.r); err != nil {
				t.Fatal(err)
			}
			if b.String() != tt.want {
				t.Errorf("report =\n%s\nwant\n%s", b.String(), tt.want)
			}
		})
	}
}

func TestLoadGroundTruthRejects(t *testing.T) {
	tests := []struct {
		name, content, wantErr string
	}{
		{"missing code", "expected_root_cause: {}\n", "UPPER_SNAKE_CASE"},
		{"lowercase code", "expected_root_cause:\n  code: inventory latency\n", "UPPER_SNAKE_CASE"},
		{"unknown field", "expected_root_cause:\n  code: X\nnotes: y\n", "field notes not found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, GroundTruthFileName), []byte(tt.content), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := LoadGroundTruth(dir)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}
