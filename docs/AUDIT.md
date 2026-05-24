# Audit Trail — Architecture

How GUVA records every action that affects the security or integrity of the platform, and how those records remain trustworthy under hostile conditions.

This document is the design reference. Operational procedures (key rotation, integrity-check cadence, regulator export) will live in a `RUNBOOK-AUDIT.md` when Phase 4 work lands; for now, the `services/audit/README.md` covers day-to-day usage.

---

## 1. Architecture at a glance

```text
                                  ┌─────────── pkg/platform/audit ───────────┐
   service handler                │                                          │
                                  │   audit.Emit(ctx, tx, event)             │
   BEGIN tx                       │     ├─ generates event_uuid              │
     INSERT business_row          │     ├─ builds CloudEvents envelope       │
     audit.Emit(tx, …) ───────────┼─→   └─ INSERT audit_outbox row           │
   COMMIT                         │                                          │
                                  │   audit.Worker (per-process goroutine)   │
                                  │     loop every 500ms:                    │
                                  │       SELECT outbox WHERE sent_at IS NULL│
                                  │       publish to Kafka                   │
                                  │       UPDATE sent_at = NOW()             │
                                  └────────────────┬─────────────────────────┘
                                                   │
                                          ┌────────▼────────┐
                                          │   Kafka topic   │
                                          │   ug.go.guva.   │
                                          │   audit.entry.  │
                                          │   appended.v1   │
                                          └────────┬────────┘
                                                   │
                            ┌──────────────────────▼──────────────────────┐
                            │           services/audit                    │
                            │                                             │
                            │  consumer (kafka-go, manual offset commit)  │
                            │    dedupe by entry_uuid                     │
                            │    SELECT prev = entry_hash ... FOR UPDATE  │
                            │    new_hash = SHA-256(canonical || prev)    │
                            │    INSERT audit_entries                     │
                            │    commit Kafka offset                      │
                            │                                             │
                            │  HTTP read API (audit:read scope)           │
                            │    GET /entries (cursor + filters)          │
                            │    GET /verify  (chain integrity walk)      │
                            └─────────────────────────────────────────────┘
```

---

## 2. Trust model

The audit trail must answer two questions for an auditor:

1. **Completeness** — every action that should have been recorded was recorded.
2. **Integrity** — no recorded action has been altered or removed since it was written.

Layered defenses:

| Layer | Guarantee | Defeats |
|---|---|---|
| Transactional outbox | Audit event commits with the business write or not at all | Producer bug or crash mid-write leaves no half-truths |
| At-least-once Kafka + dedupe-on-uuid | Every successful outbox row becomes an entry exactly once | Network blips, consumer restarts |
| Producer-side schema validation | Every envelope conforms to the registered JSON Schema or is rejected before commit | Producer drift, mis-typed event types, enum-vocabulary creep |
| Hash chain (`previous_hash + entry_hash`) | Any modification to a past row breaks every subsequent hash | Operator with DB write access |
| DB-level append-only trigger | `UPDATE`/`DELETE` always `RAISE EXCEPTION` regardless of role | Application bugs, mis-typed migrations, operator mistakes |
| Per-role DB separation | Reader role can only SELECT; writer can only SELECT/INSERT (no UPDATE/DELETE on the chain) | Even if the trigger is dropped, neither role can mutate past rows |
| (Phase 5) External Merkle anchoring | Periodic root commits to a consortium ledger | Operator collusion to rewrite the entire chain |

---

## 3. Producer contract

Every service that does anything security-relevant calls `audit.Emit` inside the transaction that performs the action. Three steps:

### 3.1 Include the outbox migration

```sql
-- services/<name>/migrations/0000N_audit_outbox.up.sql
-- Matches pkg/platform/audit.OutboxMigration exactly.

CREATE TABLE IF NOT EXISTS audit_outbox (
    id          BIGSERIAL PRIMARY KEY,
    event_id    UUID UNIQUE NOT NULL,
    payload     JSONB NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    sent_at     TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_audit_outbox_unsent
    ON audit_outbox(id) WHERE sent_at IS NULL;
```

### 3.2 Emit inside the business transaction

