// Package server exposes the /ingest endpoint APISIX's http-logger
// plugin posts to. Every batch of access log entries is transformed
// into CloudEvents envelopes and staged in audit_outbox in the same
// transaction. pkg/platform/audit.Worker drains to Kafka.
//
// Endpoint:
//
//	POST /ingest    body: APISIX http-logger batch (JSON array)
//	GET  /healthz   liveness
//	GET  /readyz    readiness
package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/guva-ug/guva-backend/pkg/platform/audit"
	"github.com/guva-ug/guva-backend/pkg/platform/health"
	"github.com/guva-ug/guva-backend/pkg/platform/httpserver"
	"github.com/guva-ug/guva-backend/pkg/platform/observability"
	"github.com/guva-ug/guva-backend/pkg/platform/problem"
	"github.com/guva-ug/guva-backend/services/apisix-adapter/internal/config"
	"github.com/guva-ug/guva-backend/services/apisix-adapter/internal/store"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AccessLog is the subset of APISIX's http-logger payload we use. The
// plugin emits more fields than this; unknown fields are ignored.
type AccessLog struct {
	StartTime int64   `json:"start_time"` // epoch millis
	ClientIP  string  `json:"client_ip"`
	Latency   float64 `json:"latency"` // total request time, milliseconds
	RouteID   string  `json:"route_id"`
	ServiceID string  `json:"service_id"`
	Upstream  string  `json:"upstream"`

	Request struct {
		Method  string            `json:"method"`
		URI     string            `json:"uri"`
		URL     string            `json:"url"`
		Size    int               `json:"size"`
		Headers map[string]string `json:"headers"`
	} `json:"request"`

	Response struct {
		Status  int               `json:"status"`
		Size    int               `json:"size"`
		Headers map[string]string `json:"headers"`
	} `json:"response"`
}

func New(cfg config.Config, logger *slog.Logger, probes *health.Probes, st *store.Store) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("POST /ingest", ingestHandler(cfg, logger, st.Pool()))

	registry, metricsHandler := observability.NewMetricsRegistry()
	audit.RegisterMetrics(registry)

	return httpserver.New(httpserver.Config{
		Addr:           cfg.HTTPAddr,
		MetricsHandler: metricsHandler,
	}, probes, mux)
}

func ingestHandler(cfg config.Config, logger *slog.Logger, pool *pgxpool.Pool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Shared-secret check: off when SharedSecret is empty (local dev).
		if cfg.SharedSecret != "" {
			if r.Header.Get("X-Adapter-Secret") != cfg.SharedSecret {
				problem.Write(w, http.StatusUnauthorized, "invalid_secret", "X-Adapter-Secret mismatch")
				return
			}
		}

		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 2<<20)) // 2 MB cap
		if err != nil {
			problem.Write(w, http.StatusBadRequest, "body_read_failed", err.Error())
			return
		}
		// APISIX's http-logger plugin can send either an array of entries
		// (default batch mode) or a single object (batch_max_size=1).
		// Handle both shapes.
		var batch []AccessLog
		trimmed := bytesTrimSpace(body)
		switch {
		case len(trimmed) == 0:
			w.WriteHeader(http.StatusNoContent)
			return
		case trimmed[0] == '[':
			if err := json.Unmarshal(body, &batch); err != nil {
				problem.Write(w, http.StatusBadRequest, "invalid_json_array", err.Error())
				return
			}
		case trimmed[0] == '{':
			var one AccessLog
			if err := json.Unmarshal(body, &one); err != nil {
				problem.Write(w, http.StatusBadRequest, "invalid_json_object", err.Error())
				return
			}
			batch = []AccessLog{one}
		default:
			problem.Write(w, http.StatusBadRequest, "invalid_body", "expected JSON object or array")
			return
		}

		emitted, skipped := 0, 0
		for _, entry := range batch {
			if shouldSkip(entry) {
				skipped++
				continue
			}
			if err := emit(r.Context(), pool, entry); err != nil {
				logger.ErrorContext(r.Context(), "ingest emit failed",
					"error", err, "route_id", entry.RouteID,
					"status", entry.Response.Status, "uri", entry.Request.URI)
				// 500 with how-many-succeeded lets APISIX retry the whole
				// batch on its next tick; the consumer dedupes by event UUID
				// so re-sending already-emitted events is harmless.
				problem.Write(w, http.StatusInternalServerError, "emit_failed",
					fmt.Sprintf("emitted %d, then failed at route_id=%q: %s",
						emitted, entry.RouteID, err.Error()))
				return
			}
			emitted++
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"emitted": emitted,
			"skipped": skipped,
		})
	})
}

