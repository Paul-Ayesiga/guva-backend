# Authentication Architecture

How GUVA authenticates API consumers, how tokens flow through the platform, and how the same auth contract holds across local, staging, and production.

This is the design reference. The day-to-day developer experience lives in [DEVELOPMENT.md](./DEVELOPMENT.md); operational procedures (key rotation, incident response) will live in `RUNBOOK-AUTH.md` once Phase 2.3 lands.

---

## 1. Architecture at a glance

```
                       ┌─────────────────────┐
                       │       Client        │
                       │ (consumer, browser, │
                       │  partner system)    │
                       └──────────┬──────────┘
                                  │  (1) GET token / bearer-call
                                  ▼
                       ┌─────────────────────┐
                       │       Caddy         │   TLS terminator
                       │  https://auth.…     │   https://api.…
                       └──────────┬──────────┘
                                  │  (2) HTTP forward inside cluster
                ┌─────────────────┼─────────────────┐
                ▼                                   ▼
       ┌────────────────┐                  ┌────────────────┐
       │    Keycloak    │   issues JWT     │ Apache APISIX  │
       │  (OIDC IdP)    │ ────────────────▶│   (gateway)    │
       │                │  validates via   │                │
       │                │  OIDC discovery  │                │
       └────────────────┘                  └────────┬───────┘
                                                    │  (3) HTTP + Bearer
                                                    ▼
                                           ┌────────────────┐
                                           │   Service      │
                                           │ (reference, …) │
                                           └────────────────┘
```

Four moving parts:

| Component | Role |
|---|---|
| **Caddy** | Terminates TLS at the public-facing names. Auto-manages local CA in dev; uses ACME / managed certs in non-dev. |
| **Keycloak** | OIDC identity provider. Issues access tokens, publishes JWKS, owns the realm scope and role catalogue. |
| **Apache APISIX** | Gateway. Bearer-only validation via OIDC discovery + JWKS. Forwards verified tokens upstream. |
| **Services** | Receive an already-validated bearer; parse claims, enforce per-route scope, do business logic. |

---

## 2. Token lifecycle

### 2.1 Issuance — client-credentials (service to service)

```
client → POST https://auth.guva.localhost/realms/guva/protocol/openid-connect/token
         Content-Type: application/x-www-form-urlencoded
         grant_type=client_credentials
         client_id=<id>
         client_secret=<secret>
```

Caddy terminates TLS and forwards to Keycloak. Keycloak returns:

```json
{
  "access_token": "<jwt>",
  "expires_in": 3600,
  "token_type": "Bearer",
  "scope": "verify:citizen audit:read"
}
```

The JWT carries (at minimum): `iss`, `sub`, `azp`, `scope`, `exp`, `iat`, `jti`.

### 2.2 Issuance — authorisation-code with PKCE (citizen-facing)

Not yet implemented in this repository. The citizen-facing consent dashboard ([§5.3](../../guva-docs/02-requirements/05-functional-requirements.md)) will use this flow when it lands.

### 2.3 Bearer call

```
client → GET https://api.guva.localhost/v1/reference/ping
         Authorization: Bearer <jwt>
```

APISIX inspects the `Authorization` header, decodes the JWT header, fetches the matching public key from its in-memory JWKS cache, and verifies the signature. If valid and the claims check out, the request is forwarded to the upstream with the same Authorization header intact. Otherwise APISIX returns `401` immediately and the upstream sees nothing.

### 2.4 Validation — what APISIX checks

| Check | Source of truth | What happens on failure |
|---|---|---|
| Signature | JWKS from Keycloak discovery doc | 401 `invalid_token` |
| `exp` | Token itself | 401 `invalid_token` |
| `iss` | Discovery doc `issuer` field | 401 `invalid_token` |
| `azp` (audience) | APISIX route's `client_id` | 401 `invalid_token` |
| Algorithm allow-list | APISIX route config (`RS256` only) | 401 `invalid_token` |

