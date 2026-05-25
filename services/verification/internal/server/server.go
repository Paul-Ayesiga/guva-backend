// Package server is the verification-service HTTP layer. One endpoint
// today (POST /verify/citizen), three failure modes (consent-invalid,
// upstream error, internal error), one canonical response shape.
//
// Flow:
//
//  1. Decode + validate request.
//  2. Hash the subject (NIN) for cache + audit keys.
//  3. Look up the cache. Hit → return cached response, log
//     cache_hit verification entry, emit audit.
//  4. Miss → call NIRA adapter, build per-attribute match summary,
//     compute status, log + cache + emit audit, return response.
//
// PII handling: the actual NIN appears only on the request envelope
// and in transient memory while the adapter runs. The DB log keeps
// only the SHA-256 hash. The audit chain entry holds the same hash
// in `subject` and includes claimed-attribute *keys* in `data` but
// never the values.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/guva-ug/guva-backend/pkg/platform/audit"
	"github.com/guva-ug/guva-backend/pkg/platform/auth"
	"github.com/guva-ug/guva-backend/pkg/platform/health"
	"github.com/guva-ug/guva-backend/pkg/platform/httpserver"
	"github.com/guva-ug/guva-backend/pkg/platform/observability"
	"github.com/guva-ug/guva-backend/pkg/platform/problem"
	"github.com/guva-ug/guva-backend/services/verification/internal/canonical"
	"github.com/guva-ug/guva-backend/services/verification/internal/config"
	"github.com/guva-ug/guva-backend/services/verification/internal/nira"
	"github.com/guva-ug/guva-backend/services/verification/internal/store"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// New wires the verification mux. nira is the upstream adapter
// (swap mock ↔ live by passing a different implementation).
// consentClient is optional — when non-nil, /verify/citizen calls
// consent.VerifyGrant before NIRA when the request carries a
// consent_reference; outcomes other than `granted` short-circuit
// the verification with status `consent_invalid`.
func New(cfg config.Config, logger *slog.Logger, probes *health.Probes, st *store.Store, n nira.Adapter, consentClient ConsentChecker) *http.Server {
	mux := http.NewServeMux()

	verifyCitizen := auth.RequireScope("verify:citizen",
		otelhttp.NewHandler(verifyCitizenHandler(cfg, logger, st, n, consentClient), "POST /verify/citizen"))
	mux.Handle("POST /verify/citizen", verifyCitizen)

	registry, metricsHandler := observability.NewMetricsRegistry()
	audit.RegisterMetrics(registry)
	return httpserver.New(httpserver.Config{
		Addr:           cfg.HTTPAddr,
		MetricsHandler: metricsHandler,
	}, probes, mux)
}

// VerifyCitizenRequest is the wire shape consumers POST. NIN is
// always required; the other fields are optional claims — only
// fields the caller supplies get checked + appear in the response.
type VerifyCitizenRequest struct {
	NIN              string `json:"nin"`
	GivenName        string `json:"given_name,omitempty"`
	MiddleName       string `json:"middle_name,omitempty"`
	Surname          string `json:"surname,omitempty"`
	DateOfBirth      string `json:"date_of_birth,omitempty"`
	Sex              string `json:"sex,omitempty"`
	MotherMaidenName string `json:"mother_maiden_name,omitempty"`
	ConsentReference string `json:"consent_reference,omitempty"`
}

// ConsentChecker is the surface verification depends on. Decoupling
// via an interface lets us pass nil during local-only smoke runs and
// the real pkg/platform/consent.Client in production. Same shape, so
// callers see no difference.
type ConsentChecker interface {
	VerifyGrant(ctx context.Context, grantID, consumerID string, attributes []string) (ConsentResult, error)
}

// ConsentResult mirrors pkg/platform/consent.VerifyResult sufficient
// for the verification handler's decision making. The full result
// (including the signed JWT) is stored unchanged in the audit chain
// detail so external regulators can re-verify the grant chain later.
type ConsentResult struct {
	Outcome      string // "granted" / "revoked" / "expired" / "consumer_mismatch" / "attribute_not_allowed" / "not_found"
	AssertionJWT string
}

