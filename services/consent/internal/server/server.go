// Package server wires the consent service HTTP API.
//
//	POST /grants                       create a new grant (consent:write)
//	GET  /grants/{id}                  read a grant (consent:read)
//	POST /grants/{id}/revoke           revoke a grant (consent:write)
//	GET  /grants/{id}/verify           inside-platform validity check (verify:citizen)
//
// Every mutation lands an audit event (consent.granted / consent.revoked).
// Every /verify call also emits (consent.verified), so the chain
// records who checked which grant when — useful for both forensics
// and consumer-facing usage reports.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/guva-ug/guva-backend/pkg/platform/audit"
	"github.com/guva-ug/guva-backend/pkg/platform/auth"
	"github.com/guva-ug/guva-backend/pkg/platform/health"
	"github.com/guva-ug/guva-backend/pkg/platform/httpserver"
	"github.com/guva-ug/guva-backend/pkg/platform/observability"
	"github.com/guva-ug/guva-backend/pkg/platform/problem"
	"github.com/guva-ug/guva-backend/services/consent/internal/config"
	"github.com/guva-ug/guva-backend/services/consent/internal/signing"
	"github.com/guva-ug/guva-backend/services/consent/internal/store"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

func New(cfg config.Config, logger *slog.Logger, probes *health.Probes, st *store.Store, signer *signing.Signer) *http.Server {
	mux := http.NewServeMux()

	mux.Handle("POST /grants", auth.RequireScope("consent:write",
		otelhttp.NewHandler(createGrantHandler(logger, st, signer), "POST /grants")))
	mux.Handle("GET /grants", auth.RequireAnyScope([]string{"consent:read", "consent:write"},
		otelhttp.NewHandler(listGrantsHandler(logger, st), "GET /grants")))
	mux.Handle("GET /grants/{id}", auth.RequireAnyScope([]string{"consent:read", "consent:write"},
		otelhttp.NewHandler(getGrantHandler(logger, st), "GET /grants/{id}")))
	mux.Handle("POST /grants/{id}/revoke", auth.RequireScope("consent:write",
		otelhttp.NewHandler(revokeGrantHandler(logger, st), "POST /grants/{id}/revoke")))
	mux.Handle("GET /grants/{id}/verify", auth.RequireAnyScope([]string{"verify:citizen", "consent:read"},
		otelhttp.NewHandler(verifyGrantHandler(logger, st, signer), "GET /grants/{id}/verify")))

	// Public-key endpoint — for external verifiers who want to
	// validate assertion JWTs without contacting the service for
	// every grant. Same shape as audit's /export/pubkey.
	mux.Handle("GET /signing-key", otelhttp.NewHandler(pubkeyHandler(signer), "GET /signing-key"))

	registry, metricsHandler := observability.NewMetricsRegistry()
	audit.RegisterMetrics(registry)
	return httpserver.New(httpserver.Config{
		Addr:           cfg.HTTPAddr,
		MetricsHandler: metricsHandler,
	}, probes, mux)
}

type createGrantReq struct {
	CitizenNIN        string   `json:"citizen_nin"` // raw NIN; hashed before storage
	ConsumerID        string   `json:"consumer_id"`
	Upstream          string   `json:"upstream"`
	Purpose           string   `json:"purpose"`
	AllowedAttributes []string `json:"allowed_attributes"`
	TTL               string   `json:"ttl"` // ISO 8601 duration shortcut, e.g. "720h"
}

