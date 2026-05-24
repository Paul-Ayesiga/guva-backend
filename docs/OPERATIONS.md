# Operations notes

A running log of platform-wide gotchas, surprising behaviours, and the
one-line fixes that resolve them. Things in here belong to the *platform*
(stack-wide), not to a single service. Service-specific runbooks live
next to the service (`services/<name>/README.md`).

Add to this file whenever you spend more than 15 minutes diagnosing
something that future-you (or a new teammate) would have solved in 30
seconds with a hint.

---

## APISIX gateway

### 1. Plugin allowlist parity

**Symptom.** A plugin block in [`deploy/compose/apisix/apisix.yaml`](../deploy/compose/apisix/apisix.yaml)
appears to do nothing. Requests come back as if the plugin were absent
(e.g. `http-logger` doesn't post, `cors` doesn't add headers). Single
startup error in `docker logs guva-apisix`:

```
failed to check item data of [global_rules] err:unknown plugin [<name>]
```

**Cause.** APISIX standalone refuses to load any plugin that isn't named
in [`deploy/compose/apisix/config.yaml`](../deploy/compose/apisix/config.yaml)
under the top-level `plugins:` allowlist. Then it serves traffic with the
plugin silently disabled. The allowlist is an attack-surface boundary,
which is good, but it means every new plugin needs to be added in **two**
places.

**Fix.** [`tools/scripts/check-apisix.sh`](../tools/scripts/check-apisix.sh)
parses both files and refuses to let the stack come up if a plugin is
referenced in `apisix.yaml` but missing from the allowlist. Runs as part
of `make up` and on demand via `make check-apisix`.

**Recovery from a fresh sighting.**

1. Add the plugin name to `config.yaml`'s `plugins:` list (one line).
2. Run `make check-apisix` to confirm.
3. `docker compose restart apisix` (or `make up` does it for you).

---

### 2. Bind-mount drift on macOS

**Symptom.** A route that used to work suddenly 404s. APISIX appears
healthy. `docker logs` shows no errors. The host file looks correct.

**Cause.** Docker Desktop's VirtioFS occasionally serves a stale or
truncated copy of the bind-mounted YAML to the container. Confirm with:

```bash
md5 deploy/compose/apisix/apisix.yaml
docker exec guva-apisix md5sum /usr/local/apisix/conf/apisix.yaml
```

When the two hashes differ, the container is reading something different
from what's on disk. We've seen the in-container copy truncated mid-line
in the middle of an unrelated route, breaking everything below it.

**Fix.** `make check-apisix` md5-diffs both `apisix.yaml` and `config.yaml`
between host and container; on mismatch it `docker restart guva-apisix`
(which re-reads the bind-mount) and re-checks. Runs as part of `make up`.

**Manual recovery.** `docker compose restart apisix`.

**Why we don't switch off bind-mount.** A copy-on-start approach kills
the hot-reload story APISIX standalone is built around — a YAML edit
should propagate in ~1 second. The drift is rare enough that detect +
restart is the right tradeoff.

---

## Apicurio Registry

### 3. API version: v2 only

**Gotcha.** The image we run (`apicurio/apicurio-registry-mem:2.6.7.Final`,
[`docker-compose.yml`](../docker-compose.yml)) serves the **v2** REST API.
v3 endpoints return 404.

| ✓ | ✗ |
|---|---|
| `GET /apis/registry/v2/system/info` | `GET /apis/registry/v3/system/info` |
| `POST /apis/registry/v2/groups/{g}/artifacts` | v3 split into multiple endpoints |

**Fix.** Use v2 paths everywhere. [`tools/scripts/seed-schemas.sh`](../tools/scripts/seed-schemas.sh)
and [`pkg/platform/audit/schema.go`](../pkg/platform/audit/schema.go) both
target v2. If we move to v3, both need to change in lockstep — keep them
in mind together.

---

### 4. Schema evolution order

**Symptom.** A producer rejects perfectly-shaped events that an older
build was accepting fine. Or vice versa.

**Cause.** The audit envelope schema lives in two places at runtime:

- The registry copy (Apicurio, served fresh on every producer startup).
- The embedded copy (`//go:embed` in
  [`pkg/platform/audit/schema.go`](../pkg/platform/audit/schema.go)).

