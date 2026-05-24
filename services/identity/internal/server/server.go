// Package server wires the identity service's routes onto the shared
// platform httpserver. Three endpoints in this first pass:
//
//	GET  /scopes             — read the platform's scope catalogue
//	POST /consumers          — register a new consumer (records intent)
//	GET  /consumers/{id}     — read back a registration
//
// The consumer endpoints do NOT yet call Keycloak's admin API to create
// the corresponding client; that's the next iteration. For now we record
// the registration intent in our own DB so the API contract is stable.
package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/guva-ug/guva-backend/pkg/platform/auth"
	"github.com/guva-ug/guva-backend/pkg/platform/health"
	"github.com/guva-ug/guva-backend/pkg/platform/httpserver"
	"github.com/guva-ug/guva-backend/pkg/platform/problem"
	"github.com/guva-ug/guva-backend/services/identity/internal/config"
	"github.com/guva-ug/guva-backend/services/identity/internal/keycloakadmin"
	"github.com/guva-ug/guva-backend/services/identity/internal/store"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// Scope is one entry in the platform's scope catalogue.
type Scope struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Category    string `json:"category"`
}

// scopeCatalogue is the authoritative list returned by GET /scopes.
// Mirrors what's in deploy/compose/keycloak/realm-export.json
// (clientScopes block). Keeping it in code lets us evolve the contract
// without round-tripping through Keycloak; a future iteration syncs.
var scopeCatalogue = []Scope{
	{Name: "verify:citizen", Description: "Verify citizen identity attributes", Category: "verification"},
	{Name: "verify:business", Description: "Verify business registration and status", Category: "verification"},
	{Name: "verify:tax", Description: "Verify tax identification and compliance", Category: "verification"},
	{Name: "verify:land", Description: "Verify land ownership and encumbrance", Category: "verification"},
	{Name: "verify:institution", Description: "Verify institution registration", Category: "verification"},
	{Name: "verify:education", Description: "Verify educational qualifications", Category: "verification"},
	{Name: "consent:read", Description: "Read consent records", Category: "consent"},
	{Name: "consent:write", Description: "Grant or revoke consent", Category: "consent"},
	{Name: "audit:read", Description: "Read audit log entries", Category: "audit"},
	{Name: "webhooks:manage", Description: "Register and manage webhook subscriptions", Category: "delivery"},
	{Name: "admin:consumers", Description: "Administer consumer registrations", Category: "admin"},
}

// New returns the identity service's *http.Server.
func New(cfg config.Config, logger *slog.Logger, probes *health.Probes, st *store.Store, kc *keycloakadmin.Client) *http.Server {
	mux := http.NewServeMux()

	// Read endpoints — protected by audit:read (a read-only scope that
	// guva-reference happens to carry, so we can demo the flow with the
	// existing dev client). In production this would be admin:consumers
	// or a dedicated identity:read scope.
	scopes := auth.RequireScope("audit:read",
		otelhttp.NewHandler(scopesHandler(logger), "GET /scopes"))
	mux.Handle("GET /scopes", scopes)

	consumersGet := auth.RequireScope("audit:read",
		otelhttp.NewHandler(getConsumerHandler(logger, st), "GET /consumers/{id}"))
	mux.Handle("GET /consumers/{id}", consumersGet)

	// Write endpoint — also audit:read in dev; production would require
	// the admin:consumers scope and probably a separate platform-admin
	// client with stricter rate limits.
	consumersPost := auth.RequireScope("audit:read",
		otelhttp.NewHandler(createConsumerHandler(logger, st, kc), "POST /consumers"))
	mux.Handle("POST /consumers", consumersPost)

	return httpserver.New(httpserver.Config{Addr: cfg.HTTPAddr}, probes, mux)
}

func scopesHandler(logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims, _ := auth.FromContext(r.Context())
		logger.InfoContext(r.Context(), "scopes.list",
			"correlation_id", r.Header.Get("X-Correlation-Id"),
			"caller_client", claims.ClientID,
		)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"scopes": scopeCatalogue,
			"count":  len(scopeCatalogue),
		})
	})
}

type createConsumerRequest struct {
	AgencyName       string   `json:"agency_name"`
	ContactEmail     string   `json:"contact_email"`
	KeycloakClientID string   `json:"keycloak_client_id"`
	Scopes           []string `json:"scopes"`
}