// shouldSkip filters out access logs that aren't worth chaining.
//   - Liveness/readiness probes from the platform's own healthcheck
//     poll APISIX constantly; we don't want them on the chain.
//   - Requests with no matched route_id are 404s, admin endpoints, or
//     anything else with no upstream — keeping them is just noise.
func shouldSkip(e AccessLog) bool {
	if e.RouteID == "" {
		return true
	}
	uri := e.Request.URI
	if i := strings.IndexByte(uri, '?'); i >= 0 {
		uri = uri[:i]
	}
	switch uri {
	case "/healthz", "/readyz", "/metrics":
		return true
	}
	return false
}

// emit turns one access-log row into one audit event and writes it to
// the outbox inside its own short-lived transaction.
func emit(ctx context.Context, pool *pgxpool.Pool, e AccessLog) error {
	txCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	tx, err := pool.BeginTx(txCtx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(txCtx) }()

	tokenActor, tokenSubject := tokenClaimsFromHeaders(e.Request.Headers)
	corr := headerLookup(e.Request.Headers, "x-correlation-id")
	if corr == "" {
		corr = headerLookup(e.Response.Headers, "x-correlation-id")
	}

	event := audit.Event{
		SourceKind:    "gateway",
		Source:        "apisix",
		Type:          "apisix.request.served",
		SubjectKind:   "route",
		Subject:       e.RouteID,
		Result:        resultFromStatus(e.Response.Status),
		CorrelationID: corr,
		Data: map[string]any{
			"method":        e.Request.Method,
			"uri":           e.Request.URI,
			"status":        e.Response.Status,
			"latency_ms":    e.Latency,
			"req_size":      e.Request.Size,
			"resp_size":     e.Response.Size,
			"client_ip":     e.ClientIP,
			"upstream":      e.Upstream,
			"token_actor":   tokenActor,
			"token_subject": tokenSubject,
			"user_agent":    headerLookup(e.Request.Headers, "user-agent"),
			"start_time_ms": e.StartTime,
		},
	}
	if _, err := audit.Emit(txCtx, tx, event); err != nil {
		return fmt.Errorf("audit.Emit: %w", err)
	}
	if err := tx.Commit(txCtx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// tokenClaimsFromHeaders pulls the JWT azp and sub out of the
// Authorization header without re-verifying the signature. APISIX has
// already verified; we just want to surface "who called" on the chain.
// Returns ("", "") on any parse failure — never blocks emission.
func tokenClaimsFromHeaders(h map[string]string) (azp, sub string) {
	auth := headerLookup(h, "authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return "", ""
	}
	parts := strings.Split(strings.TrimPrefix(auth, "Bearer "), ".")
	if len(parts) != 3 {
		return "", ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", ""
	}
	var c struct {
		AZP string `json:"azp"`
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(payload, &c); err != nil {
		return "", ""
	}
	return c.AZP, c.Sub
}

func resultFromStatus(s int) string {
	switch {
	case s == 0:
		return "inconclusive"
	case s >= 200 && s < 400:
		return "ok"
	case s == 401, s == 403:
		return "denied"
	case s >= 400 && s < 500:
		return "denied" // client errors are denials from the gateway's POV
	default:
		return "error"
	}
}

func headerLookup(h map[string]string, key string) string {
	if h == nil {
		return ""
	}
	// APISIX lowercases header names in the http-logger payload, but
	// be defensive in case versions differ.
	if v, ok := h[key]; ok {
		return v
	}
	if v, ok := h[strings.ToLower(key)]; ok {
		return v
	}
	for k, v := range h {
		if strings.EqualFold(k, key) {
			return v
		}
	}
	return ""
}

// bytesTrimSpace finds the first non-whitespace byte. Stdlib has
// bytes.TrimSpace but that allocates; we just need to look at the
// first character to decide array vs object.
func bytesTrimSpace(b []byte) []byte {
	i := 0
	for i < len(b) {
		switch b[i] {
		case ' ', '\t', '\n', '\r':
			i++
		default:
			return b[i:]
		}
	}
	return nil
}
