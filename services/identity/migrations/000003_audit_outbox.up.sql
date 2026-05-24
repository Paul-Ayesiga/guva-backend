-- Audit outbox — every identity write also stages an audit event here,
-- inside the same transaction. The pkg/platform/audit Worker tails the
-- table and publishes to Kafka; rows are marked sent once published.
--
-- Schema must match pkg/platform/audit.OutboxMigration exactly. Source
-- of truth lives there; this migration is the per-service apply step.

CREATE TABLE IF NOT EXISTS audit_outbox (
    id          BIGSERIAL PRIMARY KEY,
    event_id    UUID UNIQUE NOT NULL,
    payload     JSONB NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    sent_at     TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_audit_outbox_unsent
    ON audit_outbox(id) WHERE sent_at IS NULL;
