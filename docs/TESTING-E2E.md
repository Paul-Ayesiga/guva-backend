# GUVA — End-to-End Testing Manual (no UI)

A hands-on walkthrough for exercising the GUVA platform from a clean
boot to a fully-chained verification, using only `curl` (or Bruno).
Nothing in this document depends on the citizen portal.

> **Why no UI?** The `guva-citizen-portal` reference app is an
> internal/dev tool. The production citizen surface is the `ask-uganda`
> assistant (a separate repo), which talks to GUVA as an ordinary
> consumer through its own `guva-gateway`. For acceptance, demos, and
> regression — and for anyone integrating with GUVA — the API is the
> contract. This manual exercises the API directly.

---

## 0. What you'll do

By the end of this manual you will have:

1. Brought up the full local stack (Postgres, Kafka, APISIX, Keycloak,
   Caddy, Vault, audit, consent, verification, NIRA simulator,
   webhooks, identity).
2. Obtained both a **reference-consumer** token and an **admin** token
   from Keycloak.
3. **Registered a new consumer** through identity (creates a Keycloak
   client and lands an `identity.consumer.created` audit entry).
4. **Issued a consent grant** (admin-proxied) for one of the simulator
   NINs, with an Ed25519-signed assertion.
5. **Run a verification** as that consumer, watching:
   - the consent service short-circuit when the grant is wrong, and
   - the NIRA simulator return per-attribute booleans when it's right.
6. **Walked the audit chain** to see `consent.granted` →
   `consent.verified` → `verification.citizen.queried` land in order,
   each hash-linked to the previous.
7. **Verified chain integrity** and **exported a signed bundle** any
   regulator can verify offline.
8. **Subscribed a webhook**, received an HMAC-signed delivery, and
   inspected the dead-letter behaviour.
9. **Proven the PII-discipline property** by inspecting raw rows in
   Postgres — confirming no raw NIN landed in the audit chain or in
   the consent or verification tables.
10. **Exercised every documented failure mode** (mismatch, deceased,
    revoked, not-found, expired consent, attribute-not-allowed, scope
    denial, gateway 401).

Plan a 30–45-minute session the first time; afterwards every loop is
under a minute.

---

## 1. Prerequisites

| Tool | Why |
|---|---|
| Docker Desktop ≥ 4.20 (or compatible) | Runs the compose stack |
| GNU `make` | Wraps every workflow |
| Go ≥ 1.22 | Runs the host-side services (audit, consent, verification, webhooks, identity, integrations/nira) |
| `curl`, `jq`, `python3` | Token extraction + JSON inspection |
| (optional) Bruno | Ready-made request collections under `services/*/bruno/` |
| (optional) `psql` client | Connect to `postgres://guva:guva@localhost:5432/guva` for the PII checks |

The Makefile has helpful targets — `make help` prints the catalogue.

---

## 2. Bring the stack up

```bash
cd guva-backend
make bootstrap         # one-time: copies .env, pulls images, sanity checks
make up                # 17 containers + plugin checks + Vault/Keycloak/schema seed
make urls              # prints every endpoint
make status            # docker compose ps
```

Then run the host-process services (each in its own terminal, or
backgrounded with `&`):

```bash
make run-reference &
make run-identity &
make run-audit &
make run-apisix-adapter &
make run-webhooks &
make run-verification &
make run-consent &
(cd services/integrations/nira && go run ./cmd/server) &
```

Sanity check:

```bash
curl -fsS http://localhost:7070/healthz   # reference
curl -fsS http://localhost:7071/healthz   # identity
curl -fsS http://localhost:7072/healthz   # audit
curl -fsS http://localhost:7074/healthz   # webhooks
curl -fsS http://localhost:7075/healthz   # verification
curl -fsS http://localhost:7076/healthz   # consent
curl -fsS http://localhost:7080/healthz   # nira integration
curl -fsS http://localhost:8000/healthz   # APISIX gateway
```

All eight should return `200`. If any fail, see `docs/DEVELOPMENT.md`
§"Troubleshooting".