func createGrantHandler(logger *slog.Logger, st *store.Store, signer *signing.Signer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in createGrantReq
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in); err != nil {
			problem.Write(w, http.StatusBadRequest, "invalid_json", err.Error())
			return
		}
		in.CitizenNIN = strings.ToUpper(strings.TrimSpace(in.CitizenNIN))
		if in.CitizenNIN == "" || in.ConsumerID == "" || in.Upstream == "" || len(in.AllowedAttributes) == 0 {
			problem.Write(w, http.StatusBadRequest, "invalid_request",
				"citizen_nin, consumer_id, upstream and allowed_attributes are all required")
			return
		}
		ttl, err := time.ParseDuration(in.TTL)
		if err != nil || ttl <= 0 || ttl > 365*24*time.Hour {
			problem.Write(w, http.StatusBadRequest, "invalid_ttl",
				"ttl must parse as a Go duration (e.g. 24h, 720h) and be <= 8760h (1 year)")
			return
		}
		subjectHash := store.HashSubject(in.CitizenNIN)
		expires := time.Now().UTC().Add(ttl)

		grant := store.Grant{
			CitizenSubjectType: "nin",
			CitizenSubjectHash: subjectHash,
			ConsumerID:         in.ConsumerID,
			Upstream:           in.Upstream,
			Purpose:            in.Purpose,
			AllowedAttributes:  in.AllowedAttributes,
			ExpiresAt:          expires,
			SigningKeyID:       signer.KeyID(),
		}

		// We need the grant id before we can sign — but the assertion
		// embeds the id. Strategy: insert with a placeholder JWT, then
		// build + sign the real one using the returned id, then UPDATE.
		// Simpler alternative: sign over the rest of the row, with the
		// id discovered server-side. Since the assertion is intended
		// for external verification of "the platform issued this grant
		// with these terms", embedding the id is what makes the
		// assertion bind to one specific record. We use a transaction
		// + the trigger's allowed-mutation exception: assertion_jwt is
		// locked after the first INSERT, so we must build it inside
		// the same INSERT. We do that by computing the id client-side
		// (uuid.New) and using it in both the row and the assertion.
		//
		// The trigger allows ANY id on INSERT; only UPDATE is locked.

		assertion := signing.Assertion{
			Issuer:             "guva-consent",
			IssuedAt:           time.Now().UTC().Unix(),
			ExpiresAt:          expires.Unix(),
			CitizenSubjectType: grant.CitizenSubjectType,
			CitizenSubjectHash: grant.CitizenSubjectHash,
			ConsumerID:         grant.ConsumerID,
			Upstream:           grant.Upstream,
			Purpose:            grant.Purpose,
			AllowedAttributes:  grant.AllowedAttributes,
		}
		// Build the row with a fresh id so we can include it in the
		// assertion BEFORE the INSERT.
		grant.ID = generateGrantID()
		assertion.GrantID = grant.ID
		jwt, err := signer.Sign(assertion)
		if err != nil {
			logger.ErrorContext(r.Context(), "sign assertion failed", "error", err)
			problem.Write(w, http.StatusInternalServerError, "sign_failed", err.Error())
			return
		}
		grant.AssertionJWT = jwt

		out, err := st.CreateGrantWithID(r.Context(), grant)
		if err != nil {
			logger.ErrorContext(r.Context(), "create grant failed", "error", err)
			problem.Write(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}

		// Audit emission. Subject = citizen hash, source = consent
		// service. data carries grant id + consumer + purpose; never
		// raw NIN.
		_ = emit(r.Context(), logger, st, audit.Event{
			SourceKind:    "service",
			Source:        "consent",
			Type:          "consent.granted",
			SubjectKind:   "citizen",
			Subject:       subjectHash,
			Result:        "ok",
			CorrelationID: r.Header.Get("X-Correlation-Id"),
			Data: map[string]any{
				"grant_id":           out.ID,
				"consumer_id":        out.ConsumerID,
				"upstream":           out.Upstream,
				"purpose":            out.Purpose,
				"allowed_attributes": out.AllowedAttributes,
				"ttl_seconds":        int(out.ExpiresAt.Sub(out.GrantedAt).Seconds()),
				"signing_key_id":     out.SigningKeyID,
			},
		})

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(out)
	})
}

