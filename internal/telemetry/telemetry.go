// Package telemetry sets up OpenTelemetry tracing for a process and provides
// the HTTP server and client instrumentation used by the demo services.
//
// Spans are exported over OTLP/HTTP to the endpoint in the standard
// OTEL_EXPORTER_OTLP_ENDPOINT (or OTEL_EXPORTER_OTLP_TRACES_ENDPOINT)
// environment variable. Without one, tracing is a no-op, so services run
// unchanged outside the Compose environment and in tests.
//
// Trace context is propagated with W3C Trace Context headers.
package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/ipekutku/ai-sre-agent/internal/version"
)

// propagator is used for all trace-context propagation. It does not depend
// on OpenTelemetry's global state.
var propagator = propagation.TraceContext{}

// Setup returns a tracer provider for the process named serviceName, and a
// function that flushes and stops it. getenv is typically os.Getenv.
//
// If no OTLP endpoint is configured, or OTEL_SDK_DISABLED is "true", it
// returns a no-op provider. Export failures are logged to logger and never
// affect request handling.
func Setup(ctx context.Context, logger *slog.Logger, serviceName string, getenv func(string) string) (trace.TracerProvider, func(context.Context) error, error) {
	noopShutdown := func(context.Context) error { return nil }
	if !Enabled(getenv) {
		return noop.NewTracerProvider(), noopShutdown, nil
	}

	// The exporter reads the endpoint and other OTEL_EXPORTER_OTLP_* settings
	// from the environment itself.
	exp, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("create OTLP trace exporter: %w", err)
	}
	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(
		semconv.ServiceName(serviceName),
		semconv.ServiceVersion(version.Version),
	))
	if err != nil {
		return nil, nil, fmt.Errorf("build telemetry resource: %w", err)
	}

	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		logger.Warn("telemetry export failed", "error", err)
	}))
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	)
	return tp, tp.Shutdown, nil
}

// shutdownTimeout bounds the final flush of buffered spans.
const shutdownTimeout = 5 * time.Second

// Shutdown runs a shutdown function returned by Setup with a bounded timeout.
// It is meant for process exit, when the main context is already cancelled.
func Shutdown(shutdown func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	return shutdown(ctx)
}

// Enabled reports whether Setup would export spans with the given environment.
func Enabled(getenv func(string) string) bool {
	if getenv("OTEL_SDK_DISABLED") == "true" {
		return false
	}
	return getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" || getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != ""
}

// Handler wraps next so every request it serves is recorded as a server span
// named "<method> <route pattern>", continuing any trace propagated by the
// caller. Requests for the paths in untraced (e.g. "/metrics" for
// Prometheus scrapes) are passed through without a span.
//
// Handler must be the outermost wrapper: it reads the route pattern set by
// the ServeMux inside next.
func Handler(tp trace.TracerProvider, next http.Handler, untraced ...string) http.Handler {
	return otelhttp.NewHandler(next, "",
		otelhttp.WithTracerProvider(tp),
		otelhttp.WithPropagators(propagator),
		otelhttp.WithFilter(func(r *http.Request) bool {
			for _, p := range untraced {
				if r.URL.Path == p {
					return false
				}
			}
			return true
		}),
	)
}

// Transport wraps next so every outbound request through it is recorded as a
// client span named "<method> <peer>" and carries the trace context to the
// peer. A nil next uses http.DefaultTransport.
func Transport(tp trace.TracerProvider, peer string, next http.RoundTripper) http.RoundTripper {
	if next == nil {
		next = http.DefaultTransport
	}
	return otelhttp.NewTransport(next,
		otelhttp.WithTracerProvider(tp),
		otelhttp.WithPropagators(propagator),
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			return r.Method + " " + peer
		}),
	)
}
