package agent

import (
	"context"
	"errors"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/ipekutku/ai-sre-agent/internal/incident"
	"github.com/ipekutku/ai-sre-agent/internal/llm"
)

// An investigation is recorded as one trace:
//
//	investigation
//	├── llm.request
//	├── tool.<name>          (recorded by tools.Registry)
//	├── llm.request
//	├── ...
//	└── llm.final_diagnosis  (the request whose diagnosis was accepted)
//
// Spans carry identifiers, counts, sizes, codes, and the accepted root-cause
// code. They never carry prompts, model output, tool input or output, or the
// diagnosis text. Attribute names follow the OpenTelemetry GenAI semantic
// conventions where one exists; they are spelled out here because the semconv
// package in use no longer ships the GenAI group.
const (
	tracerScope        = "github.com/ipekutku/ai-sre-agent/internal/agent"
	investigationSpan  = "investigation"
	llmRequestSpan     = "llm.request"
	finalDiagnosisSpan = "llm.final_diagnosis"
	rejectedEvent      = "diagnosis.rejected"
)

// Investigation outcomes, recorded as investigation.outcome.
const (
	outcomeDiagnosed       = "diagnosed"
	outcomeBudgetExhausted = "budget_exhausted"
	outcomeTimeout         = "timeout"
	outcomeModelStopped    = "model_stopped"
	outcomeCancelled       = "cancelled"
	outcomeLLMError        = "llm_error"
)

func (a *Agent) startInvestigation(ctx context.Context, inc incident.Incident) (context.Context, trace.Span) {
	return a.tracer.Start(ctx, investigationSpan, trace.WithAttributes(
		attribute.String("gen_ai.operation.name", "invoke_agent"),
		attribute.String("incident.id", inc.IncidentID),
		attribute.String("incident.alert", inc.Alert),
		attribute.String("incident.service", inc.Service),
		attribute.String("incident.severity", inc.Severity),
		attribute.Int("investigation.max_llm_requests", a.cfg.MaxLLMRequests),
		attribute.Int("investigation.max_tool_calls", a.cfg.MaxToolCalls),
		attribute.Int64("investigation.max_duration_ms", a.cfg.MaxDuration.Milliseconds()),
	))
}

// endInvestigation records the outcome of an investigation on its span. The
// caller ends the span.
func endInvestigation(span trace.Span, res Result, err error) {
	s := res.Stats
	span.SetAttributes(
		attribute.String("investigation.outcome", outcome(err)),
		attribute.Int("investigation.llm_requests", s.LLMRequests),
		attribute.Int("investigation.tool_calls", s.ToolCalls),
		attribute.Int("investigation.failed_tool_calls", s.FailedToolCalls),
		attribute.Int("investigation.rejected_diagnoses", s.RejectedDiagnoses),
		attribute.Int("investigation.duplicate_tool_calls", s.DuplicateToolCalls),
		attribute.Int("investigation.invalid_tool_calls", s.InvalidToolCalls),
		attribute.Int("investigation.llm_retries", s.LLMRetries),
	)
	span.SetAttributes(usageAttributes(s.Usage)...)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return
	}
	span.SetAttributes(
		attribute.String("diagnosis.root_cause_code", string(res.Diagnosis.RootCause.Code)),
		attribute.Float64("diagnosis.confidence", res.Diagnosis.Confidence),
	)
}

func outcome(err error) string {
	switch {
	case err == nil:
		return outcomeDiagnosed
	case errors.Is(err, ErrBudgetExhausted):
		return outcomeBudgetExhausted
	case errors.Is(err, ErrTimeout):
		return outcomeTimeout
	case errors.Is(err, ErrModelStopped):
		return outcomeModelStopped
	case errors.Is(err, context.Canceled):
		return outcomeCancelled
	default:
		return outcomeLLMError
	}
}

// llmSpan records one model request.
//
// It is ended only after the response has been handled, so that the request
// that produced the accepted diagnosis can be named llm.final_diagnosis. Its
// end time is when the response arrived, so the span's duration is the
// request latency, and the tool calls the response asked for are siblings of
// the request rather than its children.
type llmSpan struct {
	span     trace.Span
	received time.Time
}

func (a *Agent) startLLMRequest(ctx context.Context, n int) (context.Context, llmSpan) {
	ctx, span := a.tracer.Start(ctx, llmRequestSpan, trace.WithAttributes(
		attribute.String("gen_ai.operation.name", "chat"),
		attribute.Int("llm.request_number", n),
	))
	return ctx, llmSpan{span: span}
}

// attempted records dispatch metadata on both successful and failed requests.
func (s *llmSpan) attempted(attempts int) {
	s.span.SetAttributes(
		attribute.Int("llm.attempts", attempts),
		attribute.Int("llm.retries", max(attempts-1, 0)),
	)
}

// failed records a failed generation and ends the span.
func (s *llmSpan) failed(err error) {
	s.span.SetAttributes(attribute.String("llm.error_kind", string(llm.KindOf(err))))
	s.span.SetStatus(codes.Error, err.Error())
	s.span.End()
}

// responded records the response's metadata. The caller must call end.
func (s *llmSpan) responded(resp llm.Response) {
	s.received = time.Now()
	s.span.SetAttributes(
		attribute.String("gen_ai.response.model", resp.Model),
		attribute.StringSlice("gen_ai.response.finish_reasons", []string{string(resp.StopReason)}),
		attribute.Int("llm.tool_uses", len(resp.ToolUses())),
	)
	s.span.SetAttributes(usageAttributes(resp.Usage)...)
}

// end ends the span of a request that returned a response. final marks the
// request whose diagnosis was accepted.
func (s *llmSpan) end(final bool) {
	if final {
		s.span.SetName(finalDiagnosisSpan)
	}
	s.span.End(trace.WithTimestamp(s.received))
}

// usageAttributes describes token usage and cost. In the GenAI conventions
// gen_ai.usage.input_tokens includes cached tokens, while llm.Usage counts
// them separately.
func usageAttributes(u llm.Usage) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attribute.Int64("gen_ai.usage.input_tokens", u.InputTokens+u.CacheReadTokens+u.CacheWriteTokens),
		attribute.Int64("gen_ai.usage.output_tokens", u.OutputTokens),
		attribute.Int64("gen_ai.usage.cache_read.input_tokens", u.CacheReadTokens),
		attribute.Int64("gen_ai.usage.cache_creation.input_tokens", u.CacheWriteTokens),
	}
	if u.CostKnown {
		attrs = append(attrs, attribute.Float64("llm.cost_usd", u.CostUSD))
	}
	return attrs
}

// traceID returns the ID of span's trace, or "" if the span is not recorded
// (tracing disabled).
func traceID(span trace.Span) string {
	if sc := span.SpanContext(); sc.IsValid() {
		return sc.TraceID().String()
	}
	return ""
}
