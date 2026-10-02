// Package evaluation scores a diagnosis against a scenario's ground truth.
//
// This is the only package that reads ground-truth.yaml. The investigation
// agent and its tools must never import it.
package evaluation

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"

	"go.yaml.in/yaml/v3"

	"github.com/ipekutku/ai-sre-agent/internal/diagnosis"
	"github.com/ipekutku/ai-sre-agent/internal/scenarios"
)

// GroundTruthFileName is the ground-truth file within a scenario directory.
const GroundTruthFileName = "ground-truth.yaml"

var codePattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// GroundTruth is the known answer for a scenario.
type GroundTruth struct {
	ExpectedRootCause ExpectedRootCause `yaml:"expected_root_cause"`
}

// ExpectedRootCause is the root-cause code a correct diagnosis must report.
type ExpectedRootCause struct {
	Code string `yaml:"code"`
}

// LoadGroundTruth reads and validates dir/ground-truth.yaml.
func LoadGroundTruth(dir string) (GroundTruth, error) {
	path := filepath.Join(dir, GroundTruthFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		return GroundTruth{}, fmt.Errorf("read ground truth: %w", err)
	}

	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var gt GroundTruth
	if err := dec.Decode(&gt); err != nil {
		return GroundTruth{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if !codePattern.MatchString(gt.ExpectedRootCause.Code) {
		return GroundTruth{}, fmt.Errorf("invalid ground truth %s: expected_root_cause.code must be UPPER_SNAKE_CASE, got %q",
			path, gt.ExpectedRootCause.Code)
	}
	return gt, nil
}

// Result is the outcome of evaluating one diagnosis.
type Result struct {
	ScenarioID string
	Expected   string
	// Actual is the diagnosed root-cause code, or empty if the diagnosis was
	// invalid.
	Actual string
	Pass   bool
	// Reason explains a failure. Empty on pass.
	Reason string
}

// Evaluate compares d against gt for scenario s. A diagnosis passes only if
// it is valid, refers to the scenario's incident, and reports the expected
// root-cause code.
func Evaluate(s scenarios.Scenario, gt GroundTruth, d diagnosis.Diagnosis) Result {
	r := Result{ScenarioID: s.ID, Expected: gt.ExpectedRootCause.Code}

	if err := d.Validate(); err != nil {
		r.Reason = "invalid diagnosis: " + err.Error()
		return r
	}
	r.Actual = d.RootCause.Code

	switch {
	case d.IncidentID != s.Incident.IncidentID:
		r.Reason = fmt.Sprintf("diagnosis is for incident %q, want %q", d.IncidentID, s.Incident.IncidentID)
	case d.RootCause.Code != gt.ExpectedRootCause.Code:
		r.Reason = "root cause does not match ground truth"
	default:
		r.Pass = true
	}
	return r
}

// WriteReport writes a human-readable summary of r.
func WriteReport(w io.Writer, r Result) error {
	actual := r.Actual
	if actual == "" {
		actual = "(none)"
	}
	verdict := "PASS"
	if !r.Pass {
		verdict = "FAIL"
	}

	_, err := fmt.Fprintf(w, "Scenario: %s\n\nExpected:\n%s\n\nActual:\n%s\n\nResult:\n%s\n",
		r.ScenarioID, r.Expected, actual, verdict)
	if err == nil && r.Reason != "" {
		_, err = fmt.Fprintf(w, "\nReason:\n%s\n", r.Reason)
	}
	return err
}
