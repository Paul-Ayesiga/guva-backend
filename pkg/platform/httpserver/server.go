package httpserver

import (
	"net/http"
	"time"

	"github.com/guva-ug/guva-backend/pkg/platform/health"
)

// Config captures the sensible-defaults inputs for New.
type Config struct {
	Addr string // e.g. ":7070"
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
	if businessRoutes != nil {
		mux.Handle("/", businessRoutes)
	}

	return &http.Server{
		Addr:              cfg.Addr,
		Handler:           WithCorrelationID(mux),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
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