```go
tx, err := db.BeginTx(ctx, pgx.TxOptions{})
if err != nil { return err }
defer tx.Rollback(ctx)

// business work
if err := store.CreateConsumerTx(ctx, tx, reg); err != nil { return err }

// audit event — same transaction
if _, err := audit.Emit(ctx, tx, audit.Event{
    SourceKind: "service",
    Source:     "identity",
    Type:       "identity.consumer.created",
    Subject:    reg.KeycloakClientID,
    SubjectKind:"consumer",
    Result:     "ok",
    CorrelationID: r.Header.Get("X-Correlation-Id"),
    Data: map[string]any{
        "consumer_id": reg.ID,
        "agency":      reg.AgencyName,
    },
}); err != nil { return err }

return tx.Commit(ctx)
```

### 3.3 Run the drain worker in main

```go
worker := audit.NewWorker(audit.WorkerConfig{
    DB:           pool,
    Logger:       logger,
    KafkaBrokers: cfg.KafkaBrokers,           // e.g. ["localhost:9094"]
    KafkaTopic:   cfg.KafkaAuditTopic,        // "ug.go.guva.audit.entry.appended.v1"
})
go worker.Run(ctx)
```

That's it. The handler is unchanged in latency profile — `audit.Emit` is one `INSERT` inside the same transaction. The Worker handles all asynchronous concerns.

---

## 4. Event naming and shape

### 4.1 Naming convention

`<service>.<entity>.<action>` — short, predictable, namespace-prefixed by service.

Examples in flight:

| Event | Emitted by | When |
|---|---|---|
| `identity.consumer.created` | identity | Successful `POST /consumers` |
| `identity.consumer.create.idempotent_replay` | identity | (deferred) replay of an Idempotency-Key |
| `identity.consumer.create.failed` | identity | (deferred) Keycloak or DB failure |
| `audit.entries.queried` | audit | Every `GET /v1/audit/entries` (meta-audit) |
| `audit.chain.verified` | audit | Every `GET /v1/audit/verify` (meta-audit) |
| `apisix.request.served` | apisix-adapter | Every gateway request (via http-logger plugin) |
| `verification.citizen.queried` | verification (future) | Each `POST /verify/citizen` |
| `consent.granted` / `consent.revoked` | consent (future) | Per [§5.3](../../guva-docs/02-requirements/05-functional-requirements.md) |

### 4.2 Envelope

CloudEvents-shaped JSON, matches [`§12.3`](../../guva-docs/03-architecture/12-event-driven-messaging.md) of the architecture:

```json
{
  "specversion": "1.0",
  "id": "9369c308-d96d-4905-9096-f369161cdb77",
  "source": "identity",
  "sourcekind": "service",
  "type": "identity.consumer.created",
  "subject": "acacia-onboarding",
  "subjectkind": "consumer",
  "time": "2026-05-24T18:00:00.000Z",
  "correlationid": "abc...",
  "result": "ok",
  "data": { /* service-defined */ }
}
```

### 4.3 What `data` should contain

| Do | Don't |
|---|---|
| Stable IDs that survive renames | Verbatim PII (names, IDs) |
| Counts, sizes, status codes | Free-form text from user input |
| Outcome categories ("denied:scope") | Stack traces |
| Up to ~1 KB per event | Multi-KB blobs |

Verbatim citizen data should be in the **system of record** (NIRA, URSB, …), not in audit. The audit log proves that the action happened; the system of record is the truth of what the action operated on.

---

## 5. Read path

### 5.1 Query

```
GET /v1/audit/entries
    ?actor_id=identity
    &subject_id=acacia-onboarding
    &action=identity.consumer.created
    &from=2026-05-24T00:00:00Z
    &to=2026-05-25T00:00:00Z
    &after=42
    &limit=100
```

Cursor-paginated: use the `next_cursor` from the response as `after` on the next call. Stable under concurrent appends because ordering is by `entry_id ASC`.

### 5.2 Integrity verification

```
GET /v1/audit/verify?from_id=1&to_id=0
```

Walks the range, recomputes every `entry_hash`, asserts `previous_hash` chains correctly. Returns `{"ok": true}` or `{"ok": false, "broken_at": N, "broken_uuid": "...", "detail": "..."}`.

Run this:

- After any operational change to the audit DB.
- Periodically (recommended: every 6 hours) from a cron-driven probe.
- Before exporting a chunk to a regulator.
- On demand when an external auditor asks "prove this row is original."

---

## 6. Failure modes and what happens

