# Verification Engine — Architecture

How GUVA answers consent-scoped "does the citizen match this claim?"
questions for consumers, and how that answer remains trustworthy
under audit.

This document tracks the first vertical slice (POST /v1/verify/citizen
against a mock NIRA). The full workstream covers all five upstream
agencies (NIRA, URSB, URA, Lands, UNEB, MoH) — each adds one adapter
under `services/verification/internal/<agency>` and one endpoint.

---

## 1. Architecture at a glance

```text
   consumer                 APISIX (OIDC, verify:citizen)
       │                          │
       │ POST /v1/verify/citizen  │
       ├─────────────────────────▶│
       │                          │ strips /v1/verify prefix
       │                          ▼
       │              ┌─────── services/verification ────────┐
       │              │                                       │
       │              │  1. hash NIN                          │
       │              │  2. cache lookup (consumer+sub+fp)    │
       │              │     hit → return cached + audit       │
       │              │  3. NIRA adapter (mock | live)        │
       │              │  4. build per-attribute match summary │
       │              │  5. INSERT verification_log row       │
       │              │  6. INSERT audit_outbox (chain)       │
       │              │  7. cache verified/mismatch responses │
       │              │  8. return canonical JSON             │
       │              │                                       │
       │              └───────────────────┬───────────────────┘
       │                                  │
       │              ┌───────────────────▼───────────────────┐
       │              │  pkg/platform/audit drain Worker      │
       │              │   → Kafka ug.go.guva.audit.entry.*    │
       │              │   → services/audit chains the row     │
       │              │   → services/webhooks fans out to     │
       │              │     subscribers of verification.*     │
       │              └───────────────────────────────────────┘
       │
       ◀──────── canonical VerificationResponse ─────────
```

---

## 2. The contract: minimum-disclosure

The bedrock design choice. A verification call answers **boolean per
attribute** the caller already asserted, never **what the actual
record says**. The caller claims "the citizen with NIN X has surname
Y and DOB Z"; the service returns "Y matches, Z matches" — or "Y
matches, Z does not match" — but **never** "Z does not match, the
actual DOB is W."

This is the difference between a *verification* API and a *lookup*
API. Lookup endpoints (give me everything about person X) are a
separate, much higher-privilege capability that doesn't ship in this
workstream. If a consumer needs the actual value, they ask the
citizen — that's whose data it is.

### Status enum

| Status | Meaning |
|---|---|
| `verified` | record found, every claimed attribute matched |
| `mismatch` | record found, at least one claimed attribute did not match |
| `not_found` | upstream has no record for the supplied identifier |
| `deceased` | record exists but is marked deceased — overrides attribute matching |
| `revoked` | record exists but is cancelled/withdrawn — overrides attribute matching |
| `consent_invalid` | consent reference unrecognised or expired (when consent service ships) |
| `error` | adapter or upstream-side failure |

`deceased` and `revoked` are *status overrides*: they fire regardless
of whether attributes match, because consumers must NOT treat such a
record as usable identity proof.

---

## 3. PII handling

The verification service is **not** a PII honeypot. Three guarantees:

1. The raw NIN appears only on the request envelope and in transient
   memory while the NIRA adapter runs. **It never lands in the
   verification_log table** — only a SHA-256 hash does.
2. The NIRA response is **not** persisted in this service at all
   except in the short-lived idempotency cache (configurable TTL,
   default 15 minutes). The cache stores the canonical response
   shape (the same per-attribute booleans the caller would have
   gotten anyway). Real PII fields (names, DOB, etc.) are not stored.
3. The audit chain entry (`verification.citizen.queried`) carries the
   subject as the **hashed NIN**, and `data` includes only the *keys*
   of attributes the caller asserted (`["given_name","surname"]`),
   never the values. The chain remains auditable but cannot be
   weaponised as a citizen-attribute lake.

### Verifying the no-PII property locally

```bash
# After a verify call, inspect the chain entry's detail JSON:
docker exec guva-postgres psql -U guva -d guva_audit -c "
  SELECT detail FROM audit_entries
  WHERE action='verification.citizen.queried'
  ORDER BY entry_id DESC LIMIT 1;"
# Expect: {match_count, mismatch_count, requested_attributes (keys only),
#          upstream, latencies, consent_reference, verification_id}
# Expect NOT to see: nin, given_name, surname, date_of_birth, etc.
```

