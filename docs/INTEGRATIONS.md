# Integration Layer — Architecture

How GUVA reaches each upstream system of record (NIRA, URSB, URA,
Lands, UNEB, MoH), and how every agency adapter ships the same
contract to the rest of the platform.

The first reference adapter — **NIRA** — is live; the other five
agencies follow the same template. This document is the spec for
"how do I add agency N+1."

---

## 1. Boundary

```text
              ┌──────────── platform side ────────────┐
              │                                       │
              │   services/verification               │
              │     │ (nira.Adapter HTTP client)      │
              │     │                                 │
              │     ▼                                 │
              │   services/integrations/<agency>      │
              │     │                                 │
              │   ──┴── INTERNAL BOUNDARY ──          │
              │     │                                 │
              │     ▼                                 │
              │   backend selector:                   │
              │     - simulator (in-memory canned)    │
              │     - upstream  (real agency HTTPS)   │
              │                                       │
              └───────────────┬───────────────────────┘
                              │ mTLS + request signing
                              ▼
                  ── EXTERNAL BOUNDARY ──
                              │
                              ▼
                  Agency endpoint (NIRA, URSB, ...)
```

The integration service is the **only** place in the platform that
knows an agency's wire format, auth scheme, error codes, or
endpoint paths. Everything else codes against the integration's
canonical record shape; the verification service translates
between agency-canonical and verification-canonical via per-agency
adapters in `services/verification/internal/<agency>/`.

This is the single point where production "live NIRA" reads
disambiguate from dev/simulator reads. Flip `NIRA_BACKEND=upstream`,
provide cert material, and the same downstream call flow works.

---

## 2. The reference adapter — NIRA

Service: [`services/integrations/nira`](../services/integrations/nira/)
Listening: `:7080` (internal-only; no APISIX route)
Database: `guva_integrations_nira`
Audit events: `nira.lookup.requested` (per call, with hashed subject)

### Two backends

