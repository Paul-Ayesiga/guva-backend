// Verification service — answers consent-scoped "does the citizen
// match this claim?" questions for consumers, against upstream
// systems of record (NIRA today, URSB / URA / Lands / UNEB / MoH
// adding one adapter at a time).
//
// Single goroutine model today: HTTP server + audit drain worker.
// Adding a periodic cache pruner is a small follow-up.
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
	"github.com/guva-ug/guva-backend/services/verification/internal/config"
	"github.com/guva-ug/guva-backend/services/verification/internal/nira"
	"github.com/guva-ug/guva-backend/services/verification/internal/server"
	"github.com/guva-ug/guva-backend/services/verification/internal/store"
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
		c, cn := context.WithTimeout(context.Background(), 5*time.Second)
		defer cn()
		_ = shutdownTracing(c)
	}()

	// DB password from Vault (fall back to env / default for dev).
	vault, err := secrets.NewClient(secrets.Config{Addr: cfg.VaultAddr, Token: cfg.VaultToken})
	if err != nil {
		logger.Error("vault client init failed", "error", err)
		os.Exit(1)
	}
	vc, vcancel := context.WithTimeout(ctx, 10*time.Second)
	dbPassword, err := vault.GetString(vc, "services/verification/config", "db-password")
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

	// Audit envelope validator (same wiring as every other producer).
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

	// NIRA adapter selection.
	//   mock        — in-process canned data; zero-dependency dev + unit tests.
	//   integration — HTTP client against services/integrations/nira;
	//                 production-shaped (the integration service handles
	//                 mTLS, retries, circuit breaker, audit, lookup_log).
	//   live        — reserved; verification calling NIRA directly bypasses
	//                 the integration service which is the wrong boundary.
	//                 Use integration + NIRA_BACKEND=upstream instead.
	var n nira.Adapter
	switch cfg.NIRAMode {
	case "integration":
		base := envOr("NIRA_INTEGRATION_URL", "http://localhost:7080")
		tokenURL := envOr("NIRA_TOKEN_URL",
			"https://auth.guva.localhost/realms/"+envOr("CONSENT_REALM", "guva")+"/protocol/openid-connect/token")
		clientID := envOr("NIRA_CLIENT_ID", envOr("CONSENT_CLIENT_ID", "guva-reference"))
		clientSecret := envOr("NIRA_CLIENT_SECRET", envOr("CONSENT_CLIENT_SECRET", "reference-dev-secret"))
		tf := newTokenFetcher(tokenURL, clientID, clientSecret, logger)
		n = nira.NewHTTPClient(base, tf.Token)
		logger.Info("NIRA adapter: integration", "base_url", base, "client_id", clientID)
	case "live":
		logger.Error("live NIRA adapter (direct from verification) not supported; use NIRA_MODE=integration + integration-service NIRA_BACKEND=upstream")
		os.Exit(1)
	default:
		n = nira.NewMock()
		logger.Info("NIRA adapter: mock", "test_records", 5,
			"hint", "set NIRA_MODE=integration + NIRA_INTEGRATION_URL when services/integrations/nira is up")
	}

	// Consent client — when CONSENT_BASE_URL is set, every verify call
	// carrying a `consent_reference` is checked against the consent
	// service before reaching NIRA. Absent base URL → nil client →
	// verification continues to work but skips consent (dev parity
	// with the pre-consent slice).
	var consentChecker server.ConsentChecker
	if base := os.Getenv("CONSENT_BASE_URL"); base != "" {
		// Re-use this service's verify:citizen token (own client-cred
		// flow) as the bearer when calling consent. In production a
		// dedicated service identity would carry both verify:citizen
		// and consent:read. For dev we reuse the guva-reference creds
		// from env or fall back to platform defaults.
		clientID := envOr("CONSENT_CLIENT_ID", "guva-reference")
		clientSecret := envOr("CONSENT_CLIENT_SECRET", "reference-dev-secret")
		realm := envOr("CONSENT_REALM", "guva")
		tokenURL := envOr("CONSENT_TOKEN_URL",
			"http://keycloak:8080/realms/"+realm+"/protocol/openid-connect/token")
		consentChecker = newConsentChecker(base, tokenURL, clientID, clientSecret, logger)
		logger.Info("consent checker wired",
			"base_url", base, "client_id", clientID)
	} else {
		logger.Info("consent checker not wired; verify calls will skip consent check (set CONSENT_BASE_URL to enable)")
	}

	probes := health.New()
	probes.MarkReady()
	srv := server.New(cfg, logger, probes, st, n, consentChecker)

	// Audit drain worker.
	auditWorker := audit.NewWorker(audit.WorkerConfig{
		DB:           st.Pool(),
		Logger:       logger,
		KafkaBrokers: cfg.KafkaBrokers,
		KafkaTopic:   cfg.KafkaAuditTopic,
		Producer:     "verification",
	})

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		logger.Info("verification service listening", "addr", cfg.HTTPAddr)
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

func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}
