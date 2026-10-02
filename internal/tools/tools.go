// Package tools implements the investigation agent's tools and the registry
// that dispatches calls to them.
//
// Every tool call goes through Registry.Call, which validates the tool name,
// enforces a per-call timeout and an output-size limit, measures latency,
// logs the call, and converts failures into structured errors that can be
// returned to the model.
//
// Tools must be read-only and must never expose fault-injection state or
// scenario ground truth.
package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"
)

// Tool is a capability the investigation agent can invoke.
type Tool interface {
	// Name is the identifier the model uses to call the tool.
	Name() string
	// Description tells the model what the tool does and when to use it.
	Description() string
	// InputSchema is the JSON Schema of the tool's input object.
	InputSchema() json.RawMessage
	// Call executes the tool. input is untrusted model output and must be
	// validated. Invalid input should be reported with InvalidInput.
	Call(ctx context.Context, input json.RawMessage) (any, error)
}

// Definition describes a tool to the model.
type Definition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// ErrorCode classifies a failed tool call.
type ErrorCode string

const (
	ErrUnknownTool     ErrorCode = "unknown_tool"
	ErrInvalidInput    ErrorCode = "invalid_input"
	ErrTimeout         ErrorCode = "timeout"
	ErrOutputTooLarge  ErrorCode = "output_too_large"
	ErrExecutionFailed ErrorCode = "execution_failed"
)

// Error is a structured tool failure. Its message is returned to the model,
// so it must not contain secrets.
type Error struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message"`
}

// Error is nil-safe: Result.Err is a *Error, and formatting a nil one (e.g.
// with %v after a successful call) must not crash or hang the caller.
func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	return string(e.Code) + ": " + e.Message
}

// InvalidInput returns an error telling the model its input was rejected.
func InvalidInput(format string, args ...any) error {
	return &Error{Code: ErrInvalidInput, Message: fmt.Sprintf(format, args...)}
}

// Result is the outcome of one tool call.
type Result struct {
	Tool     string
	Output   json.RawMessage // set on success
	Err      *Error          // set on failure
	Duration time.Duration
}

// Registry holds the available tools and dispatches calls to them.
type Registry struct {
	tools          map[string]Tool
	callTimeout    time.Duration
	maxOutputBytes int
	logger         *slog.Logger
}

// Options configures a Registry.
type Options struct {
	// CallTimeout bounds each tool call. Default 15s.
	CallTimeout time.Duration
	// MaxOutputBytes bounds the JSON size of a tool's output. Default 32 KiB.
	MaxOutputBytes int
}

// NewRegistry returns a registry of the given tools. Tool names must be unique.
func NewRegistry(logger *slog.Logger, opts Options, tools ...Tool) (*Registry, error) {
	if opts.CallTimeout <= 0 {
		opts.CallTimeout = 15 * time.Second
	}
	if opts.MaxOutputBytes <= 0 {
		opts.MaxOutputBytes = 32 << 10
	}
	r := &Registry{
		tools:          make(map[string]Tool, len(tools)),
		callTimeout:    opts.CallTimeout,
		maxOutputBytes: opts.MaxOutputBytes,
		logger:         logger,
	}
	for _, t := range tools {
		if _, dup := r.tools[t.Name()]; dup {
			return nil, fmt.Errorf("duplicate tool name %q", t.Name())
		}
		r.tools[t.Name()] = t
	}
	return r, nil
}

// Definitions returns the tool definitions, sorted by name.
func (r *Registry) Definitions() []Definition {
	defs := make([]Definition, 0, len(r.tools))
	for _, t := range r.tools {
		defs = append(defs, Definition{Name: t.Name(), Description: t.Description(), InputSchema: t.InputSchema()})
	}
	sort.Slice(defs, func(i, j int) bool { return defs[i].Name < defs[j].Name })
	return defs
}

// Call runs the named tool. It never returns a Go error: every failure is
// reported in Result.Err so it can be passed back to the model.
func (r *Registry) Call(ctx context.Context, name string, input json.RawMessage) Result {
	start := time.Now()
	res := r.call(ctx, name, input)
	res.Tool = name
	res.Duration = time.Since(start)

	attrs := []any{"tool", name, "duration_ms", res.Duration.Milliseconds(), "input", truncate(string(input), maxLoggedInputBytes)}
	if res.Err != nil {
		r.logger.WarnContext(ctx, "tool call failed", append(attrs, "error_code", res.Err.Code, "error", res.Err.Message)...)
	} else {
		r.logger.InfoContext(ctx, "tool call succeeded", append(attrs, "output_bytes", len(res.Output))...)
	}
	return res
}

func (r *Registry) call(ctx context.Context, name string, input json.RawMessage) Result {
	t, ok := r.tools[name]
	if !ok {
		return Result{Err: &Error{Code: ErrUnknownTool, Message: fmt.Sprintf("no tool named %q", name)}}
	}

	ctx, cancel := context.WithTimeout(ctx, r.callTimeout)
	defer cancel()

	out, err := t.Call(ctx, input)
	if err != nil {
		return Result{Err: classify(ctx, err)}
	}

	b, err := json.Marshal(out)
	if err != nil {
		return Result{Err: &Error{Code: ErrExecutionFailed, Message: "encode tool output: " + err.Error()}}
	}
	if len(b) > r.maxOutputBytes {
		return Result{Err: &Error{
			Code:    ErrOutputTooLarge,
			Message: fmt.Sprintf("output is %d bytes, limit is %d; narrow the request", len(b), r.maxOutputBytes),
		}}
	}
	return Result{Output: b}
}

func classify(ctx context.Context, err error) *Error {
	var te *Error
	if errors.As(err, &te) {
		return te
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return &Error{Code: ErrTimeout, Message: "tool call timed out"}
	}
	return &Error{Code: ErrExecutionFailed, Message: err.Error()}
}

// maxLoggedInputBytes bounds how much of a (model-generated) tool input is logged.
const maxLoggedInputBytes = 1 << 10

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}

// decodeInput strictly decodes a tool input object into v.
func decodeInput(input json.RawMessage, v any) error {
	if len(input) == 0 {
		input = json.RawMessage("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(input))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return InvalidInput("input must be a JSON object matching the schema: %v", err)
	}
	if dec.More() {
		return InvalidInput("input must be a single JSON object")
	}
	return nil
}