Producers prefer the registry but fall back to the embedded copy on a
registry outage. If the two diverge, you can end up with mixed validation
behaviour across producer instances.

**Fix.** When you edit
[`pkg/platform/audit/schemas/audit-event-envelope-v1.json`](../pkg/platform/audit/schemas/audit-event-envelope-v1.json),
do **both** of these and in this order:

1. `make seed-schemas` — registers the new bytes in Apicurio as a new
   version (canonical compare; no-op if unchanged).
2. Rebuild the producer binaries so the `go:embed` picks up the new bytes.
3. Restart producers — they fetch the new registry version, the embedded
   sha-256 logged at startup matches it, and no drift warning fires.

If you ship a producer first and forget to seed, the producer logs:

```
WARN  registry schema differs from embedded copy
      registry_sha256=<old>  embedded_sha256=<new>
      action=using registry; ship a new binary to align embed
```

The producer continues with whatever the registry serves. The way out is
to run `make seed-schemas` and restart producers.

---

### 5. JSON Schema `format` keywords are advisory by default

**Symptom.** A bad UUID or a malformed date-time slips past
`v.Validate(envelope)`. Schema looks right (`"format": "uuid"`), the
validator is wired, but rejection never fires.

**Cause.** Draft 2020-12 treats `format` as **annotation only** unless
the validator is explicitly configured to assert it. The library we
use (`github.com/santhosh-tekuri/jsonschema/v5`) requires
`compiler.AssertFormat = true` for `format` to be enforced.

**Fix.** Our compiler in [`pkg/platform/audit/schema.go`](../pkg/platform/audit/schema.go)
sets `AssertFormat = true`. If you ever build a JSON Schema validator
**outside** this package, remember to do the same — otherwise `format`
becomes a silent comment in the schema. Unit tests in
[`pkg/platform/audit/schema_test.go`](../pkg/platform/audit/schema_test.go)
include a case (`id not a uuid`) that would silently pass if assertion
ever regresses; do not delete it.

---

## Database migrations

### 6. Service directory naming → database name

**Symptom.** `make migrate` reports
`failed to open database: pq: database "guva_<service>-foo" does not exist`
when the service directory contains a hyphen.

**Cause.** Convention is `guva_${service_dir}` and Postgres identifiers
can't carry unquoted hyphens.

**Fix.** [`tools/scripts/db-migrate.sh`](../tools/scripts/db-migrate.sh)
translates hyphens to underscores when deriving the DB name. The
matching `CREATE DATABASE` in
[`deploy/compose/postgres/initdb.d/00-databases.sql`](../deploy/compose/postgres/initdb.d/00-databases.sql)
must also use the underscored name. Example: directory `apisix-adapter`
→ database `guva_apisix_adapter`.

When you add a new service, do all three:

1. Create `services/<name>/migrations/000001_*.sql`.
2. Add `CREATE DATABASE guva_<name_underscored>;` to `00-databases.sql`.
3. Add `\connect ... CREATE EXTENSION ...` block to `01-extensions.sql`.

For an existing Postgres volume the init scripts won't re-run; create
the database manually with `docker exec guva-postgres psql -U guva -d guva
-c "CREATE DATABASE guva_<name_underscored>;"` then `make migrate`.

---

## Audit chain — outbox health

### 7. Backlog in audit_outbox

**Symptom.** Auditors notice gaps in the chain ("events for 14:02–14:08
on identity are missing"). `audit.Emit` in producer logs looks happy.
Kafka is up. The chain consumer is up.

**Cause.** Each producer stages events in its own `audit_outbox` table
and a `pkg/platform/audit.Worker` drains them to Kafka every 500 ms.
When the Worker can't publish (Kafka unreachable, network partitioned,
schema validator misconfigured at the consumer end, etc.), rows
accumulate in `audit_outbox` with `sent_at = NULL`. The producer's
business transactions keep committing — they're decoupled from Kafka
on purpose — but the chain doesn't see them until the drain catches up.

**Detection.** Each producer exposes a Prometheus gauge:

```promql
guva_audit_outbox_unsent_count{producer="identity"|"audit"|"apisix-adapter"}
```

