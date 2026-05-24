// APISIX-adapter — turns APISIX access logs into audit chain entries.
//
// APISIX's http-logger plugin POSTs batches of access-log JSON to
// /ingest. The adapter transforms each entry into a CloudEvents
// envelope and stages it in audit_outbox; pkg/platform/audit.Worker
// drains to Kafka; the audit service hash-chains it like any other
// producer's events.
//
// The whole point: every gateway request becomes audit chain coverage,
// without modifying any of the upstream services.
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
	"github.com/guva-ug/guva-backend/pkg/platform/observability"
	"github.com/guva-ug/guva-backend/services/apisix-adapter/internal/config"
	"github.com/guva-ug/guva-backend/services/apisix-adapter/internal/server"
	"github.com/guva-ug/guva-backend/services/apisix-adapter/internal/store"
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

	dbPassword := envOr("POSTGRES_PASSWORD", "guva")
	dbCtx, dbCancel := context.WithTimeout(ctx, 15*time.Second)
	st, err := store.Open(dbCtx, cfg.DSN(dbPassword))
	dbCancel()
	if err != nil {
		logger.Error("db connect failed", "error", err, "host", cfg.DBHost, "db", cfg.DBName)
		os.Exit(1)
	}
	defer st.Close()
	logger.Info("db connected", "host", cfg.DBHost, "db", cfg.DBName)

	probes := health.New()
	probes.MarkReady()

	// Audit envelope validator — pulls schema from Apicurio with
	// embedded fallback. Every event the adapter stages goes through it.
	if validator, err := audit.NewValidator(ctx, audit.ValidatorConfig{
		RegistryURL: cfg.ApicurioURL,
		Group:       "guva-audit",
		ArtifactID:  "audit-event-envelope",
		Logger:      logger,
	}); err != nil {
		logger.Error("audit validator init failed", "error", err)
		os.Exit(1)
	} else {
		audit.SetDefaultValidator(validator)
		logger.Info("audit envelope validator ready",
			"source", validator.Source(), "schema_sha256", validator.Digest())
	}

	srv := server.New(cfg, logger, probes, st)

	worker := audit.NewWorker(audit.WorkerConfig{
		DB:           st.Pool(),
		Logger:       logger,
		KafkaBrokers: cfg.KafkaBrokers,
		KafkaTopic:   cfg.KafkaAuditTopic,
		Producer:     "apisix-adapter",
	})

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		logger.Info("apisix-adapter listening", "addr", cfg.HTTPAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "error", err)
			cancel()
		}
	}()

	go func() {
		defer wg.Done()
		if err := worker.Run(ctx); err != nil {
			logger.Error("audit worker exited with error", "error", err)
		}
	}()

	<-ctx.Done()
	logger.Info("shutdown signal received")
	probes.MarkNotReady()

	shutdownCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
	defer c()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "error", err)
	}
	wg.Wait()
	logger.Info("bye")
}

func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}