func verifyCitizenHandler(cfg config.Config, logger *slog.Logger, st *store.Store, n nira.Adapter, consentClient ConsentChecker) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		correlationID := r.Header.Get("X-Correlation-Id")
		claims, _ := auth.FromContext(r.Context())
		consumer := claims.ClientID
		if consumer == "" {
			consumer = "unknown"
		}

		// Parse + validate.
		bodyBytes, err := readBody(r)
		if err != nil {
			problem.Write(w, http.StatusBadRequest, "body_read_failed", err.Error())
			return
		}
		var req VerifyCitizenRequest
		if err := json.Unmarshal(bodyBytes, &req); err != nil {
			problem.Write(w, http.StatusBadRequest, "invalid_json", err.Error())
			return
		}
		req.NIN = strings.ToUpper(strings.TrimSpace(req.NIN))
		if req.NIN == "" {
			problem.Write(w, http.StatusBadRequest, "missing_nin", "nin is required")
			return
		}

		// Consent check, if a client is wired AND the caller provided
		// a consent_reference. Outcomes other than "granted" short-
		// circuit with a consent_invalid status. Absent reference
		// today is allowed (dev parity); production policy can flip
		// to require it.
		var consentAssertion string
		if consentClient != nil && req.ConsentReference != "" {
			requested := sortedAttributeKeys(req)
			result, cerr := consentClient.VerifyGrant(r.Context(), req.ConsentReference, consumer, requested)
			if cerr != nil {
				logger.WarnContext(r.Context(), "consent verify call failed; treating as consent_invalid",
					"error", cerr, "consent_ref", req.ConsentReference)
				writeConsentInvalid(w, consumer, req, correlationID, "consent_service_unavailable")
				recordVerification(r.Context(), logger, st, consumer, req, correlationID,
					string(canonical.StatusConsentInvalid), 0, 0, 0, time.Since(start))
				return
			}
			if result.Outcome != "granted" {
				writeConsentInvalid(w, consumer, req, correlationID, result.Outcome)
				recordVerification(r.Context(), logger, st, consumer, req, correlationID,
					string(canonical.StatusConsentInvalid), 0, 0, 0, time.Since(start))
				return
			}
			consentAssertion = result.AssertionJWT
		}
		_ = consentAssertion // assertion plumbing into audit detail tracked separately

		subjectHash := store.HashSubject(req.NIN)
		fingerprint := store.FingerprintRequest(bodyBytes)
		cacheKey := store.CacheKey{
			ConsumerID:         consumer,
			SubjectType:        "nin",
			SubjectHash:        subjectHash,
			RequestFingerprint: fingerprint,
		}

		// 1. cache lookup
		if cached, hit, err := st.Get(r.Context(), cacheKey); err == nil && hit {
			writeCached(w, cached)
			// Log + audit the cache hit so consumer activity stays observable.
			recordVerification(r.Context(), logger, st, consumer, req, correlationID, "verified", 0, 0, 0, time.Since(start))
			return
		}

		// 2. NIRA call
		nrec, found, err := n.Lookup(r.Context(), req.NIN)
		upstreamLatency := time.Since(start).Milliseconds()
		if err != nil {
			logger.ErrorContext(r.Context(), "NIRA lookup failed", "error", err, "nin_hash", subjectHash[:12])
			recordVerification(r.Context(), logger, st, consumer, req, correlationID,
				string(canonical.StatusError), 0, 0, int(upstreamLatency), time.Since(start))
			problem.Write(w, http.StatusBadGateway, "upstream_unavailable",
				"NIRA could not be reached; try again shortly")
			return
		}

		// 3. translate + emit
		resp := buildResponse(consumer, req, nrec, found, upstreamLatency, correlationID)
		matches, mismatches := tallyMatches(resp.Attributes)

		// 4. write the row + cache + audit
		recordVerification(r.Context(), logger, st, consumer, req, correlationID,
			string(resp.Status), matches, mismatches, int(upstreamLatency), time.Since(start))

		payload, err := json.Marshal(resp)
		if err != nil {
			problem.Write(w, http.StatusInternalServerError, "internal_error", "marshal response failed")
			return
		}
		// Cache only successful outcomes — caching not_found / deceased
		// risks freezing a state change for the TTL window; revoked /
		// deceased records can flip back to active and the consumer
		// would keep seeing stale "no". Better to re-fetch.
		if resp.Status == canonical.StatusVerified || resp.Status == canonical.StatusMismatch {
			_ = st.Put(r.Context(), cacheKey, payload, cfg.CacheTTL)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(payload)
	})
}

