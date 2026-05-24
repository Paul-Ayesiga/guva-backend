// Package server wires the reference service's HTTP routes onto the
// platform-provided server scaffolding.
//
// This is the only service-specific HTTP code; everything reusable
// (correlation-ID middleware, health probes, scope-enforcement
// middleware, problem-details writer) lives in pkg/platform/*.
package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/guva-ug/guva-backend/pkg/platform/auth"
	"github.com/guva-ug/guva-backend/pkg/platform/health"
	"github.com/guva-ug/guva-backend/pkg/platform/httpserver"
	"github.com/guva-ug/guva-backend/services/reference/internal/config"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// New returns the reference service's *http.Server, wired up with
// health probes, Prometheus metrics, and the /ping business endpoint
// (gated by the verify:citizen scope).
func New(cfg config.Config, logger *slog.Logger, probes *health.Probes) *http.Server {
	registry := prometheus.NewRegistry()
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))

	ping := auth.RequireScope("verify:citizen",
		otelhttp.NewHandler(pingHandler(cfg, logger), "GET /ping"))
	mux.Handle("/ping", ping)
	mux.Handle("/v1/ping", ping) // legacy alias

	return httpserver.New(httpserver.Config{Addr: cfg.HTTPAddr}, probes, mux)
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
