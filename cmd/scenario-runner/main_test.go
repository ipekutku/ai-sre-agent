package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ipekutku/ai-sre-agent/internal/agent"
	"github.com/ipekutku/ai-sre-agent/internal/scenarios"
)

func TestWriteDetailsIncludesInvestigationCounters(t *testing.T) {
	var out bytes.Buffer
	writeDetails(&out, agent.Result{Stats: agent.Stats{
		LLMRequests: 4, LLMRetries: 2, ToolCalls: 5, FailedToolCalls: 3,
		DuplicateToolCalls: 1, InvalidToolCalls: 2, RejectedDiagnoses: 3,
		ToolCallsByName: map[string]int{"query_metrics": 3, "inspect_service": 2},
	}}, scenarios.TrafficStats{})
	for _, want := range []string{
		"llm requests:    4\n", "llm retries:     2\n",
		"tool calls:      5 (3 failed): inspect_service=2, query_metrics=3\n",
		"duplicate calls: 1\n", "invalid calls:   2\n", "rejected diagnoses: 3\n",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report missing %q:\n%s", want, out.String())
		}
	}
}