| Failure | What happens | What you see |
|---|---|---|
| Producer's DB tx rolls back | No business row, no audit row | Caller gets 5xx; chain unaffected |
| Producer's DB commits, Worker can't reach Kafka | Outbox row stays unsent | Next tick retries; producer's tx already committed so handler latency unaffected |
| Kafka delivers same message twice | Audit consumer dedupes by `entry_uuid` | Chain unchanged; metric increments |
| Audit consumer crashes mid-INSERT | Kafka offset NOT committed | Restart re-reads; dedupe catches the case where INSERT succeeded but offset failed |
| Operator runs `UPDATE audit_entries SET ...` | Trigger `RAISE EXCEPTION` | Operation rejected at DB layer |
| Operator drops the trigger and runs UPDATE | Modification succeeds locally | Chain hash breaks; `GET /verify` reports the offending row |
| Operator rebuilds the chain with new hashes to hide a row | Internal chain looks valid | (Phase 5) External Merkle root won't match → tamper visible to the anchoring ledger |

---

## 7. Meta-audit (auditing reads of the audit log)

The audit service is itself a producer. Every call to a read endpoint
emits an event to the local `audit_outbox`, which the drain worker
publishes to Kafka, and the audit consumer (same service) appends to
the chain. The result: reads of the ledger are themselves on the ledger.

| Read endpoint | Event type | `subject` | `data` (sanitised) |
|---|---|---|---|
| `GET /v1/audit/entries` | `audit.entries.queried` | the `subject_id` filter if any | `{filters, returned, latency_ms}` |
| `GET /v1/audit/verify` | `audit.chain.verified` | (empty) | `{from_id, to_id, latency_ms, broken_at?}` |

The actor is the JWT's authorized party (`azp`) — the consumer or
service whose bearer token APISIX validated. We default `SourceKind`
to `consumer`; service-to-service callers come through the same gateway
path with their own client-credentials token.

**Fail-closed policy.** If `audit.Emit` fails (e.g. transient DB issue)
the read response is withheld and the caller gets `500 meta_audit_failed`.
The premise of the system is that reads of the ledger leave a trace; if
we can't write the trace, we don't serve the data.

**No loop hazard.** Meta-audit is emitted on reads, not on the consumer
inserting chain rows. Appending a meta-audit entry does not trigger
another meta-audit emission, so the recursion bottoms out after one step.

## 8. Gateway as a producer (`services/apisix-adapter`)

APISIX's `http-logger` plugin batches every access log entry to the
adapter's `POST /ingest`. The adapter transforms each entry into the
same CloudEvents envelope every other producer uses, stages it in its
own `audit_outbox`, and lets `pkg/platform/audit.Worker` drain to Kafka.
The audit chain consumer treats it like any other producer — one
chain entry per gateway request.

**Why a separate service.** The adapter could have lived inside APISIX
as a Lua plugin, but two things pushed us out:

1. Hash chaining + outbox transactional semantics are written in Go and
   already cover every other producer; rewriting them in Lua is a step
   back, not forward.
2. APISIX restarts (config reload, container restart) would otherwise
   drop in-flight events; a separate process with its own outbox table
   gives us the same at-least-once + dedupe-on-uuid guarantees we get
   from identity.

**Event shape.**

| Field | Value |
|---|---|
| `type` | `apisix.request.served` |
| `sourcekind` | `gateway` |
| `source` | `apisix` |
| `subjectkind` | `route` |
| `subject` | the APISIX `route_id` (e.g. `identity-public`) |
| `result` | `ok` for 2xx/3xx, `denied` for 4xx, `error` for 5xx |
| `correlationid` | `X-Correlation-Id` from the request |
| `data` | `{method, uri, status, latency_ms, req_size, resp_size, client_ip, upstream, token_actor, token_subject, user_agent, start_time_ms}` |

`token_actor` and `token_subject` are pulled from the JWT's `azp` and
`sub` claims without re-verifying the signature — APISIX already did
that, and the adapter just wants to surface "who called" on the chain.

**Skipped events.** The adapter drops rows with no `route_id` (admin
routes, prometheus scrape paths) — except APISIX assigns
`route_id="no-matched"` to 404s, so attempted access to non-existent
paths still lands on the chain. That's intentional: failed reconnaissance
is worth seeing.

**Throughput envelope.** Chain inserts serialise via `SELECT FOR UPDATE`
on the latest row; with hash compute + insert ~5 ms each, the ceiling
sits around 200 entries/s on the current single-consumer-per-partition
setup. Stay under that and the gateway's outbox stays empty. Once
sustained traffic exceeds it, the relief options are: per-minute
route-level aggregation, per-route sub-chains, or external Merkle
anchoring with batched logs (Phase 5).

