package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
)

func TestNewLoggerLocalOnly(t *testing.T) {
	var buf syncBuffer
	logger := NewLogger(&buf, "test-service", nil)

	logger.Info("no context")
	_, tp := newRecorder()
	ctx, span := tp.Tracer("test").Start(withRequestID(context.Background(), "req-1"), "op")
	logger.WarnContext(ctx, "with context", "sku", "abc")
	span.End()

	lines := buf.lines(t)
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2:\n%s", len(lines), buf.String())
	}
	plain, correlated := lines[0], lines[1]
	if plain["service"] != "test-service" || plain["msg"] != "no context" || plain["level"] != "INFO" {
		t.Errorf("plain line = %v", plain)
	}
	for _, k := range []string{"request_id", "trace_id", "span_id"} {
		if _, ok := plain[k]; ok {
			t.Errorf("line logged without context has %s", k)
		}
	}

	want := map[string]any{
		"service":    "test-service",
		"level":      "WARN",
		"msg":        "with context",
		"sku":        "abc",
		"request_id": "req-1",
		"trace_id":   span.SpanContext().TraceID().String(),
		"span_id":    span.SpanContext().SpanID().String(),
	}
	for k, v := range want {
		if correlated[k] != v {
			t.Errorf("%s = %v, want %v", k, correlated[k], v)
		}
	}
	if _, ok := correlated["time"]; !ok {
		t.Error("line has no time")
	}
}

func TestNewLoggerExports(t *testing.T) {
	var buf syncBuffer
	exp, lp := newLogRecorder()
	logger := NewLogger(&buf, "test-service", lp)

	_, tp := newRecorder()
	ctx, span := tp.Tracer("test").Start(withRequestID(context.Background(), "req-1"), "op")
	logger.ErrorContext(ctx, "lookup failed", "sku", "abc")
	span.End()

	if n := len(buf.lines(t)); n != 1 {
		t.Errorf("got %d local lines, want 1", n)
	}
	records := exp.Records()
	if len(records) != 1 {
		t.Fatalf("exported %d records, want 1", len(records))
	}
	r := records[0]
	if got := r.Body().AsString(); got != "lookup failed" {
		t.Errorf("body = %q, want %q", got, "lookup failed")
	}
	if r.Severity() != log.SeverityError {
		t.Errorf("severity = %v, want error", r.Severity())
	}
	if r.TraceID() != span.SpanContext().TraceID() || r.SpanID() != span.SpanContext().SpanID() {
		t.Errorf("record trace/span = %s/%s, want %s/%s", r.TraceID(), r.SpanID(),
			span.SpanContext().TraceID(), span.SpanContext().SpanID())
	}
	attrs := attributes(r)
	if attrs["request_id"] != "req-1" || attrs["sku"] != "abc" {
		t.Errorf("attributes = %v, want request_id=req-1 and sku=abc", attrs)
	}
	// Carried natively (record fields, service.name resource), not as attributes.
	for _, k := range []string{"service", "trace_id", "span_id"} {
		if _, ok := attrs[k]; ok {
			t.Errorf("exported record has attribute %s", k)
		}
	}
}

// logRecorder is an in-memory log exporter.
type logRecorder struct {
	mu      sync.Mutex
	records []sdklog.Record
}

// newLogRecorder returns a recorder and a provider that exports to it
// synchronously.
func newLogRecorder() (*logRecorder, *sdklog.LoggerProvider) {
	exp := &logRecorder{}
	return exp, sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(exp)))
}

func (e *logRecorder) Export(_ context.Context, records []sdklog.Record) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, r := range records {
		e.records = append(e.records, r.Clone())
	}
	return nil
}

func (e *logRecorder) Shutdown(context.Context) error   { return nil }
func (e *logRecorder) ForceFlush(context.Context) error { return nil }

func (e *logRecorder) Records() []sdklog.Record {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]sdklog.Record(nil), e.records...)
}

// attributes returns a record's attributes as strings.
func attributes(r sdklog.Record) map[string]string {
	out := map[string]string{}
	r.WalkAttributes(func(kv attribute.KeyValue) bool {
		out[string(kv.Key)] = kv.Value.Emit()
		return true
	})
	return out
}

// syncBuffer is a bytes.Buffer safe for use by a server goroutine and a test.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// lines decodes the buffer as JSON lines.
func (b *syncBuffer) lines(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(b.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line is not JSON: %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}
