package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ipekutku/ai-sre-agent/internal/diagnosis"
	"github.com/ipekutku/ai-sre-agent/internal/incident"
	"github.com/ipekutku/ai-sre-agent/internal/llm"
)

// submitToolName is the tool the model calls to finish an investigation.
// It is handled by the loop itself, not the tool registry.
const submitToolName = "submit_diagnosis"

// systemPrompt is identical for every investigation. It must stay
// scenario-neutral: no hints about any particular incident's cause.
var systemPrompt = func() string {
	var codes strings.Builder
	for _, c := range diagnosis.Codes {
		fmt.Fprintf(&codes, "- %s: %s\n", c.Code, c.Description)
	}
	return `You are an SRE investigating a production incident in a system of HTTP services.

You can observe the system only through the tools provided. Base every conclusion on what the
tools return; do not assume facts you have not observed.

Investigate before concluding:
- Check the alerted service and the services it depends on.
- Compare current behaviour with earlier behaviour (range queries help).
- Separate a service's own latency and errors from those of its dependencies, and confirm a
  suspected cause from more than one angle before accepting it.
- Rule out the plausible alternatives the evidence allows you to rule out.

When you have enough evidence, call ` + submitToolName + ` exactly once. Choose the root-cause code that
best matches the evidence:
` + codes.String() + `
Cite the specific observations (metric, service, values) that support your conclusion.`
}()

func incidentPrompt(inc incident.Incident) string {
	b, _ := json.MarshalIndent(inc, "", "  ")
	return "A new incident was raised:\n\n" + string(b) + "\n\nInvestigate it and submit your diagnosis."
}

const nudgeSubmit = "Continue the investigation with the tools, or call " + submitToolName +
	" if you have enough evidence. Reply only through tool calls."

// submitTool describes submit_diagnosis to the model.
var submitTool = func() llm.Tool {
	codes := make([]string, 0, len(diagnosis.Codes))
	for _, c := range diagnosis.Codes {
		codes = append(codes, c.Code)
	}
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"root_cause": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"code":    map[string]any{"type": "string", "enum": codes},
					"summary": map[string]any{"type": "string", "description": "One or two sentences explaining the cause"},
				},
				"required":             []string{"code", "summary"},
				"additionalProperties": false,
			},
			"confidence": map[string]any{"type": "number", "minimum": 0, "maximum": 1},
			"evidence": map[string]any{
				"type":     "array",
				"minItems": 1,
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"source":      map[string]any{"type": "string", "description": "Tool or system the observation came from, e.g. prometheus"},
						"observation": map[string]any{"type": "string", "description": "What was observed, with concrete values"},
					},
					"required":             []string{"source", "observation"},
					"additionalProperties": false,
				},
			},
			"recommended_actions": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		},
		"required":             []string{"root_cause", "confidence", "evidence", "recommended_actions"},
		"additionalProperties": false,
	}
	b, err := json.Marshal(schema)
	if err != nil {
		panic(err)
	}
	return llm.Tool{
		Name:        submitToolName,
		Description: "Submit the final diagnosis. Ends the investigation.",
		InputSchema: b,
	}
}()

// submission is the input of submit_diagnosis. The incident ID is filled in
// by the loop, not the model.
type submission struct {
	RootCause          diagnosis.RootCause  `json:"root_cause"`
	Confidence         float64              `json:"confidence"`
	Evidence           []diagnosis.Evidence `json:"evidence"`
	RecommendedActions []string             `json:"recommended_actions"`
}

func parseSubmission(input json.RawMessage, incidentID string) (diagnosis.Diagnosis, error) {
	dec := json.NewDecoder(strings.NewReader(string(input)))
	dec.DisallowUnknownFields()
	var s submission
	if err := dec.Decode(&s); err != nil {
		return diagnosis.Diagnosis{}, fmt.Errorf("input does not match the schema: %w", err)
	}
	d := diagnosis.Diagnosis{
		IncidentID:         incidentID,
		RootCause:          s.RootCause,
		Confidence:         s.Confidence,
		Evidence:           s.Evidence,
		RecommendedActions: s.RecommendedActions,
	}
	if d.RecommendedActions == nil {
		d.RecommendedActions = []string{}
	}
	return d, d.Validate()
}
