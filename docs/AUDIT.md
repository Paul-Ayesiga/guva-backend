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
| Hash chain (`previous_hash + entry_hash`) | Any modification to a past row breaks every subsequent hash | Operator with DB write access |
| DB-level append-only trigger | `UPDATE`/`DELETE` always `RAISE EXCEPTION` regardless of role | Application bugs, mis-typed migrations, operator mistakes |
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
| `verification.citizen.queried` | verification (future) | Each `POST /verify/citizen` |
| `consent.granted` / `consent.revoked` | consent (future) | Per [§5.3](../../guva-docs/02-requirements/05-functional-requirements.md) |
| `apisix.token.validated` | (deferred) gateway access log adapter | Bulk-emit per request |

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

## 7. Phase A status

| Step | Goal | Status |
|---|---|---|
| 1 | `services/audit` skeleton: schema, chain writer, Kafka consumer, query API | ✅ committed |
| 2 | `pkg/platform/audit` producer library: outbox helper + drain worker | ✅ committed |
| 3 | Identity adopts: `audit_outbox` migration + `audit.Emit` in handlers | ✅ committed |
| 4 | **OpenAPI + Bruno + this doc** | ✅ **this commit** |

### What's deferred (Phase B and beyond)

- Meta-audit on queries against the audit log
- Monthly partitioning with `pg_partman`
- Per-role DB separation (`audit_writer` / `audit_reader`)
- SIEM export endpoint with signed bundles
- APISIX access-log adapter so the gateway also produces events
- Apicurio schema registration for the event envelope
- Prometheus alert on unsent outbox depth
- External Merkle anchoring to a consortium ledger (Phase 5)

---

## 8. Cross-references

- [`services/audit/README.md`](../services/audit/README.md) — service-specific operator notes.
- [`pkg/platform/audit/audit.go`](../pkg/platform/audit/audit.go) — producer library.
- [`guva-docs/02-requirements/05-functional-requirements.md`](../../guva-docs/02-requirements/05-functional-requirements.md) §5.4 — requirements.
- [`guva-docs/03-architecture/07-system-architecture.md`](../../guva-docs/03-architecture/07-system-architecture.md) §7.2.5 — Audit Service component.
- [`guva-docs/03-architecture/08-database-design.md`](../../guva-docs/03-architecture/08-database-design.md) §8.3 — schema.
- [`guva-docs/03-architecture/12-event-driven-messaging.md`](../../guva-docs/03-architecture/12-event-driven-messaging.md) §12.4 — transactional outbox.
- [`guva-docs/08-appendix/18-future-enhancements.md`](../../guva-docs/08-appendix/18-future-enhancements.md) §18.2 — blockchain anchoring.
