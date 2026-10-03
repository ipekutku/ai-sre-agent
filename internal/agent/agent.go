// Package agent runs incident investigations: it drives the LLM, dispatches
// the tools it requests, enforces budgets, and returns a validated diagnosis.
//
// The agent must never access scenario definitions or ground truth; it only
// receives an incident. A test enforces that this package does not import
// internal/scenarios or internal/evaluation.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ipekutku/ai-sre-agent/internal/diagnosis"
	"github.com/ipekutku/ai-sre-agent/internal/incident"
	"github.com/ipekutku/ai-sre-agent/internal/llm"
	"github.com/ipekutku/ai-sre-agent/internal/tools"
)

var (
	// ErrBudgetExhausted means the LLM request budget ran out before a
	// diagnosis was submitted.
	ErrBudgetExhausted = errors.New("investigation budget exhausted without a diagnosis")
	// ErrTimeout means the investigation exceeded its maximum duration.
	ErrTimeout = errors.New("investigation timed out")
	// ErrModelStopped means the model stopped for a reason the loop cannot
	// continue from (e.g. refusal or output truncation).
	ErrModelStopped = errors.New("model stopped without a diagnosis")
)

// Config bounds an investigation.
type Config struct {
	// MaxLLMRequests bounds model calls. Default 20.
	MaxLLMRequests int
	// MaxToolCalls bounds tool executions. Further tool requests are answered
	// with an error asking the model to submit. Default 15.
	MaxToolCalls int
	// MaxDuration bounds the whole investigation. Default 5m.
	MaxDuration time.Duration
}

// Agent investigates incidents.
type Agent struct {
	llm    llm.Client
	tools  *tools.Registry
	cfg    Config
	logger *slog.Logger
}

// New returns an Agent.
func New(client llm.Client, registry *tools.Registry, logger *slog.Logger, cfg Config) *Agent {
	if cfg.MaxLLMRequests <= 0 {
		cfg.MaxLLMRequests = 20
	}
	if cfg.MaxToolCalls <= 0 {
		cfg.MaxToolCalls = 15
	}
	if cfg.MaxDuration <= 0 {
		cfg.MaxDuration = 5 * time.Minute
	}
	return &Agent{llm: client, tools: registry, cfg: cfg, logger: logger}
}

// Stats describes how an investigation ran. It is populated even when the
// investigation fails.
type Stats struct {
	LLMRequests     int
	ToolCalls       int
	FailedToolCalls int
	Duration        time.Duration
	Usage           llm.Usage
	// Models lists the models that served requests, in order of first use.
	Models []string
}

// Result is the outcome of an investigation.
type Result struct {
	Diagnosis diagnosis.Diagnosis
	Stats     Stats
}

