// Package server wires the integration service HTTP API. One real
// endpoint (POST /lookup), one health/admin endpoint (GET /backend
// + healthz/readyz). Internal-only — there is intentionally no
// public APISIX route in front of this. Verification reaches it
// directly over the docker network.
package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/guva-ug/guva-backend/pkg/platform/audit"
	"github.com/guva-ug/guva-backend/pkg/platform/auth"
	"github.com/guva-ug/guva-backend/pkg/platform/health"
	"github.com/guva-ug/guva-backend/pkg/platform/httpserver"
	"github.com/guva-ug/guva-backend/pkg/platform/observability"
	"github.com/guva-ug/guva-backend/pkg/platform/problem"
	"github.com/guva-ug/guva-backend/services/integrations/nira/internal/backend"
	"github.com/guva-ug/guva-backend/services/integrations/nira/internal/canonical"
	"github.com/guva-ug/guva-backend/services/integrations/nira/internal/config"
	"github.com/guva-ug/guva-backend/services/integrations/nira/internal/store"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

func New(cfg config.Config, logger *slog.Logger, probes *health.Probes, st *store.Store, b backend.Backend) *http.Server {
	mux := http.NewServeMux()

	// POST /lookup — the only business endpoint. Requires
	// verify:citizen since the caller will use the result to
	// answer a verify request. Future tighter policy: a dedicated
	// integrations:read scope mapped only to the verification
	// service's identity.
	mux.Handle("POST /lookup", auth.RequireScope("verify:citizen",
		otelhttp.NewHandler(lookupHandler(cfg, logger, st, b), "POST /lookup")))

	// GET /backend — operator visibility ("which backend am I running").
	mux.Handle("GET /backend", otelhttp.NewHandler(backendHandler(b), "GET /backend"))

	registry, metricsHandler := observability.NewMetricsRegistry()
	audit.RegisterMetrics(registry)
	return httpserver.New(httpserver.Config{
		Addr:           cfg.HTTPAddr,
		MetricsHandler: metricsHandler,
	}, probes, mux)
}

// LookupRequest is the wire shape. NIN is the only required field;
// CorrelationID gets propagated through to the upstream as a header
// (when supported) and into the audit chain entry.
type LookupRequest struct {
	NIN           string `json:"nin"`
	CorrelationID string `json:"correlation_id,omitempty"`
}

func lookupHandler(cfg config.Config, logger *slog.Logger, st *store.Store, b backend.Backend) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		claims, _ := auth.FromContext(r.Context())
		caller := claims.ClientID
		if caller == "" {
			caller = "unknown"
		}

		var req LookupRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
			problem.Write(w, http.StatusBadRequest, "invalid_json", err.Error())
			return
		}
		req.NIN = strings.ToUpper(strings.TrimSpace(req.NIN))
		if req.NIN == "" {
			problem.Write(w, http.StatusBadRequest, "missing_nin", "nin is required")
			return
		}
		corr := req.CorrelationID
		if corr == "" {
			corr = r.Header.Get("X-Correlation-Id")
		}
		subjectHash := hashSubject(req.NIN)

		rec, found, err := b.Lookup(r.Context(), req.NIN)
		latency := int(time.Since(start).Milliseconds())

		status := classifyOutcome(rec, found, err)
		var upstreamCode *int
		// In a future enhancement the upstream backend returns the
		// last HTTP status alongside the error; we leave it nil here
		// since the simulator never has one.

		lookupID, lerr := st.LogLookup(r.Context(), store.LookupEntry{
			Backend:            b.Name(),
			Caller:             caller,
			SubjectType:        "nin",
			SubjectHash:        subjectHash,
			Status:             status,
			UpstreamStatusCode: upstreamCode,
			LatencyMS:          latency,
			CorrelationID:      corr,
		})
		if lerr != nil {
			logger.WarnContext(r.Context(), "lookup_log insert failed", "error", lerr)
		}

		emitErr := emitAudit(r.Context(), logger, st, audit.Event{
			SourceKind:    "service",
			Source:        "integrations-nira",
			Type:          "nira.lookup.requested",
			SubjectKind:   "citizen",
			Subject:       subjectHash,
			Result:        auditResult(status),
			CorrelationID: corr,
			Data: map[string]any{
				"lookup_id":  lookupID,
				"caller":     caller,
				"backend":    b.Name(),
				"status":     status,
				"latency_ms": latency,
			},
		})
		if emitErr != nil {
			logger.WarnContext(r.Context(), "audit emit failed (lookup recorded, chain entry will be missing)", "error", emitErr)
		}

		// Map the backend error to an HTTP response. The simulator
		// never returns errors other than ctx cancellation; the
		// upstream backend uses our two sentinels.
		switch {
		case err == nil:
			// found OR not-found; write canonical response
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(canonical.LookupResponse{
				LookupID: lookupID,
				Found:    found,
				Record:   rec, // zero if !found
			})
		case errors.Is(err, backend.ErrCircuitOpen):
			problem.Write(w, http.StatusServiceUnavailable, "circuit_open",
				"NIRA upstream circuit breaker is currently open; retry after the breaker window")
		case errors.Is(err, backend.ErrUpstreamUnavailable):
			problem.Write(w, http.StatusBadGateway, "upstream_unavailable",
				"NIRA upstream is unavailable after retries")
		default:
			logger.ErrorContext(r.Context(), "nira backend error", "error", err)
			problem.Write(w, http.StatusInternalServerError, "internal_error", err.Error())
		}
	})
}