Scope checks are **not** done at the gateway. Services do them in their own handlers — defence in depth (see [§4](#4-service-side-claim-handling)).

### 2.5 JWKS refresh

APISIX fetches the JWKS lazily on first request, then caches it. When Keycloak rotates its realm key:

1. Keycloak publishes a new key alongside the old one (both in the JWKS doc).
2. APISIX's cache holds the old set; new tokens fail signature verification.
3. APISIX automatically re-fetches the JWKS when it sees a `kid` it doesn't know.
4. Both old and new tokens validate during the rotation window.

No manual `make refresh-keys` step. No pinned keys anywhere.

---

## 3. Issuer URL alignment across environments

This is the load-bearing design decision of Phase 2.

**The problem.** Tokens carry an `iss` claim — the URL of the issuer (`https://auth.guva.localhost/realms/guva` in dev). The gateway validates that `token.iss == discovery.issuer`. When the gateway and the client reach the IdP via different hostnames, naive setups break:

- Client fetches token via `https://auth.<env>/realms/guva` → token has `iss=https://auth.<env>/...`.
- Gateway fetches discovery via internal `http://keycloak:8080/...` → discovery's `issuer` field would default to that internal URL → mismatch.

**The fix — two layers:**

1. **`KC_HOSTNAME=https://auth.<env>`** makes Keycloak always report `issuer=https://auth.<env>/...` in the discovery doc, regardless of which network path queried it. Tokens always carry that same value in `iss`.
2. **`KC_HOSTNAME_BACKCHANNEL_DYNAMIC=true`** keeps the discovery doc's *endpoint URLs* (`jwks_uri`, `token_endpoint`, …) derived from the request host. So when APISIX queries `http://keycloak:8080/.well-known/openid-configuration` internally, it gets back an issuer field of `https://auth.<env>/...` (stable) but a `jwks_uri` of `http://keycloak:8080/realms/guva/protocol/openid-connect/certs` (in-network, reachable, no TLS dance needed).

The same configuration shape works in every environment:

| Env | `KC_HOSTNAME` | Discovery URL (gateway → IdP) | Note |
|---|---|---|---|
| local | `https://auth.guva.localhost` | `http://keycloak:8080/realms/guva/.well-known/openid-configuration` | Caddy fronts Keycloak; `.localhost` resolves to 127.0.0.1 via RFC 6761. |
| staging | `https://auth.staging.guva.go.ug` | `http://keycloak.internal:8080/...` | Internal Kubernetes service DNS. |
| production | `https://auth.guva.go.ug` | `http://keycloak.svc.cluster.local:8080/...` | Same shape; only the URL changes. |

The gateway, services, and clients work with no per-environment code changes. Only the env vars differ.

---

## 4. Service-side claim handling

The gateway hands off a verified bearer token, but services still enforce their own scope contract. This is intentional:

1. **Defence in depth**: a gateway misconfiguration shouldn't open a hole. The service rejects any request without the scope it requires.
2. **Locality of context**: the service knows what scope its routes need; the gateway doesn't need a central registry of scope→route mappings.
3. **Cross-cutting auth**: services can read other claims (`sub`, `azp`, custom claims) for audit logging, multi-tenancy, etc.

In the reference service, this is `services/reference/internal/auth` — about 100 lines, no external dependencies:

```go
import "github.com/guva-ug/guva-backend/services/reference/internal/auth"

mux.Handle("/ping",
    auth.RequireScope("verify:citizen", pingHandler))
```

The package does **not** re-verify the signature (the gateway already did). It parses the JWT payload, checks scope, attaches `auth.Claims` to the context. Direct calls to the service port (bypassing the gateway) are intended for development only — production network policies enforce that services are reachable only through the gateway.

---

## 5. Local development setup

A complete description of the dev flow lives in [DEVELOPMENT.md §3–4](./DEVELOPMENT.md). The short version:

```bash
make bootstrap            # one-time, pulls images
make up                   # brings up stack; Caddy generates a local root CA on first boot
make trust-ca             # one-time, installs Caddy's root CA into system trust store
make ping                 # fetch a token from https://auth.guva.localhost and call /v1/reference/ping
```

After `make trust-ca`, browsers and `curl` accept `https://auth.guva.localhost` without warnings. The trust step is reversible with `make untrust-ca`.

For machines where `*.localhost` doesn't auto-resolve (notably Windows), add a single hosts entry:

```
127.0.0.1 auth.guva.localhost
```

Linux glibc 2.27+ and modern macOS resolve `.localhost` via the system resolver per RFC 6761; no hosts file changes needed.

---

## 6. Observability

Auth-specific signals live on a dedicated Grafana dashboard, **GUVA — Auth Overview** (provisioned at boot from `deploy/compose/grafana/dashboards/auth-overview.json`). Open Grafana at <http://localhost:3000> and the dashboard appears under the default folder.

Panels:

| Panel | Metric source | What it tells you |
|---|---|---|
| 401 / 403 / total / auth-fail % | `apisix_http_status` | Spike in unauthorised traffic, or a bad credential rollout |
| Gateway responses by status code | `apisix_http_status` | Status-code timeseries — easy to spot a 5xx storm or 401 cliff |
| Gateway request latency (p95/p99 by route) | `apisix_http_latency_bucket` | Whether OIDC validation is impacting latency on a specific route |
| APISIX / Keycloak scrape health | `up{job="…"}` | Distinguishes a real auth incident from a Prometheus-collector outage |
| Keycloak token issuance rate (by client) | `keycloak_user_logins_total` | Detects unusual issuance patterns per client |

For incident response on any of these signals, see [RUNBOOK-AUTH.md](./RUNBOOK-AUTH.md). The metric names above are exactly what APISIX 3.16 emits at `apisix:9091/apisix/prometheus/metrics`; the scrape config is in `deploy/compose/prometheus/prometheus.yml`.

A natural follow-up (Phase 2.5) is adding **Prometheus alerts** that page when auth-failure share exceeds a threshold sustained over 5 minutes. Deferred until alerting routes are wired up.

---

## 7. Phase 2 status

| Slice | Goal | Status |
|---|---|---|
| 2.1 | Drop pinned keys; APISIX uses OIDC discovery | ✅ |
| 2.2 | Caddy + `*.localhost` TLS; production-shaped issuer URLs in local | ✅ |
| 2.3 | Operational runbook + environment migration guide + rollback strategy | ✅ |
| 2.4 | Observability (auth metrics, JWKS refresh dashboards, 401/403 panels) | ✅ |
| **2.5** | **Security hardening checklist; stricter rate limits on auth-sensitive endpoints** | ✅ **landed in this commit** |

**Phase 2 is complete.** The platform now has standards-compliant OIDC authentication, no pinned key material, environment-shaped URLs in local, auth-specific observability, and a documented security posture. See [SECURITY-CHECKLIST.md](./SECURITY-CHECKLIST.md) for the canonical hardening list.

---

## 8. RBAC — admin vs consumer

The realm carries two flavours of client today:

| Client | Default scopes | Used for |
|---|---|---|
| `guva-reference` | `verify:citizen`, `audit:read` | Smoke-test consumer; reaches /scopes (legacy) + audit reads only |
| `guva-platform-admin` | `audit:read`, `admin:consumers`, `admin:scopes`, `admin:audit`, `admin:keys`, `admin:webhooks` | Platform operators; reaches consumer admin + bulk audit export |

Admin scopes added in this slice and what they gate:

| Scope | Gates |
|---|---|
| `admin:consumers` | `POST /v1/identity/consumers`, `GET /v1/identity/consumers/{id}` |
| `admin:scopes` | `GET /v1/identity/scopes` (legacy `audit:read` still accepted via OR semantics for the dev `guva-reference` client; collapse to admin-only in prod) |
| `admin:audit` | `GET /v1/audit/export` (bulk signed bundles) |
| `admin:keys` | (reserved) signing-key rotation endpoints, when added |
| `admin:webhooks` | (reserved) cross-consumer webhook inspection + replay |

Enforcement is via [`pkg/platform/auth.RequireAnyScope`](../pkg/platform/auth/auth.go) at each route. Scopes added to the realm at boot via [`tools/scripts/seed-keycloak.sh`](../tools/scripts/seed-keycloak.sh) (Admin API; idempotent so re-running is a no-op). `make up` invokes the seed; `make seed-keycloak` re-runs on demand.

**Test it:** Bruno requests `00b Get Admin Token` in both `services/identity/bruno` and `services/audit/bruno` pull a token from `guva-platform-admin`; the privileged requests (`03 List Scopes`, `04 Create Consumer`, `05 Get Consumer`, `07 Export Bundle`) consume `{{adminAccessToken}}`. Run them with the consumer-flavoured `{{accessToken}}` from `00 Get Token` instead and you'll get the expected `403 insufficient_scope`.

---

## 9. Workload mTLS

The platform supports per-service TLS with mandatory client certs (mTLS) on the server side. Wired today as an opt-in capability via env vars; production adoption pattern is service-mesh auto-mTLS (Istio / Linkerd at the pod sidecar).

### Capability shipped

| Layer | Where | What it does |
|---|---|---|
| Cert minting | [`tools/scripts/mint-service-certs.sh`](../tools/scripts/mint-service-certs.sh) | One-shot dev CA under `.cache/dev-ca/`, leaf certs per service under `.cache/certs/<svc>/`. Idempotent; `--force` to rotate a leaf. |
| Loader | [`pkg/platform/tlsbundle`](../pkg/platform/tlsbundle/tlsbundle.go) | `Load(cfg)` reads cert/key/CA from disk; `ServerConfig()` returns `*tls.Config` with `RequireAndVerifyClientCert` + TLS 1.3 min. |
| Server | [`pkg/platform/httpserver`](../pkg/platform/httpserver/server.go) | `Config.TLS *tls.Config` + `ListenAndServeAny(srv)` — TLS when configured, plain HTTP otherwise; same call site. |
| Identity | [`services/identity/cmd/server/main.go`](../services/identity/cmd/server/main.go) | Reads `GUVA_TLS_CERT` / `GUVA_TLS_KEY` / `GUVA_TLS_CA` env vars; if all set, enables mTLS. |

### Try it (the demonstration)

```bash
make mint-service-certs   # if not already done

GUVA_TLS_CERT=$(pwd)/.cache/certs/identity/cert.pem \
GUVA_TLS_KEY=$(pwd)/.cache/certs/identity/key.pem \
GUVA_TLS_CA=$(pwd)/.cache/certs/identity/ca.pem \
  make run-identity

# Plain HTTP — refused (server speaks TLS now):
curl -i http://localhost:7071/healthz
#   HTTP/1.1 400 Bad Request

# HTTPS without client cert — TLS handshake fails:
curl -k https://localhost:7071/healthz
#   curl: (35) error:0A00045C:SSL routines::tlsv13 alert certificate required

# HTTPS WITH client cert — 200:
curl -k --cert .cache/certs/identity/cert.pem \
        --key  .cache/certs/identity/key.pem \
        --cacert .cache/certs/identity/ca.pem \
        https://localhost:7071/healthz
#   {"status":"alive"}
```

### What's deliberately not wired (and why)

APISIX → identity over mTLS is **not** enabled in the standard dev flow. Two reasons:

1. **APISIX standalone YAML inlines PEMs.** The gateway's bind-mount file would need 60+ lines of PEM per upstream, and we've already hit one bind-mount truncation bug on macOS (`docs/OPERATIONS.md §2`). Inlining is fragile and not worth the brittleness for dev.
2. **The production answer isn't APISIX-managed mTLS.** It's a service mesh (Istio, Linkerd) doing auto-mTLS between pods, where neither the app nor the gateway needs cert config — sidecars handle it transparently. The platform code we shipped here is the right primitive for that future (the loader + server config), and **already runs unchanged inside a meshed pod** because the mesh mounts the cert at the same path conventions.

If you want APISIX → identity mTLS in dev today, the wiring is:

- Mount `.cache/dev-ca/ca.crt` into the apisix container.
- Add `apisix.ssl.ssl_trusted_certificate: /usr/local/apisix/conf/ca/dev-ca.crt` to `config.yaml`.
- On the `identity-public` route in `apisix.yaml`, set `upstream.scheme: https` and embed the APISIX cert + key as PEM literal blocks under `upstream.tls`.
- Restart the gateway and identity (with the env vars from the demo above).
- Re-run `make check-apisix` to confirm the bind-mount survived.

### Production migration

Pick **one** of:

- **Service mesh** (recommended): Istio or Linkerd, mTLS in PERMISSIVE then STRICT mode, no app changes needed; the [tlsbundle](../pkg/platform/tlsbundle/tlsbundle.go) package is unused at runtime because the sidecar terminates and originates TLS.
- **SPIFFE/SPIRE workload identity**: each service mounts its SVID; [tlsbundle](../pkg/platform/tlsbundle/tlsbundle.go) reads it the same way it reads the dev cert today. Add a SPIRE agent sidecar; the GUVA service code doesn't change.
- **Vault PKI + cert-manager**: certs minted by Vault's PKI engine, mounted via cert-manager-issued secrets. Same loader interface.

---

## 10. Open questions and follow-ups

- **Citizen-facing auth code flow** — not implemented; needs PKCE + session handling at APISIX or a dedicated front-end gateway.
- **`api.guva.localhost`** — currently the API is on `localhost:8000`. Routing it through Caddy at `https://api.guva.localhost` is a small follow-up but means more env var changes; deferred to keep this slice focused.
- **Vault as a backing store for client secrets** — `guva-reference`'s client secret is in the realm export today. Phase 3 should source it from Vault and inject at Keycloak boot.
- **Token introspection vs JWT-only validation** — JWT-only (signature + claims) is what we do now. Introspection adds a round-trip per request but enables instant revocation; revisit when the platform's threat model requires it.
- **Audience claim semantics** — APISIX checks `azp` against `client_id`. For multi-audience scenarios, switch to checking the `aud` claim properly.

---

## 11. Cross-references

- [DEVELOPMENT.md](./DEVELOPMENT.md) — developer-facing walkthrough.
- [../../guva-docs/05-security/10-security-architecture.md](../../guva-docs/05-security/10-security-architecture.md) — platform-wide security architecture.
- [../../guva-docs/03-architecture/07-system-architecture.md §7.2.1, §7.2.2](../../guva-docs/03-architecture/07-system-architecture.md) — API gateway and Identity Provider components.
- [../../guva-docs/09-delivery/03-task-list.md WS2-*](../../guva-docs/09-delivery/03-task-list.md) — Identity workstream tasks.
