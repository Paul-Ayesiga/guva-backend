// Identity service — owns the platform's scope catalogue and the
// consumer-registration audit trail.
//
// In production this service is the first thing every other service
// integrates with: it tells consumers what scopes exist, and it tracks
// who's registered to use the platform. The Keycloak realm is the
// source of truth for credentials; identity sits beside it tracking
// the lifecycle metadata (who onboarded when, by whom).
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
	"github.com/guva-ug/guva-backend/services/identity/internal/config"
	"github.com/guva-ug/guva-backend/services/identity/internal/keycloakadmin"
	"github.com/guva-ug/guva-backend/services/identity/internal/server"
	"github.com/guva-ug/guva-backend/services/identity/internal/store"
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

	// Resolve DB password from Vault. The Vault path is shared with
	// the Postgres bootstrap; if we ever rotate the password we update
	// Vault and restart this service rather than juggling .env files.
	vault, err := secrets.NewClient(secrets.Config{Addr: cfg.VaultAddr, Token: cfg.VaultToken})
	if err != nil {
		logger.Error("vault client init failed", "error", err)
		os.Exit(1)
	}
	fetchCtx, cancelFetch := context.WithTimeout(ctx, 10*time.Second)
	dbPassword, err := vault.GetString(fetchCtx, "services/identity/config", "db-password")
	cancelFetch()
	if err != nil {
		// Fall back to env var for local dev where Vault may not have
		// been seeded with the db password key.
		dbPassword = os.Getenv("POSTGRES_PASSWORD")
		if dbPassword == "" {
			dbPassword = "guva"
		}
		logger.Warn("vault fetch failed for db-password, falling back to env",
			"path", "services/identity/config", "key", "db-password",
			"fallback_source", "POSTGRES_PASSWORD env (or default 'guva')")
	}

	dbCtx, dbCancel := context.WithTimeout(ctx, 15*time.Second)
	st, err := store.Open(dbCtx, cfg.DSN(dbPassword))
	dbCancel()
	if err != nil {
		logger.Error("db connect failed", "error", err, "host", cfg.DBHost, "db", cfg.DBName)
		os.Exit(1)
	}
	defer st.Close()
	logger.Info("db connected", "host", cfg.DBHost, "db", cfg.DBName)

	// Resolve Keycloak admin credentials from Vault. These let identity
	// create/manage clients in the guva realm via Keycloak's Admin REST
	// API. Fall back to env vars only if Vault is unseeded (dev mode).
	kcAdminUser, _ := vault.GetString(ctx, "services/identity/config", "keycloak-admin-username")
	if kcAdminUser == "" {
		kcAdminUser = envOr("KEYCLOAK_ADMIN", "admin")
	}
	kcAdminPass, err := vault.GetString(ctx, "services/identity/config", "keycloak-admin-password")
	if err != nil {
		kcAdminPass = envOr("KEYCLOAK_ADMIN_PASSWORD", "admin")
		logger.Warn("vault fetch failed for keycloak-admin-password, falling back to env",
			"fallback_source", "KEYCLOAK_ADMIN_PASSWORD env (or default 'admin')")
	}
	kcAdmin, err := keycloakadmin.NewClient(keycloakadmin.Config{
		BaseURL:       cfg.KeycloakBackendURL,
		Realm:         cfg.KeycloakRealm,
		AdminUser:     kcAdminUser,
		AdminPassword: kcAdminPass,
	})
	if err != nil {
		logger.Error("keycloakadmin client init failed", "error", err)
		os.Exit(1)
	}
	logger.Info("keycloak admin client ready", "backend_url", cfg.KeycloakBackendURL, "realm", cfg.KeycloakRealm)

	probes := health.New()
	probes.MarkReady()

	srv := server.New(cfg, logger, probes, st, kcAdmin, vault)

	go func() {
		logger.Info("identity service listening", "addr", cfg.HTTPAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "error", err)
			cancel()
		}
	}()

	<-ctx.Done()
	logger.Info("shutdown signal received")
	probes.MarkNotReady()

	shutdownCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
	defer c()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "error", err)
		os.Exit(1)
	}
	logger.Info("bye")
}

func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}
