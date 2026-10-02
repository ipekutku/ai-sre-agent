// Command inventory-api serves the inventory-api demo service.
//
// Configuration (environment variables):
//
//	ADDR  listen address (default ":8081")
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/ipekutku/ai-sre-agent/internal/httpserver"
	"github.com/ipekutku/ai-sre-agent/internal/services/inventory"
	"github.com/ipekutku/ai-sre-agent/internal/version"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", "inventory-api")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	addr := envOr("ADDR", ":8081")
	logger.Info("starting", "version", version.Version)
	if err := httpserver.Run(ctx, logger, addr, inventory.NewHandler()); err != nil {
		logger.Error("server failed", "error", err)
		os.Exit(1)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
