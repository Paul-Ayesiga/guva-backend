package httpserver

import (
	"crypto/tls"
	"net/http"
	"time"

	"github.com/guva-ug/guva-backend/pkg/platform/health"
)

// Config captures the sensible-defaults inputs for New.
type Config struct {
	Addr string // e.g. ":7070"
	// MetricsHandler, if non-nil, is mounted at GET /metrics. Build one
	// with observability.NewMetricsRegistry. Services that already mount
	// /metrics on their own mux can leave this nil to avoid the conflict.
	MetricsHandler http.Handler
	// TLS, if non-nil, makes the returned server serve HTTPS with mTLS
	// — the peer (e.g. APISIX) must present a client cert signed by
	// the bundle's CA, and unauthenticated direct curl access fails.
	// Build with tlsbundle.Load(...).ServerConfig(). Leave nil for
	// plain HTTP (the dev default; APISIX is the only "client" today
	// and the gateway boundary is the trust boundary).
	TLS *tls.Config
}

// New returns an *http.Server with platform-standard timeouts and a
// minimal route set:
//
//	GET /healthz   — always 200 (liveness)
//	GET /readyz    — 200 / 503 depending on the Probes argument
//
// Services attach their own routes by extending the returned mux via
// the Mux field on the wrapper struct, or by composing handlers and
// passing them to NewWithHandler.
//
// The handler is wrapped in WithCorrelationID; services do not need to
// re-wrap unless they want to skip it (don't).
func New(cfg Config, probes *health.Probes, businessRoutes http.Handler) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/healthz", Liveness())
	mux.Handle("/readyz", Readiness(probes))
	if cfg.MetricsHandler != nil {
		mux.Handle("/metrics", cfg.MetricsHandler)
	}
	if businessRoutes != nil {
		mux.Handle("/", businessRoutes)
	}

	return &http.Server{
		Addr:              cfg.Addr,
		Handler:           WithCorrelationID(mux),
		TLSConfig:         cfg.TLS,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}

// ListenAndServeAny picks between ListenAndServe and ListenAndServeTLS
// based on whether the server has a TLSConfig. Convenience for service
// mains so they don't have to branch:
//
//	if err := httpserver.ListenAndServeAny(srv); err != nil && ...
//
// Internally, ListenAndServeTLS reads the cert/key from the TLSConfig
// directly when called with empty string arguments.
func ListenAndServeAny(srv *http.Server) error {
	if srv.TLSConfig != nil {
		return srv.ListenAndServeTLS("", "")
	}
	return srv.ListenAndServe()
}

// Liveness returns a handler that always responds 200 with a minimal
// JSON body. The fact that it answers proves the process is up.
func Liveness() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"alive"}`))
	})
}

// Readiness returns a handler tied to the given Probes. 200 when ready,
// 503 otherwise. Services flip readiness on after they finish their
// startup checks (DB migrated, secrets fetched, etc.).
func Readiness(p *health.Probes) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !p.IsReady() {
			http.Error(w, `{"status":"not_ready"}`, http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ready"}`))
	})
}