A Bruno test in `02 Verify Citizen (mismatch).bru` asserts the
response also doesn't leak — passing a deliberately-wrong surname
and checking that the actual one doesn't appear in the body.

---

## 4. Failure modes and what happens

| Failure | What happens | Caller sees |
|---|---|---|
| Missing `nin` | request rejected before any DB / NIRA call | `400 missing_nin` |
| Invalid bearer | gateway rejects | `401` from APISIX |
| Token lacks `verify:citizen` | service rejects | `403 insufficient_scope` |
| NIRA upstream times out / errors | log row written with status=error; audit emitted with result=error | `502 upstream_unavailable` |
| Cache write fails | response still returned; subsequent requests will re-fetch | response unaffected |
| Audit emit fails | log row + response unaffected; warning in service log | response unaffected (best-effort emit) |

The cache + audit are deliberately best-effort on the response path —
the response semantics (you got the right answer) take precedence
over the operational write semantics (we recorded that you asked).
Producers retry the audit drain; cache misses are cheap.

---

## 5. Consent — current state, future state

Today: the request takes `consent_reference` as opaque text, records
it in the verification_log + audit chain, and proceeds. No
verification of the reference against a consent service.

When the consent workstream (1.3.4) ships:

1. The handler will look the reference up against the consent service
   (HTTP call, or a co-located consent-store package).
2. Missing / expired / revoked reference → status `consent_invalid`,
   audit result `denied`, no NIRA call made.
3. Granted reference → proceed as today.

Plumbing the eventual call site is one line in `verifyCitizenHandler`;
the canonical response already has the status enum value for it.

---

## 6. The mock NIRA

`services/verification/internal/nira.NewMock()` returns an adapter
backed by 5 hand-curated records spanning every status enum:

| NIN | Name | DOB | Sex | Status |
|---|---|---|---|---|
| `CM91051512345001` | Sarah Nansubuga Nakato | 1991-05-15 | F | active |
| `CM85031298765002` | John Wasswa Mukasa | 1985-03-12 | M | active |
| `CF95071587654003` | Grace Akello Achieng | 1995-07-15 | F | active |
| `CM72042098765004` | Patrick Kato Ssali | 1972-04-20 | M | deceased |
| `CM88010198765005` | David Ojok Okello | 1988-01-01 | M | revoked |

These names + NINs are fictional. They do not correspond to any real
Ugandan and are not valid against the production NIRA system.

The live adapter (real NIRA endpoint, mTLS, contract testing against
NIRA's spec) ships as a separate change once the integration
agreement is in place. Switch via `NIRA_MODE=live` once that
adapter exists.

---

## 7. Cross-references

- [`services/verification/api/openapi.yaml`](../services/verification/api/openapi.yaml) — endpoint contract + response shape.
- [`services/verification/bruno/Verification/`](../services/verification/bruno/Verification/) — 6 ready-to-run requests covering every status enum.
- [`docs/AUDIT.md`](AUDIT.md) — chain semantics that verification produces into.
- [`docs/AUTH.md`](AUTH.md) §8 — RBAC; `verify:citizen` scope is on `guva-reference` by default.
- [`guva-docs/02-requirements/05-functional-requirements.md`](../../guva-docs/02-requirements/05-functional-requirements.md) §5.1 — verification requirements.
- [`guva-docs/04-api/09-api-documentation.md`](../../guva-docs/04-api/09-api-documentation.md) — the canonical-model spec this implementation realises.
- [`guva-docs/03-architecture/11-interoperability-framework.md`](../../guva-docs/03-architecture/11-interoperability-framework.md) — agency-adapter pattern.

---

## 8. Vertical-slice status

| Slice | Status |
|---|---|
| Canonical citizen model + status enum | ✅ |
| Mock NIRA adapter (5 records) | ✅ |
| POST /v1/verify/citizen + scope enforcement | ✅ |
| verification_log + idempotency cache | ✅ |
| Audit chain emission (`verification.citizen.queried`, PII-safe) | ✅ |
| OpenAPI + 6 Bruno requests | ✅ |
| **Live NIRA adapter** | ⏳ (agency agreement) |
| **Consent service integration** | ⏳ (1.3.4) |
| **Additional endpoints** (business, tax, land, education, health) | ⏳ (1.3.7 per-agency adapters) |
| **Contract tests** | ⏳ (Pact / similar; this workstream's polish phase) |
