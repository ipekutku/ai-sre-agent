// Command inventory-api serves the inventory-api demo service.
//
// It runs two listeners: the service API (instrumented, scraped by
// Prometheus) and a separate fault-injection admin API used by scenarios.
//
// Configuration (environment variables):
//
//	ADDR          service listen address (default ":8081")
//	ADMIN_ADDR    fault-injection admin listen address (default "127.0.0.1:9081")
//	BASE_LATENCY  normal response delay for GET /inventory, Go duration (default "20ms")
//
// Logs are written to stdout as JSON. Trace and log export of the service API
// are enabled by the standard OTEL_EXPORTER_OTLP_*ENDPOINT variables (see
// internal/telemetry). The admin API is never traced and its logs are never
// exported.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/trace"

	"github.com/ipekutku/ai-sre-agent/internal/httpmetrics"
	"github.com/ipekutku/ai-sre-agent/internal/httpserver"
	"github.com/ipekutku/ai-sre-agent/internal/services/inventory"
	"github.com/ipekutku/ai-sre-agent/internal/telemetry"
	"github.com/ipekutku/ai-sre-agent/internal/version"
)

type config struct {
	addr        string
	adminAddr   string
	baseLatency time.Duration
}

const serviceName = "inventory-api"

func main() {
	// localLogger never exports, so it can report telemetry failures.
	localLogger := telemetry.NewLogger(os.Stdout, serviceName, nil)

	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		localLogger.Error("invalid configuration", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	providers, shutdownTelemetry, err := telemetry.Setup(ctx, localLogger, serviceName, os.Getenv)
	if err != nil {
		localLogger.Error("telemetry setup failed", "error", err)
		os.Exit(1)
	}

	logger, adminLogger := loggers(os.Stdout, providers.Logger)
	logger.Info("starting", "version", version.Version, "base_latency", cfg.baseLatency.String(),
		"trace_export", telemetry.TracesEnabled(os.Getenv), "log_export", telemetry.LogsEnabled(os.Getenv))
	err = run(ctx, logger, adminLogger, cfg, providers.Tracer)
	if serr := telemetry.Shutdown(shutdownTelemetry); serr != nil {
		localLogger.Warn("telemetry shutdown failed", "error", serr)
	}
	if err != nil {
		localLogger.Error("server failed", "error", err)
		os.Exit(1)
	}
}

// loggers returns the service logger, which also exports through lp, and the
// fault-admin logger, which writes to w only. Fault changes are ground truth,
// so they must never reach the log backend the agent can query (ADR 0001).
func loggers(w io.Writer, lp log.LoggerProvider) (service, admin *slog.Logger) {
	service = telemetry.NewLogger(w, serviceName, lp)
	admin = telemetry.NewLogger(w, serviceName, nil).With("component", "fault-admin")
	return service, admin
}

// run serves the service and admin APIs until ctx is cancelled or either
// server fails, in which case both are shut down.
func run(ctx context.Context, logger, adminLogger *slog.Logger, cfg config, tp trace.TracerProvider) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	service, admin := handlers(logger, adminLogger, cfg, tp)

	errc := make(chan error, 2)
	go func() { errc <- httpserver.Run(ctx, logger, cfg.addr, service) }()
	go func() { errc <- httpserver.Run(ctx, adminLogger, cfg.adminAddr, admin) }()

	first := <-errc
	cancel()
	return errors.Join(first, <-errc)
}

// handlers returns the service API and the fault-injection admin API. The
// admin API is neither instrumented nor traced, so fault state never appears
// in metrics or traces (ADR 0001).
func handlers(logger, adminLogger *slog.Logger, cfg config, tp trace.TracerProvider) (service, admin http.Handler) {
	faults := inventory.NewFaults(cfg.baseLatency)
	service = telemetry.Handler(tp, logger,
		httpmetrics.Handler(httpmetrics.NewRegistry(), inventory.NewHandler(logger, faults)), "/metrics")
	admin = inventory.NewAdminHandler(adminLogger, faults)
	return service, admin
}

func loadConfig(getenv func(string) string) (config, error) {
	cfg := config{
		addr:        ":8081",
		adminAddr:   "127.0.0.1:9081",
		baseLatency: 20 * time.Millisecond,
	}
	if v := getenv("ADDR"); v != "" {
		cfg.addr = v
	}
	if v := getenv("ADMIN_ADDR"); v != "" {
		cfg.adminAddr = v
	}
	if v := getenv("BASE_LATENCY"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 || d > inventory.MaxFaultLatency {
			return config{}, fmt.Errorf("BASE_LATENCY must be a duration in [0, %s], got %q", inventory.MaxFaultLatency, v)
		}
		cfg.baseLatency = d
	}
	if cfg.addr == cfg.adminAddr {
		return config{}, fmt.Errorf("ADDR and ADMIN_ADDR must differ, both are %q", cfg.addr)
	}
	return cfg, nil
}
