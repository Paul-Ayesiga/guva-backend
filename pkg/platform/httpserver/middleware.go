// Package httpserver provides reusable HTTP middleware and server-builder
// helpers for GUVA backend services.
package httpserver

import (
	"net/http"

	"github.com/google/uuid"
)

// WithCorrelationID echoes any inbound X-Correlation-Id header back to
// the caller and generates one if missing. The gateway (APISIX with the
// request-id plugin, see deploy/compose/apisix/apisix.yaml) already
// emits this header for traffic that goes through it; this middleware
// handles the case of direct calls (local development, tests).
func WithCorrelationID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cid := r.Header.Get("X-Correlation-Id")
		if cid == "" {
			cid = uuid.NewString()
			r.Header.Set("X-Correlation-Id", cid)
		}
		w.Header().Set("X-Correlation-Id", cid)
		next.ServeHTTP(w, r)
	})
}