Two alerts in [`deploy/compose/prometheus/alerts.yml`](../deploy/compose/prometheus/alerts.yml)
fire on it:

| Alert | Threshold | Severity |
|---|---|---|
| `AuditOutboxBacklogWarning` | gauge > 100 for 5m | warning |
| `AuditOutboxBacklogCritical` | gauge > 1000 for 5m | critical |
| `AuditOutboxDrainErrors` | drain-error rate > 0.1/s for 5m | warning |

Companion metrics for forensics: `guva_audit_outbox_drain_total`
(monotonic — should always be growing under traffic) and
`guva_audit_outbox_drain_errors_total` (should be ~flat).

**Verifying the wiring without breaking anything.** Stage rows directly
in any producer's outbox to watch the gauge spike and recover:

```bash
PGPASSWORD=guva psql -h localhost -U guva -d guva_identity -c "
INSERT INTO audit_outbox (event_id, payload)
SELECT gen_random_uuid(),
       jsonb_build_object('specversion','1.0','id', gen_random_uuid()::text,
         'source','identity','sourcekind','service','type','identity.burst.test',
         'subject','load-test','subjectkind','test',
         'time', to_char(now() at time zone 'utc','YYYY-MM-DD\"T\"HH24:MI:SS.MS\"Z\"'),
         'correlationid','','result','ok','data', jsonb_build_object('n',n))
FROM generate_series(1,250) AS s(n);"
curl -s http://localhost:7071/metrics | grep guva_audit_outbox
# Wait a few seconds, watch drain_total advance and unsent_count return to 0.
```

**Recovery from a real backlog.**

1. Check producer logs for `audit drain tick failed` errors — the message
   tells you whether Kafka or the DB is at fault.
2. Confirm Kafka is reachable from the producer:
   `docker exec guva-kafka /opt/kafka/bin/kafka-topics.sh --bootstrap-server localhost:9092 --list`.
3. Once Kafka is back, the Worker drains 100 rows/tick (2 ticks/sec), so
   a 10k row backlog clears in ~50 seconds without intervention.
4. If the gauge keeps growing despite a healthy Kafka, the schema
   validator is the next suspect (§4, §5 above).

**Why the gauge is per-producer.** Each service has its own outbox; the
audit service itself emits meta-audit, so `producer="audit"` is normal
and expected (don't confuse "audit producing audit" for a loop —
[`docs/AUDIT.md`](AUDIT.md) §7 explains the bounded recursion).

---

## Postgres roles

### 8. Audit chain — least-privilege roles

**What's protected.** Two Postgres roles back the audit service against
its database:

| Role | Privileges | Used by |
|---|---|---|
| `guva_audit_writer` | SELECT, INSERT on `audit_entries`; SELECT, INSERT, UPDATE on `audit_outbox`; sequence USAGE | Kafka chain consumer (AppendEntry), meta-audit emit, outbox drain Worker |
| `guva_audit_reader` | SELECT on `audit_entries` only | HTTP `/entries`, `/verify` handlers |

Neither role can DELETE from `audit_entries`. Neither can UPDATE it.
Even if the BEFORE UPDATE/DELETE trigger were dropped, both roles
would still be refused at the privilege layer — this is the second
of the two defenses on the chain.

**Where the boundary is enforced.**

- Roles created in [`deploy/compose/postgres/initdb.d/02-roles.sql`](../deploy/compose/postgres/initdb.d/02-roles.sql)
  at DB bootstrap (no grants — runs before migrations).
- Grants applied in [`services/audit/migrations/000003_grant_roles.up.sql`](../services/audit/migrations/000003_grant_roles.up.sql)
  so they re-apply alongside any schema change.
- Passwords held in Vault at `secret/services/audit/config`
  (`db-writer-password`, `db-reader-password`); seeded by
  [`tools/scripts/seed-vault.sh`](../tools/scripts/seed-vault.sh).
- Pool wiring in [`services/audit/internal/store/store.go`](../services/audit/internal/store/store.go) —
  `Store` holds two `*pgxpool.Pool`; `Reader()` and `Writer()`
  accessors keep the call sites explicit about which one they want.

**Verifying isolation locally.**