func backendHandler(b backend.Backend) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"backend": b.Name(),
		})
	})
}

// classifyOutcome translates the (rec, found, err) tuple into the
// status string we put in lookup_log + the audit chain detail.
func classifyOutcome(rec canonical.Record, found bool, err error) string {
	switch {
	case errors.Is(err, backend.ErrCircuitOpen):
		return "circuit_open"
	case errors.Is(err, backend.ErrUpstreamUnavailable):
		return "upstream_error"
	case err != nil:
		return "error"
	case !found:
		return "not_found"
	}
	switch rec.Status {
	case canonical.StatusDeceased:
		return "deceased"
	case canonical.StatusRevoked:
		return "revoked"
	default:
		return "found"
	}
}

// auditResult maps the lookup status onto the audit envelope's
// result vocabulary. found / deceased / revoked → ok (we did find
// a record, the outcome category is in `data.status`); not_found →
// denied; upstream / circuit / error → error.
func auditResult(status string) string {
	switch status {
	case "found", "deceased", "revoked":
		return "ok"
	case "not_found":
		return "denied"
	default:
		return "error"
	}
}

func emitAudit(ctx interface{ Done() <-chan struct{} }, logger *slog.Logger, st *store.Store, e audit.Event) error {
	// Cast back to context.Context for our DB pool's BeginTx signature.
	c, _ := ctx.(interface {
		Done() <-chan struct{}
		Err() error
		Deadline() (time.Time, bool)
		Value(any) any
	})
	_ = c
	// Local short-lived tx; same pattern as other services.
	cctx, cancel := withTimeout(ctx)
	defer cancel()
	tx, err := st.Pool().BeginTx(cctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(cctx) }()
	if _, err := audit.Emit(cctx, tx, e); err != nil {
		return err
	}
	return tx.Commit(cctx)
}

// withTimeout returns the context cast back to context.Context with
// a 3s deadline. The handler always passes r.Context() which is a
// context.Context, but the local helper signature was simplified.
func withTimeout(ctx interface{ Done() <-chan struct{} }) (cContext, cancelFn) {
	return contextWithTimeout(ctx, 3*time.Second)
}

// hashSubject reuses the platform's canonical recipe: SHA-256 of
// the trimmed upper-cased identifier. Same as services/verification
// and services/consent so cross-service joins via the hash work.
func hashSubject(rawNIN string) string {
	sum := sha256.Sum256([]byte(rawNIN))
	return hex.EncodeToString(sum[:])
}
