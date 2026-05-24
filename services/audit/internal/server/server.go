// Package server wires the audit query/verify HTTP API. Writes happen
// via Kafka; this surface is READ-ONLY.
//
//	GET /entries                  list with cursor pagination + filters
//	GET /verify                   walk the chain in a range, report breaks
//	GET /healthz /readyz          standard probes
//
// Every successful (and every failed) read of /entries or /verify also
// emits a meta-audit event through the local audit_outbox, so the chain
// itself records who queried what. Without this an insider with the
// audit:read scope could exfiltrate the ledger leaving no trace.
package server

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/guva-ug/guva-backend/pkg/platform/audit"
	"github.com/guva-ug/guva-backend/pkg/platform/auth"
	"github.com/guva-ug/guva-backend/pkg/platform/health"
	"github.com/guva-ug/guva-backend/pkg/platform/httpserver"
	"github.com/guva-ug/guva-backend/pkg/platform/observability"
	"github.com/guva-ug/guva-backend/pkg/platform/problem"
	"github.com/guva-ug/guva-backend/services/audit/internal/config"
	"github.com/guva-ug/guva-backend/services/audit/internal/store"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

func New(cfg config.Config, logger *slog.Logger, probes *health.Probes, st *store.Store, signingKey ed25519.PrivateKey) *http.Server {
	mux := http.NewServeMux()
	// Meta-audit emission writes to audit_outbox, so it goes through the
	// writer pool. The read handlers themselves still operate on the
	// reader pool (via the *store.Store passed into the handlers).
	pool := st.Writer()

	entries := auth.RequireScope("audit:read",
		otelhttp.NewHandler(listEntriesHandler(logger, st, pool), "GET /entries"))
	mux.Handle("GET /entries", entries)

	verify := auth.RequireScope("audit:read",
		otelhttp.NewHandler(verifyHandler(logger, st, pool), "GET /verify"))
	mux.Handle("GET /verify", verify)

	// SIEM export — signs the requested range with Ed25519 and emits
	// audit.bundle.exported as meta-audit so the chain records who
	// exported what range and when. Privileged: bulk-exports of the
	// ledger are an admin:audit operation. A self-service consumer
	// hitting /entries with a tight filter is the right pattern for
	// non-admin reads.
	exportH := auth.RequireScope("admin:audit",
		otelhttp.NewHandler(exportHandler(logger, st, pool, signingKey), "GET /export"))
	mux.Handle("GET /export", exportH)

	// Public-key endpoint. Intentionally unauthenticated: a verifier
	// who has a bundle but no token still needs the public key to
	// check the signature. The key is, by definition, public.
	mux.Handle("GET /export/pubkey", otelhttp.NewHandler(
		exportPubkeyHandler(signingKey), "GET /export/pubkey"))

	// Merkle anchor endpoints. Anchors summarise a contiguous range
	// of the chain into a single hash root; an operator publishes
	// roots externally and an outside verifier can later confirm a
	// single entry's membership via the inclusion proof. See
	// docs/AUDIT.md §"External Merkle anchoring".
	mux.Handle("GET /anchors", auth.RequireScope("audit:read",
		otelhttp.NewHandler(listAnchorsHandler(logger, st), "GET /anchors")))
	mux.Handle("GET /anchors/{id}", auth.RequireScope("audit:read",
		otelhttp.NewHandler(getAnchorHandler(logger, st), "GET /anchors/{id}")))
	mux.Handle("GET /anchors/{id}/proof", auth.RequireScope("audit:read",
		otelhttp.NewHandler(anchorProofHandler(logger, st), "GET /anchors/{id}/proof")))

	registry, metricsHandler := observability.NewMetricsRegistry()
	audit.RegisterMetrics(registry)

	return httpserver.New(httpserver.Config{
		Addr:           cfg.HTTPAddr,
		MetricsHandler: metricsHandler,
	}, probes, mux)
}