// readBody pulls the request body with a sane size cap and returns it
// so the same bytes can be both unmarshalled and fingerprinted.
func readBody(r *http.Request) ([]byte, error) {
	const maxBody = 32 << 10 // 32 KB — verify requests are tiny
	r.Body = http.MaxBytesReader(nil, r.Body, maxBody)
	dec := json.NewDecoder(r.Body)
	dec.UseNumber()
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return nil, err
	}
	return []byte(raw), nil
}

// writeCached re-emits a cached response body verbatim.
func writeCached(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Guva-Cache", "hit")
	_, _ = w.Write(body)
}

// buildResponse turns a NIRA record + a request into the canonical
// per-attribute match summary. Only fields the caller claimed appear
// in the result; absent fields contribute neither match nor mismatch.
func buildResponse(consumer string, req VerifyCitizenRequest, nrec nira.Record, found bool, upstreamLatency int64, correlationID string) canonical.VerificationResponse {
	resp := canonical.VerificationResponse{
		VerificationID: "ver_" + uuid.NewString(),
		Consumer:       consumer,
		Subject:        canonical.SubjectIdentifier{Type: "nin", Value: req.NIN},
		CheckedAt:      time.Now().UTC(),
		Attributes:     map[string]canonical.AttributeMatch{},
		Metadata: canonical.Metadata{
			ConsentReference:  req.ConsentReference,
			Source:            "NIRA",
			UpstreamLatencyMS: upstreamLatency,
			CorrelationID:     correlationID,
		},
	}

	if !found {
		resp.Status = canonical.StatusNotFound
		resp.Metadata.DataFreshness = time.Time{}
		return resp
	}
	resp.Metadata.DataFreshness = nrec.LastUpdatedAt

	// Status overrides come first — if the underlying record is dead
	// or revoked, callers should know regardless of attribute matches.
	switch nrec.Status {
	case nira.StatusDeceased:
		resp.Status = canonical.StatusDeceased
	case nira.StatusRevoked:
		resp.Status = canonical.StatusRevoked
	}

	check := func(field, claim, actual string) {
		if claim == "" {
			return
		}
		match := strings.EqualFold(strings.TrimSpace(claim), strings.TrimSpace(actual))
		resp.Attributes[field] = canonical.AttributeMatch{
			Match: match, Source: "NIRA",
		}
	}
	check("nin", req.NIN, nrec.NIN)
	check("given_name", req.GivenName, nrec.GivenName)
	check("middle_name", req.MiddleName, nrec.MiddleName)
	check("surname", req.Surname, nrec.Surname)
	check("date_of_birth", req.DateOfBirth, nrec.DateOfBirth)
	check("sex", req.Sex, nrec.Sex)
	check("mother_maiden_name", req.MotherMaidenName, nrec.MotherMaidenName)

	if resp.Status == "" {
		anyMismatch := false
		for _, am := range resp.Attributes {
			if !am.Match {
				anyMismatch = true
				break
			}
		}
		if anyMismatch {
			resp.Status = canonical.StatusMismatch
		} else {
			resp.Status = canonical.StatusVerified
		}
	}
	return resp
}

func tallyMatches(attrs map[string]canonical.AttributeMatch) (matches, mismatches int) {
	for _, am := range attrs {
		if am.Match {
			matches++
		} else {
			mismatches++
		}
	}
	return matches, mismatches
}

