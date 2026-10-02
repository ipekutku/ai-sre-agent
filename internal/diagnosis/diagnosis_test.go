package diagnosis

import (
	"strings"
	"testing"
)

const validJSON = `{
  "incident_id": "inc-001",
  "root_cause": {
    "code": "INVENTORY_DOWNSTREAM_LATENCY",
    "summary": "checkout-api latency is caused by elevated response latency from inventory-api"
  },
  "confidence": 0.94,
  "evidence": [
    {"source": "prometheus", "observation": "checkout-api p95 latency increased significantly"},
    {"source": "prometheus", "observation": "inventory-api server latency increased during the incident"}
  ],
  "recommended_actions": ["Investigate the cause of elevated latency in inventory-api"]
}`

func TestParseValid(t *testing.T) {
	d, err := Parse([]byte(validJSON))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if d.RootCause.Code != "INVENTORY_DOWNSTREAM_LATENCY" || d.Confidence != 0.94 || len(d.Evidence) != 2 {
		t.Errorf("parsed diagnosis = %+v", d)
	}
}

func TestParseRejects(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr string
	}{
		{"not json", `diagnosis: inventory`, "decode diagnosis"},
		{"unknown field", strings.Replace(validJSON, `"confidence"`, `"ground_truth": "x", "confidence"`, 1), "unknown field"},
		{"trailing data", validJSON + `{}`, "unexpected data"},
		{"lowercase code", strings.Replace(validJSON, "INVENTORY_DOWNSTREAM_LATENCY", "inventory latency", 1), "UPPER_SNAKE_CASE"},
		{"confidence above 1", strings.Replace(validJSON, "0.94", "1.5", 1), "confidence must be in [0, 1]"},
		{"negative confidence", strings.Replace(validJSON, "0.94", "-0.1", 1), "confidence must be in [0, 1]"},
		{"missing incident", strings.Replace(validJSON, `"inc-001"`, `""`, 1), "incident_id is required"},
		{"empty summary", `{"incident_id":"i","root_cause":{"code":"X","summary":""},"confidence":0.5,"evidence":[{"source":"s","observation":"o"}]}`, "summary is required"},
		{"no evidence", `{"incident_id":"i","root_cause":{"code":"X","summary":"s"},"confidence":0.5,"evidence":[]}`, "at least one evidence"},
		{"empty evidence fields", `{"incident_id":"i","root_cause":{"code":"X","summary":"s"},"confidence":0.5,"evidence":[{"source":"","observation":"o"}]}`, "evidence[0]"},
		{"empty action", `{"incident_id":"i","root_cause":{"code":"X","summary":"s"},"confidence":0.5,"evidence":[{"source":"s","observation":"o"}],"recommended_actions":[""]}`, "recommended_actions[0]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.input))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}
