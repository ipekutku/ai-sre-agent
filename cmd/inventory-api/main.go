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
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ipekutku/ai-sre-agent/internal/httpmetrics"
	"github.com/ipekutku/ai-sre-agent/internal/httpserver"
	"github.com/ipekutku/ai-sre-agent/internal/services/inventory"
	"github.com/ipekutku/ai-sre-agent/internal/version"
)

type config struct {
	addr        string
	adminAddr   string
	baseLatency time.Duration
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", "inventory-api")

	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		logger.Error("invalid configuration", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger.Info("starting", "version", version.Version, "base_latency", cfg.baseLatency.String())
	if err := run(ctx, logger, cfg); err != nil {
		logger.Error("server failed", "error", err)
		os.Exit(1)
	}
}

// run serves the service and admin APIs until ctx is cancelled or either
// server fails, in which case both are shut down.
func run(ctx context.Context, logger *slog.Logger, cfg config) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	latency := inventory.NewLatency(cfg.baseLatency)
	service := httpmetrics.Handler(httpmetrics.NewRegistry(), inventory.NewHandler(latency))
	adminLogger := logger.With("component", "fault-admin")
	admin := inventory.NewAdminHandler(adminLogger, latency)

	errc := make(chan error, 2)
	go func() { errc <- httpserver.Run(ctx, logger, cfg.addr, service) }()
	go func() { errc <- httpserver.Run(ctx, adminLogger, cfg.adminAddr, admin) }()

	first := <-errc
	cancel()
	return errors.Join(first, <-errc)
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
