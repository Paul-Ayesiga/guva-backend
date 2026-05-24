-- APISIX-adapter audit outbox. The adapter receives access-log batches
-- from APISIX (http-logger plugin), transforms each entry into a
-- CloudEvents envelope, and stages it here. pkg/platform/audit.Worker
-- drains to Kafka; the audit consumer hash-chains it like any other
-- producer's events.
--
-- Schema mirrors pkg/platform/audit.OutboxMigration; do not diverge.

CREATE TABLE IF NOT EXISTS audit_outbox (
    id          BIGSERIAL PRIMARY KEY,
    event_id    UUID UNIQUE NOT NULL,
    payload     JSONB NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    sent_at     TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_audit_outbox_unsent
    ON audit_outbox(id) WHERE sent_at IS NULL;
