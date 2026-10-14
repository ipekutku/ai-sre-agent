package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

type fakeTool struct {
	name string
	call func(ctx context.Context, input json.RawMessage) (any, error)
}

func (f fakeTool) Name() string                 { return f.name }
func (f fakeTool) Description() string          { return "fake " + f.name }
func (f fakeTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (f fakeTool) Call(ctx context.Context, in json.RawMessage) (any, error) {
	return f.call(ctx, in)
}

func newRegistry(t *testing.T, opts Options, tools ...Tool) *Registry {
	t.Helper()
	r, err := NewRegistry(discardLogger(), opts, tools...)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRegistrySuccess(t *testing.T) {
	r := newRegistry(t, Options{}, fakeTool{name: "echo", call: func(_ context.Context, in json.RawMessage) (any, error) {
		return map[string]string{"got": string(in)}, nil
	}})

	res := r.Call(context.Background(), "echo", json.RawMessage(`{"x":1}`))
	if res.Err != nil {
		t.Fatalf("unexpected error: %v", res.Err)
	}
	if string(res.Output) != `{"got":"{\"x\":1}"}` {
		t.Errorf("output = %s", res.Output)
	}
	if res.Tool != "echo" || res.Duration <= 0 {
		t.Errorf("result metadata = %+v", res)
	}
}

func TestRegistryErrors(t *testing.T) {
	slow := fakeTool{name: "slow", call: func(ctx context.Context, _ json.RawMessage) (any, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	invalid := fakeTool{name: "invalid", call: func(context.Context, json.RawMessage) (any, error) {
		return nil, InvalidInput("bad field %q", "x")
	}}
	broken := fakeTool{name: "broken", call: func(context.Context, json.RawMessage) (any, error) {
		return nil, errors.New("disk on fire")
	}}
	big := fakeTool{name: "big", call: func(context.Context, json.RawMessage) (any, error) {
		return strings.Repeat("x", 200), nil
	}}
	unencodable := fakeTool{name: "unencodable", call: func(context.Context, json.RawMessage) (any, error) {
		return make(chan int), nil
	}}
	r := newRegistry(t, Options{CallTimeout: 50 * time.Millisecond, MaxOutputBytes: 100},
		slow, invalid, broken, big, unencodable)

	tests := []struct {
		tool     string
		wantCode ErrorCode
		wantMsg  string
	}{
		{"nope", ErrUnknownTool, `no tool named "nope"`},
		{"slow", ErrTimeout, "timed out"},
		{"invalid", ErrInvalidInput, `bad field "x"`},
		{"broken", ErrExecutionFailed, "disk on fire"},
		{"big", ErrOutputTooLarge, "limit is 100"},
		{"unencodable", ErrExecutionFailed, "encode tool output"},
	}
	for _, tt := range tests {
		t.Run(tt.tool, func(t *testing.T) {
			res := r.Call(context.Background(), tt.tool, nil)
			if res.Err == nil {
				t.Fatalf("expected error, got output %s", res.Output)
			}
			if res.Err.Code != tt.wantCode || !strings.Contains(res.Err.Message, tt.wantMsg) {
				t.Errorf("err = %+v, want code %s with message containing %q", res.Err, tt.wantCode, tt.wantMsg)
			}
			if res.Output != nil {
				t.Errorf("failed call has output %s", res.Output)
			}
		})
	}
}

func TestRegistryParentCancellation(t *testing.T) {
	r := newRegistry(t, Options{}, fakeTool{name: "wait", call: func(ctx context.Context, _ json.RawMessage) (any, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := r.Call(ctx, "wait", nil)
	if res.Err == nil || res.Err.Code != ErrExecutionFailed {
		t.Errorf("err = %+v, want execution_failed for a cancelled investigation", res.Err)
	}
}

func TestRegistryTracesCalls(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	echo := fakeTool{name: "echo", call: func(_ context.Context, in json.RawMessage) (any, error) {
		return map[string]string{"got": string(in)}, nil
	}}
	broken := fakeTool{name: "broken", call: func(context.Context, json.RawMessage) (any, error) {
		return nil, errors.New("backend says secret-query failed")
	}}
	r := newRegistry(t, Options{TracerProvider: tp}, echo, broken)

	ctx, parent := tp.Tracer("test").Start(context.Background(), "investigation")
	r.Call(ctx, "echo", json.RawMessage(`{"query":"up"}`))
	r.Call(ctx, "broken", json.RawMessage(`{}`))
	r.Call(ctx, "execute_shell", json.RawMessage(`{"cmd":"cat ground-truth.yaml"}`))
	parent.End()

	spans := rec.Ended()
	if len(spans) != 4 {
		t.Fatalf("got %d spans, want 3 tool spans and the parent", len(spans))
	}
	tests := []struct {
		name, tool, errorCode string
	}{
		{"tool.echo", "echo", ""},
		{"tool.broken", "broken", "execution_failed"},
		// The requested name is model output and stays out of the span.
		{"tool.unknown", "", "unknown_tool"},
	}
	for i, tt := range tests {
		s := spans[i]
		attrs := map[attribute.Key]attribute.Value{}
		for _, kv := range s.Attributes() {
			attrs[kv.Key] = kv.Value
		}
		if s.Name() != tt.name || s.Parent().SpanID() != parent.SpanContext().SpanID() {
			t.Errorf("span %d = %s (parent %s), want %s under the caller's span", i, s.Name(), s.Parent().SpanID(), tt.name)
		}
		if attrs["gen_ai.tool.name"].AsString() != tt.tool || attrs["tool.error_code"].AsString() != tt.errorCode {
			t.Errorf("%s attributes = %v", tt.name, attrs)
		}
		if failed := s.Status().Code == codes.Error; failed != (tt.errorCode != "") {
			t.Errorf("%s status = %+v", tt.name, s.Status())
		}
		if attrs["tool.input_bytes"].AsInt64() == 0 {
			t.Errorf("%s has no input size", tt.name)
		}
	}
	if got := spans[0].Attributes(); !strings.Contains(fmt.Sprint(got), "tool.output_bytes") {
		t.Errorf("successful call has no output size: %v", got)
	}

	// Sizes and codes only: no tool input, output, error messages, or
	// unknown tool names.
	var dump strings.Builder
	for _, s := range spans {
		fmt.Fprintln(&dump, s.Name(), s.Status().Description, s.Attributes(), s.Events())
	}
	for _, leaked := range []string{"query", "secret-query", "execute_shell", "ground-truth"} {
		if strings.Contains(dump.String(), leaked) {
			t.Errorf("spans contain %q:\n%s", leaked, dump.String())
		}
	}
}

func TestRegistryDefinitions(t *testing.T) {
	noop := func(context.Context, json.RawMessage) (any, error) { return nil, nil }
	r := newRegistry(t, Options{}, fakeTool{name: "b", call: noop}, fakeTool{name: "a", call: noop})
	defs := r.Definitions()
	if len(defs) != 2 || defs[0].Name != "a" || defs[1].Name != "b" {
		t.Errorf("definitions = %+v, want sorted a, b", defs)
	}

	if _, err := NewRegistry(discardLogger(), Options{}, fakeTool{name: "a"}, fakeTool{name: "a"}); err == nil {
		t.Error("expected duplicate name error")
	}
}

func TestDecodeInput(t *testing.T) {
	var v struct {
		A string `json:"a"`
	}
	for _, in := range []string{`{"a":"x","b":1}`, `[1]`, `{"a":"x"}{}`, `nope`} {
		if err := decodeInput(json.RawMessage(in), &v); err == nil {
			t.Errorf("decodeInput(%s) succeeded, want invalid input", in)
		} else {
			var te *Error
			if !errors.As(err, &te) || te.Code != ErrInvalidInput {
				t.Errorf("decodeInput(%s) err = %v, want invalid_input", in, err)
			}
		}
	}
}

// Regression: formatting a nil *Error (Result.Err after a successful call)
// used to hang the process.
func TestNilErrorFormats(t *testing.T) {
	var e *Error
	if got := fmt.Sprintf("%v", e); got != "<nil>" {
		t.Errorf("formatted nil *Error = %q, want <nil>", got)
	}
}