func listEntriesHandler(logger *slog.Logger, st *store.Store, pool *pgxpool.Pool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		q := r.URL.Query()
		params := store.QueryParams{
			ActorID:   q.Get("actor_id"),
			SubjectID: q.Get("subject_id"),
			Action:    q.Get("action"),
			Result:    q.Get("result"),
		}
		if v := q.Get("after"); v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				problem.Write(w, http.StatusBadRequest, "invalid_request", "after must be an integer entry_id cursor")
				return
			}
			params.AfterID = n
		}
		if v := q.Get("limit"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 {
				problem.Write(w, http.StatusBadRequest, "invalid_request", "limit must be a positive integer")
				return
			}
			params.Limit = n
		}
		if v := q.Get("from"); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				problem.Write(w, http.StatusBadRequest, "invalid_request", "from must be RFC3339")
				return
			}
			params.From = t
		}
		if v := q.Get("to"); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				problem.Write(w, http.StatusBadRequest, "invalid_request", "to must be RFC3339")
				return
			}
			params.To = t
		}

		rows, err := st.List(r.Context(), params)
		if err != nil {
			logger.ErrorContext(r.Context(), "list entries failed", "error", err)
			emitMetaAudit(r.Context(), pool, logger, metaEvent(r, "audit.entries.queried", params.SubjectID, "error", map[string]any{
				"filters":    redactFilters(params),
				"returned":   0,
				"latency_ms": time.Since(start).Milliseconds(),
				"error":      "list_failed",
			}))
			problem.Write(w, http.StatusInternalServerError, "internal_error", "failed to read audit log")
			return
		}
		var nextCursor int64
		if len(rows) > 0 {
			nextCursor = rows[len(rows)-1].EntryID
		}

		// Emit BEFORE writing the response. If meta-audit fails we fail
		// the read — the system's whole premise is that reads of the
		// ledger are themselves auditable.
		if err := emitMetaAudit(r.Context(), pool, logger, metaEvent(r, "audit.entries.queried", params.SubjectID, "ok", map[string]any{
			"filters":    redactFilters(params),
			"returned":   len(rows),
			"latency_ms": time.Since(start).Milliseconds(),
		})); err != nil {
			problem.Write(w, http.StatusInternalServerError, "meta_audit_failed",
				"could not record this read in the audit log; response withheld")
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"entries":     rows,
			"count":       len(rows),
			"next_cursor": nextCursor,
		})
	})
}

func verifyHandler(logger *slog.Logger, st *store.Store, pool *pgxpool.Pool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		q := r.URL.Query()
		var fromID, toID int64
		if v := q.Get("from_id"); v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				problem.Write(w, http.StatusBadRequest, "invalid_request", "from_id must be an integer")
				return
			}
			fromID = n
		}
		if v := q.Get("to_id"); v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				problem.Write(w, http.StatusBadRequest, "invalid_request", "to_id must be an integer")
				return
			}
			toID = n
		}
		if toID == 0 {
			toID = 1<<62 - 1 // walk to the end
		}
		bad, err := st.VerifyRange(r.Context(), fromID, toID)
		if err != nil {
			logger.ErrorContext(r.Context(), "verify failed", "error", err)
			_ = emitMetaAudit(r.Context(), pool, logger, metaEvent(r, "audit.chain.verified", "", "error", map[string]any{
				"from_id":    fromID,
				"to_id":      toID,
				"latency_ms": time.Since(start).Milliseconds(),
				"error":      "verify_failed",
			}))
			problem.Write(w, http.StatusInternalServerError, "internal_error", "verification could not complete")
			return
		}
		result := "ok"
		data := map[string]any{
			"from_id":    fromID,
			"to_id":      toID,
			"latency_ms": time.Since(start).Milliseconds(),
		}
		if bad != nil {
			result = "broken"
			data["broken_at"] = bad.EntryID
			data["broken_uuid"] = bad.EntryUUID
		}
		if err := emitMetaAudit(r.Context(), pool, logger, metaEvent(r, "audit.chain.verified", "", result, data)); err != nil {
			problem.Write(w, http.StatusInternalServerError, "meta_audit_failed",
				"could not record this verify in the audit log; response withheld")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if bad != nil {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":          false,
				"broken_at":   bad.EntryID,
				"broken_uuid": bad.EntryUUID,
				"detail":      "previous_hash mismatch or entry_hash recompute mismatch",
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":      true,
			"from_id": fromID,
			"to_id":   toID,
		})
	})
}

