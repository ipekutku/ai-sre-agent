package incident

import (
	"strings"
	"testing"
)

func valid() Incident {
	return Incident{
		IncidentID:  "inc-001",
		Alert:       "HighCheckoutLatency",
		Service:     "checkout-api",
		Severity:    "warning",
		Description: "p95 request latency exceeded threshold",
	}
}

func TestValidate(t *testing.T) {
	if err := valid().Validate(); err != nil {
		t.Fatalf("valid incident: %v", err)
	}

	tests := []struct {
		name    string
		mutate  func(*Incident)
		wantErr string
	}{
		{"missing id", func(i *Incident) { i.IncidentID = "" }, "incident_id is required"},
		{"missing alert", func(i *Incident) { i.Alert = "" }, "alert is required"},
		{"missing service", func(i *Incident) { i.Service = "" }, "service is required"},
		{"missing description", func(i *Incident) { i.Description = "" }, "description is required"},
		{"unknown severity", func(i *Incident) { i.Severity = "sev1" }, "severity must be"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inc := valid()
			tt.mutate(&inc)
			err := inc.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}
