// Package server is the HTTP API for webhook subscriptions + delivery
// inspection.
package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/guva-ug/guva-backend/pkg/platform/auth"
	"github.com/guva-ug/guva-backend/pkg/platform/health"
	"github.com/guva-ug/guva-backend/pkg/platform/httpserver"
	"github.com/guva-ug/guva-backend/pkg/platform/observability"
	"github.com/guva-ug/guva-backend/pkg/platform/problem"
	"github.com/guva-ug/guva-backend/services/webhooks/internal/config"
	"github.com/guva-ug/guva-backend/services/webhooks/internal/store"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

func New(cfg config.Config, logger *slog.Logger, probes *health.Probes, st *store.Store) *http.Server {
	mux := http.NewServeMux()

	// Self-service: consumers carrying `webhooks:manage` can create / list
	// their own subscriptions. Admin: `admin:webhooks` can read everyone.
	mux.Handle("POST /subscriptions",
		auth.RequireAnyScope([]string{"webhooks:manage", "admin:webhooks"},
			otelhttp.NewHandler(createSubscriptionHandler(logger, st), "POST /subscriptions")))
	mux.Handle("GET /subscriptions",
		auth.RequireAnyScope([]string{"webhooks:manage", "admin:webhooks"},
			otelhttp.NewHandler(listSubscriptionsHandler(logger, st), "GET /subscriptions")))
	mux.Handle("GET /subscriptions/{id}",
		auth.RequireAnyScope([]string{"webhooks:manage", "admin:webhooks"},
			otelhttp.NewHandler(getSubscriptionHandler(logger, st), "GET /subscriptions/{id}")))
	mux.Handle("DELETE /subscriptions/{id}",
		auth.RequireAnyScope([]string{"webhooks:manage", "admin:webhooks"},
			otelhttp.NewHandler(deleteSubscriptionHandler(logger, st), "DELETE /subscriptions/{id}")))
	mux.Handle("GET /subscriptions/{id}/deliveries",
		auth.RequireAnyScope([]string{"webhooks:manage", "admin:webhooks"},
			otelhttp.NewHandler(listDeliveriesHandler(logger, st), "GET /subscriptions/{id}/deliveries")))

	registry, metricsHandler := observability.NewMetricsRegistry()
	_ = registry
	return httpserver.New(httpserver.Config{
		Addr:           cfg.HTTPAddr,
		MetricsHandler: metricsHandler,
	}, probes, mux)
}

type createSubscriptionReq struct {
	ConsumerID        string   `json:"consumer_id"`
	TargetURL         string   `json:"target_url"`
	EventTypePatterns []string `json:"event_type_patterns"`
}

func createSubscriptionHandler(logger *slog.Logger, st *store.Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in createSubscriptionReq
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in); err != nil {
			problem.Write(w, http.StatusBadRequest, "invalid_json", err.Error())
			return
		}
		// Self-service callers (webhooks:manage but no admin:webhooks)
		// can only create for themselves — claim's azp must match.
		claims, _ := auth.FromContext(r.Context())
		if !claims.HasScope("admin:webhooks") {
			if in.ConsumerID == "" {
				in.ConsumerID = claims.ClientID
			} else if in.ConsumerID != claims.ClientID {
				problem.Write(w, http.StatusForbidden, "consumer_mismatch",
					"non-admin callers may only create subscriptions for themselves")
				return
			}
		}
		sub, err := st.CreateSubscription(r.Context(), store.Subscription{
			ConsumerID:        in.ConsumerID,
			TargetURL:         in.TargetURL,
			EventTypePatterns: in.EventTypePatterns,
		})
		if err != nil {
			logger.ErrorContext(r.Context(), "create subscription failed", "error", err)
			problem.Write(w, http.StatusBadRequest, "invalid_subscription", err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		// `secret` is returned exactly once at creation. Subsequent GETs
		// never echo it.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":                  sub.ID,
			"consumer_id":         sub.ConsumerID,
			"target_url":          sub.TargetURL,
			"event_type_patterns": sub.EventTypePatterns,
			"enabled":             sub.Enabled,
			"created_at":          sub.CreatedAt,
			"secret":              sub.Secret,
			"_note":               "Secret is shown ONCE. Store it; use it as the HMAC-SHA256 key when verifying X-Guva-Signature.",
		})
	})
}

func listSubscriptionsHandler(logger *slog.Logger, st *store.Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims, _ := auth.FromContext(r.Context())
		consumerID := claims.ClientID
		if claims.HasScope("admin:webhooks") {
			if v := r.URL.Query().Get("consumer_id"); v != "" {
				consumerID = v
			} else {
				consumerID = "" // admin without filter: list all
			}
		}
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		rows, err := st.ListSubscriptions(r.Context(), consumerID, limit)
		if err != nil {
			logger.ErrorContext(r.Context(), "list subscriptions failed", "error", err)
			problem.Write(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"subscriptions": rows,
			"count":         len(rows),
		})
	})
}

func getSubscriptionHandler(logger *slog.Logger, st *store.Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		sub, err := st.GetSubscription(r.Context(), id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				problem.Write(w, http.StatusNotFound, "subscription_not_found", "no subscription with that id")
				return
			}
			logger.ErrorContext(r.Context(), "get subscription failed", "error", err)
			problem.Write(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}
		if err := mayAccessSub(r, sub); err != nil {
			problem.Write(w, http.StatusForbidden, "consumer_mismatch", err.Error())
			return
		}
		sub.Secret = ""
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(sub)
	})
}

func deleteSubscriptionHandler(logger *slog.Logger, st *store.Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		sub, err := st.GetSubscription(r.Context(), id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				problem.Write(w, http.StatusNotFound, "subscription_not_found", "no subscription with that id")
				return
			}
			problem.Write(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}
		if err := mayAccessSub(r, sub); err != nil {
			problem.Write(w, http.StatusForbidden, "consumer_mismatch", err.Error())
			return
		}
		if err := st.DeleteSubscription(r.Context(), id); err != nil {
			logger.ErrorContext(r.Context(), "delete subscription failed", "error", err)
			problem.Write(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func listDeliveriesHandler(logger *slog.Logger, st *store.Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		sub, err := st.GetSubscription(r.Context(), id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				problem.Write(w, http.StatusNotFound, "subscription_not_found", "no subscription with that id")
				return
			}
			problem.Write(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}
		if err := mayAccessSub(r, sub); err != nil {
			problem.Write(w, http.StatusForbidden, "consumer_mismatch", err.Error())
			return
		}
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		rows, err := st.ListDeliveries(r.Context(), id, limit)
		if err != nil {
			problem.Write(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"deliveries": rows,
			"count":      len(rows),
		})
	})
}

// mayAccessSub enforces "consumer can only see their own; admin sees all".
func mayAccessSub(r *http.Request, sub store.Subscription) error {
	claims, _ := auth.FromContext(r.Context())
	if claims.HasScope("admin:webhooks") {
		return nil
	}
	if sub.ConsumerID != claims.ClientID {
		return errors.New("subscription belongs to a different consumer")
	}
	return nil
}
