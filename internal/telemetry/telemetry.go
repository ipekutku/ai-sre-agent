// Package telemetry sets up OpenTelemetry tracing and log export for a
// process, and provides the HTTP server and client instrumentation and the
// logger used by the demo services.
//
// Spans and log records are exported over OTLP/HTTP. Endpoints come from the
// standard environment variables: OTEL_EXPORTER_OTLP_ENDPOINT for both
// signals, or OTEL_EXPORTER_OTLP_TRACES_ENDPOINT and
// OTEL_EXPORTER_OTLP_LOGS_ENDPOINT per signal (Compose uses these, because
// traces go to Tempo and logs to Loki). A signal with no endpoint is a no-op,
// so services run unchanged outside the Compose environment and in tests.
//
// Trace context is propagated with W3C Trace Context headers, and request IDs
// with the X-Request-ID header.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
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

// Providers are the telemetry providers of a process.
type Providers struct {
	// Tracer records spans. It is a no-op if traces are not exported.
	Tracer trace.TracerProvider
	// Logger exports log records. It is nil if logs are not exported;
	// NewLogger then writes locally only.
	Logger log.LoggerProvider
}

// Setup returns the telemetry providers for the process named serviceName,
// and a function that flushes and stops them. getenv is typically os.Getenv.
//
// Each signal is exported only if TracesEnabled or LogsEnabled reports so.
// Export failures are logged to logger and never affect request handling;
// logger must not itself export (use NewLogger with a nil provider), or a
// failing backend would feed its own errors back into the export queue.
func Setup(ctx context.Context, logger *slog.Logger, serviceName string, getenv func(string) string) (Providers, func(context.Context) error, error) {
	p := Providers{Tracer: noop.NewTracerProvider()}
	var shutdowns []func(context.Context) error
	shutdown := func(ctx context.Context) error {
		var errs []error
		for _, s := range shutdowns {
			errs = append(errs, s(ctx))
		}
		return errors.Join(errs...)
	}
	if !TracesEnabled(getenv) && !LogsEnabled(getenv) {
		return p, shutdown, nil
	}

	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(
		semconv.ServiceName(serviceName),
		semconv.ServiceVersion(version.Version),
	))
	if err != nil {
		return Providers{}, nil, fmt.Errorf("build telemetry resource: %w", err)
	}
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		logger.Warn("telemetry export failed", "error", err)
	}))

	// The exporters read the endpoint and other OTEL_EXPORTER_OTLP_*
	// settings from the environment themselves.
	if TracesEnabled(getenv) {
		exp, err := otlptracehttp.New(ctx)
		if err != nil {
			return Providers{}, nil, fmt.Errorf("create OTLP trace exporter: %w", err)
		}
		tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp), sdktrace.WithResource(res))
		p.Tracer = tp
		shutdowns = append(shutdowns, tp.Shutdown)
	}
	if LogsEnabled(getenv) {
		exp, err := otlploghttp.New(ctx)
		if err != nil {
			_ = shutdown(ctx)
			return Providers{}, nil, fmt.Errorf("create OTLP log exporter: %w", err)
		}
		lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewBatchProcessor(exp)), sdklog.WithResource(res))
		p.Logger = lp
		shutdowns = append(shutdowns, lp.Shutdown)
	}
	return p, shutdown, nil
}

// shutdownTimeout bounds the final flush of buffered spans and log records.
const shutdownTimeout = 5 * time.Second

// Shutdown runs a shutdown function returned by Setup with a bounded timeout.
// It is meant for process exit, when the main context is already cancelled.
func Shutdown(shutdown func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	return shutdown(ctx)
}

// TracesEnabled reports whether Setup would export spans with the given
// environment.
func TracesEnabled(getenv func(string) string) bool {
	return signalEnabled(getenv, "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT")
}

// LogsEnabled reports whether Setup would export log records with the given
// environment.
func LogsEnabled(getenv func(string) string) bool {
	return signalEnabled(getenv, "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT")
}

func signalEnabled(getenv func(string) string, signalEndpoint string) bool {
	if getenv("OTEL_SDK_DISABLED") == "true" {
		return false
	}
	return getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" || getenv(signalEndpoint) != ""
}

// Handler wraps next so every request it serves is recorded as a server span
// named "<method> <route pattern>", continuing any trace propagated by the
// caller. Each request also gets a request ID (the caller's X-Request-ID if
// valid, otherwise a new one), available to handlers through RequestID and
// echoed in the response, and is logged to logger as one access-log line.
// Requests for the paths in skip (e.g. "/metrics" for Prometheus scrapes) are
// passed through without a span or log line.
//
// Handler must be the outermost wrapper: it reads the route pattern set by
// the ServeMux inside next, and next must pass the request it receives to
// that ServeMux unchanged.
func Handler(tp trace.TracerProvider, logger *slog.Logger, next http.Handler, skip ...string) http.Handler {
	skipped := func(r *http.Request) bool {
		for _, p := range skip {
			if r.URL.Path == p {
				return true
			}
		}
		return false
	}
	// otelhttp hands the next handler a copy of the request, and so does
	// assignRequestID. Only accessLog sits between otelhttp and the ServeMux,
	// because it passes the request through unchanged and otelhttp can read
	// the route pattern.
	traced := otelhttp.NewHandler(accessLog(logger, next, skipped), "",
		otelhttp.WithTracerProvider(tp),
		otelhttp.WithPropagators(propagator),
		otelhttp.WithFilter(func(r *http.Request) bool { return !skipped(r) }),
	)
	return assignRequestID(traced, skipped)
}

// Transport wraps next so every outbound request through it is recorded as a
// client span named "<method> <peer>" and carries the trace context and the
// request ID to the peer. A nil next uses http.DefaultTransport.
func Transport(tp trace.TracerProvider, peer string, next http.RoundTripper) http.RoundTripper {
	if next == nil {
		next = http.DefaultTransport
	}
	return otelhttp.NewTransport(requestIDTransport{next: next},
		otelhttp.WithTracerProvider(tp),
		otelhttp.WithPropagators(propagator),
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			return r.Method + " " + peer
		}),
	)
}