**Failure modes.** APISIX retries up to `max_retry_count` (3 by default,
1-second backoff). If the adapter is fully down for longer than that,
events are lost from the gateway's buffer — the chain stays correct
for what arrived, but completeness for that window is on the gateway's
buffer. That's the cost of running the adapter outside the gateway
process; the upside is that an adapter crash never takes down request
serving.

## 9. Schema discipline (Apicurio + envelope validation)

The CloudEvents envelope every producer writes is locked by a JSON
Schema kept in [`pkg/platform/audit/schemas/audit-event-envelope-v1.json`](../pkg/platform/audit/schemas/audit-event-envelope-v1.json).
Two copies of that schema live at runtime:

1. **The registry copy** — `make seed-schemas` POSTs it to Apicurio
   (group `guva-audit`, artifact `audit-event-envelope`). Re-running
   is a no-op if the bytes match canonically; otherwise a new version
   is created.
2. **The embedded copy** — Go's `//go:embed` bakes the same bytes into
   every producer binary, providing a fallback when the registry is
   unreachable at startup.

At startup each producer constructs a `Validator` that:

- tries to fetch the latest version from Apicurio,
- on success: logs `source=registry` and the SHA-256 of what was loaded,
- on failure: logs a warning and falls back to the embedded copy,
- if the loaded bytes differ from the embedded copy: logs a drift
  warning naming both hashes so the operator can ship a new binary
  to realign.

Every call to `audit.Emit` runs the marshalled envelope through the
validator before the outbox insert. A non-conforming envelope returns
an error, the caller's transaction rolls back, and the bad event never
reaches Kafka or the chain. This is the load-bearing guarantee: a
producer can no longer accidentally publish an event the consumer
won't understand or the auditors can't query against.

**What the schema enforces** (as of v1):

- `specversion` is the literal string `"1.0"`.
- `id` is a UUID; `time` is ISO 8601 (`format` keywords are asserted).
- `sourcekind` ∈ `{service, consumer, gateway, operator, system}`.
- `result` ∈ `{ok, denied, error, inconclusive, broken}`.
- `type` matches `^[a-z][a-z0-9]*([._-][a-z0-9]+)*$` (lowercase dotted).
- No additional top-level fields are allowed — drift is caught at
  publish time, not at chain consumption.
- `data` is unrestricted (service-defined; keep small and stable).

**Evolving the schema.** Edit
[`pkg/platform/audit/schemas/audit-event-envelope-v1.json`](../pkg/platform/audit/schemas/audit-event-envelope-v1.json),
re-build the producer binaries (so the embedded copy refreshes), run
`make seed-schemas` (so the registry copy refreshes). Producers
restarted after both steps will load the new shape from the registry
and their embedded copy will match — no drift warning.

If a producer ships before the registry is updated, it falls back to
its embedded copy (which is the new shape) and emits new-shaped events;
the chain consumer happily accepts them because the consumer doesn't
re-validate (the producer's validation is authoritative — that's
where the gate is). If a registry-served schema is *older* than what
a producer expects, the producer logs the drift warning and uses the
registry version, which may reject envelopes whose new fields haven't
been added yet — surface fast in dev, never silent in prod.

**Coverage check.** `go test ./pkg/platform/audit/...` runs unit tests
that drive both the happy path and six rejection cases (unknown enum,
missing required field, additional property, malformed UUID, etc.).
CI runs these on every PR.

## 10. Phase A status

| Step | Goal | Status |
|---|---|---|
| 1 | `services/audit` skeleton: schema, chain writer, Kafka consumer, query API | ✅ committed |
| 2 | `pkg/platform/audit` producer library: outbox helper + drain worker | ✅ committed |
| 3 | Identity adopts: `audit_outbox` migration + `audit.Emit` in handlers | ✅ committed |
| 4 | OpenAPI + Bruno + this doc | ✅ committed |
| 5 | Meta-audit on `/entries` and `/verify` | ✅ committed |
| 6 | APISIX access-log adapter (`services/apisix-adapter`) | ✅ committed |
| 7 | Apicurio schema registration + producer-side envelope validation | ✅ committed |
| 8 | Prometheus metrics for outbox drain + alert rules | ✅ committed |
| 9 | Per-role DB separation (`audit_writer` / `audit_reader`) | ✅ committed |
| 10 | SIEM export with Ed25519-signed bundles + standalone verifier | ✅ committed |
| 11 | External Merkle anchoring (anchor log + inclusion proofs) | ✅ committed |
| 12 | Monthly partitioning with `pg_partman` | ✅ committed |
| 13 | **Loki + Promtail for log search** | ✅ **this commit** |