```bash
# Reader cannot mutate.
PGPASSWORD=audit-reader-dev psql -h localhost -U guva_audit_reader -d guva_audit \
  -c "DELETE FROM audit_entries WHERE entry_id = 1;"
# → ERROR:  permission denied for table audit_entries

# Writer cannot UPDATE or DELETE (only INSERT).
PGPASSWORD=audit-writer-dev psql -h localhost -U guva_audit_writer -d guva_audit \
  -c "UPDATE audit_entries SET action='tampered' WHERE entry_id = 1;"
# → ERROR:  permission denied for table audit_entries
```

**Recovery from a fresh sighting** (db migrated but roles missing):

```bash
docker exec guva-postgres psql -U guva -d guva -c "
  CREATE ROLE guva_audit_writer WITH LOGIN PASSWORD 'audit-writer-dev';
  CREATE ROLE guva_audit_reader WITH LOGIN PASSWORD 'audit-reader-dev';
  GRANT CONNECT ON DATABASE guva_audit TO guva_audit_writer, guva_audit_reader;
"
make migrate    # re-applies the grant migration
```

**Why other producers (identity, apisix-adapter) didn't get the same
split.** Their writes span business tables AND `audit_outbox` in a
single transaction (that's the whole point of the transactional outbox
pattern — atomic with the business write). Splitting their role would
either break the transaction or require complex grant juggling per
table. The audit service is the only place where read and write are
genuinely independent code paths, so it's where the split pays off.

If we later run those producers in a stricter mode, the pattern is the
same: create `<service>_writer` (INSERT on its business tables +
`audit_outbox`) and `<service>_reader` (SELECT on whatever the read
side touches), then thread two pools through the service.

---

### 9. Audit chain — monthly partitions (pg_partman)