// exportHandler returns a signed bundle covering [from_id, to_id]. The
// bundle includes the anchor (entry_hash of the row preceding from_id,
// or genesis) so the verifier can chain validation back to a known
// state. Meta-audit is emitted on success or failure.
func exportHandler(logger *slog.Logger, st *store.Store, pool *pgxpool.Pool, signingKey ed25519.PrivateKey) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		q := r.URL.Query()
		var fromID, toID int64 = 1, 0
		if v := q.Get("from_id"); v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 1 {
				problem.Write(w, http.StatusBadRequest, "invalid_request", "from_id must be a positive integer")
				return
			}
			fromID = n
		}
		if v := q.Get("to_id"); v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < fromID {
				problem.Write(w, http.StatusBadRequest, "invalid_request", "to_id must be >= from_id")
				return
			}
			toID = n
		}
		if toID == 0 {
			toID = 1<<62 - 1
		}

		entries, err := st.List(r.Context(), store.QueryParams{
			AfterID: fromID - 1, // List uses entry_id > AfterID
			Limit:   500,        // hard cap to keep bundles manageable
		})
		if err != nil {
			logger.ErrorContext(r.Context(), "export list failed", "error", err)
			_ = emitMetaAudit(r.Context(), pool, logger, metaEvent(r, "audit.bundle.exported", "", "error", map[string]any{
				"from_id": fromID, "to_id": toID, "latency_ms": time.Since(start).Milliseconds(),
				"error": "list_failed",
			}))
			problem.Write(w, http.StatusInternalServerError, "internal_error", "failed to read entries")
			return
		}

		// Cap entries at requested to_id.
		var trimmed []store.EntryRecord
		for _, e := range entries {
			if e.EntryID > toID {
				break
			}
			trimmed = append(trimmed, e)
		}

		// Anchor: previous_hash of the first entry in the range. If
		// fromID == 1 we use genesis; otherwise we read row fromID-1's
		// entry_hash from the chain (via the writer pool — small lookup,
		// no cross-tx issue).
		anchor := audit.BundleAnchor{AnchorEntryID: 0, AnchorEntryHash: audit.GenesisAnchorHash}
		if fromID > 1 && len(trimmed) > 0 {
			// The first trimmed entry already carries its previous_hash;
			// that IS the anchor's hash. Use it directly to avoid an
			// extra query.
			anchor = audit.BundleAnchor{
				AnchorEntryID:   fromID - 1,
				AnchorEntryHash: trimmed[0].PreviousHash,
			}
		}

		// Build the bundle.
		bundle := audit.Bundle{
			FormatVersion: audit.BundleFormatVersion,
			Generator:     "guva-audit",
			GeneratedAt:   time.Now().UTC(),
			RangeFromID:   fromID,
			RangeToID:     toID,
			Anchor:        anchor,
			Entries:       make([]audit.BundleEntry, 0, len(trimmed)),
		}
		for _, e := range trimmed {
			bundle.Entries = append(bundle.Entries, audit.BundleEntry{
				EntryID: e.EntryID, EntryUUID: e.EntryUUID, OccurredAt: e.OccurredAt,
				ActorKind: e.ActorKind, ActorID: e.ActorID,
				SubjectKind: e.SubjectKind, SubjectID: e.SubjectID,
				Action: e.Action, Result: e.Result,
				CorrelationID: e.CorrelationID, Detail: e.Detail,
				PreviousHash: e.PreviousHash, EntryHash: e.EntryHash,
			})
		}

		if err := audit.SignBundle(&bundle, signingKey); err != nil {
			logger.ErrorContext(r.Context(), "sign bundle failed", "error", err)
			problem.Write(w, http.StatusInternalServerError, "sign_failed", "could not sign bundle")
			return
		}

		// Meta-audit BEFORE writing the response, fail-closed as the
		// rest of the read API does. Record the actual range covered
		// (to/from after trim) and the number of entries shipped.
		actualTo := toID
		if len(bundle.Entries) > 0 {
			actualTo = bundle.Entries[len(bundle.Entries)-1].EntryID
		}
		if err := emitMetaAudit(r.Context(), pool, logger, metaEvent(r, "audit.bundle.exported", "", "ok", map[string]any{
			"from_id":         fromID,
			"to_id_requested": toID,
			"to_id_actual":    actualTo,
			"count":           len(bundle.Entries),
			"latency_ms":      time.Since(start).Milliseconds(),
		})); err != nil {
			problem.Write(w, http.StatusInternalServerError, "meta_audit_failed",
				"could not record this export in the audit log; bundle withheld")
			return
		}

		w.Header().Set("Content-Type", "application/json")
		// Suggest the bundle as a file when fetched from a browser.
		w.Header().Set("Content-Disposition", fmt.Sprintf(
			`attachment; filename="guva-audit-bundle-%d-%d.json"`, fromID, actualTo))
		_ = json.NewEncoder(w).Encode(bundle)
	})
}