// listGrantsHandler returns the grants for a specific citizen
// (?citizen_subject_hash=<sha256 hex>). Used by the citizen portal's
// dashboard to render the citizen's own grant inventory. Mandatory
// query parameter — there's no "list all grants" path (that would
// be a privacy footgun across consumers).
func listGrantsHandler(logger *slog.Logger, st *store.Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hash := strings.TrimSpace(r.URL.Query().Get("citizen_subject_hash"))
		if hash == "" {
			problem.Write(w, http.StatusBadRequest, "missing_citizen",
				"citizen_subject_hash query parameter is required")
			return
		}
		limit := 100
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				limit = n
			}
		}
		grants, err := st.ListGrantsForCitizen(r.Context(), hash, limit)
		if err != nil {
			logger.ErrorContext(r.Context(), "list grants failed", "error", err)
			problem.Write(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}
		// Strip the signed assertion from the list view — it's stored
		// alongside the row but only returned on the per-grant GET to
		// keep the list payload lean.
		out := make([]map[string]any, 0, len(grants))
		for _, g := range grants {
			out = append(out, map[string]any{
				"id":                 g.ID,
				"consumer_id":        g.ConsumerID,
				"upstream":           g.Upstream,
				"purpose":            g.Purpose,
				"allowed_attributes": g.AllowedAttributes,
				"granted_at":         g.GrantedAt,
				"expires_at":         g.ExpiresAt,
				"revoked_at":         g.RevokedAt,
				"revocation_reason":  g.RevocationReason,
				"signing_key_id":     g.SigningKeyID,
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"grants": out,
			"count":  len(out),
		})
	})
}

func getGrantHandler(logger *slog.Logger, st *store.Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g, err := st.GetGrant(r.Context(), r.PathValue("id"))
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				problem.Write(w, http.StatusNotFound, "grant_not_found", "no consent grant with that id")
				return
			}
			problem.Write(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(g)
	})
}

type revokeReq struct {
	Reason string `json:"reason"`
}

func revokeGrantHandler(logger *slog.Logger, st *store.Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in revokeReq
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&in)
		g, err := st.RevokeGrant(r.Context(), r.PathValue("id"), in.Reason)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				problem.Write(w, http.StatusNotFound, "grant_not_found", "no consent grant with that id")
				return
			}
			problem.Write(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}
		_ = emit(r.Context(), logger, st, audit.Event{
			SourceKind:    "service",
			Source:        "consent",
			Type:          "consent.revoked",
			SubjectKind:   "citizen",
			Subject:       g.CitizenSubjectHash,
			Result:        "ok",
			CorrelationID: r.Header.Get("X-Correlation-Id"),
			Data: map[string]any{
				"grant_id":          g.ID,
				"consumer_id":       g.ConsumerID,
				"revocation_reason": g.RevocationReason,
			},
		})
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(g)
	})
}

