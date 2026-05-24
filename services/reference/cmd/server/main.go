// Reference service for the GUVA backend.
//
// Wires the shared platform library (pkg/platform/*) onto the service's
// own routes (internal/server). See pkg/platform for the reusable parts:
// auth, observability, httpserver, health, problem.
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

	"github.com/guva-ug/guva-backend/pkg/platform/health"
	"github.com/guva-ug/guva-backend/pkg/platform/observability"
	"github.com/guva-ug/guva-backend/pkg/secrets"
	"github.com/guva-ug/guva-backend/services/reference/internal/config"
	"github.com/guva-ug/guva-backend/services/reference/internal/server"
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

	shutdownTracing, err := observability.InitTracing(ctx, observability.TracingConfig{
		ServiceName:  cfg.ServiceName,
		Namespace:    "guva",
		Environment:  cfg.Environment,
		OTLPEndpoint: cfg.OTLPEndpoint,
	})
	if err != nil {
		logger.Warn("tracing init failed; continuing without traces", "error", err)
	}
	defer func() {
		shutdownCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = shutdownTracing(shutdownCtx)
	}()

	// Fetch the greeting from Vault. Demonstrates the pattern every
	// future service uses: env tells us where Vault is and which token
	// to use; Vault answers what's actually in our config. Service is
	// "not ready" until the secret resolves.
	vault, err := secrets.NewClient(secrets.Config{Addr: cfg.VaultAddr, Token: cfg.VaultToken})
	if err != nil {
		logger.Error("vault client init failed", "error", err)
		os.Exit(1)
	}
	fetchCtx, cancelFetch := context.WithTimeout(ctx, 10*time.Second)
	greeting, err := vault.GetString(fetchCtx, "services/reference/config", "greeting")
	cancelFetch()
	if err != nil {
		logger.Error("vault fetch failed", "path", "services/reference/config", "key", "greeting", "error", err)
		os.Exit(1)
	}
	logger.Info("loaded service config from vault", "greeting_chars", len(greeting))

	probes := health.New()
	probes.MarkReady() // Secrets resolved; nothing else to wait on in this skeleton.

	srv := server.New(cfg, server.ServiceConfig{Greeting: greeting}, logger, probes)

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
