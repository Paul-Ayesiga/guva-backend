-- Rolling back partitioning. Lossless except that the new entries
-- inserted while partitioned are kept (they survive the un-partition).
--
-- The procedure:
--   1. Drop pg_partman management of the parent.
--   2. Build a plain (non-partitioned) staging table.
--   3. Copy every row out of the partitioned parent.
--   4. Swap names.
--   5. Recreate trigger + grants on the plain table.

BEGIN;

-- Stop pg_partman from auto-managing the parent.
SELECT partman.undo_partition(
    p_parent_table  => 'public.audit_entries',
    p_keep_table    => false  -- partman drops its own metadata for this parent
);

-- Build plain replacement.
CREATE TABLE audit_entries_plain (
    entry_id           BIGSERIAL PRIMARY KEY,
    entry_uuid         UUID UNIQUE NOT NULL DEFAULT gen_random_uuid(),
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
    entry_hash         CHAR(64) NOT NULL
);

INSERT INTO audit_entries_plain (
    entry_id, entry_uuid, occurred_at,
    actor_kind, actor_id, subject_kind, subject_id,
    action, result, correlation_id, detail,
    previous_hash, entry_hash)
SELECT
    entry_id, entry_uuid, occurred_at,
    actor_kind, actor_id, subject_kind, subject_id,
    action, result, correlation_id, detail,
    previous_hash, entry_hash
  FROM audit_entries
 ORDER BY entry_id ASC;

DROP TABLE audit_entries CASCADE;
ALTER TABLE audit_entries_plain RENAME TO audit_entries;
ALTER SEQUENCE audit_entries_plain_entry_id_seq RENAME TO audit_entries_entry_id_seq;

SELECT setval('audit_entries_entry_id_seq',
              COALESCE((SELECT MAX(entry_id) FROM audit_entries), 0));

CREATE INDEX idx_audit_occurred ON audit_entries(occurred_at DESC);
CREATE INDEX idx_audit_actor ON audit_entries(actor_kind, actor_id);
CREATE INDEX idx_audit_subject ON audit_entries(subject_kind, subject_id);
CREATE INDEX idx_audit_correlation ON audit_entries(correlation_id);

CREATE TRIGGER audit_entries_no_update
    BEFORE UPDATE ON audit_entries
    FOR EACH ROW EXECUTE FUNCTION audit_entries_block_modify();
CREATE TRIGGER audit_entries_no_delete
    BEFORE DELETE ON audit_entries
    FOR EACH ROW EXECUTE FUNCTION audit_entries_block_modify();

GRANT SELECT, INSERT ON audit_entries TO guva_audit_writer;
GRANT SELECT         ON audit_entries TO guva_audit_reader;
GRANT USAGE, SELECT ON SEQUENCE audit_entries_entry_id_seq TO guva_audit_writer;

COMMIT;
