-- Audit outbox — every verification call also stages an audit event
-- here in the same transaction as the verification_log INSERT. The
-- pkg/platform/audit Worker tails this table and publishes to Kafka
-- where the audit consumer hash-chains it.
--
-- Schema must match pkg/platform/audit.OutboxMigration exactly.

CREATE TABLE IF NOT EXISTS audit_outbox (
    id          BIGSERIAL PRIMARY KEY,
    event_id    UUID UNIQUE NOT NULL,
    payload     JSONB NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    sent_at     TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_audit_outbox_unsent
    ON audit_outbox(id) WHERE sent_at IS NULL;
