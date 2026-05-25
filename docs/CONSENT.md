# Consent Management — Architecture

How GUVA represents citizen authorisation for verification calls,
and how that authorisation remains trustworthy under audit.

This document tracks the first vertical slice (`services/consent` +
the platform-side `pkg/platform/consent` client + the integration
into `services/verification`). The fuller workstream adds a
citizen-facing dashboard, grant templates, and policy automation —
those come once the verification + integration layers are filled
out and the citizen UX team starts.

---

## 1. Architecture at a glance

```text
   citizen (today: admin proxy)              consumer (e.g. guva-reference)
       │                                              │
       │ POST /v1/consent/grants                      │ POST /v1/verify/citizen
       │   {citizen_nin, consumer_id,                 │   {nin, ..., consent_reference}
       │    upstream, purpose, attrs, ttl}            │
       │                                              │
       ▼                                              ▼
   ┌─── services/consent ────┐               ┌─── services/verification ───┐
   │                         │               │                              │
   │  hash NIN               │               │  hash NIN                    │
   │  generate id            │               │  ┌─────────────────────────┐ │
   │  build signed assertion │               │  │ if consent_reference is │ │
   │  INSERT consent_grants  │               │  │   set:                  │ │
   │  emit consent.granted   │               │  │   GET /grants/{ref}/    │ │
   │                         │               │  │       verify            │ │
   └──────────┬──────────────┘               │  │     (pkg/platform/      │ │
              │ Kafka                        │  │      consent.Client)    │ │
              ▼                              │  │   if != granted →       │ │
       services/audit                        │  │     short-circuit       │ │
              │                              │  │     status =            │ │
              ▼                              │  │     consent_invalid     │ │
       audit chain (hash-linked)             │  └─────────────────────────┘ │
                                             │  call NIRA adapter           │
                                             │  emit verification.citizen.  │
                                             │       queried (chained)      │
                                             └──────────────────────────────┘
```

The verification service depends only on `pkg/platform/consent.Client`;
the actual HTTP call is a one-line `client.VerifyGrant(...)`. The
client is constructed in verification's `cmd/server/consent.go` with
a token-cache adapter so each verify call doesn't pay the
client-credentials roundtrip.

---

## 2. The grant record

```sql
consent_grants:
  id                    UUID PRIMARY KEY
  citizen_subject_type  VARCHAR   -- "nin"
  citizen_subject_hash  CHAR(64)  -- SHA-256(NIN), same recipe as verification
  consumer_id           VARCHAR
  upstream              VARCHAR   -- "NIRA" / "URSB" / ...
  purpose               TEXT      -- "loan-application", etc
  allowed_attributes    TEXT[]    -- subset of canonical citizen fields, or {"*"}
  granted_at            TIMESTAMPTZ
  expires_at            TIMESTAMPTZ
  revoked_at            TIMESTAMPTZ  -- NULL when active
  revocation_reason     TEXT
  assertion_jwt         TEXT      -- the Ed25519-signed assertion
  signing_key_id        VARCHAR   -- first 8 hex chars of SHA-256(public_key)
```

**Append-only enforcement.** A `BEFORE UPDATE OR DELETE` trigger
RAISEs unless the mutation is the **single allowed transition**:
`revoked_at IS NULL → NOT NULL` (and `revocation_reason` may also
fill in). Every other field is locked at insert. DELETE is
unconditionally forbidden. This survives even direct DBA access —
the trigger fires regardless of session.

**PII discipline.** The citizen's NIN never lands here. We store only
`SHA-256(NIN)`, using the same recipe (`services/verification/internal/store.HashSubject`)
the verification service uses, so cross-service joins via the hash
work without ever putting the NIN on disk in two places.

---

## 3. The signed assertion

Every grant ships with a JWT-like assertion signed by the consent
service's Ed25519 key:

```
base64url(header) . base64url(payload) . base64url(Ed25519_sig)
```

Where:

- `header` = `{"alg":"Ed25519","kid":"<key id>"}`
- `payload` = the substantive grant fields (grant_id, iss, iat, exp,
  citizen_subject_hash, consumer_id, upstream, purpose,
  allowed_attributes)
- `sig` = `Ed25519(header_b64 + "." + payload_b64)`

The verification service includes this assertion in the audit chain
entry for every `verification.citizen.queried` event (Phase B work —
not yet wired today; the assertion is fetched on every consent
verify and stored in the verification log). An external regulator
who later inspects the chain can:

1. Pull the assertion text out of the audit detail.
2. Fetch the consent service's public key from `/v1/consent/signing-key`.
3. Verify the signature locally — no further call into GUVA needed.

This is the load-bearing trust property: **the chain doesn't just
say "consent was checked"; it carries cryptographic proof that the
platform asserted the grant existed with these terms at that time.**

Key rotation: the `kid` (key id) is the first 8 hex chars of
SHA-256(public_key). When a new key is seeded, old assertions remain
verifiable as long as a verifier has the historical public key. A
`/signing-keys` listing endpoint with rotation history is a Phase B
follow-up.