// exportPubkeyHandler exposes the current Ed25519 public key in base64.
// Unauthenticated — the key is by definition shareable. A verifier
// with a bundle uses this to confirm the bundle's signing_pubkey
// matches what the service is currently signing with.
func exportPubkeyHandler(signingKey ed25519.PrivateKey) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pub, err := audit.PublicKeyOf(signingKey)
		if err != nil {
			problem.Write(w, http.StatusInternalServerError, "no_signing_key", err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"algorithm":      "Ed25519",
			"public_key_b64": base64.StdEncoding.EncodeToString(pub),
		})
	})
}

// listAnchorsHandler returns recent anchors, cursor-paginated by anchor_id.
func listAnchorsHandler(logger *slog.Logger, st *store.Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		var after int64
		if v := q.Get("after"); v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				problem.Write(w, http.StatusBadRequest, "invalid_request", "after must be an integer anchor_id cursor")
				return
			}
			after = n
		}
		limit := 50
		if v := q.Get("limit"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 {
				problem.Write(w, http.StatusBadRequest, "invalid_request", "limit must be a positive integer")
				return
			}
			limit = n
		}
		rows, err := st.ListAnchors(r.Context(), after, limit)
		if err != nil {
			logger.ErrorContext(r.Context(), "list anchors failed", "error", err)
			problem.Write(w, http.StatusInternalServerError, "internal_error", "list anchors failed")
			return
		}
		var nextCursor int64
		if n := len(rows); n > 0 {
			nextCursor = rows[n-1].AnchorID
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"anchors":     rows,
			"count":       len(rows),
			"next_cursor": nextCursor,
		})
	})
}

// getAnchorHandler returns a single anchor by id.
func getAnchorHandler(logger *slog.Logger, st *store.Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idStr := r.PathValue("id")
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			problem.Write(w, http.StatusBadRequest, "invalid_request", "id must be an integer")
			return
		}
		rec, err := st.GetAnchor(r.Context(), id)
		if err != nil {
			if errors.Is(err, store.ErrNoAnchors) {
				problem.Write(w, http.StatusNotFound, "anchor_not_found", "no anchor with that id")
				return
			}
			logger.ErrorContext(r.Context(), "get anchor failed", "error", err, "anchor_id", id)
			problem.Write(w, http.StatusInternalServerError, "internal_error", "get anchor failed")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(rec)
	})
}

// anchorProofHandler returns the Merkle inclusion proof for a specific
// entry_id within an anchor's range. The proof, together with the
// leaf (entry's entry_hash) and the anchor's merkle_root, lets an
// external party confirm the entry was in the chain when the anchor
// was computed.
//
//	GET /anchors/{id}/proof?entry_id=N
func anchorProofHandler(logger *slog.Logger, st *store.Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idStr := r.PathValue("id")
		anchorID, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			problem.Write(w, http.StatusBadRequest, "invalid_request", "id must be an integer")
			return
		}
		entryIDStr := r.URL.Query().Get("entry_id")
		entryID, err := strconv.ParseInt(entryIDStr, 10, 64)
		if err != nil {
			problem.Write(w, http.StatusBadRequest, "invalid_request", "entry_id query parameter required")
			return
		}

		anc, err := st.GetAnchor(r.Context(), anchorID)
		if err != nil {
			if errors.Is(err, store.ErrNoAnchors) {
				problem.Write(w, http.StatusNotFound, "anchor_not_found", "no anchor with that id")
				return
			}
			logger.ErrorContext(r.Context(), "get anchor failed", "error", err)
			problem.Write(w, http.StatusInternalServerError, "internal_error", "get anchor failed")
			return
		}
		if entryID < anc.RangeFromID || entryID > anc.RangeToID {
			problem.Write(w, http.StatusBadRequest, "entry_out_of_range",
				fmt.Sprintf("entry_id %d is outside anchor range [%d,%d]", entryID, anc.RangeFromID, anc.RangeToID))
			return
		}

		leaves, err := st.EntryHashRange(r.Context(), anc.RangeFromID, anc.RangeToID)
		if err != nil {
			logger.ErrorContext(r.Context(), "anchor leaves read failed", "error", err)
			problem.Write(w, http.StatusInternalServerError, "internal_error", "read leaves failed")
			return
		}
		pos, err := st.EntryIDPosition(r.Context(), anc.RangeFromID, anc.RangeToID, entryID)
		if err != nil {
			logger.ErrorContext(r.Context(), "entry position lookup failed", "error", err)
			problem.Write(w, http.StatusInternalServerError, "internal_error", "position lookup failed")
			return
		}
		proof, err := audit.BuildInclusionProof(leaves, pos)
		if err != nil {
			logger.ErrorContext(r.Context(), "proof build failed", "error", err, "pos", pos)
			problem.Write(w, http.StatusInternalServerError, "internal_error", "proof build failed")
			return
		}
		leafHash := leaves[pos]
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"anchor_id":     anc.AnchorID,
			"merkle_root":   anc.MerkleRoot,
			"algorithm":     anc.Algorithm,
			"range_from_id": anc.RangeFromID,
			"range_to_id":   anc.RangeToID,
			"entry_id":      entryID,
			"leaf_index":    pos,
			"leaf_hash":     leafHash,
			"proof":         proof,
		})
	})
}

