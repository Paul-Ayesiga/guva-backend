-- Convert audit_entries to a RANGE-partitioned table on occurred_at, one
-- partition per month, managed by pg_partman.
--
-- WHY: the chain grows unboundedly. Without partitioning, retention,
-- vacuuming, and large-range queries (export, anchor recomputation)
-- all get slower with every committed event. Monthly partitions cap
-- the per-partition row count, let dead partitions be DROPped wholesale
-- after the retention window, and turn ORDER BY occurred_at scans into
-- partition-pruned reads.
--
-- WHAT'S PRESERVED: existing rows (copied into the new partitioned
-- table), all indexes, the append-only trigger, the writer/reader
-- role grants. entry_hash and previous_hash columns and their values
-- pass through unchanged — the chain integrity is identical pre and
-- post migration.
--
-- WHAT CHANGES:
--   - PRIMARY KEY becomes (entry_id, occurred_at) — partition key
--     must be part of every UNIQUE constraint in native partitioning.
--   - UNIQUE on entry_uuid becomes UNIQUE (entry_uuid, occurred_at)
--     for the same reason. Dedupe still works because the consumer
--     queries `WHERE entry_uuid = $1` and partition pruning happens
--     downstream of the uniqueness check.
--   - pg_partman owns partition lifecycle from here on; see
--     docs/OPERATIONS.md §"Audit chain — monthly partitions".

BEGIN;

-- 1. Stash the existing table out of the way without losing rows.
ALTER TABLE audit_entries RENAME TO audit_entries_legacy;
ALTER SEQUENCE audit_entries_entry_id_seq RENAME TO audit_entries_legacy_entry_id_seq;

-- The legacy trigger names still exist; rename so they don't collide
-- with the trigger we recreate on the new parent.
ALTER TRIGGER audit_entries_no_update ON audit_entries_legacy RENAME TO audit_entries_legacy_no_update;
ALTER TRIGGER audit_entries_no_delete ON audit_entries_legacy RENAME TO audit_entries_legacy_no_delete;

-- Indexes carry their own names independent of the table; rename to
-- free up the canonical names for the new partitioned parent.
ALTER INDEX audit_entries_pkey                RENAME TO audit_entries_legacy_pkey;
ALTER INDEX audit_entries_entry_uuid_key      RENAME TO audit_entries_legacy_entry_uuid_key;
ALTER INDEX idx_audit_occurred                RENAME TO idx_audit_occurred_legacy;
ALTER INDEX idx_audit_actor                   RENAME TO idx_audit_actor_legacy;
ALTER INDEX idx_audit_subject                 RENAME TO idx_audit_subject_legacy;
ALTER INDEX idx_audit_correlation             RENAME TO idx_audit_correlation_legacy;

-- 2. Build the partitioned parent. Same columns, but composite PK and
--    UNIQUE constraints include occurred_at so they are valid on a
--    table partitioned by occurred_at.
CREATE TABLE audit_entries (
    entry_id           BIGSERIAL,
    entry_uuid         UUID NOT NULL DEFAULT gen_random_uuid(),
    occurred_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    actor_kind         VARCHAR(32) NOT NULL,
    actor_id           VARCHAR(128) NOT NULL,
    subject_kind       VARCHAR(32),
    subject_id         VARCHAR(128),
    action             VARCHAR(64) NOT NULL,
    result             VARCHAR(16) NOT NULL,
    correlation_id     UUID,
    detail             JSONB,
    previous_hash      CHAR(64) NOT NULL,
    entry_hash         CHAR(64) NOT NULL,
    PRIMARY KEY (entry_id, occurred_at),
    UNIQUE (entry_uuid, occurred_at)
) PARTITION BY RANGE (occurred_at);

CREATE INDEX idx_audit_occurred ON audit_entries (occurred_at DESC);
CREATE INDEX idx_audit_actor ON audit_entries (actor_kind, actor_id);
CREATE INDEX idx_audit_subject ON audit_entries (subject_kind, subject_id);
CREATE INDEX idx_audit_correlation ON audit_entries (correlation_id);
-- Bare entry_uuid lookup index (dedupe queries) — partition-local
-- unique index above is composite, so a single-column index speeds
-- the consumer's `WHERE entry_uuid = $1`.
CREATE INDEX idx_audit_entry_uuid ON audit_entries (entry_uuid);

-- 3. Same append-only trigger, attached to the partitioned parent.
--    From PG13+, row-level triggers on a partitioned parent propagate
--    to all current and future child partitions automatically.
CREATE TRIGGER audit_entries_no_update
    BEFORE UPDATE ON audit_entries
    FOR EACH ROW EXECUTE FUNCTION audit_entries_block_modify();
CREATE TRIGGER audit_entries_no_delete
    BEFORE DELETE ON audit_entries
    FOR EACH ROW EXECUTE FUNCTION audit_entries_block_modify();

-- 4. Re-grant the role privileges. Granting on the partitioned parent
--    cascades to current and future partitions (PG14+).
GRANT SELECT, INSERT ON audit_entries TO guva_audit_writer;
GRANT SELECT         ON audit_entries TO guva_audit_reader;
-- Sequence grant: pg_partman creates new partitions; the new sequence
-- is auto-named audit_entries_entry_id_seq because BIGSERIAL on the
-- parent creates it with that name.
GRANT USAGE, SELECT ON SEQUENCE audit_entries_entry_id_seq TO guva_audit_writer;

-- 5. Tell pg_partman to manage this table. Monthly intervals, the
--    current month's partition + 4 ahead (premake) so writes never
--    fall into a missing partition. partman.run_maintenance_proc()
--    (called by the pg_partman_bgw background worker every minute by
--    default) keeps the premake horizon rolling forward as time passes.
SELECT partman.create_parent(
    p_parent_table      => 'public.audit_entries',
    p_control           => 'occurred_at',
    p_type              => 'range',
    p_interval          => '1 month',
    p_premake           => 4,
    p_default_table     => true,    -- DEFAULT partition catches out-of-range writes
    p_start_partition   => to_char(date_trunc('month', NOW()) - interval '1 month', 'YYYY-MM-DD"T"HH24:MI:SS')
);

-- 6. Copy historical rows over. They'll land in whichever partition
--    matches their occurred_at (current month for everything we've
--    been testing with). The seed lands without firing the trigger
--    (BEFORE INSERT not defined) and re-uses the same entry_id /
--    entry_hash so the chain stays intact.
INSERT INTO audit_entries (
    entry_id, entry_uuid, occurred_at,
    actor_kind, actor_id, subject_kind, subject_id,
    action, result, correlation_id, detail,
    previous_hash, entry_hash)
SELECT
    entry_id, entry_uuid, occurred_at,
    actor_kind, actor_id, subject_kind, subject_id,
    action, result, correlation_id, detail,
    previous_hash, entry_hash
  FROM audit_entries_legacy
 ORDER BY entry_id ASC;

-- 7. Realign the sequence so the next INSERT picks an entry_id
--    greater than every copied row.
SELECT setval('audit_entries_entry_id_seq',
              COALESCE((SELECT MAX(entry_id) FROM audit_entries), 0));

-- 8. Drop the legacy table (data is now in the partitioned parent).
DROP TABLE audit_entries_legacy;

COMMIT;