Per-producer metrics emitted (see [`pkg/platform/audit/metrics.go`](../pkg/platform/audit/metrics.go)):

- `guva_audit_outbox_unsent_count{producer}` — backlog gauge, updated by Worker
- `guva_audit_outbox_drain_total{producer}` — successful drain batches
- `guva_audit_outbox_drain_errors_total{producer}` — drain failures

Alert rules in [`deploy/compose/prometheus/alerts.yml`](../deploy/compose/prometheus/alerts.yml)
under group `audit-outbox`. Runbook for both:
[`docs/OPERATIONS.md` §7](OPERATIONS.md#7-backlog-in-audit_outbox).

## SIEM export — signed bundles

External auditors and SIEM systems take a slice of the chain off-platform
and verify it independently. Two endpoints make this self-contained:

| Endpoint | Auth | Returns |
|---|---|---|
| `GET /v1/audit/export?from_id=A&to_id=B` | bearer + `audit:read` | Signed bundle (up to 500 entries per call; page by from_id) |
| `GET /v1/audit/export/pubkey` | none (key is public) | `{"algorithm":"Ed25519","public_key_b64":"..."}` |

The bundle is a CloudEvents-style JSON envelope: `format_version`,
`generator`, `generated_at`, the range, the **anchor** (entry_hash of
the row preceding `from_id`, or genesis), the entries, plus
`signing_pubkey` (base64 of the public key that signed) and
`signature` (Ed25519 over canonical JSON of the bundle with signature
blanked). Canonical JSON sorts keys at every depth — so the same
logical bundle hashes to the same bytes regardless of which language
built it.

### Verification

Two layers, both required for an honest "this is the platform's word":

1. **Signature** — Ed25519 over the canonical bytes. Fetch the public
   key from `/export/pubkey` (separate channel) and verify against
   the bundle's `signature`. The bundle's own `signing_pubkey` field
   is only an identifier — never trust it as the verifying key.
2. **Chain walk** — `anchor.anchor_entry_hash` must equal
   `entries[0].previous_hash`; each subsequent entry's `previous_hash`
   must equal the prior entry's `entry_hash`; each `entry_hash` must
   recompute from its canonical content + `previous_hash`.

### The verifier binary

[`tools/audit-verify`](../tools/audit-verify/) is a tiny Go CLI that does
exactly the two steps above. It depends only on `pkg/platform/audit`
(and the Go stdlib) — no DB, no platform code, no network at verify time.
Drop the binary on any machine; pass the bundle and the public key.

```bash
# Build
cd tools/audit-verify && go build -o audit-verify ./...

# Verify against the current platform key
audit-verify --bundle bundle.json --pubkey-url http://audit.example/v1/audit/export/pubkey

# Or offline with a previously-saved key
audit-verify --bundle bundle.json --pubkey-file pub.b64
```

Exit codes: `0` ok · `1` signature failure · `2` chain break · `3`
usage error. Suitable for cron in a SIEM pipeline.

### Key management

Production seeds the signing key once at provisioning time into Vault
at `secret/services/audit/config:signing-key-b64` (base64 of the
64-byte Go `ed25519.PrivateKey`). Local dev auto-generates the key on
first boot if Vault has none, then writes it back so subsequent
restarts in the same Vault session reuse it. The dev key is, by
construction, fresh per Vault reset — always fetch the pubkey from
`/export/pubkey` rather than caching.

Rotation: write a new key to the same Vault path, restart the audit
service. The next bundle exported is signed with the new key; the
public key endpoint returns the new key. Verifiers handling bundles
across a rotation need the historical pubkey for older bundles
(future work: `/export/pubkeys` listing all keys ever active, with
their valid-from timestamps).

## External Merkle anchoring

The chain proves internal consistency (every row's hash chains to the
previous). The chain alone does **not** prove the platform isn't lying
about what events ever existed — an operator with sufficient privilege
could in principle rebuild the whole chain with different entries.

External anchoring closes that gap:

1. The audit service periodically (`AUDIT_ANCHOR_INTERVAL`, default
   5 min) computes a binary Merkle root over every new entry since
   the last anchor, then INSERTs a row into [`audit_anchors`](../services/audit/migrations/000004_audit_anchors.up.sql).
2. (Optional) The root is POSTed to an external witness configured
   via `AUDIT_ANCHOR_WITNESS_URL` — examples: Sigstore Rekor, a
   consortium ledger, an Ethereum transaction, a peer platform's
   anchor-receiver endpoint.
3. The operator stores the witness's receipt (entry id, tx hash, etc.)
   in the row's `external_proof` column. The `audit_anchors` trigger
   permits this single mutation from the `guva_audit_operator` role
   and forbids everything else.
4. A third party who later gets one chain entry + an inclusion proof
   from this platform can confirm membership against the externally-
   witnessed root — no trust in GUVA required, only in the witness.

### Endpoints

| Endpoint | Returns |
|---|---|
| `GET /v1/audit/anchors` | Recent anchors, cursor-paginated |
| `GET /v1/audit/anchors/{id}` | One anchor (incl. `external_proof` if attached) |
| `GET /v1/audit/anchors/{id}/proof?entry_id=N` | Merkle inclusion proof for entry N |

### Verifying an inclusion proof

The proof is a list of sibling hashes with their side (L/R). Recompute
the root by walking from the leaf upward:

```python
import hashlib, json
p = json.load(open("proof.json"))

def sha256(b): return hashlib.sha256(b).digest()

h = sha256(bytes.fromhex(p["leaf_hash"]))
for step in p["proof"]:
    sib = bytes.fromhex(step["hash"])
    h = sha256(sib + h) if step["side"] == "L" else sha256(h + sib)

assert h.hex() == p["merkle_root"], "proof does not reproduce root"
```

If the proof reproduces `merkle_root`, AND `merkle_root` matches what
the external witness published, the entry is proven to have been on
GUVA's chain at anchor time.

### Algorithm — `sha256-binary-merkle-v1`

- Leaf hash:  `sha256(hex_decode(entry_hash))`
- Internal:   `sha256(left || right)`
- Odd tail:   pair the last node with itself (RFC 6962 convention)
- Tree shape: left-to-right in entry_id ascending order

Code: [`pkg/platform/audit/merkle.go`](../pkg/platform/audit/merkle.go).
Unit-tested across n ∈ {1..100} leaves with both happy and tamper
cases ([`merkle_test.go`](../pkg/platform/audit/merkle_test.go)).

### External publish — what to wire today vs. later

Today's default is internal-only: the anchor table is the witness.
Setting `AUDIT_ANCHOR_WITNESS_URL` to any HTTP endpoint causes the
service to POST every new anchor as JSON
(`{anchor_id, range_from_id, range_to_id, merkle_root, algorithm, computed_at}`).
Three deployment-grade targets to point it at when you're ready:

| Target | What it gives you |
|---|---|
| **Sigstore Rekor** | Free, public, append-only transparency log; widely audited |
| **A peer government platform's anchor-receive endpoint** | Mutual witnessing inside a consortium |
| **Public blockchain anchor (Ethereum, Bitcoin via OpenTimestamps)** | Tamper-evidence by an entity GUVA does not control |

Each comes with its own response format; current code stashes the
publish in logs, not in `external_proof`. Phase 5 adds the
`POST /v1/audit/anchors/{id}/external-proof` admin endpoint
(operator-role only) so the receipt can be persisted programmatically.

## What's deferred (post-Phase A)

- Programmatic `POST /v1/audit/anchors/{id}/external-proof` admin
  endpoint so an external witness's receipt can be persisted by
  automation (today: hand-update via the `guva_audit_operator` role).
- Multi-key support for `/v1/audit/export/pubkeys` (rotation history)
  so verifiers of older bundles can still get the historical pubkey.
- Cold-month archival driver: when `pg_partman` retention drops a
  partition, automatically write its rows to object storage with the
  matching anchor's `external_proof` for offline replay.

---

## 11. Cross-references

- [`services/audit/README.md`](../services/audit/README.md) — service-specific operator notes.
- [`pkg/platform/audit/audit.go`](../pkg/platform/audit/audit.go) — producer library.
- [`guva-docs/02-requirements/05-functional-requirements.md`](../../guva-docs/02-requirements/05-functional-requirements.md) §5.4 — requirements.
- [`guva-docs/03-architecture/07-system-architecture.md`](../../guva-docs/03-architecture/07-system-architecture.md) §7.2.5 — Audit Service component.
- [`guva-docs/03-architecture/08-database-design.md`](../../guva-docs/03-architecture/08-database-design.md) §8.3 — schema.
- [`guva-docs/03-architecture/12-event-driven-messaging.md`](../../guva-docs/03-architecture/12-event-driven-messaging.md) §12.4 — transactional outbox.
- [`guva-docs/08-appendix/18-future-enhancements.md`](../../guva-docs/08-appendix/18-future-enhancements.md) §18.2 — blockchain anchoring.
