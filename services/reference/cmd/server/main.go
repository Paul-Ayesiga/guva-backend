// Reference service for the GUVA backend.
//
// Demonstrates the structure that production services are expected to follow:
//
//   - Configuration loaded from environment, validated at startup, no panics
//     past main.
//   - Structured logging via slog.
//   - OpenTelemetry traces exported to the collector at OTEL_EXPORTER_OTLP_ENDPOINT.
//   - Prometheus metrics exposed on /metrics.
//   - Liveness and readiness on /healthz and /readyz, suitable for Kubernetes
//     probes (mirrors §6.6 of the non-functional requirements).
//   - Graceful shutdown on SIGINT / SIGTERM.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/guva-ug/guva-backend/services/reference/internal/config"
	"github.com/guva-ug/guva-backend/services/reference/internal/health"
	"github.com/guva-ug/guva-backend/services/reference/internal/httpserver"
	"github.com/guva-ug/guva-backend/services/reference/internal/observability"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("config load failed", "error", err)
		os.Exit(1)
	}

	logger := observability.NewLogger(cfg.LogLevel)
	slog.SetDefault(logger)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	shutdownTracing, err := observability.InitTracing(ctx, cfg)
	if err != nil {
		logger.Warn("tracing init failed; continuing without traces", "error", err)
		shutdownTracing = func(context.Context) error { return nil }
	}
	defer func() {
		shutdownCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = shutdownTracing(shutdownCtx)
	}()

	probes := health.New()
	probes.MarkReady() // Nothing to wait on in this skeleton.

	srv := httpserver.New(cfg, logger, probes)

	go func() {
		logger.Info("reference service listening", "addr", cfg.HTTPAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "error", err)
			cancel()
		}
	}()

	<-ctx.Done()
	logger.Info("shutdown signal received")

	shutdownCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
	defer c()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "error", err)
		os.Exit(1)
	}
	logger.Info("bye")
}