> **TLS note.** Keycloak is reachable via Caddy at
> `https://auth.guva.localhost` (run `make trust-ca` once) **and** at
> the raw `http://localhost:8080` (bypasses Caddy; useful for
> scripts that can't trust the dev CA). The Makefile's `make token`
> uses the Caddy URL; the manual curl below uses the raw URL to keep
> the trust-store step optional.

---

## 3. Get tokens

GUVA's authn is OAuth 2.0 client-credentials against Keycloak. Two
clients are seeded by default:

| Client | Secret | Default scopes |
|---|---|---|
| `guva-reference` | `reference-dev-secret` | `verify:citizen`, `audit:read` |
| `guva-platform-admin` | `platform-admin-dev-secret` | `audit:read`, `admin:consumers`, `admin:scopes`, `admin:audit`, `admin:keys`, `admin:webhooks`, `consent:read`, `consent:write` |

```bash
# Consumer token (verifies citizens, reads audit)
CONSUMER=$(curl -fsS -X POST http://localhost:8080/realms/guva/protocol/openid-connect/token \
  -H 'Content-Type: application/x-www-form-urlencoded' \
  -d grant_type=client_credentials \
  -d client_id=guva-reference \
  -d client_secret=reference-dev-secret \
  | jq -r .access_token)

# Admin token (creates consents, registers consumers, exports bundles)
ADMIN=$(curl -fsS -X POST http://localhost:8080/realms/guva/protocol/openid-connect/token \
  -H 'Content-Type: application/x-www-form-urlencoded' \
  -d grant_type=client_credentials \
  -d client_id=guva-platform-admin \
  -d client_secret=platform-admin-dev-secret \
  | jq -r .access_token)

echo "consumer token: ${CONSUMER:0:40}..."
echo "admin token:    ${ADMIN:0:40}..."
```

A token expires in 5 minutes (Keycloak default). Re-issue when an
endpoint returns `401`.

### What's in the token

```bash
echo "$CONSUMER" | cut -d. -f2 | base64 -d 2>/dev/null | jq '{azp, scope, exp}'
```

Expect `azp = "guva-reference"`, `scope = "verify:citizen audit:read"`.
APISIX enforces scopes at the gateway; the services double-check.

---

## 4. The simulator NIN catalogue

The NIRA simulator has 5 fixtures, one per status enum:

| NIN | Identity | Status |
|---|---|---|
| `CM91051512345001` | Sarah Nansubuga Nakato (DOB 1991-05-15, F) | active |
| `CM85031298765002` | John Wasswa Mukasa (DOB 1985-03-12, M) | active |
| `CF95071587654003` | Grace Akello Achieng (DOB 1995-07-15, F) | active |
| `CM72042098765004` | Patrick Kato Ssali (DOB 1972-04-20, M) | deceased |
| `CM88010198765005` | David Ojok Okello (DOB 1988-01-01, M) | revoked |

These are **fictional**. They do not correspond to any real Ugandan
and are not valid against any production system.

Use these for every example below.

---

## 5. The canonical end-to-end loop

This is the load-bearing scenario. Six steps, three services, one
audit chain.

```text
            (5.1)                   (5.2)                       (5.3)
        admin creates           verify with the              audit shows
        consent grant     →     consent_reference     →      consent.granted
        (sig: Ed25519)          (consent.verified           consent.verified
                                + verification.queried)     verification.queried
                                                            (hash-chained)
                                       │
                                       ▼
                                  (5.4) chain
                                  integrity walk
                                  (5.5) signed
                                  bundle export
                                  (5.6) revoke
                                  + re-verify =
                                  consent_invalid
```

### 5.1 Issue a consent grant

The admin token stands in for a citizen-side authentication today.
In production the citizen authenticates (ask-uganda chat consent
moment, or OIDC PKCE), then the platform proxies the grant.

```bash
GRANT=$(curl -fsS -X POST http://localhost:8000/v1/consent/grants \
  -H "Authorization: Bearer $ADMIN" \
  -H "Content-Type: application/json" \
  -d '{
    "citizen_nin": "CM91051512345001",
    "consumer_id": "guva-reference",
    "upstream":    "NIRA",
    "purpose":     "e2e-manual-test",
    "allowed_attributes": ["nin","given_name","surname","date_of_birth"],
    "ttl": "1h"
  }')

GRANT_ID=$(echo "$GRANT" | jq -r .id)
echo "grant: $GRANT_ID"
echo "$GRANT" | jq '{id, expires_at, allowed_attributes, signing_key_id, assertion_jwt: (.assertion_jwt | .[0:60] + "...")}'
```

Notice four properties:

- `citizen_subject_hash` is a 64-hex SHA-256 — the NIN **never lands
  on disk** in the consent service.
- `assertion_jwt` is `base64url(header).base64url(payload).base64url(sig)`,
  signed with consent's Ed25519 key. This is returned **only on
  create** (`POST /grants`) and re-fetched via `GET /grants/{id}`.
- `signing_key_id` is the first 8 hex chars of SHA-256(public_key) —
  use this to look up which key signed when keys rotate.
- A `consent.granted` event has just been written to the audit
  outbox; the drain worker flushes it within ~500ms.

### 5.2 Run a verification

```bash
curl -fsS -X POST http://localhost:8000/v1/verify/citizen \
  -H "Authorization: Bearer $CONSUMER" \
  -H "Content-Type: application/json" \
  -d "{
    \"nin\": \"CM91051512345001\",
    \"given_name\": \"Sarah\",
    \"surname\":    \"Nakato\",
    \"date_of_birth\": \"1991-05-15\",
    \"consent_reference\": \"$GRANT_ID\"
  }" | jq
```

Expect `status: "verified"` and every claimed attribute marked
`match: true`. Two important things happened server-side:

1. Verification called `GET /v1/consent/grants/$GRANT_ID/verify?consumer_id=guva-reference&attributes=nin,given_name,surname,date_of_birth` and got `status=granted`. That call emitted **`consent.verified`** on the chain.
2. Verification called the NIRA simulator at `http://localhost:7080/lookup` (internal, no gateway), built the canonical response, and emitted **`verification.citizen.queried`** on the chain. The audit detail records *which attribute keys* the caller asserted, never the values.

A repeat of the same request within `VERIFICATION_CACHE_TTL`
(default 15 min) returns `X-Guva-Cache: hit` and does not call NIRA;
it still re-emits `consent.verified` + `verification.citizen.queried`
so the chain reflects every consumer attempt.

### 5.3 Inspect the chain

```bash
curl -fsS "http://localhost:8000/v1/audit/entries?limit=10" \
  -H "Authorization: Bearer $ADMIN" \
  | jq '.entries[] | {entry_id, action, result, subject_id, detail: (.detail | tostring | .[0:120])}'
```

You should see (newest first):

```
verification.citizen.queried   ok    <hashed-NIN>   {match_count:4, mismatch_count:0, requested_attributes:["nin",...]}
consent.verified                ok    <grant-id>     {grant_id:..., outcome:"granted", consumer_id:"guva-reference"}
consent.granted                 ok    <hashed-NIN>   {grant_id:..., consumer_id:"guva-reference", upstream:"NIRA", purpose:"e2e-manual-test"}
audit.entries.queried           ok    ...            (meta-audit from the read you just did)
```

Each row carries `previous_hash` + `entry_hash`. The chain is
SHA-256 of canonical-row || previous-hash; entry 1 anchors to
`000...000`.

### 5.4 Walk the chain for integrity

```bash
curl -fsS "http://localhost:8000/v1/audit/verify?from_id=1&to_id=0" \
  -H "Authorization: Bearer $ADMIN" \
  | jq
```

Expect `{ "ok": true, "from_id": 1, "to_id": N }`. If anyone has
tampered with a row, the response is `{ ok:false, broken_at:M, ... }`
naming the first broken link.

### 5.5 Export a signed bundle

```bash
curl -fsS "http://localhost:8000/v1/audit/export?from_id=1&to_id=20" \
  -H "Authorization: Bearer $ADMIN" \
  | jq '{format_version, range_from_id, range_to_id, anchor, signing_pubkey, signature: (.signature | .[0:40] + "...")}'
```

The bundle carries:

- The chain range (capped at 500 entries per bundle).
- The anchor — `entry_hash` of the row immediately before the range
  (or 64×`0` if from_id=1).
- The Ed25519 public key + signature over canonical-JSON of the
  bundle with `signature` blanked.

A regulator can verify offline without ever calling back into GUVA.
Use the `audit-verify` tool in `tools/audit-verify/` (or
`pkg/platform/audit.VerifyBundle`).

### 5.6 Revoke and re-verify

Revoke the grant:

```bash
curl -fsS -X POST "http://localhost:8000/v1/consent/grants/$GRANT_ID/revoke" \
  -H "Authorization: Bearer $ADMIN" \
  -H "Content-Type: application/json" \
  -d '{"reason":"e2e revoke test"}' | jq '{revoked_at, revocation_reason}'
```

Re-run the verify from 5.2. Expect `status: "consent_invalid"` and
`consent_outcome: "revoked"`. NIRA was **not** called this time.

A new audit row appeared: `consent.revoked` (between your verifies),
plus another `consent.verified` (this one with `outcome: "revoked"`)
and `verification.citizen.queried` (with `result: "denied"`, the
chain still records the *attempt*).

That's the canonical loop end-to-end.

---

## 6. Service-by-service endpoint reference

Each section lists every endpoint, its scope requirement, and a copy-pasteable curl.

### 6.1 Reference (`:7070` / `/v1/reference/*`)

```bash
curl http://localhost:7070/healthz
curl -H "Authorization: Bearer $CONSUMER" http://localhost:8000/v1/reference/ping | jq
make ping   # shortcut
```

Reference is the dev pulse-check: gateway → service → token validation → 200.

### 6.2 Identity (`:7071` / `/v1/identity/*`)

Scope-protected. The catalogue and consumer registration:

```bash
# List scopes (no extra scope needed beyond a valid token)
curl -fsS -H "Authorization: Bearer $ADMIN" \
  http://localhost:8000/v1/identity/scopes | jq

# Register a new consumer (admin:consumers)
curl -fsS -X POST http://localhost:8000/v1/identity/consumers \
  -H "Authorization: Bearer $ADMIN" \
  -H "Content-Type: application/json" \
  -d '{
    "client_id":   "acme-bank",
    "agency_name": "Acme Bank",
    "scopes":      ["verify:citizen"],
    "description": "Acme Bank KYC integration"
  }' | jq

# Get a registered consumer
curl -fsS -H "Authorization: Bearer $ADMIN" \
  http://localhost:8000/v1/identity/consumers/acme-bank | jq
```

Registering a consumer creates a Keycloak client (so the new
`client_id`/`client_secret` can issue tokens) **and** emits
`identity.consumer.created` on the chain. To prove the new client
works, grab its secret from the response and issue a token:

```bash
curl -X POST http://localhost:8080/realms/guva/protocol/openid-connect/token \
  -d grant_type=client_credentials \
  -d client_id=acme-bank \
  -d client_secret=<from-response> | jq .access_token
```

### 6.3 Consent (`:7076` / `/v1/consent/*`)

```bash
# Create (consent:write)
curl -X POST http://localhost:8000/v1/consent/grants -H "Authorization: Bearer $ADMIN" -H "Content-Type: application/json" \
  -d '{"citizen_nin":"CF95071587654003","consumer_id":"guva-reference","upstream":"NIRA","purpose":"demo","allowed_attributes":["nin","given_name"],"ttl":"1h"}' | jq

# List grants for a citizen (consent:read OR consent:write)
HASH=$(printf 'CF95071587654003' | shasum -a 256 | cut -d' ' -f1)
curl -fsS -H "Authorization: Bearer $ADMIN" \
  "http://localhost:8000/v1/consent/grants?citizen_subject_hash=$HASH" | jq

# Get one
curl -H "Authorization: Bearer $ADMIN" \
  http://localhost:8000/v1/consent/grants/$GRANT_ID | jq

# Verify a grant (verify:citizen OR consent:read; called internally by verification)
curl -H "Authorization: Bearer $CONSUMER" \
  "http://localhost:8000/v1/consent/grants/$GRANT_ID/verify?consumer_id=guva-reference&attributes=nin,given_name" | jq

# Revoke (consent:write, idempotent)
curl -X POST http://localhost:8000/v1/consent/grants/$GRANT_ID/revoke \
  -H "Authorization: Bearer $ADMIN" -d '{"reason":"manual revoke"}' | jq

# Signing pubkey (unauth — verifiers need it without a token)
curl http://localhost:8000/v1/consent/signing-key | jq
```

### 6.4 Verification (`:7075` / `/v1/verify/*`)

Single endpoint today (citizen). Future agencies (business, tax,
land, education, health) follow the same canonical shape.

```bash
# Verified
curl -X POST http://localhost:8000/v1/verify/citizen -H "Authorization: Bearer $CONSUMER" -H "Content-Type: application/json" \
  -d '{"nin":"CM91051512345001","given_name":"Sarah","surname":"Nakato","date_of_birth":"1991-05-15","consent_reference":"'"$GRANT_ID"'"}' | jq

# Mismatch (one wrong attribute)
curl -X POST http://localhost:8000/v1/verify/citizen -H "Authorization: Bearer $CONSUMER" -H "Content-Type: application/json" \
  -d '{"nin":"CM91051512345001","surname":"NotNakato","consent_reference":"'"$GRANT_ID"'"}' | jq '.status,.attributes'

# Not found
curl -X POST http://localhost:8000/v1/verify/citizen -H "Authorization: Bearer $CONSUMER" -H "Content-Type: application/json" \
  -d '{"nin":"CM00000000000000","consent_reference":"'"$GRANT_ID"'"}' | jq '.status'

# Deceased override
curl -X POST http://localhost:8000/v1/verify/citizen -H "Authorization: Bearer $CONSUMER" -H "Content-Type: application/json" \
  -d '{"nin":"CM72042098765004","consent_reference":"'"$GRANT_ID"'"}' | jq '.status'

# Revoked override
curl -X POST http://localhost:8000/v1/verify/citizen -H "Authorization: Bearer $CONSUMER" -H "Content-Type: application/json" \
  -d '{"nin":"CM88010198765005","consent_reference":"'"$GRANT_ID"'"}' | jq '.status'
```

Pre-canned in Bruno: `services/verification/bruno/Verification/`.

### 6.5 Audit (`:7072` / `/v1/audit/*`)

```bash
# List entries (cursor + filters)
curl -H "Authorization: Bearer $ADMIN" \
  "http://localhost:8000/v1/audit/entries?limit=20&action=consent.granted" | jq

# Walk the chain integrity
curl -H "Authorization: Bearer $ADMIN" \
  "http://localhost:8000/v1/audit/verify?from_id=1&to_id=0" | jq

# Signed bundle export
curl -H "Authorization: Bearer $ADMIN" \
  "http://localhost:8000/v1/audit/export?from_id=1&to_id=50" > bundle.json
curl http://localhost:8000/v1/audit/export/pubkey | jq

# Merkle anchors over chain ranges (5-minute interval by default)
curl -H "Authorization: Bearer $ADMIN" \
  http://localhost:8000/v1/audit/anchors | jq

# Inclusion proof for a specific entry
curl -H "Authorization: Bearer $ADMIN" \
  "http://localhost:8000/v1/audit/anchors/1/proof?entry_id=3" | jq
```

### 6.6 Webhooks (`:7074` / `/v1/webhooks/*`)

End-to-end webhook test:

```bash
# 1. Start a receiver (any HTTP echo)
docker run --rm -d -p 9999:80 --name guva-wh-echo mendhak/http-https-echo:31

# 2. Subscribe (consumer self-service with webhooks:manage, or admin)
SUB=$(curl -fsS -X POST http://localhost:8000/v1/webhooks/subscriptions \
  -H "Authorization: Bearer $ADMIN" -H "Content-Type: application/json" \
  -d '{
    "consumer_id":"guva-reference",
    "target_url":"http://host.docker.internal:9999/",
    "event_type_patterns":["verification.*","consent.*"]
  }')
echo "$SUB" | jq '{id, secret: (.secret | .[0:20] + "..."), event_type_patterns}'

# 3. Cause an event (re-run the verify from 5.2)
# 4. Watch deliveries land
docker logs --tail=20 guva-wh-echo
curl -H "Authorization: Bearer $ADMIN" \
  "http://localhost:8000/v1/webhooks/subscriptions/$(echo "$SUB" | jq -r .id)/deliveries" | jq
```

The receiver should print an `X-Guva-Signature: sha256=<hmac>`
header on each POST. Recipe: `HMAC-SHA256(secret, raw_body)`.

```bash
docker stop guva-wh-echo
```

### 6.7 NIRA integration (`:7080`, internal only)

The verification service uses this on every citizen lookup. You can
poke it directly during development:

```bash
# Which backend is running? (simulator | upstream)
curl http://localhost:7080/backend | jq

# Lookup (needs verify:citizen — the consumer token has it)
curl -X POST http://localhost:7080/lookup -H "Authorization: Bearer $CONSUMER" -H "Content-Type: application/json" \
  -d '{"nin":"CM91051512345001"}' | jq
```

Toggle to the production-shaped upstream client (still pointing at
a local simulator unless you've wired a real NIRA endpoint):

```bash
NIRA_MODE=upstream make run-integrations/nira
```

See `docs/INTEGRATIONS.md` for the URSB/URA/Lands playbook —
adding a new agency is three changes inside `wireRecord`, the auth
scheme, and the status map.

---

## 7. Failure-mode test plan

Each row below has a specific expected response. If any of these
diverge, file the divergence — they encode the contracts.

| # | Action | Expected status | Audit row |
|---|---|---|---|
| F1 | Verify with no `Authorization` header | `401` from APISIX | none |
| F2 | Verify with consumer token but wrong scope (e.g. `audit:read`-only token) | `403 insufficient_scope` | `verification.access.denied` |
| F3 | Verify with no `nin` | `400 missing_nin` | none (request rejected pre-handler) |
| F4 | Verify with attribute outside the grant's `allowed_attributes` | `200 status:"consent_invalid"`, `consent_outcome:"attribute_not_allowed"` | `consent.verified` (denied) + `verification.citizen.queried` (denied) |
| F5 | Verify after grant `expires_at` passes | `200 status:"consent_invalid"`, `consent_outcome:"expired"` | same as F4 with `expired` |
| F6 | Verify after grant revoked | `200 status:"consent_invalid"`, `consent_outcome:"revoked"` | same as F4 with `revoked` |
| F7 | Verify with `consent_reference` that doesn't exist | `200 status:"consent_invalid"`, `consent_outcome:"not_found"` | same |
| F8 | Verify with token for a different `consumer_id` than the grant lists | `200 consent_outcome:"consumer_mismatch"` | same |
| F9 | NIRA simulator stopped → verify | `502 upstream_unavailable` | `verification.citizen.queried` (error) |
| F10 | POST `/grants` with `ttl` > 8760h | `400` | none |
| F11 | DELETE / UPDATE on `consent_grants` row in psql | DB raises `RAISE EXCEPTION` from append-only trigger | n/a — trigger fires before any change |
| F12 | Mutate a row in `audit_entries` in psql, then re-verify chain | `/v1/audit/verify` returns `{ok:false, broken_at:X, ...}` | `audit.chain.verified` (the meta-row records the failed walk) |

Pre-canned variants of F4–F8 live in
`services/consent/bruno/Consent/04 Verify Grant (denials).bru`.

---

## 8. The PII discipline check

This is the regulator-facing property the audit log must satisfy:
**no raw NIN appears on disk** in any persisted row.

```bash
# psql into the platform DB
docker compose exec -it postgres psql -U guva -d guva

# 1. The audit chain
\c guva_audit
SELECT detail FROM audit_entries WHERE action='verification.citizen.queried' ORDER BY entry_id DESC LIMIT 1;
-- Expect keys like match_count, mismatch_count, requested_attributes (the *keys*, not the values), upstream, consent_reference, verification_id.
-- Expect NOT to see: nin, given_name, surname, date_of_birth values.

-- 2. The consent table
\c guva_consent
SELECT citizen_subject_type, citizen_subject_hash, consumer_id, upstream, purpose FROM consent_grants ORDER BY granted_at DESC LIMIT 3;
-- Expect citizen_subject_type='nin', citizen_subject_hash=64-hex.
-- Expect NO column carrying the raw NIN.

-- 3. The verification log
\c guva_verification
SELECT subject_hash, status, requested_attributes FROM verification_log ORDER BY checked_at DESC LIMIT 3;
-- Same pattern: hashed subject only; requested_attributes is the key list.
```

If you see a raw NIN anywhere except in transient request memory,
that's a bug to file immediately.

---

## 9. Observability — confirming what you did

| Where | URL | What to check |
|---|---|---|
| Grafana | http://localhost:3000 (admin/admin) | Dashboards: "Audit — Log search", "Auth Overview". Filter logs by `service` + `trace_id`. |
| Jaeger | http://localhost:16686 | Traces for the verify call should span APISIX → verification → consent → NIRA. |
| Prometheus | http://localhost:9090 | `guva_verification_requests_total`, `guva_consent_grants_total`, `guva_audit_chain_appended_total`. |
| APISIX metrics | http://localhost:9091/apisix/prometheus/metrics | Per-route latency / status distribution. |
| Loki (via Grafana) | "Audit — Log search" | All-service log search by trace id. |
| Apicurio | http://localhost:8081 | Audit event schemas — used by services to validate emit shape. |
| Vault (raw) | http://localhost:8200 (token `dev-root-token`) | Dev secrets backing the services. |

A useful smoke check after running the 5.1–5.3 loop:

```bash
# Tail the latest 5 audit-chain entries directly out of Loki via Grafana,
# or quickly through Prometheus by checking the counter advanced:
curl -s http://localhost:9090/api/v1/query?query=guva_audit_chain_appended_total | jq '.data.result[0].value'
```

---

## 10. Pointing `ask-uganda` at this stack

The canonical citizen flow in production is *not* the GUVA citizen
portal — it's the ask-uganda assistant calling GUVA as a consumer.
To prove that loop end-to-end against this local stack:

```bash
# In ../ask-uganda-backend
export GUVA_GATEWAY_BACKEND=live
export GUVA_GATEWAY_BASE_URL=http://host.docker.internal:8000
export GUVA_GATEWAY_CLIENT_ID=ask-uganda
export GUVA_GATEWAY_CLIENT_SECRET=<from guva-backend Keycloak after registering the consumer>
make restart
make smoke
```

The `smoke.sh` script walks the ask-uganda → GUVA chain: a personal
question triggers a consent moment in the orchestration API, a
consent receipt is recorded via the GUVA gateway, and the next
re-ask returns a `verified_fact` message with the GUVA verification
result. Every GUVA call should appear in the audit chain alongside
your manual ones.

First-time setup: register `ask-uganda` as a consumer via §6.2,
note the secret, and use it in `GUVA_GATEWAY_CLIENT_SECRET`. Make
sure the new client has `verify:citizen` and `consent:write` in its
default scopes (today the seed has it on the admin client; you can
attach via `tools/scripts/seed-keycloak.sh` patterns).

---

## 11. Tearing down

```bash
# Stop host-side services (Ctrl-C each, or):
pkill -f 'go run ./cmd/server'

# Stop containers, keep volumes
make down

# Wipe everything (destructive)
make reset
```

---

## 12. Where to look next

| Topic | File |
|---|---|
| Auth model + scope catalogue | `docs/AUTH.md` |
| Audit chain semantics + key rotation | `docs/AUDIT.md` |
| Consent architecture + assertion details | `docs/CONSENT.md` |
| Verification engine internals | `docs/VERIFICATION.md` |
| Adding a new agency adapter | `docs/INTEGRATIONS.md` |
| Per-service endpoint contracts | `services/*/api/openapi.yaml` |
| Pre-canned request collections | `services/*/bruno/` |
| Local dev environment + troubleshooting | `docs/DEVELOPMENT.md` |
| Security checklist | `docs/SECURITY-CHECKLIST.md` |
| Production-vs-dev posture | `docs/ENVIRONMENTS.md` |

---

## Appendix A — One-shot smoke script

A copy-pasteable script that walks 5.1 → 5.3 with assertions. Save
as `tools/scripts/smoke-e2e.sh` and run after `make up` + the
host-side services are running.

```bash
#!/usr/bin/env bash
set -euo pipefail

KC=http://localhost:8080
GW=http://localhost:8000

token() {
  curl -fsS -X POST "$KC/realms/guva/protocol/openid-connect/token" \
    -d grant_type=client_credentials -d "client_id=$1" -d "client_secret=$2" \
    | jq -r .access_token
}

ADMIN=$(token guva-platform-admin platform-admin-dev-secret)
CONS=$(token  guva-reference     reference-dev-secret)

GRANT=$(curl -fsS -X POST "$GW/v1/consent/grants" \
  -H "Authorization: Bearer $ADMIN" -H "Content-Type: application/json" \
  -d '{"citizen_nin":"CM91051512345001","consumer_id":"guva-reference","upstream":"NIRA","purpose":"smoke","allowed_attributes":["nin","given_name","surname"],"ttl":"15m"}')
GID=$(jq -r .id <<<"$GRANT")
echo "✓ grant created: $GID"

V=$(curl -fsS -X POST "$GW/v1/verify/citizen" \
  -H "Authorization: Bearer $CONS" -H "Content-Type: application/json" \
  -d "{\"nin\":\"CM91051512345001\",\"given_name\":\"Sarah\",\"surname\":\"Nakato\",\"consent_reference\":\"$GID\"}")
[ "$(jq -r .status <<<"$V")" = "verified" ] || { echo "✗ verify: $(jq .status <<<"$V")"; exit 1; }
echo "✓ verify returned 'verified'"

# Audit chain should have grown
sleep 1
N=$(curl -fsS "$GW/v1/audit/entries?limit=10" -H "Authorization: Bearer $ADMIN" | jq '.entries | length')
[ "$N" -ge 3 ] || { echo "✗ audit only has $N entries"; exit 1; }
echo "✓ audit chain advanced ($N recent entries)"

curl -fsS "$GW/v1/audit/verify?from_id=1&to_id=0" \
  -H "Authorization: Bearer $ADMIN" | jq -e '.ok == true' >/dev/null \
  && echo "✓ chain integrity ok"

curl -fsS -X POST "$GW/v1/consent/grants/$GID/revoke" \
  -H "Authorization: Bearer $ADMIN" -d '{"reason":"smoke"}' >/dev/null
V2=$(curl -fsS -X POST "$GW/v1/verify/citizen" \
  -H "Authorization: Bearer $CONS" -H "Content-Type: application/json" \
  -d "{\"nin\":\"CM91051512345001\",\"consent_reference\":\"$GID\"}")
[ "$(jq -r .status <<<"$V2")" = "consent_invalid" ] \
  && echo "✓ revoked grant denied as expected" \
  || { echo "✗ post-revoke status: $(jq .status <<<"$V2")"; exit 1; }

echo
echo "ALL CHECKS PASSED"
```
