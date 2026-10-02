// Package httpserver runs the HTTP servers used by the demo services with
// consistent timeouts and graceful shutdown.
package httpserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"
)

const (
	readHeaderTimeout = 5 * time.Second
	shutdownTimeout   = 10 * time.Second
)

// Run serves handler on addr until ctx is cancelled, then shuts down
// gracefully. It returns nil after a clean shutdown.
func Run(ctx context.Context, logger *slog.Logger, addr string, handler http.Handler) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}
	return Serve(ctx, logger, ln, handler)
}

// Serve is like Run but uses an existing listener.
func Serve(ctx context.Context, logger *slog.Logger, ln net.Listener, handler http.Handler) error {
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}

	errc := make(chan error, 1)
	go func() {
		logger.Info("http server listening", "addr", ln.Addr().String())
		errc <- srv.Serve(ln)
	}()

	select {
	case err := <-errc:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}

	logger.Info("http server shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}
