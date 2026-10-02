// Package incident defines the alert an investigation starts from.
//
// An Incident is the only scenario information the investigation agent
// receives. It describes the symptom, never the cause.
package incident

import (
	"errors"
	"fmt"
)

// Incident is an alert that triggered an investigation.
type Incident struct {
	IncidentID  string `json:"incident_id" yaml:"incident_id"`
	Alert       string `json:"alert" yaml:"alert"`
	Service     string `json:"service" yaml:"service"`
	Severity    string `json:"severity" yaml:"severity"`
	Description string `json:"description" yaml:"description"`
}

var severities = map[string]bool{"info": true, "warning": true, "critical": true}

// Validate reports whether all fields are set and severity is known.
func (i Incident) Validate() error {
	var errs []error
	for _, f := range []struct{ name, value string }{
		{"incident_id", i.IncidentID},
		{"alert", i.Alert},
		{"service", i.Service},
		{"description", i.Description},
	} {
		if f.value == "" {
			errs = append(errs, fmt.Errorf("%s is required", f.name))
		}
	}
	if !severities[i.Severity] {
		errs = append(errs, fmt.Errorf("severity must be info, warning, or critical, got %q", i.Severity))
	}
	return errors.Join(errs...)
}
