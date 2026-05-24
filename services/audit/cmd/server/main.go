// Audit service — owns the append-only, hash-chained audit log.
//
// Writes happen via Kafka (subscribe to ug.go.guva.audit.entry.appended.v1).
// Reads happen via HTTP (/v1/audit/entries, /v1/audit/verify). No
// synchronous write API: every producer goes through Kafka. This is the
// load-bearing decision that decouples producer availability from audit
// availability.
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

	"github.com/guva-ug/guva-backend/pkg/platform/health"
	"github.com/guva-ug/guva-backend/pkg/platform/observability"
	"github.com/guva-ug/guva-backend/services/audit/internal/config"
	"github.com/guva-ug/guva-backend/services/audit/internal/consumer"
	"github.com/guva-ug/guva-backend/services/audit/internal/server"
	"github.com/guva-ug/guva-backend/services/audit/internal/store"
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

	srv := server.New(cfg, logger, probes, st)

	cons := consumer.New(consumer.Config{
		Brokers: cfg.KafkaBrokers,
		Topic:   cfg.KafkaAuditTopic,
		GroupID: cfg.KafkaConsumerGroup,
	}, logger, st)

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		logger.Info("audit service listening", "addr", cfg.HTTPAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "error", err)
			cancel()
		}
	}()

	go func() {
		defer wg.Done()
		if err := cons.Run(ctx); err != nil {
			logger.Error("kafka consumer exited with error", "error", err)
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