// createConsumerResponse extends the persisted record with the one-time
// secret returned by Keycloak. The secret is omitted from every other
// response (GET /consumers/{id}, list endpoints when they exist) —
// callers MUST capture it from this 201 body.
type createConsumerResponse struct {
	store.ConsumerRegistration
	GeneratedClientSecret string `json:"generated_client_secret"`
	SecretDisclosureNote  string `json:"_note"`
}

func createConsumerHandler(logger *slog.Logger, st *store.Store, kc *keycloakadmin.Client) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req createConsumerRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			problem.Write(w, http.StatusBadRequest, "invalid_request", "request body is not valid JSON")
			return
		}
		if strings.TrimSpace(req.AgencyName) == "" {
			problem.Write(w, http.StatusBadRequest, "invalid_request", "agency_name is required")
			return
		}
		if strings.TrimSpace(req.KeycloakClientID) == "" {
			problem.Write(w, http.StatusBadRequest, "invalid_request", "keycloak_client_id is required")
			return
		}

		// 1. Create the Keycloak client first — if this fails we don't
		//    want a half-registered row in our DB. Idempotency would be
		//    nicer (retry the same request and continue from where we
		//    left off) but is a follow-up; today: fail-fast.
		kcResult, err := kc.CreateConfidentialClient(r.Context(), keycloakadmin.CreateClientRequest{
			ClientID:            req.KeycloakClientID,
			Name:                req.AgencyName,
			DefaultClientScopes: req.Scopes,
		})
		if err != nil {
			switch {
			case errors.Is(err, keycloakadmin.ErrClientExists):
				problem.Write(w, http.StatusConflict, "client_exists",
					"a Keycloak client with that id already exists in the guva realm")
				return
			case errors.Is(err, keycloakadmin.ErrAdminCredentials):
				logger.ErrorContext(r.Context(), "keycloak admin auth failed", "error", err)
				problem.Write(w, http.StatusBadGateway, "upstream_unavailable",
					"identity service cannot authenticate to Keycloak; admin credentials may have rotated")
				return
			default:
				logger.ErrorContext(r.Context(), "keycloak client create failed", "error", err)
				problem.Write(w, http.StatusBadGateway, "upstream_error",
					"failed to create client in Keycloak")
				return
			}
		}

		// 2. Persist the audit record. Now our DB and Keycloak agree.
		reg := store.ConsumerRegistration{
			ID:               uuid.NewString(),
			AgencyName:       req.AgencyName,
			ContactEmail:     req.ContactEmail,
			KeycloakClientID: kcResult.ClientID,
			Scopes:           req.Scopes,
			Status:           "active",
			CreatedAt:        time.Now().UTC(),
		}
		if err := st.CreateConsumer(r.Context(), reg); err != nil {
			// The Keycloak client now exists but we couldn't record it.
			// Log loud so an operator can manually reconcile; the next
			// iteration is to also delete the Keycloak client on this
			// branch (compensating action).
			logger.ErrorContext(r.Context(), "persist consumer failed AFTER keycloak client created — RECONCILE REQUIRED",
				"keycloak_internal_id", kcResult.InternalID,
				"keycloak_client_id", kcResult.ClientID,
				"error", err,
			)
			problem.Write(w, http.StatusInternalServerError, "internal_error",
				"client created in Keycloak but registration could not be persisted")
			return
		}

		logger.InfoContext(r.Context(), "consumer.created",
			"correlation_id", r.Header.Get("X-Correlation-Id"),
			"consumer_id", reg.ID,
			"keycloak_internal_id", kcResult.InternalID,
			"agency", reg.AgencyName,
		)

		resp := createConsumerResponse{
			ConsumerRegistration:  reg,
			GeneratedClientSecret: kcResult.Secret,
			SecretDisclosureNote:  "This secret is shown only here. Store it securely; subsequent GETs will not include it.",
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Location", "/consumers/"+reg.ID)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(resp)
	})
}

func getConsumerHandler(logger *slog.Logger, st *store.Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "" {
			problem.Write(w, http.StatusBadRequest, "invalid_request", "id is required")
			return
		}
		reg, err := st.GetConsumer(r.Context(), id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				problem.Write(w, http.StatusNotFound, "not_found", "no consumer registration with that id")
				return
			}
			logger.ErrorContext(r.Context(), "get consumer failed", "error", err, "id", id)
			problem.Write(w, http.StatusInternalServerError, "internal_error", "failed to read registration")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(reg)
	})
}
