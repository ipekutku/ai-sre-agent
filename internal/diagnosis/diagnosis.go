// Package diagnosis defines the structured result of an investigation.
package diagnosis

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
)

// Diagnosis is the machine-readable conclusion of an investigation.
type Diagnosis struct {
	IncidentID         string     `json:"incident_id"`
	RootCause          RootCause  `json:"root_cause"`
	Confidence         float64    `json:"confidence"`
	Evidence           []Evidence `json:"evidence"`
	RecommendedActions []string   `json:"recommended_actions"`
}

// RootCause identifies the most likely cause of the incident.
type RootCause struct {
	// Code is an UPPER_SNAKE_CASE identifier, e.g. INVENTORY_DOWNSTREAM_LATENCY.
	Code    string `json:"code"`
	Summary string `json:"summary"`
}

// Evidence is one observation supporting the root cause.
type Evidence struct {
	// Source is where the observation came from, e.g. "prometheus".
	Source      string `json:"source"`
	Observation string `json:"observation"`
}

var codePattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// Parse decodes a JSON diagnosis strictly (unknown fields and trailing data
// are rejected) and validates it.
func Parse(data []byte) (Diagnosis, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var d Diagnosis
	if err := dec.Decode(&d); err != nil {
		return Diagnosis{}, fmt.Errorf("decode diagnosis: %w", err)
	}
	if dec.More() {
		return Diagnosis{}, errors.New("decode diagnosis: unexpected data after JSON object")
	}
	if err := d.Validate(); err != nil {
		return Diagnosis{}, fmt.Errorf("invalid diagnosis: %w", err)
	}
	return d, nil
}

// Validate reports whether d has every required field with a valid value.
// A diagnosis must cite at least one piece of evidence.
func (d Diagnosis) Validate() error {
	var errs []error
	if d.IncidentID == "" {
		errs = append(errs, errors.New("incident_id is required"))
	}
	if !codePattern.MatchString(d.RootCause.Code) {
		errs = append(errs, fmt.Errorf("root_cause.code must be UPPER_SNAKE_CASE, got %q", d.RootCause.Code))
	} else if !IsKnownCode(d.RootCause.Code) {
		errs = append(errs, fmt.Errorf("root_cause.code %q is not a known root-cause code", d.RootCause.Code))
	}
	if d.RootCause.Summary == "" {
		errs = append(errs, errors.New("root_cause.summary is required"))
	}
	if d.Confidence < 0 || d.Confidence > 1 {
		errs = append(errs, fmt.Errorf("confidence must be in [0, 1], got %v", d.Confidence))
	}
	if len(d.Evidence) == 0 {
		errs = append(errs, errors.New("at least one evidence item is required"))
	}
	for i, e := range d.Evidence {
		if e.Source == "" || e.Observation == "" {
			errs = append(errs, fmt.Errorf("evidence[%d]: source and observation are required", i))
		}
	}
	for i, a := range d.RecommendedActions {
		if a == "" {
			errs = append(errs, fmt.Errorf("recommended_actions[%d] is empty", i))
		}
	}
	return errors.Join(errs...)
}
