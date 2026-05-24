// Package server wires the audit query/verify HTTP API. Writes happen
// via Kafka; this surface is READ-ONLY.
//
//	GET /entries                  list with cursor pagination + filters
//	GET /verify                   walk the chain in a range, report breaks
//	GET /healthz /readyz          standard probes
package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/guva-ug/guva-backend/pkg/platform/auth"
	"github.com/guva-ug/guva-backend/pkg/platform/health"
	"github.com/guva-ug/guva-backend/pkg/platform/httpserver"
	"github.com/guva-ug/guva-backend/pkg/platform/problem"
	"github.com/guva-ug/guva-backend/services/audit/internal/config"
	"github.com/guva-ug/guva-backend/services/audit/internal/store"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

func New(cfg config.Config, logger *slog.Logger, probes *health.Probes, st *store.Store) *http.Server {
	mux := http.NewServeMux()

	entries := auth.RequireScope("audit:read",
		otelhttp.NewHandler(listEntriesHandler(logger, st), "GET /entries"))
	mux.Handle("GET /entries", entries)

	verify := auth.RequireScope("audit:read",
		otelhttp.NewHandler(verifyHandler(logger, st), "GET /verify"))
	mux.Handle("GET /verify", verify)

	return httpserver.New(httpserver.Config{Addr: cfg.HTTPAddr}, probes, mux)
}

func listEntriesHandler(logger *slog.Logger, st *store.Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			problem.Write(w, http.StatusInternalServerError, "internal_error", "failed to read audit log")
			return
		}
		var nextCursor int64
		if len(rows) > 0 {
			nextCursor = rows[len(rows)-1].EntryID
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"entries":     rows,
			"count":       len(rows),
			"next_cursor": nextCursor,
		})
	})
}

func verifyHandler(logger *slog.Logger, st *store.Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			problem.Write(w, http.StatusInternalServerError, "internal_error", "verification could not complete")
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