// verifyGrantHandler is the internal "is this consent still valid for
// this consumer for these attributes" check the verification service
// hits before calling NIRA. Returns the parsed grant + a status:
//   - granted      : grant exists, not expired, not revoked, consumer matches, attributes covered
//   - expired      : grant exists but expires_at is in the past
//   - revoked      : grant exists but revoked_at is set
//   - consumer_mismatch : grant exists but consumer_id is different
//   - attribute_not_allowed : the requested attribute set isn't a subset of allowed_attributes
//   - not_found    : no such grant
//
// Query params: consumer_id (required), attributes (comma-separated, required)
func verifyGrantHandler(logger *slog.Logger, st *store.Store, signer *signing.Signer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		consumerID := r.URL.Query().Get("consumer_id")
		attrsCSV := r.URL.Query().Get("attributes")
		if consumerID == "" {
			problem.Write(w, http.StatusBadRequest, "missing_consumer_id", "consumer_id query parameter is required")
			return
		}
		requested := splitCSV(attrsCSV)

		g, err := st.GetGrant(r.Context(), id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeVerify(w, "not_found", nil, "")
				_ = emit(r.Context(), logger, st, audit.Event{
					SourceKind: "service", Source: "consent", Type: "consent.verified",
					SubjectKind: "consent_grant", Subject: id, Result: "denied",
					CorrelationID: r.Header.Get("X-Correlation-Id"),
					Data: map[string]any{
						"grant_id": id, "consumer_id": consumerID,
						"requested_attributes": requested, "outcome": "not_found",
					},
				})
				return
			}
			problem.Write(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}

		status := evaluate(g, consumerID, requested)
		assertion := ""
		if status == "granted" {
			assertion = g.AssertionJWT
		}
		writeVerify(w, status, &g, assertion)
		_ = emit(r.Context(), logger, st, audit.Event{
			SourceKind: "service", Source: "consent", Type: "consent.verified",
			SubjectKind: "consent_grant", Subject: g.ID, Result: result(status),
			CorrelationID: r.Header.Get("X-Correlation-Id"),
			Data: map[string]any{
				"grant_id": g.ID, "consumer_id": consumerID,
				"requested_attributes": requested, "outcome": status,
				"signing_key_id": g.SigningKeyID,
			},
		})
	})
}

func evaluate(g store.Grant, consumerID string, requested []string) string {
	if g.RevokedAt != nil {
		return "revoked"
	}
	if time.Now().UTC().After(g.ExpiresAt) {
		return "expired"
	}
	if g.ConsumerID != consumerID {
		return "consumer_mismatch"
	}
	if !subset(requested, g.AllowedAttributes) {
		return "attribute_not_allowed"
	}
	return "granted"
}

func subset(want, allowed []string) bool {
	if len(want) == 0 {
		return true
	}
	for _, a := range allowed {
		if a == "*" {
			return true
		}
	}
	have := make(map[string]struct{}, len(allowed))
	for _, a := range allowed {
		have[a] = struct{}{}
	}
	for _, w := range want {
		if _, ok := have[w]; !ok {
			return false
		}
	}
	return true
}

type verifyResponse struct {
	Status       string       `json:"status"`
	Grant        *store.Grant `json:"grant,omitempty"`
	AssertionJWT string       `json:"assertion_jwt,omitempty"`
}

func writeVerify(w http.ResponseWriter, status string, g *store.Grant, assertion string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(verifyResponse{Status: status, Grant: g, AssertionJWT: assertion})
}

func pubkeyHandler(signer *signing.Signer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pub := signer.PublicKey()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"algorithm":      "Ed25519",
			"public_key_b64": signing.EncodePublicKey(pub),
			"key_id":         signer.KeyID(),
		})
	})
}

// emit writes one audit event in its own short tx. Best-effort by
// design — the caller has already done the user-visible work; an
// audit-tx failure shouldn't fail the response. Logged loudly so
// operators see drift.
func emit(ctx context.Context, logger *slog.Logger, st *store.Store, e audit.Event) error {
	c, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	tx, err := st.Pool().BeginTx(c, pgx.TxOptions{})
	if err != nil {
		logger.WarnContext(ctx, "consent audit begin failed", "error", err)
		return err
	}
	defer func() { _ = tx.Rollback(c) }()
	if _, err := audit.Emit(c, tx, e); err != nil {
		logger.WarnContext(ctx, "consent audit emit failed", "error", err, "type", e.Type)
		return err
	}
	if err := tx.Commit(c); err != nil {
		logger.WarnContext(ctx, "consent audit commit failed", "error", err)
		return err
	}
	return nil
}

// result maps a verify-outcome string onto the audit result enum.
func result(status string) string {
	switch status {
	case "granted":
		return "ok"
	default:
		return "denied"
	}
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(s, ",") {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// generateGrantID returns a fresh UUID for a new grant. Lives here
// rather than calling uuid.NewString inline so tests can swap if they
// need deterministic ids later.
func generateGrantID() string {
	return newUUID()
}