// metaEvent builds the audit.Event for a read against the audit log.
// The actor is the JWT's authorized party (azp) — the consumer or
// service whose token APISIX validated. SourceKind defaults to
// "consumer" since the only path here is through the gateway with a
// client-credentials token; internal-only readers should pass through
// the same path.
func metaEvent(r *http.Request, eventType, subject, result string, data map[string]any) audit.Event {
	claims, _ := auth.FromContext(r.Context())
	actor := claims.ClientID
	if actor == "" {
		actor = claims.Subject
	}
	if actor == "" {
		actor = "unknown"
	}
	return audit.Event{
		SourceKind:    "consumer",
		Source:        actor,
		Type:          eventType,
		SubjectKind:   "audit_query",
		Subject:       subject,
		Result:        result,
		CorrelationID: r.Header.Get("X-Correlation-Id"),
		Data:          data,
	}
}

// redactFilters strips potentially-sensitive query inputs down to what
// is safe to record in the chain. Right now the inputs are already
// stable IDs/strings; this is a forward-compat seam for when we add
// free-form filters that could carry PII.
func redactFilters(p store.QueryParams) map[string]any {
	out := map[string]any{}
	if p.ActorID != "" {
		out["actor_id"] = p.ActorID
	}
	if p.SubjectID != "" {
		out["subject_id"] = p.SubjectID
	}
	if p.Action != "" {
		out["action"] = p.Action
	}
	if p.Result != "" {
		out["result"] = p.Result
	}
	if !p.From.IsZero() {
		out["from"] = p.From.UTC().Format(time.RFC3339)
	}
	if !p.To.IsZero() {
		out["to"] = p.To.UTC().Format(time.RFC3339)
	}
	if p.AfterID != 0 {
		out["after"] = p.AfterID
	}
	if p.Limit != 0 {
		out["limit"] = p.Limit
	}
	return out
}

// emitMetaAudit opens a short-lived transaction, calls audit.Emit, and
// commits. Returns the error so the caller can decide whether to fail
// the user-facing read. We use a 3s timeout because a stuck DB here
// must not stall a user request indefinitely.
func emitMetaAudit(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger, e audit.Event) error {
	txCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	tx, err := pool.BeginTx(txCtx, pgx.TxOptions{})
	if err != nil {
		logger.ErrorContext(ctx, "meta-audit begin failed", "error", err, "type", e.Type)
		return err
	}
	defer func() { _ = tx.Rollback(txCtx) }()
	if _, err := audit.Emit(txCtx, tx, e); err != nil {
		logger.ErrorContext(ctx, "meta-audit emit failed", "error", err, "type", e.Type)
		return err
	}
	if err := tx.Commit(txCtx); err != nil {
		logger.ErrorContext(ctx, "meta-audit commit failed", "error", err, "type", e.Type)
		return err
	}
	return nil
}
