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
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/guva-ug/guva-backend/pkg/platform/auth"
	"github.com/guva-ug/guva-backend/pkg/platform/health"
	"github.com/guva-ug/guva-backend/pkg/platform/httpserver"
	"github.com/guva-ug/guva-backend/pkg/platform/problem"
	"github.com/guva-ug/guva-backend/pkg/secrets"
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

// scopeCatalogueFallback is the baseline list used when Keycloak is
// unreachable on the first request (no cache yet). After the first
// successful Keycloak fetch the cache wins; this only matters at
// cold-boot when the IdP is also down.
var scopeCatalogueFallback = []Scope{
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

// scopeCache caches Keycloak's client-scope list with TTL. Thread-safe;
// on Keycloak error returns stale data when available, otherwise the
// fallback list. The TTL is short (60s) because scope changes are rare
// but should propagate quickly when they happen.
type scopeCache struct {
	kc      *keycloakadmin.Client
	logger  *slog.Logger
	ttl     time.Duration
	mu      sync.RWMutex
	value   []Scope
	expires time.Time
}

func newScopeCache(kc *keycloakadmin.Client, logger *slog.Logger) *scopeCache {
	return &scopeCache{kc: kc, logger: logger, ttl: 60 * time.Second}
}

// Get returns the current scope catalogue. Refreshes from Keycloak if
// the cache is empty or expired. Serves stale on Keycloak error rather
// than failing the request; falls back to the baseline list only when
// nothing has ever been cached.
func (s *scopeCache) Get(ctx context.Context) []Scope {
	s.mu.RLock()
	if len(s.value) > 0 && time.Now().Before(s.expires) {
		v := s.value
		s.mu.RUnlock()
		return v
	}
	s.mu.RUnlock()

	s.mu.Lock()
	defer s.mu.Unlock()
	// Re-check after acquiring the write lock — another goroutine may
	// have refreshed in the meantime.
	if len(s.value) > 0 && time.Now().Before(s.expires) {
		return s.value
	}

	raw, err := s.kc.ListClientScopes(ctx)
	if err != nil {
		s.logger.WarnContext(ctx, "scope catalogue refresh failed; serving stale or fallback",
			"have_cached", len(s.value) > 0, "error", err)
		if len(s.value) > 0 {
			return s.value
		}
		return scopeCatalogueFallback
	}

	out := make([]Scope, 0, len(raw))
	for _, r := range raw {
		// Filter out OpenID-Connect built-ins; our convention is that
		// platform scopes always contain `:`.
		if !strings.Contains(r.Name, ":") {
			continue
		}
		out = append(out, Scope{
			Name:        r.Name,
			Description: r.Description,
			Category:    deriveCategory(r.Name),
		})
	}
	s.value = out
	s.expires = time.Now().Add(s.ttl)
	return s.value
}

// deriveCategory pulls the category from the scope-name prefix
// (`verify:citizen` → `verification`). Keeping this here rather than
// in Keycloak lets us evolve the categorisation without an IdP change.
func deriveCategory(name string) string {
	prefix := name
	if i := strings.Index(name, ":"); i > 0 {
		prefix = name[:i]
	}
	switch prefix {
	case "verify":
		return "verification"
	case "consent":
		return "consent"
	case "audit":
		return "audit"
	case "webhooks":
		return "delivery"
	case "admin":
		return "admin"
	default:
		return "other"
	}
}

// New returns the identity service's *http.Server.
func New(cfg config.Config, logger *slog.Logger, probes *health.Probes, st *store.Store, kc *keycloakadmin.Client, vault *secrets.Client) *http.Server {
	mux := http.NewServeMux()

	cache := newScopeCache(kc, logger)

	// Read endpoints — protected by audit:read (a read-only scope that
	// guva-reference happens to carry, so we can demo the flow with the
	// existing dev client). In production this would be admin:consumers
	// or a dedicated identity:read scope.
	scopes := auth.RequireScope("audit:read",
		otelhttp.NewHandler(scopesHandler(logger, cache), "GET /scopes"))
	mux.Handle("GET /scopes", scopes)

	consumersGet := auth.RequireScope("audit:read",
		otelhttp.NewHandler(getConsumerHandler(logger, st), "GET /consumers/{id}"))
	mux.Handle("GET /consumers/{id}", consumersGet)

	// Write endpoint — also audit:read in dev; production would require
	// the admin:consumers scope and probably a separate platform-admin
	// client with stricter rate limits.
	consumersPost := auth.RequireScope("audit:read",
		otelhttp.NewHandler(createConsumerHandler(logger, st, kc, vault), "POST /consumers"))
	mux.Handle("POST /consumers", consumersPost)

	return httpserver.New(httpserver.Config{Addr: cfg.HTTPAddr}, probes, mux)
}

func scopesHandler(logger *slog.Logger, cache *scopeCache) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims, _ := auth.FromContext(r.Context())
		scopes := cache.Get(r.Context())
		logger.InfoContext(r.Context(), "scopes.list",
			"correlation_id", r.Header.Get("X-Correlation-Id"),
			"caller_client", claims.ClientID,
			"count", len(scopes),
		)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"scopes": scopes,
			"count":  len(scopes),
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
// secret returned by Keycloak and the Vault path where it's been
// stashed for operational recovery. The secret is omitted from every
// other response (GET /consumers/{id}, list endpoints when they exist);
// the Vault path is the recoverable backup if the caller drops it.
type createConsumerResponse struct {
	store.ConsumerRegistration
	GeneratedClientSecret string `json:"generated_client_secret"`
	SecretVaultPath       string `json:"secret_vault_path,omitempty"`
	SecretDisclosureNote  string `json:"_note"`
}

// idempotencyTTL is how long a cached response stays replayable. Tuned
// to cover realistic retry windows (timeouts, transient network) but
// short enough that a re-onboarding effort weeks later doesn't pick up
// a stale response.
const idempotencyTTL = 24 * time.Hour

func createConsumerHandler(logger *slog.Logger, st *store.Store, kc *keycloakadmin.Client, vault *secrets.Client) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Read the raw body so we can both fingerprint it for
		// Idempotency-Key matching AND still feed it into the JSON
		// decoder below. http.Request.Body is one-shot — we replace
		// it with a fresh bytes.Reader after the slurp.
		rawBody, err := io.ReadAll(r.Body)
		if err != nil {
			problem.Write(w, http.StatusBadRequest, "invalid_request", "could not read request body")
			return
		}

		// Idempotency-Key handling — if the caller supplies a key,
		// look up any cached response and either replay it (same body
		// fingerprint) or reject (fingerprint mismatch = client bug).
		idemKey := r.Header.Get("Idempotency-Key")
		var fingerprint string
		if idemKey != "" {
			sum := sha256.Sum256(rawBody)
			fingerprint = hex.EncodeToString(sum[:])

			cached, err := st.GetIdempotencyRecord(r.Context(), idemKey)
			switch {
			case err == nil:
				if cached.RequestFingerprint != fingerprint {
					problem.Write(w, http.StatusUnprocessableEntity, "idempotency_fingerprint_mismatch",
						"this Idempotency-Key was previously used with a different request body")
					return
				}
				logger.InfoContext(r.Context(), "consumer.idempotent_replay",
					"correlation_id", r.Header.Get("X-Correlation-Id"),
					"idempotency_key", idemKey,
					"original_age", time.Since(cached.CreatedAt).String(),
				)
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Idempotency-Replay", "true")
				w.WriteHeader(cached.ResponseStatus)
				_, _ = w.Write(cached.ResponseBody)
				return
			case errors.Is(err, store.ErrNotFound):
				// New key — fall through to create.
			default:
				logger.ErrorContext(r.Context(), "idempotency lookup failed; proceeding without dedupe",
					"idempotency_key", idemKey, "error", err)
			}
		}

		r.Body = io.NopCloser(bytes.NewReader(rawBody))

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
			// Compensating delete — the Keycloak client now exists but
			// our DB rejected the registration. Roll back so we don't
			// leave an orphaned client in Keycloak with no audit trail.
			//
			// We use a fresh context with a short timeout because the
			// caller's context might already be on the way out (the
			// failed Postgres call could have been a context deadline).
			compCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if delErr := kc.DeleteClient(compCtx, kcResult.InternalID); delErr != nil {
				logger.ErrorContext(r.Context(), "compensating delete failed — RECONCILE REQUIRED",
					"keycloak_internal_id", kcResult.InternalID,
					"keycloak_client_id", kcResult.ClientID,
					"persist_error", err,
					"delete_error", delErr,
				)
			} else {
				logger.WarnContext(r.Context(), "compensating delete succeeded",
					"keycloak_internal_id", kcResult.InternalID,
					"keycloak_client_id", kcResult.ClientID,
					"persist_error", err,
				)
			}
			problem.Write(w, http.StatusInternalServerError, "internal_error",
				"failed to persist registration; the Keycloak client was rolled back")
			return
		}

		// Stash the secret in Vault for operational recovery if the
		// caller drops the response. This is best-effort: if the write
		// fails, we still succeed the request — the secret is in the
		// response body and that's the authoritative one-time disclosure.
		// The Vault path is identity-scoped (services/identity/...) so
		// only the identity service's own policy can read it.
		vaultPath := "services/identity/consumers/" + kcResult.ClientID
		vaultStashed := false
		if vault != nil {
			vaultCtx, vaultCancel := context.WithTimeout(r.Context(), 5*time.Second)
			err := vault.Put(vaultCtx, vaultPath, map[string]string{
				"client_secret": kcResult.Secret,
				"consumer_id":   reg.ID,
				"created_at":    reg.CreatedAt.Format(time.RFC3339Nano),
			})
			vaultCancel()
			if err != nil {
				logger.WarnContext(r.Context(), "vault stash failed; response body remains the only copy",
					"vault_path", vaultPath, "error", err)
			} else {
				vaultStashed = true
			}
		}

		logger.InfoContext(r.Context(), "consumer.created",
			"correlation_id", r.Header.Get("X-Correlation-Id"),
			"consumer_id", reg.ID,
			"keycloak_internal_id", kcResult.InternalID,
			"agency", reg.AgencyName,
			"vault_stashed", vaultStashed,
		)

		resp := createConsumerResponse{
			ConsumerRegistration:  reg,
			GeneratedClientSecret: kcResult.Secret,
			SecretDisclosureNote:  "This secret is shown only here. Store it securely; subsequent GETs will not include it.",
		}
		if vaultStashed {
			resp.SecretVaultPath = vaultPath
			resp.SecretDisclosureNote += " A backup copy is in Vault at " + vaultPath + " (identity-scoped policy)."
		}

		// Serialise once so we can both store under the idempotency
		// key (if any) AND write to the wire. Cache stores the exact
		// bytes the client receives so replay is byte-identical.
		respBytes, err := json.Marshal(resp)
		if err != nil {
			logger.ErrorContext(r.Context(), "marshal response failed", "error", err)
			problem.Write(w, http.StatusInternalServerError, "internal_error", "failed to encode response")
			return
		}
		if idemKey != "" {
			saveErr := st.SaveIdempotencyRecord(r.Context(), store.IdempotencyRecord{
				Key:                idemKey,
				RequestFingerprint: fingerprint,
				ResponseStatus:     http.StatusCreated,
				ResponseBody:       respBytes,
				ExpiresAt:          time.Now().Add(idempotencyTTL),
			})
			if saveErr != nil {
				// Non-fatal — the consumer was created, the response
				// will be returned; we just won't replay this key.
				logger.WarnContext(r.Context(), "idempotency record save failed; replay won't work",
					"idempotency_key", idemKey, "error", saveErr)
			}
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Location", "/consumers/"+reg.ID)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(respBytes)
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
