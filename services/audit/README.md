# audit

The platform's append-only, hash-chained audit ledger. Sole writer to `audit_entries`.

Producers don't talk to this service directly. They emit events on Kafka via [pkg/platform/audit](../../pkg/platform/audit); this service consumes them and extends the chain.

## What's exposed

| Endpoint | Auth | Description |
|---|---|---|
| `GET /entries` | bearer + `audit:read` | Cursor-paginated query with filters (actor, subject, action, result, time range). |
| `GET /verify` | bearer + `audit:read` | Walks the chain end-to-end, recomputes every hash, returns `{ok: true}` or the offending row. |
| `GET /healthz` `/readyz` | none | Probes. |

## What's NOT exposed

There is **no HTTP write endpoint**. Producers must use the Kafka path. This is intentional — synchronous emission would couple every producer's request latency to the audit service's availability.

## How writes happen

```text
producer's transaction:
  INSERT business_row
  INSERT audit_outbox row    <-- via pkg/platform/audit.Emit(ctx, tx, event)
COMMIT                       <-- atomic with business write

producer's audit.Worker (one-per-process, polls every 500ms):
  SELECT outbox rows WHERE sent_at IS NULL
  publish to Kafka (ug.go.guva.audit.entry.appended.v1)
  UPDATE outbox SET sent_at = NOW()

services/audit consumer:
  read Kafka message
  dedupe by entry_uuid
  SELECT entry_hash FROM audit_entries ORDER BY entry_id DESC LIMIT 1 FOR UPDATE
  compute hash = SHA-256(canonical(row) || previous_hash)
  INSERT audit_entries
  commit Kafka offset
```

At-least-once delivery is fine because dedupe by `entry_uuid` catches replays.

## Run locally

```bash
make up                    # full stack
make migrate               # creates audit_entries + the trigger
make run-audit             # listens on :7072
make run-identity          # emits events into the chain

TOKEN=$(make token)
curl -sH "Authorization: Bearer $TOKEN" \
  http://localhost:8000/v1/audit/entries | jq
curl -sH "Authorization: Bearer $TOKEN" \
  http://localhost:8000/v1/audit/verify  | jq
```

The Bruno collection at [`bruno/`](./bruno) wraps all of the above with assertions.

## Layout

```text
services/audit/
├── api/openapi.yaml           Single source of truth for the API surface
├── bruno/                     Ready-to-import API test collection
├── cmd/server/main.go         Wires storage, consumer, HTTP server
├── internal/
│   ├── chain/                 Canonical serialisation + SHA-256
│   ├── config/                Env-driven config
│   ├── consumer/              Kafka subscriber → store.AppendEntry
│   ├── server/                HTTP query + verify handlers
│   └── store/                 pgx; AppendEntry, List, VerifyRange
├── migrations/000001          audit_entries + append-only trigger
└── README.md                  This file
```

## Tamper-evidence in three layers

1. **Hash chain.** Modifying any past row breaks every subsequent `entry_hash` recomputation.
2. **DB-level append-only.** `BEFORE UPDATE OR DELETE` triggers `RAISE EXCEPTION` regardless of role.
3. **External anchoring** (Phase 5 of the original delivery plan; see [`guva-docs/08-appendix/18-future-enhancements.md`](../../../guva-docs/08-appendix/18-future-enhancements.md) §18.2). Hourly Merkle roots committed to a permissioned consortium ledger. Defends against operator collusion that the chain alone cannot.

## What's not yet done

- **Meta-audit**: queries against the audit log are themselves auditable.
- **SIEM export**: signed bundles of entries for regulatory submission.
- **Monthly partitioning** with `pg_partman` (already installed; just needs config).
- **Per-role DB separation**: today the service connects as the schema owner. Production should split `audit_writer` (INSERT-only) from `audit_reader` (SELECT-only).
- **External anchoring** (Phase 5).
- **Schema registration in Apicurio**: event envelope shape should be registered for evolution governance.