// Investigate runs one investigation of inc.
func (a *Agent) Investigate(ctx context.Context, inc incident.Incident) (res Result, err error) {
	if err := inc.Validate(); err != nil {
		return Result{}, fmt.Errorf("invalid incident: %w", err)
	}

	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, a.cfg.MaxDuration)
	defer cancel()
	log := a.logger.With("incident_id", inc.IncidentID)

	defer func() {
		res.Stats.Duration = time.Since(start)
		attrs := []any{
			"duration_ms", res.Stats.Duration.Milliseconds(),
			"llm_requests", res.Stats.LLMRequests,
			"tool_calls", res.Stats.ToolCalls,
			"failed_tool_calls", res.Stats.FailedToolCalls,
			"input_tokens", res.Stats.Usage.InputTokens,
			"output_tokens", res.Stats.Usage.OutputTokens,
			"cost_usd", res.Stats.Usage.CostUSD,
		}
		if err != nil {
			log.Error("investigation failed", append(attrs, "error", err)...)
		} else {
			log.Info("investigation completed", append(attrs, "root_cause", res.Diagnosis.RootCause.Code)...)
		}
	}()

	req := llm.Request{
		System:   systemPrompt,
		Messages: []llm.Message{llm.UserText(incidentPrompt(inc))},
		Tools:    a.toolDefinitions(),
	}
	res.Stats.Usage.CostKnown = true

	for {
		if res.Stats.LLMRequests >= a.cfg.MaxLLMRequests {
			return res, ErrBudgetExhausted
		}

		resp, err := a.llm.Generate(ctx, req)
		res.Stats.LLMRequests++
		if err != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return res, fmt.Errorf("%w after %s: %w", ErrTimeout, a.cfg.MaxDuration, err)
			}
			return res, fmt.Errorf("llm request %d: %w", res.Stats.LLMRequests, err)
		}
		res.Stats.Usage = res.Stats.Usage.Add(resp.Usage)
		res.Stats.Models = appendUnique(res.Stats.Models, resp.Model)
		log.Info("llm request",
			"request", res.Stats.LLMRequests, "model", resp.Model, "stop_reason", resp.StopReason,
			"latency_ms", resp.Latency.Milliseconds(), "input_tokens", resp.Usage.InputTokens,
			"output_tokens", resp.Usage.OutputTokens, "tool_uses", len(resp.ToolUses()))

		// Append-only: the assistant turn goes into the history unchanged.
		req.Messages = append(req.Messages, resp.Message)

		switch resp.StopReason {
		case llm.StopToolUse:
		case llm.StopEndTurn:
			req.Messages = append(req.Messages, llm.UserText(nudgeSubmit))
			continue
		default:
			return res, fmt.Errorf("%w: stop reason %q", ErrModelStopped, resp.StopReason)
		}

		results, d, done := a.handleToolUses(ctx, log, inc, resp.ToolUses(), &res.Stats)
		if done {
			res.Diagnosis = d
			return res, nil
		}
		req.Messages = append(req.Messages, llm.Message{Role: llm.RoleUser, Content: results})
	}
}

// handleToolUses answers every tool_use block. If a valid diagnosis is
// submitted, it returns it with done=true.
func (a *Agent) handleToolUses(ctx context.Context, log *slog.Logger, inc incident.Incident, uses []llm.Block, stats *Stats) (results []llm.Block, d diagnosis.Diagnosis, done bool) {
	for _, use := range uses {
		if use.ToolName == submitToolName {
			d, err := parseSubmission(use.ToolInput, inc.IncidentID)
			if err == nil {
				return nil, d, true
			}
			log.Warn("invalid diagnosis submitted", "error", err)
			results = append(results, errorResult(use.ToolUseID, "invalid_input",
				"diagnosis rejected: "+err.Error()+". Fix it and call "+submitToolName+" again."))
			continue
		}

		if stats.ToolCalls >= a.cfg.MaxToolCalls {
			results = append(results, errorResult(use.ToolUseID, "budget_exhausted",
				fmt.Sprintf("tool-call budget of %d is used up; call %s now with the evidence you have", a.cfg.MaxToolCalls, submitToolName)))
			continue
		}

		r := a.tools.Call(ctx, use.ToolName, use.ToolInput)
		stats.ToolCalls++
		if r.Err != nil {
			stats.FailedToolCalls++
			results = append(results, errorResult(use.ToolUseID, string(r.Err.Code), r.Err.Message))
			continue
		}
		results = append(results, llm.ToolResult(use.ToolUseID, string(r.Output), false))
	}
	return results, diagnosis.Diagnosis{}, false
}

func (a *Agent) toolDefinitions() []llm.Tool {
	defs := a.tools.Definitions()
	out := make([]llm.Tool, 0, len(defs)+1)
	for _, d := range defs {
		out = append(out, llm.Tool{Name: d.Name, Description: d.Description, InputSchema: d.InputSchema})
	}
	return append(out, submitTool)
}

func errorResult(toolUseID, code, message string) llm.Block {
	b, _ := json.Marshal(map[string]any{"error": map[string]string{"code": code, "message": message}})
	return llm.ToolResult(toolUseID, string(b), true)
}

func appendUnique(s []string, v string) []string {
	for _, x := range s {
		if x == v {
			return s
		}
	}
	return append(s, v)
}