---

## 4. Verify outcomes

The `/grants/{id}/verify` endpoint returns one of:

| Outcome | Meaning | Verification service maps to |
|---|---|---|
| `granted` | grant exists, active, consumer matches, attribute set is a subset | proceed to NIRA call |
| `expired` | grant exists, past `expires_at` | `consent_invalid` |
| `revoked` | grant exists, `revoked_at` is set | `consent_invalid` |
| `consumer_mismatch` | grant exists, different consumer | `consent_invalid` |
| `attribute_not_allowed` | requested attribute set not a subset of allowed | `consent_invalid` |
| `not_found` | no such grant | `consent_invalid` |

Verification returns the underlying outcome in `consent_outcome`
field of the response when status is `consent_invalid`, so the
consumer knows whether to refresh their consent (revoked / expired)
or expand the grant (attribute_not_allowed).

---

## 5. Audit emissions

Three event types land on the chain:

- `consent.granted` — every new grant.
- `consent.revoked` — every revoke transition.
- `consent.verified` — every `/verify` call (one per consumer attempt).

All three have the same PII shape as the verification service:
subject is the **citizen's hashed NIN** for grant / revoke;
**grant id** for verify (since verify is about the grant, not
directly the citizen). `data` carries grant_id, consumer_id,
upstream, purpose, allowed_attributes — NEVER the raw NIN.

Run-through:

```bash
ADMIN=$(curl -sk -X POST https://auth.guva.localhost/realms/guva/protocol/openid-connect/token \
  -d grant_type=client_credentials \
  -d client_id=guva-platform-admin \
  -d client_secret=platform-admin-dev-secret \
  | jq -r .access_token)

# Grant
curl -X POST http://localhost:8000/v1/consent/grants \
  -H "Authorization: Bearer $ADMIN" -H "Content-Type: application/json" \
  -d '{"citizen_nin":"CF95071587654003","consumer_id":"guva-reference",
       "upstream":"NIRA","purpose":"demo","allowed_attributes":["nin","given_name"],
       "ttl":"1h"}'

# Verify (called internally by verification service; you can hit
# it directly to see the dance)
curl "http://localhost:8000/v1/consent/grants/<id>/verify?consumer_id=guva-reference&attributes=nin"

# Revoke
curl -X POST http://localhost:8000/v1/consent/grants/<id>/revoke \
  -H "Authorization: Bearer $ADMIN" -d '{"reason":"demo"}'
```

The cross-service flow (consent → verification) leaves a chain trail
ordered by entry_id where `consent.verified` precedes
`verification.citizen.queried` for every consumer call.

---

## 6. RBAC

| Scope | Granted to | Endpoints |
|---|---|---|
| `consent:write` | citizen-facing app (today: admin proxy via `guva-platform-admin`) | POST /grants, POST /grants/{id}/revoke |
| `consent:read` | citizen dashboard, admin | GET /grants/{id} |
| `verify:citizen` | verification service (carried by guva-reference today) | GET /grants/{id}/verify |

The `/signing-key` endpoint is unauthenticated by design — verifiers
may need the public key without holding a platform token.

In production the citizen-facing grant flow uses a citizen
authentication flow (authorisation code with PKCE, see
[`docs/AUTH.md` §2.2](AUTH.md#22-issuance-authorisation-code-with-pkce-citizen-facing));
the dev demo uses the platform-admin token as a proxy.

---

## 7. Open follow-ups (Phase B)

- **Assertion-in-audit-detail**: include the assertion JWT in the
  `verification.citizen.queried` audit detail so external proof
  doesn't require calling consent at audit-replay time.
- **Pubkey history endpoint**: `/signing-keys` listing every key
  that has ever been active with valid-from timestamps, so
  verifiers of older assertions can find the right key.
- **Grant templates**: pre-canned `(purpose, attribute set, ttl)`
  combinations citizens can grant in one click via the dashboard.
- **Policy-driven attribute allow-lists**: per-consumer policy that
  caps what attributes a grant can include, regardless of what the
  citizen-facing UI offers.
- **Citizen dashboard**: 1.3.8 work; the API surface is ready.

---

## 8. Cross-references

- [`services/consent/api/openapi.yaml`](../services/consent/api/openapi.yaml) — endpoint contract.
- [`services/consent/bruno/Consent/`](../services/consent/bruno/Consent/) — 7 ready-to-run requests.
- [`pkg/platform/consent/client.go`](../pkg/platform/consent/client.go) — the in-process client other services depend on.
- [`docs/VERIFICATION.md`](VERIFICATION.md) §5 — how verification consumes consent.
- [`docs/AUDIT.md`](AUDIT.md) §4 — event naming convention; consent.* sits alongside verification.*, identity.*, audit.*, apisix.*.
- [`docs/AUTH.md`](AUTH.md) §8 — RBAC scopes (consent:read, consent:write added).
