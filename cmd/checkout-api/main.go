// Command checkout-api serves the checkout-api demo service.
//
// Configuration (environment variables):
//
//	ADDR               listen address (default ":8080")
//	INVENTORY_URL      base URL of inventory-api (default "http://localhost:8081")
//	INVENTORY_TIMEOUT  timeout per inventory-api call, Go duration (default "2s")
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ipekutku/ai-sre-agent/internal/httpserver"
	"github.com/ipekutku/ai-sre-agent/internal/services/checkout"
	"github.com/ipekutku/ai-sre-agent/internal/version"
)

type config struct {
	addr             string
	inventoryURL     string
	inventoryTimeout time.Duration
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", "checkout-api")

	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		logger.Error("invalid configuration", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	inv := checkout.NewInventoryClient(cfg.inventoryURL, &http.Client{Timeout: cfg.inventoryTimeout})
	logger.Info("starting", "version", version.Version,
		"inventory_url", cfg.inventoryURL, "inventory_timeout", cfg.inventoryTimeout.String())
	if err := httpserver.Run(ctx, logger, cfg.addr, checkout.NewHandler(logger, inv)); err != nil {
		logger.Error("server failed", "error", err)
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
