package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ipekutku/ai-sre-agent/internal/agent"
	"github.com/ipekutku/ai-sre-agent/internal/diagnosis"
	"github.com/ipekutku/ai-sre-agent/internal/evaluation"
	"github.com/ipekutku/ai-sre-agent/internal/incident"
	"github.com/ipekutku/ai-sre-agent/internal/llm"
	"github.com/ipekutku/ai-sre-agent/internal/runner"
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

func TestWriteDetailsPrintsDiagnosis(t *testing.T) {
	for _, code := range []string{"INVENTORY_DOWNSTREAM_LATENCY", ""} {
		t.Run("code="+code, func(t *testing.T) {
			result := agent.Result{
				Stats: agent.Stats{
					Usage:   llm.Usage{CostKnown: true, CostUSD: 0.0308},
					TraceID: "73366e683a1c2f6afc232dd78e6d4611",
				},
				Diagnosis: diagnosis.Diagnosis{
					RootCause:          diagnosis.RootCause{Code: code, Summary: "inventory-api is slow"},
					Confidence:         0.9,
					Evidence:           []diagnosis.Evidence{{Source: "prometheus", Observation: "inventory latency is elevated"}},
					RecommendedActions: []string{"Inspect inventory-api", "Check recent deployments"},
				},
			}
			var out bytes.Buffer
			writeDetails(&out, result, scenarios.TrafficStats{})
			for _, want := range []string{
				"  estimated cost:  $0.0308\n",
				"  trace id:        73366e683a1c2f6afc232dd78e6d4611\n",
			} {
				if !strings.Contains(out.String(), want) {
					t.Errorf("report missing %q:\n%s", want, out.String())
				}
			}
			if code == "" {
				if strings.Contains(out.String(), "Diagnosis (") {
					t.Errorf("empty root-cause code printed diagnosis:\n%s", out.String())
				}
				return
			}
			for _, want := range []string{
				"Diagnosis (confidence 0.90):\n  inventory-api is slow\n",
				"Evidence:\n  - [prometheus] inventory latency is elevated\n",
				"Recommended actions:\n  - Inspect inventory-api\n  - Check recent deployments\n",
			} {
				if !strings.Contains(out.String(), want) {
					t.Errorf("report missing %q:\n%s", want, out.String())
				}
			}
		})
	}
}

func TestReportOutcomes(t *testing.T) {
	sc := scenarios.Scenario{ID: "example", Incident: incident.Incident{IncidentID: "inc-001"}}
	gt := evaluation.GroundTruth{ExpectedRootCause: evaluation.ExpectedRootCause{Code: "INVENTORY_DOWNSTREAM_LATENCY"}}
	valid := diagnosis.Diagnosis{
		IncidentID: "inc-001", RootCause: diagnosis.RootCause{Code: gt.ExpectedRootCause.Code, Summary: "slow dependency"},
		Confidence: 0.9, Evidence: []diagnosis.Evidence{{Source: "prometheus", Observation: "elevated latency"}},
	}
	cleanupErr := errors.New("cleanup failed")
	for _, tc := range []struct {
		name       string
		started    bool
		diagnosis  diagnosis.Diagnosis
		invErr     error
		runErr     error
		wantPass   bool
		wantReport string
		wantActual string
	}{
		{name: "pass", started: true, diagnosis: valid, wantPass: true, wantReport: "Result:\nPASS\n"},
		{name: "invalid diagnosis", started: true, wantReport: "Result:\nFAIL\n"},
		{name: "investigation failure", started: true, invErr: agent.ErrBudgetExhausted, wantReport: "investigation failed: " + agent.ErrBudgetExhausted.Error()},
		{name: "valid diagnosis with investigation failure", started: true, diagnosis: valid, invErr: agent.ErrBudgetExhausted, wantReport: "Result:\nFAIL\n", wantActual: "(none)"},
		{name: "cancelled", started: true, invErr: context.Canceled, runErr: context.Canceled, wantReport: "Result:\nFAIL\n"},
		{name: "diagnosed but cancelled afterwards", started: true, diagnosis: valid, runErr: context.Canceled, wantReport: "Result:\nPASS\n"},
		{name: "diagnosed but cleanup failed", started: true, diagnosis: valid, runErr: cleanupErr, wantReport: "Result:\nPASS\n"},
		{name: "investigation and cleanup failed", started: true, invErr: agent.ErrBudgetExhausted, runErr: cleanupErr, wantReport: "investigation failed: " + agent.ErrBudgetExhausted.Error()},
		{name: "setup failure", runErr: errors.New("not ready")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			record := runner.RunRecord{InvestigationStarted: tc.started, InvestigationError: tc.invErr,
				Result: agent.Result{Diagnosis: tc.diagnosis}}
			var out bytes.Buffer
			pass, err := report(&out, sc, gt, record, tc.runErr)
			if pass != tc.wantPass || !errors.Is(err, tc.runErr) {
				t.Errorf("report = %v, %v; want %v, %v", pass, err, tc.wantPass, tc.runErr)
			}
			if !tc.started && out.Len() != 0 {
				t.Errorf("setup failure produced diagnosis report: %s", out.String())
			}
			if tc.wantReport != "" && !strings.Contains(out.String(), tc.wantReport) {
				t.Errorf("report missing %q:\n%s", tc.wantReport, out.String())
			}
			if tc.wantActual != "" && !strings.Contains(out.String(), "Actual:\n"+tc.wantActual+"\n") {
				t.Errorf("report actual != %q:\n%s", tc.wantActual, out.String())
			}
		})
	}
}

type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

func TestReportPreservesLifecycleErrorWhenWritingFails(t *testing.T) {
	writeErr, cleanupErr := errors.New("write failed"), errors.New("cleanup failed")
	for _, tc := range []struct {
		name   string
		runErr error
	}{
		{"with lifecycle error", cleanupErr},
		{"without lifecycle error", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pass, err := report(failingWriter{writeErr}, scenarios.Scenario{}, evaluation.GroundTruth{},
				runner.RunRecord{InvestigationStarted: true}, tc.runErr)
			if pass || !errors.Is(err, writeErr) || (tc.runErr != nil && !errors.Is(err, tc.runErr)) {
				t.Errorf("report = %v, %v; want false, write error and lifecycle error %v", pass, err, tc.runErr)
			}
		})
	}
}
