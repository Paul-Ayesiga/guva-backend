// Package httpserver builds the HTTP server for the reference service.
//
// Routes:
//
//	GET  /healthz   liveness probe
//	GET  /readyz    readiness probe
//	GET  /metrics   Prometheus metrics
//	GET  /ping      canonical sample endpoint used for end-to-end verification
//	GET  /v1/ping   legacy alias retained for backwards compatibility
package httpserver

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/guva-ug/guva-backend/services/reference/internal/auth"
	"github.com/guva-ug/guva-backend/services/reference/internal/config"
	"github.com/guva-ug/guva-backend/services/reference/internal/health"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// New returns a fully-configured *http.Server.
func New(cfg config.Config, logger *slog.Logger, probes *health.Probes) *http.Server {
	registry := prometheus.NewRegistry()
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	mux := http.NewServeMux()
	mux.Handle("/healthz", liveness())
	mux.Handle("/readyz", readiness(probes))
	mux.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
	// /ping requires verify:citizen scope. The gateway already validates
	// the JWT signature; this check is defence in depth + a way to fail
	// closed if the service is ever reached without going through Kong.
	ping := auth.RequireScope("verify:citizen",
		otelhttp.NewHandler(pingHandler(cfg, logger), "GET /ping"))
	mux.Handle("/ping", ping)
	mux.Handle("/v1/ping", ping)

	return &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           withCorrelationID(mux),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}

func liveness() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"alive"}`))
	})
}

func readiness(p *health.Probes) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !p.IsReady() {
			http.Error(w, `{"status":"not_ready"}`, http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ready"}`))
	})
}

func pingHandler(cfg config.Config, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims, _ := auth.FromContext(r.Context())
		logger.InfoContext(r.Context(), "ping",
			"correlation_id", r.Header.Get("X-Correlation-Id"),
			"caller_subject", claims.Subject,
			"caller_client", claims.ClientID,
		)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"service":     cfg.ServiceName,
			"environment": cfg.Environment,
			"timestamp":   time.Now().UTC().Format(time.RFC3339Nano),
			"caller": map[string]any{
				"client":  claims.ClientID,
				"subject": claims.Subject,
				"scopes":  claims.Scopes(),
				"issuer":  claims.Issuer,
			},
		})
	})
}
