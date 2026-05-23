package httpserver

import (
	"net/http"

	"github.com/google/uuid"
)

// withCorrelationID echoes any inbound X-Correlation-Id header back to the
// caller and generates one if missing. Mirrors the gateway convention in
// §9.1 so that traces stitched together by the gateway and by services use
// the same identifier.
func withCorrelationID(next http.Handler) http.Handler {
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
