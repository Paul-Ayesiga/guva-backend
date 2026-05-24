-- audit_entries: the append-only, hash-chained ledger of every action
-- that affects the security or integrity of the platform. Schema from
-- guva-docs/03-architecture/08-database-design.md §8.3.
--
-- The hash chain is single-writer (only this service writes; producers
-- emit events to Kafka, never INSERT directly). previous_hash on the
-- first row is the genesis "0000...0000".

CREATE TABLE audit_entries (
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

CREATE INDEX idx_audit_occurred ON audit_entries(occurred_at DESC);
CREATE INDEX idx_audit_actor ON audit_entries(actor_kind, actor_id);
CREATE INDEX idx_audit_subject ON audit_entries(subject_kind, subject_id);
CREATE INDEX idx_audit_correlation ON audit_entries(correlation_id);

-- Append-only enforcement at the DB layer. Application bugs and operator
-- mistakes are inevitable; this trigger guarantees the invariant holds
-- regardless of which connection or role attempts the modification.
CREATE OR REPLACE FUNCTION audit_entries_block_modify()
RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'audit_entries is append-only; UPDATE and DELETE are forbidden';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER audit_entries_no_update
    BEFORE UPDATE ON audit_entries
    FOR EACH ROW EXECUTE FUNCTION audit_entries_block_modify();

CREATE TRIGGER audit_entries_no_delete
    BEFORE DELETE ON audit_entries
    FOR EACH ROW EXECUTE FUNCTION audit_entries_block_modify();

-- No genesis row. The first real entry uses previous_hash = repeat('0',64)
-- and the verifier treats that literal as the chain anchor. This avoids
-- having to embed a precomputed hash in the migration and re-derive it
-- on every test environment.
