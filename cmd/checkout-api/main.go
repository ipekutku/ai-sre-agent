// Command checkout-api serves the checkout-api demo service.
//
// Configuration (environment variables):
//
//	ADDR               listen address (default ":8080")
//	INVENTORY_URL      base URL of inventory-api (default "http://localhost:8081")
//	INVENTORY_TIMEOUT  timeout per inventory-api call, Go duration (default "2s")
//
// Logs are written to stdout as JSON. Trace and log export are enabled by the
// standard OTEL_EXPORTER_OTLP_*ENDPOINT variables (see internal/telemetry).
package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ipekutku/ai-sre-agent/internal/httpmetrics"
	"github.com/ipekutku/ai-sre-agent/internal/httpserver"
	"github.com/ipekutku/ai-sre-agent/internal/services/checkout"
	"github.com/ipekutku/ai-sre-agent/internal/telemetry"
	"github.com/ipekutku/ai-sre-agent/internal/version"
)

type config struct {
	addr             string
	inventoryURL     string
	inventoryTimeout time.Duration
}

const serviceName = "checkout-api"

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
	logger := telemetry.NewLogger(os.Stdout, serviceName, providers.Logger)

	reg := httpmetrics.NewRegistry()
	inv := checkout.NewInventoryClient(cfg.inventoryURL, &http.Client{
		Timeout: cfg.inventoryTimeout,
		Transport: telemetry.Transport(providers.Tracer, "inventory-api",
			httpmetrics.NewClient(reg).Transport("inventory-api", nil)),
	})
	handler := telemetry.Handler(providers.Tracer, logger,
		httpmetrics.Handler(reg, checkout.NewHandler(logger, inv)), "/metrics")

	logger.Info("starting", "version", version.Version,
		"inventory_url", cfg.inventoryURL, "inventory_timeout", cfg.inventoryTimeout.String(),
		"trace_export", telemetry.TracesEnabled(os.Getenv), "log_export", telemetry.LogsEnabled(os.Getenv))
	err = httpserver.Run(ctx, logger, cfg.addr, handler)
	if serr := telemetry.Shutdown(shutdownTelemetry); serr != nil {
		localLogger.Warn("telemetry shutdown failed", "error", serr)
	}
	if err != nil {
		localLogger.Error("server failed", "error", err)
		os.Exit(1)
	}
}

func loadConfig(getenv func(string) string) (config, error) {
	cfg := config{
		addr:             ":8080",
		inventoryURL:     "http://localhost:8081",
		inventoryTimeout: 2 * time.Second,
	}
	if v := getenv("ADDR"); v != "" {
		cfg.addr = v
	}
	if v := getenv("INVENTORY_URL"); v != "" {
		cfg.inventoryURL = v
	}
	if u, err := url.Parse(cfg.inventoryURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return config{}, fmt.Errorf("INVENTORY_URL must be an absolute http(s) URL, got %q", cfg.inventoryURL)
	}
	if v := getenv("INVENTORY_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return config{}, fmt.Errorf("INVENTORY_TIMEOUT must be a positive duration, got %q", v)
		}
		cfg.inventoryTimeout = d
	}
	return cfg, nil
}