**What's set up.** `audit_entries` is RANGE-partitioned on `occurred_at`,
one partition per month, managed by `pg_partman` (background worker
`pg_partman_bgw`, loaded via Postgres' `shared_preload_libraries`).
Five months of partitions are kept hot (premake=4 ahead + the current
month); a `DEFAULT` partition catches any out-of-range writes so
inserts never fail because a partition is missing.

**Why partition.** The chain grows unboundedly. Partitioning gives:

- O(1) drop of cold months (a `DROP TABLE` of the month's partition,
  vs. a `DELETE FROM` scan of a giant table).
- Partition pruning on date-range queries (export, anchor recompute,
  RFC3339 filters on `/v1/audit/entries`).
- Per-partition vacuum and analyze — write amplification stays bounded.
- Easy off-platform archival: dump one month's partition, attach
  the dump as the cold-storage record, detach the partition.

**Inspecting partition state.**

```bash
# What partitions exist
docker exec guva-postgres psql -U guva -d guva_audit -c \
  "SELECT inhrelid::regclass AS partition,
          (SELECT COUNT(*) FROM audit_entries WHERE tableoid = inhrelid) AS rows
     FROM pg_inherits WHERE inhparent='audit_entries'::regclass ORDER BY 1;"

# pg_partman's view of its config
docker exec guva-postgres psql -U guva -d guva_audit -c \
  "SELECT parent_table, partition_type, partition_interval, premake,
          retention, retention_keep_table
     FROM partman.part_config WHERE parent_table='public.audit_entries';"

# Which partition is a given entry in (use tableoid)
docker exec guva-postgres psql -U guva -d guva_audit -c \
  "SELECT entry_id, occurred_at, tableoid::regclass AS partition
     FROM audit_entries WHERE entry_id = 123;"
```

**Tuning retention.** By default no retention is set — every partition
sticks forever (correct posture for an audit chain in regulatory mode).
When you're ready to age cold months out:

```sql
UPDATE partman.part_config
   SET retention = '24 months',
       retention_keep_table = false  -- 'true' detaches but keeps; 'false' DROPs
 WHERE parent_table = 'public.audit_entries';
```

`pg_partman_bgw` will then DROP partitions older than the window on the
next maintenance tick (every minute by default). Coordinate with your
external Merkle anchor retention — dropping a partition before its
anchor is externally witnessed loses the ability to reconstruct
inclusion proofs for those entries.

**Adding a new producer table.** When you add another append-only
table that needs the same treatment (e.g. a future `audit_anchors`
partitioning by `computed_at`), follow the same pattern from
[`services/audit/migrations/000006_partition_audit_entries.up.sql`](../services/audit/migrations/000006_partition_audit_entries.up.sql):

1. Rename the existing table + its indexes + its sequence + its
   triggers to `_legacy`.
2. Build the partitioned parent with the partition key included in
   every UNIQUE/PRIMARY KEY constraint.
3. Re-attach triggers and grants on the parent (they cascade to all
   current/future partitions in PG14+).
4. Call `partman.create_parent(...)`.
5. `INSERT INTO ... SELECT * FROM ..._legacy ORDER BY ...`
6. Realign the sequence with `setval()`.
7. `DROP TABLE ..._legacy`.

**Common gotcha: index name collision.** Renaming a table does not
rename its indexes. If you create a new index with the canonical name
on the partitioned parent before renaming the legacy index, you get
`relation "idx_foo" already exists` and the whole transaction rolls
back. Always rename legacy indexes (and the PRIMARY KEY auto-index)
in the same step as the table rename.

**Prod-migration playbook.** What we did in dev is a single transaction
(atomic but blocking). For production where the table is large:

1. Build the partitioned parent under a temporary name in a non-blocking
   migration. Index it.
2. Backfill in batches: `INSERT INTO ..._new SELECT * FROM audit_entries
   WHERE entry_id BETWEEN $1 AND $2`, sized to fit your write window.
3. Switch new writes to the partitioned parent via a feature flag in
   the consumer code (write to both tables in parallel for a window).
4. Cutover: rename old → `_legacy`, rename new → `audit_entries`.
5. Tail: drain residual writes to `_legacy` into the new parent,
   then drop `_legacy`.

The dev-style atomic migration is fine until `audit_entries` crosses
~10M rows or so; past that, the lock and IO of the in-line copy push
you into the staged migration above.

---

## Log search

### 10. LogQL via Loki + Promtail

**Where logs live.** Two sources feed Loki:

1. **Container logs** — Docker writes each container's stdout/stderr
   to `/var/lib/docker/containers/<id>/<id>-json.log`. Promtail tails
   them all under `{job="docker"}`. The container name is auto-labelled
   via `container_id` (use `{container_id="..."}`).
2. **Host run-logs** — `make run-identity` etc. write to `/tmp/<svc>.log`.
   Promtail bind-mounts the host's `/tmp` and labels them as
   `{job="host", run_log="identity"}`.

**Querying.** Grafana: open the **"Audit — Log search"** dashboard
([deploy/compose/grafana/dashboards/audit-logs.json](../deploy/compose/grafana/dashboards/audit-logs.json))
or "Explore" → Loki datasource. CLI:

```bash
# Range query (LogQL requires a time range; the instant form isn't
# supported for log queries — only metric queries).
START=$(($(date -u +%s) * 1000000000 - 3600000000000))   # 1h ago, ns
END=$(($(date -u +%s) * 1000000000))

curl -s -G http://localhost:3100/loki/api/v1/query_range \
  --data-urlencode 'query={run_log="identity"} |= "consumer.created"' \
  --data-urlencode "start=$START" --data-urlencode "end=$END" \
  --data-urlencode 'limit=20' | jq '.data.result[].values'
```

**Useful LogQL snippets.**

```logql
# Every error from any host-side producer in the last 30m
{run_log=~"identity|audit|apisix-adapter"} |= "\"level\":\"ERROR\""

# Audit chain-append rate (set Worker log level to debug to see these)
sum (count_over_time({run_log="audit"} |= "audit event appended" [1m]))

# All gateway 5xx — joins via the request_id you'll see in the
# response and the JWT azp on the upstream side
{container_id=~".*"} |= "request_id" |= " 5"

# Trace a single request: take the correlation_id from a response
# header (X-Correlation-Id), then
{run_log=~"identity|audit"} |= "your-correlation-id-here"
```

**Cardinality discipline.** Promtail's labels (`job`, `run_log`,
`container_id`, `level`, `stream`) are intentionally low-cardinality.
Don't add per-request labels (request_id, user_id) — they belong in
the log message body, query with `|=` filters. Loki indexes labels;
adding high-cardinality ones blows up the index.

