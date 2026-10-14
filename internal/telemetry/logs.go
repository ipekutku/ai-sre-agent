package telemetry

import (
	"context"
	"io"
	"log/slog"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/trace"
)

// logScope is the instrumentation scope of exported log records.
const logScope = "github.com/ipekutku/ai-sre-agent"

// NewLogger returns the logger for the process named service.
//
// Every record is written to w as a JSON line with a "service" field and,
// when the record is logged with a context that has them, "request_id",
// "trace_id" and "span_id" fields.
//
// If lp is not nil, every record is also exported through lp. Exported
// records carry request_id as an attribute; the trace and span IDs are set on
// the record itself and the service name comes from the provider's
// service.name resource, so they are not repeated as attributes.
//
// The correlation fields are added at the top level, so they end up inside
// any group opened with WithGroup. The services do not use groups.
func NewLogger(w io.Writer, service string, lp log.LoggerProvider) *slog.Logger {
	local := contextHandler{
		Handler:  slog.NewJSONHandler(w, nil).WithAttrs([]slog.Attr{slog.String("service", service)}),
		traceIDs: true,
	}
	if lp == nil {
		return slog.New(local)
	}
	exported := contextHandler{Handler: otelslog.NewHandler(logScope, otelslog.WithLoggerProvider(lp))}
	return slog.New(slog.NewMultiHandler(local, exported))
}

// contextHandler adds the request ID and, if traceIDs is set, the trace and
// span IDs from the context to every record.
type contextHandler struct {
	slog.Handler
	traceIDs bool
}

func (h contextHandler) Handle(ctx context.Context, r slog.Record) error {
	id := RequestID(ctx)
	sc := trace.SpanContextFromContext(ctx)
	withTrace := h.traceIDs && sc.IsValid()
	if id == "" && !withTrace {
		return h.Handler.Handle(ctx, r)
	}
	r = r.Clone()
	if id != "" {
		r.AddAttrs(slog.String("request_id", id))
	}
	if withTrace {
		r.AddAttrs(slog.String("trace_id", sc.TraceID().String()), slog.String("span_id", sc.SpanID().String()))
	}
	return h.Handler.Handle(ctx, r)
}

func (h contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return contextHandler{Handler: h.Handler.WithAttrs(attrs), traceIDs: h.traceIDs}
}

func (h contextHandler) WithGroup(name string) slog.Handler {
	return contextHandler{Handler: h.Handler.WithGroup(name), traceIDs: h.traceIDs}
}
