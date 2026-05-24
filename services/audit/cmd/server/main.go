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

	"github.com/guva-ug/guva-backend/pkg/platform/audit"
	"github.com/guva-ug/guva-backend/pkg/platform/health"
	"github.com/guva-ug/guva-backend/pkg/platform/observability"
	"github.com/guva-ug/guva-backend/pkg/secrets"
	"github.com/guva-ug/guva-backend/services/audit/internal/anchor"
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

	// Two roles, one connection pool each. Vault holds the dev passwords;
	// fall back to the well-known dev values when Vault is unseeded so
	// `go run` against a bare Postgres still works.
	vault, err := secrets.NewClient(secrets.Config{Addr: cfg.VaultAddr, Token: cfg.VaultToken})
	if err != nil {
		logger.Error("vault client init failed", "error", err)
		os.Exit(1)
	}
	signingKey, err := audit.LoadOrCreateSigner(ctx, audit.SignerConfig{
		Vault:     vault,
		VaultPath: "services/audit/config",
		VaultKey:  "signing-key-b64",
		Logger:    logger,
	})
	if err != nil {
		logger.Error("audit signing key load failed", "error", err)
		os.Exit(1)
	}
	vaultCtx, vaultCancel := context.WithTimeout(ctx, 10*time.Second)
	writerPwd, err := vault.GetString(vaultCtx, "services/audit/config", "db-writer-password")
	if err != nil || writerPwd == "" {
		writerPwd = envOr("AUDIT_DB_WRITER_PASSWORD", "audit-writer-dev")
		logger.Warn("vault fetch failed for db-writer-password; using fallback",
			"fallback_source", "AUDIT_DB_WRITER_PASSWORD env (or default)")
	}
	readerPwd, err := vault.GetString(vaultCtx, "services/audit/config", "db-reader-password")
	vaultCancel()
	if err != nil || readerPwd == "" {
		readerPwd = envOr("AUDIT_DB_READER_PASSWORD", "audit-reader-dev")
		logger.Warn("vault fetch failed for db-reader-password; using fallback",
			"fallback_source", "AUDIT_DB_READER_PASSWORD env (or default)")
	}

	dbCtx, dbCancel := context.WithTimeout(ctx, 15*time.Second)
	st, err := store.Open(dbCtx,
		cfg.DSNFor(cfg.DBUserReader, readerPwd),
		cfg.DSNFor(cfg.DBUserWriter, writerPwd),
	)
	dbCancel()
	if err != nil {
		logger.Error("db connect failed", "error", err,
			"host", cfg.DBHost, "db", cfg.DBName,
			"reader_user", cfg.DBUserReader, "writer_user", cfg.DBUserWriter)
		os.Exit(1)
	}
	defer st.Close()
	logger.Info("db connected (two-role)",
		"host", cfg.DBHost, "db", cfg.DBName,
		"reader_user", cfg.DBUserReader, "writer_user", cfg.DBUserWriter)

	probes := health.New()
	probes.MarkReady()

	srv := server.New(cfg, logger, probes, st, signingKey)

	cons := consumer.New(consumer.Config{
		Brokers: cfg.KafkaBrokers,
		Topic:   cfg.KafkaAuditTopic,
		GroupID: cfg.KafkaConsumerGroup,
	}, logger, st)

	// Audit envelope validator — same wiring as every other producer.
	// The audit service emits its own meta-audit events; those events
	// must also conform to the registered schema.
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

	// Meta-audit drain — the audit service emits an event every time
	// its read API is called (see internal/server). Those rows land in
	// the local audit_outbox; this worker publishes them to Kafka where
	// the consumer above turns them into chain entries. Read traffic
	// against the audit log thus becomes part of the audit log.
	metaWorker := audit.NewWorker(audit.WorkerConfig{
		DB:           st.Writer(),
		Logger:       logger,
		KafkaBrokers: cfg.KafkaBrokers,
		KafkaTopic:   cfg.KafkaAuditTopic,
		Producer:     "audit",
	})

	// Periodic anchor job — every cfg.AnchorInterval, computes the
	// Merkle root over (last_anchored+1 .. max(entry_id)) and stages a
	// new audit_anchors row. If AUDIT_ANCHOR_WITNESS_URL is set, the
	// anchor is POSTed there for external corroboration.
	anchorJob := anchor.New(anchor.Config{
		Store:      st,
		Logger:     logger,
		Interval:   cfg.AnchorInterval,
		WitnessURL: cfg.AnchorWitnessURL,
	})

	var wg sync.WaitGroup
	wg.Add(4)

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

	go func() {
		defer wg.Done()
		if err := metaWorker.Run(ctx); err != nil {
			logger.Error("meta-audit worker exited with error", "error", err)
		}
	}()

	go func() {
		defer wg.Done()
		if err := anchorJob.Run(ctx); err != nil {
			logger.Error("anchor job exited with error", "error", err)
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
