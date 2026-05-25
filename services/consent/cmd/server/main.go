// Consent service — citizen-authorisation records the verification
// path consults before calling agency upstreams. Three goroutines:
// HTTP server, audit drain worker, (background) signer-key
// fingerprint logger.
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
	"github.com/guva-ug/guva-backend/services/consent/internal/config"
	"github.com/guva-ug/guva-backend/services/consent/internal/server"
	"github.com/guva-ug/guva-backend/services/consent/internal/signing"
	"github.com/guva-ug/guva-backend/services/consent/internal/store"
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

	vault, err := secrets.NewClient(secrets.Config{Addr: cfg.VaultAddr, Token: cfg.VaultToken})
	if err != nil {
		logger.Error("vault client init failed", "error", err)
		os.Exit(1)
	}

	// DB password from Vault (fallback to dev default).
	vc, vcancel := context.WithTimeout(ctx, 10*time.Second)
	dbPassword, err := vault.GetString(vc, "services/consent/config", "db-password")
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

	// Signing key — same load-or-generate-and-write-back pattern as
	// audit (pkg/platform/audit.LoadOrCreateSigner is audit-flavoured
	// so we duplicate the few lines here rather than couple packages).
	skCtx, skCancel := context.WithTimeout(ctx, 5*time.Second)
	rawKey, err := vault.GetString(skCtx, "services/consent/config", "signing-key-b64")
	skCancel()
	var priv []byte
	if err == nil && rawKey != "" {
		k, perr := signing.ParsePrivateKey(rawKey)
		if perr != nil {
			logger.Warn("consent signing key in vault unparseable; regenerating", "error", perr)
		} else {
			priv = k
		}
	}
	if priv == nil {
		generated, gerr := signing.Generate()
		if gerr != nil {
			logger.Error("generate signing key failed", "error", gerr)
			os.Exit(1)
		}
		priv = generated
		c, cn := context.WithTimeout(ctx, 5*time.Second)
		defer cn()
		if perr := vault.Put(c, "services/consent/config", map[string]string{
			"signing-key-b64": signing.EncodePrivateKey(generated),
		}); perr != nil {
			logger.Error("could not persist signing key to vault; using in-memory key only", "error", perr)
		} else {
			logger.Warn("consent signing key auto-generated and stashed in vault — dev only",
				"action", "for production, seed this key out-of-band before first start")
		}
	}
	signer := signing.NewSigner(priv)
	logger.Info("consent signing key ready", "key_id", signer.KeyID())

	// Audit envelope validator.
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

	probes := health.New()
	probes.MarkReady()
	srv := server.New(cfg, logger, probes, st, signer)

	auditWorker := audit.NewWorker(audit.WorkerConfig{
		DB:           st.Pool(),
		Logger:       logger,
		KafkaBrokers: cfg.KafkaBrokers,
		KafkaTopic:   cfg.KafkaAuditTopic,
		Producer:     "consent",
	})

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		logger.Info("consent service listening", "addr", cfg.HTTPAddr)
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
