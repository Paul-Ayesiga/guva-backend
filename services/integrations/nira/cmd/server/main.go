// NIRA integration service — the agency-side translator. Speaks
// two backends:
//
//   - simulator: 5-record in-memory canned set, for dev + the
//     platform demo. The default.
//   - upstream:  production HTTP client with mTLS + retries +
//     circuit breaker, against the real NIRA endpoint.
//
// Flip via NIRA_BACKEND={simulator|upstream}. The upstream backend
// additionally requires NIRA_UPSTREAM_URL + the three mTLS cert
// paths; absent any of them, startup fails with a clear message.
//
// This service is intentionally NOT exposed via the public APISIX
// gateway. Only the verification service (and any future internal
// caller — e.g. an admin replay tool) talks to it directly over
// the docker network.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/guva-ug/guva-backend/pkg/platform/audit"
	"github.com/guva-ug/guva-backend/pkg/platform/health"
	"github.com/guva-ug/guva-backend/pkg/platform/httpserver"
	"github.com/guva-ug/guva-backend/pkg/platform/observability"
	"github.com/guva-ug/guva-backend/pkg/secrets"
	"github.com/guva-ug/guva-backend/services/integrations/nira/internal/backend"
	"github.com/guva-ug/guva-backend/services/integrations/nira/internal/config"
	"github.com/guva-ug/guva-backend/services/integrations/nira/internal/server"
	"github.com/guva-ug/guva-backend/services/integrations/nira/internal/store"
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
		ServiceName: cfg.ServiceName, Namespace: "guva",
		Environment: cfg.Environment, OTLPEndpoint: cfg.OTLPEndpoint,
	})
	if err != nil {
		logger.Warn("tracing init failed; continuing without traces", "error", err)
	}
	defer func() {
		c, cn := context.WithTimeout(context.Background(), 5*time.Second)
		defer cn()
		_ = shutdownTracing(c)
	}()

	// Vault → DB password (fallback to env / default for dev).
	vault, err := secrets.NewClient(secrets.Config{Addr: cfg.VaultAddr, Token: cfg.VaultToken})
	if err != nil {
		logger.Error("vault client init failed", "error", err)
		os.Exit(1)
	}
	vc, vcancel := context.WithTimeout(ctx, 10*time.Second)
	dbPassword, err := vault.GetString(vc, "services/integrations-nira/config", "db-password")
	vcancel()
	if err != nil || dbPassword == "" {
		dbPassword = envOr("POSTGRES_PASSWORD", "guva")
		logger.Warn("vault fetch failed for db-password; using fallback",
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

	// Audit envelope validator — same wiring as every other producer.
	if v, err := audit.NewValidator(ctx, audit.ValidatorConfig{
		RegistryURL: cfg.ApicurioURL,
		Group:       "guva-audit",
		ArtifactID:  "audit-event-envelope",
		Logger:      logger,
	}); err != nil {
		logger.Error("audit validator init failed", "error", err)
		os.Exit(1)
	} else {
		audit.SetDefaultValidator(v)
		logger.Info("audit envelope validator ready",
			"source", v.Source(), "schema_sha256", v.Digest())
	}

	// Backend selection.
	var b backend.Backend
	switch cfg.Backend {
	case "upstream":
		up, err := backend.NewUpstream(backend.UpstreamConfig{
			BaseURL:           cfg.UpstreamBaseURL,
			Cert:              cfg.UpstreamCertFile,
			Key:               cfg.UpstreamKeyFile,
			CA:                cfg.UpstreamCAFile,
			Timeout:           cfg.UpstreamTimeout,
			MaxAttempts:       cfg.UpstreamRetries,
			BackoffBase:       cfg.UpstreamBackoff,
			CircuitThreshold:  cfg.CircuitThreshold,
			CircuitOpenWindow: cfg.CircuitOpenWindow,
		}, logger)
		if err != nil {
			logger.Error("upstream backend init failed", "error", err)
			os.Exit(1)
		}
		b = up
		logger.Info("NIRA backend: upstream",
			"base_url", cfg.UpstreamBaseURL,
			"timeout", cfg.UpstreamTimeout,
			"max_attempts", cfg.UpstreamRetries,
			"circuit_threshold", cfg.CircuitThreshold)
	default:
		b = backend.NewSimulator()
		logger.Info("NIRA backend: simulator", "test_records", 5,
			"hint", "set NIRA_BACKEND=upstream + NIRA_UPSTREAM_* when the agency agreement is ready")
	}

	probes := health.New()
	probes.MarkReady()
	srv := server.New(cfg, logger, probes, st, b)

	auditWorker := audit.NewWorker(audit.WorkerConfig{
		DB:           st.Pool(),
		Logger:       logger,
		KafkaBrokers: cfg.KafkaBrokers,
		KafkaTopic:   cfg.KafkaAuditTopic,
		Producer:     "integrations-nira",
	})

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		logger.Info("integrations-nira listening", "addr", cfg.HTTPAddr)
		if err := httpserver.ListenAndServeAny(srv); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "error", err)
			cancel()
		}
	}()
	go func() {
		defer wg.Done()
		if err := auditWorker.Run(ctx); err != nil {
			logger.Error("audit worker exited with error", "error", err)
		}
	}()

	<-ctx.Done()
	logger.Info("shutdown signal received")
	probes.MarkNotReady()
	c, cn := context.WithTimeout(context.Background(), 10*time.Second)
	defer cn()
	if err := srv.Shutdown(c); err != nil {
		logger.Error("graceful shutdown failed", "error", err)
	}
	wg.Wait()
	logger.Info("bye")
}

func envOr(k, fb string) string {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		return v
	}
	return fb
}