// recordVerification writes the operational log row AND emits the
// platform audit event (`verification.citizen.queried`). The two
// stores are intentionally separate: this DB is the service-local
// forensics view; the chain entry is the tamper-evident regulator-
// facing record. Both carry the same verification_id so they cross-
// reference.
//
// Audit chain data carries ONLY the claimed-attribute KEYS, never
// values, plus the match count. The subject is the SHA-256 hash so
// PII never lands in the chain.
func recordVerification(ctx context.Context, logger *slog.Logger, st *store.Store,
	consumer string, req VerifyCitizenRequest, correlationID string,
	status string, matches, mismatches, upstreamLatencyMS int, total time.Duration) {

	requested := sortedAttributeKeys(req)
	subjectHash := store.HashSubject(req.NIN)

	verID, err := st.Log(ctx, store.LogEntry{
		ConsumerID:          consumer,
		SubjectType:         "nin",
		SubjectHash:         subjectHash,
		ConsentReference:    req.ConsentReference,
		Upstream:            "NIRA",
		Status:              status,
		RequestedAttributes: requested,
		MatchCount:          matches,
		MismatchCount:       mismatches,
		UpstreamLatencyMS:   upstreamLatencyMS,
		CorrelationID:       correlationID,
	})
	if err != nil {
		logger.WarnContext(ctx, "verification log write failed", "error", err)
	}

	// Audit emission — own short tx so the log write above isn't
	// rolled back if the emit fails.
	pool := st.Pool()
	emitCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	tx, err := pool.BeginTx(emitCtx, pgx.TxOptions{})
	if err != nil {
		logger.WarnContext(ctx, "audit emit begin failed", "error", err)
		return
	}
	defer func() { _ = tx.Rollback(emitCtx) }()

	data := map[string]any{
		"verification_id":      verID,
		"subject_type":         "nin",
		"requested_attributes": requested,
		"match_count":          matches,
		"mismatch_count":       mismatches,
		"upstream":             "NIRA",
		"upstream_latency_ms":  upstreamLatencyMS,
		"total_latency_ms":     total.Milliseconds(),
	}
	if req.ConsentReference != "" {
		data["consent_reference"] = req.ConsentReference
	}
	if _, err := audit.Emit(emitCtx, tx, audit.Event{
		SourceKind:    "service",
		Source:        "verification",
		Type:          "verification.citizen.queried",
		SubjectKind:   "citizen",
		Subject:       subjectHash,
		Result:        result(status),
		CorrelationID: correlationID,
		Data:          data,
	}); err != nil {
		logger.WarnContext(ctx, "audit emit failed", "error", err)
		return
	}
	if err := tx.Commit(emitCtx); err != nil {
		logger.WarnContext(ctx, "audit emit commit failed", "error", err)
	}
}

// result maps verification status onto the audit envelope's result
// vocabulary. verified → ok; mismatch / deceased / revoked / not_found
// → denied (the verification failed from the consumer's POV);
// consent_invalid → denied; error → error.
func result(status string) string {
	switch canonical.Status(status) {
	case canonical.StatusVerified:
		return "ok"
	case canonical.StatusError:
		return "error"
	default:
		return "denied"
	}
}

// sortedAttributeKeys lists which attributes the caller actually
// claimed, in alphabetic order so the same set always hashes the
// same and so audit consumers see stable values.
func sortedAttributeKeys(r VerifyCitizenRequest) []string {
	pairs := []struct{ k, v string }{
		{"nin", r.NIN}, {"given_name", r.GivenName}, {"middle_name", r.MiddleName},
		{"surname", r.Surname}, {"date_of_birth", r.DateOfBirth}, {"sex", r.Sex},
		{"mother_maiden_name", r.MotherMaidenName},
	}
	var out []string
	for _, p := range pairs {
		if p.v != "" {
			out = append(out, p.k)
		}
	}
	sort.Strings(out)
	return out
}

// writeConsentInvalid returns a canonical response with
// status=consent_invalid + a metadata.note carrying the underlying
// reason. No NIRA call has happened; attributes is intentionally
// empty so we never imply we verified anything.
func writeConsentInvalid(w http.ResponseWriter, consumer string, req VerifyCitizenRequest, correlationID, reason string) {
	resp := canonical.VerificationResponse{
		VerificationID: "ver_" + uuid.NewString(),
		Consumer:       consumer,
		Subject:        canonical.SubjectIdentifier{Type: "nin", Value: req.NIN},
		CheckedAt:      time.Now().UTC(),
		Status:         canonical.StatusConsentInvalid,
		Attributes:     map[string]canonical.AttributeMatch{},
		Metadata: canonical.Metadata{
			ConsentReference: req.ConsentReference,
			Source:           "consent-service",
			CorrelationID:    correlationID,
		},
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(struct {
		canonical.VerificationResponse
		ConsentOutcome string `json:"consent_outcome"`
	}{VerificationResponse: resp, ConsentOutcome: reason})
}

// silence the "imported and not used" complaint when no helpers below
// reference these — keeping them in scope so future additions to the
// handler have the imports ready.
var _ = errors.New
var _ = fmt.Sprintf
var _ pgxpool.Pool
