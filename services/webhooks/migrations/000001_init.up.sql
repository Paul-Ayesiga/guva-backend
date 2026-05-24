-- Webhook delivery service tables.
--
-- A subscription is "consumer X wants events matching pattern Y POSTed
-- to URL Z, signed with shared secret S". The matcher reads audit
-- events off Kafka, looks up matching subscriptions, and publishes a
-- delivery job to RabbitMQ. The delivery worker pulls jobs, signs
-- payloads with HMAC-SHA256, POSTs to the target URL with exponential
-- backoff, and writes the outcome to webhook_deliveries.

CREATE TABLE IF NOT EXISTS webhook_subscriptions (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    consumer_id           VARCHAR(128) NOT NULL,   -- maps to identity's consumer registration's keycloak_client_id
    target_url            TEXT NOT NULL,
    event_type_patterns   TEXT[] NOT NULL,         -- e.g. ["identity.*", "audit.bundle.exported"] or ["*"]
    secret                TEXT NOT NULL,           -- shared HMAC-SHA256 secret; returned once on creation
    enabled               BOOLEAN NOT NULL DEFAULT true,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_delivery_at      TIMESTAMPTZ,
    last_delivery_status  INT,
    last_delivery_error   TEXT
);

CREATE INDEX IF NOT EXISTS idx_subs_consumer ON webhook_subscriptions(consumer_id);
CREATE INDEX IF NOT EXISTS idx_subs_enabled ON webhook_subscriptions(enabled) WHERE enabled = true;

CREATE TABLE IF NOT EXISTS webhook_deliveries (
    id              BIGSERIAL PRIMARY KEY,
    delivery_uuid   UUID UNIQUE NOT NULL DEFAULT gen_random_uuid(),
    subscription_id UUID NOT NULL REFERENCES webhook_subscriptions(id) ON DELETE CASCADE,
    event_uuid      UUID NOT NULL,
    event_type      VARCHAR(128) NOT NULL,
    attempt         INT NOT NULL DEFAULT 0,
    status          VARCHAR(16) NOT NULL,    -- pending, ok, retry, dlq, error
    http_status     INT,
    response_excerpt TEXT,
    error           TEXT,
    queued_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    attempted_at    TIMESTAMPTZ,
    next_retry_at   TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_deliveries_sub ON webhook_deliveries(subscription_id);
CREATE INDEX IF NOT EXISTS idx_deliveries_event ON webhook_deliveries(event_uuid);
CREATE INDEX IF NOT EXISTS idx_deliveries_status ON webhook_deliveries(status);