**Retention.** Loki dev config keeps everything (`compactor.retention_enabled: false`).
For prod, set `limits_config.retention_period: 720h` (or whatever the
data-protection window is) and turn on the compactor's retention.

**Datasource UID.** `P8E80F9AEF21F6940` (autogenerated by Grafana,
referenced in the audit-logs dashboard JSON). If you regenerate the
dashboard, leave the UID alone or update the dashboard panels in
lockstep.

---

## Webhooks

### 11. Outbound delivery pipeline

**What's wired.** `services/webhooks` exposes a subscription HTTP API
and runs three goroutines in one process:

1. **HTTP server** (`:7074`, also through the gateway at
   `/v1/webhooks/*`) — subscription CRUD + delivery list.
2. **Matcher** — Kafka consumer on the audit topic with its own group
   (`guva-webhooks-matcher`). For every event, looks up
   `webhook_subscriptions` whose `event_type_patterns` match, INSERTs
   a `webhook_deliveries` row, and publishes a delivery job to
   RabbitMQ exchange `guva.webhooks` (routing key `deliver`).
3. **Delivery worker** — consumes from queue `webhooks.delivery` with
   a per-message concurrency semaphore (8 in flight). Signs the body
   with HMAC-SHA256, POSTs to the subscription's `target_url`,
   exponential backoff on failure (30s → 2m → 8m → … capped 24h, 7
   attempts), terminal failures nack-without-requeue → dead-letter
   to `webhooks.delivery.dead`.

**HMAC signature recipe.** Stripe-style:

```
ts  = unix_epoch_seconds
sig = hex(hmac_sha256(secret, "<ts>." + raw_body))
header = "X-Guva-Signature: t=<ts>,v1=<sig>"
```

Verifier (any language):

```python
import hmac, hashlib
def verify(secret, body, header):
    parts = dict(p.split("=",1) for p in header.split(","))
    ts, sig = parts["t"], parts["v1"]
    mac = hmac.new(secret.encode(), (ts + ".").encode() + body, hashlib.sha256)
    return hmac.compare_digest(mac.hexdigest(), sig)
```

Also reject if `|now - ts| > 5min` to prevent replay.

**Subscription patterns.** `event_type_patterns` is a list of globs:

- `"*"` matches every event
- `"identity.*"` matches `identity.consumer.created` etc.
- `"audit.bundle.exported"` is exact-match

A subscription matches if ANY of its patterns matches the event's `type`.

**Demo end-to-end.**

```bash
# 1. Start the receiver in a separate shell.
WH_SECRET=$(jq -r .secret < /tmp/sub.json) \
  go run /tmp/webhook-receiver-demo/main.go

# 2. Create a subscription (admin token; webhooks Bruno collection 01).
curl -X POST http://localhost:8000/v1/webhooks/subscriptions \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H "Content-Type: application/json" \
  -d '{"consumer_id":"demo","target_url":"http://localhost:9099/hook","event_type_patterns":["identity.*"]}' \
  > /tmp/sub.json

# 3. Trigger an audit event — e.g. create a consumer.
curl -X POST http://localhost:8000/v1/identity/consumers ...

# 4. Receiver shows the POST + verified signature.
```

**Recovery: DLQ replay.** Failed deliveries land in
`webhooks.delivery.dead` (RabbitMQ queue). Inspect via the management UI
at http://localhost:15672 (guva/guva), or pull them back to the main
queue with `rabbitmqctl shovel` once you've fixed the consumer endpoint.
A programmatic replay endpoint is planned but not yet exposed.

**Throughput envelope.** The matcher chains 1 INSERT + 1 AMQP publish
per matching subscription per event. With prefetch 8 + 8 worker
goroutines, sustainable delivery rate is bounded by the slowest
consumer's response time. Add a separate worker pool per route or
shard subscriptions across multiple consumer instances when needed.

---

## Cross-references

- [`docs/AUDIT.md`](AUDIT.md) — audit chain architecture and producer
  contract (the source of truth for everything audit-related).
- [`services/apisix-adapter/README.md`](../services/apisix-adapter/README.md)
  *(planned)* — operator notes for the gateway access-log adapter.
- [`services/audit/README.md`](../services/audit/README.md) — operator
  notes for the audit service itself.