| Backend | Where defined | When to use |
|---|---|---|
| `simulator` | [`internal/backend/simulator.go`](../services/integrations/nira/internal/backend/simulator.go) | dev, integration tests, platform demos — 5 fictional citizens spanning every status enum |
| `upstream` | [`internal/backend/upstream.go`](../services/integrations/nira/internal/backend/upstream.go) | production (or staging against NIRA's sandbox once available) — mTLS, retries, circuit breaker, span emission |

### What the upstream backend implements

The upstream is the reference for every other agency. Every piece
below is part of the agency-side production posture:

| Concern | How |
|---|---|
| Workload identity | mTLS via `pkg/platform/tlsbundle.Load()` — same loader other services use |
| TLS posture | Minimum TLS 1.3, server-cert verified against agency-issued CA |
| Network resilience | Configurable `MaxAttempts` × exponential backoff (`BackoffBase × 2^attempt-1`) |
| Fast-fail when down | Circuit breaker (closed → open after N consecutive failures → half-open after window → closed on probe success / re-open on probe failure) |
| Per-attempt observability | OpenTelemetry `nira.lookup` spans; child spans per HTTP attempt; `attempt_failed` events on retries |
| Wire-format translation | Inline `wireRecord` struct + `decodeRecord` — change these two for a new agency |
| Status mapping | `mapStatus(agency-string) → canonical.Status` |
| Error classification | `doLookup` returns `retry: bool` so caller distinguishes transient vs terminal failure |

### What the integration service handles (regardless of backend)

| Concern | How |
|---|---|
| Lookup audit log | `lookup_log` table — backend, caller, hashed subject, status, latency, correlation_id |
| Chain emission | `nira.lookup.requested` event per call, hashed subject, no PII in `data` |
| Health probes | Standard `/healthz` + `/readyz` + a backend-specific `/backend` |
| Auth | OIDC bearer (verify:citizen scope) enforced at the handler layer |
| Connection pool | pgxpool against own database |

---

## 3. Audit chain — three actors per consumer call

A single consumer-facing verify call produces three audit chain
entries, each from a different service:

```text
   consumer
     │
     ▼
   verification.citizen.queried   ← actor: verification    (the consumer-facing record)
   consent.verified               ← actor: consent         (the authorisation check)
   nira.lookup.requested          ← actor: integrations-nira  (the agency call)
```

Every subject identifier is the **hashed NIN** (or the grant id for
consent.verified). The platform never lands a raw NIN on the
audit chain or in any DB.

External auditors get the full trail from a single source — the
audit-chain export. They don't need to correlate across services
to prove "consumer X verified citizen Y, with consent Z, against
NIRA, at time T, with outcome O".

---

## 4. Verification's two NIRA paths

Verification ([`services/verification/cmd/server/main.go`](../services/verification/cmd/server/main.go))
selects its adapter at startup via `NIRA_MODE`:

| Mode | What it does | When to use |
|---|---|---|
| `mock` | In-process [`nira.NewMock()`](../services/verification/internal/nira/mock.go) — same 5 canned records | Unit tests, zero-dependency dev |
| `integration` | HTTP client [`nira.NewHTTPClient()`](../services/verification/internal/nira/client.go) → integration service | Local end-to-end + every non-dev environment |

The mock and the integration's simulator backend ship the same 5
records, so verification's existing tests still pass when run
against the integration service.

The `live` mode (verification talking to NIRA directly, bypassing
the integration) is intentionally rejected — the integration
boundary is where production-shaped agency code lives and it would
be wrong to leak it into verification.

---

## 5. Adding a new agency — the playbook

When you stand up URSB (business registration), follow the NIRA
template exactly:

1. **New service directory**: `services/integrations/<agency>/`.
2. **Copy** `services/integrations/nira/` as a template. Rename
   the module path + package names.
3. **Define your canonical record** in `internal/canonical/`. One
   struct per agency — verification will translate it.
4. **Build the simulator** with a small canned data set covering
   every status the upstream can return.
5. **Build the upstream backend** by editing three things in
   `internal/backend/upstream.go`:
   - `wireRecord` struct + `decodeRecord` (the agency's JSON/SOAP shape)
   - auth scheme inside `doLookup` (add HMAC / JWT / API-key headers)
   - `mapStatus` table (agency status codes → canonical)
6. **Server handler** — usually just rename `lookupHandler` and the
   audit event type (`<agency>.lookup.requested`).
7. **Migration** — same `lookup_log` + `audit_outbox` schema, new
   DB (`guva_integrations_<agency>`).
8. **Postgres init** — add the database to
   [`deploy/compose/postgres/initdb.d/00-databases.sql`](../deploy/compose/postgres/initdb.d/00-databases.sql).
9. **Vault** — seed `services/integrations-<agency>/config:db-password`.
10. **Makefile + nested migration target** — already handles
    `services/integrations/*` automatically; nothing to add.
11. **Verification-side adapter** — a new file
    `services/verification/internal/<agency>/client.go` translating
    the agency's canonical record into the verification engine's
    per-endpoint response shape (e.g. business verification for URSB,
    tax verification for URA).
12. **OpenAPI + Bruno + docs** — copy the structure.

Three things will be agency-specific:

- the wire format
- the auth scheme (in practice each agency has different MTLS + token requirements)
- the status enum

Everything else — outboxing, retry, circuit breaker, audit chaining,
observability, health probes, RBAC, the `lookup_log` schema — is
genuinely the same pattern and can be lifted from NIRA.

---

## 6. Production handoff — flipping the upstream switch

When the NIRA agreement lands and you have:

- the production endpoint URL
- a client cert + key issued by NIRA
- the CA that signs NIRA's server cert

Set:

```bash
export NIRA_BACKEND=upstream
export NIRA_UPSTREAM_URL=https://api.nira.go.ug
export NIRA_UPSTREAM_CERT=/etc/guva/nira/client.crt
export NIRA_UPSTREAM_KEY=/etc/guva/nira/client.key
export NIRA_UPSTREAM_CA=/etc/guva/nira/agency-ca.crt
```

Restart the integration service. Verify with `/backend` — should
return `{"backend":"upstream"}`. Smoke-test one known-good NIN
(from the NIRA-provided test fixtures, NOT the simulator's
fictional NINs). The verification service code path is unchanged
— it still calls the integration; the integration's upstream
backend does the real call.

If the NIRA agreement provides different field names or different
status codes than the wireRecord struct assumes:
[`internal/backend/upstream.go`](../services/integrations/nira/internal/backend/upstream.go)
is where those changes go. Re-read this doc's §5 step 5.

---

## 7. Open follow-ups

- **Request signing**: NIRA may require HMAC or JWS on every request.
  The hook is in `upstream.doLookup` — a single signing block before
  `u.http.Do(req)`. Stubbed in code with a comment.
- **Per-route retry policy**: today the policy is per-service; some
  agencies may want different retry budgets per endpoint.
- **Adapter SDK**: once 3+ agency adapters ship, factor the retry +
  circuit-breaker code into `pkg/platform/agencyclient`. Premature
  abstraction with only one in flight.
- **Bulk lookups**: some agencies expose `/v1/citizens?nin=A,B,C`.
  No call site needs it yet; add when the verification flow can
  batch.
- **Per-agency status dashboards**: each adapter emits its own
  Prometheus metrics (latency, error rate, breaker state). Wire
  these into Grafana once two agencies are live.

---

## 8. Cross-references

- [`services/integrations/nira/api/openapi.yaml`](../services/integrations/nira/api/openapi.yaml) — endpoint contract.
- [`services/integrations/nira/bruno/NIRA/`](../services/integrations/nira/bruno/NIRA/) — 6 ready-to-run requests.
- [`services/verification/internal/nira/client.go`](../services/verification/internal/nira/client.go) — verification-side HTTP client.
- [`docs/VERIFICATION.md`](VERIFICATION.md) — how verification consumes the integration.
- [`docs/AUTH.md`](AUTH.md) — the `verify:citizen` scope guards the lookup endpoint.
- [`docs/AUDIT.md`](AUDIT.md) — chain semantics for `nira.lookup.requested`.
