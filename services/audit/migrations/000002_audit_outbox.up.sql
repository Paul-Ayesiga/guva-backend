-- Meta-audit outbox — the audit service itself emits events when it is
-- READ. Every /entries query and /verify walk lands a row here, the
-- pkg/platform/audit Worker drains it to Kafka, and the audit consumer
-- (this same service) inserts it as a chain entry.
--
-- This closes the "who read the audit log" loop: an insider with
-- audit:read scope can no longer query the ledger without leaving a
-- trace in the ledger itself.
--
-- Schema must match pkg/platform/audit.OutboxMigration. Identical to
-- services/identity/migrations/000003_audit_outbox.up.sql.

CREATE TABLE IF NOT EXISTS audit_outbox (
    id          BIGSERIAL PRIMARY KEY,
    event_id    UUID UNIQUE NOT NULL,
    payload     JSONB NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    sent_at     TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_audit_outbox_unsent
    ON audit_outbox(id) WHERE sent_at IS NULL;
