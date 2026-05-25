-- Integration-service tables. Two things only:
--
--   1. The audit outbox (every lookup emits an event).
--   2. A short operational log of lookups for forensics + the per-
--      agency observability dashboards.
--
-- The actual NIRA response is NEVER stored — only metadata about
-- the call (subject hash, status, latency, error class). That keeps
-- this DB out of the PII honeypot category; the citizen's identity
-- attributes live exactly twice — at NIRA itself, and in transient
-- memory for the duration of the lookup.

CREATE TABLE IF NOT EXISTS lookup_log (
    id              BIGSERIAL PRIMARY KEY,
    lookup_id       UUID UNIQUE NOT NULL DEFAULT gen_random_uuid(),
    backend         VARCHAR(32) NOT NULL,    -- "simulator" or "upstream"
    caller          VARCHAR(128) NOT NULL,   -- JWT azp claim, e.g. "verification"
    subject_type    VARCHAR(32) NOT NULL,    -- "nin"
    subject_hash    CHAR(64) NOT NULL,       -- SHA-256(NIN); same recipe as verification + consent
    status          VARCHAR(32) NOT NULL,    -- "found" | "not_found" | "deceased" | "revoked" | "upstream_error" | "timeout"
    upstream_status_code INT,                -- HTTP status if upstream backend
    latency_ms      INT NOT NULL,
    correlation_id  UUID,
    requested_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_lookup_subject ON lookup_log (subject_hash, requested_at DESC);
CREATE INDEX IF NOT EXISTS idx_lookup_caller  ON lookup_log (caller, requested_at DESC);
CREATE INDEX IF NOT EXISTS idx_lookup_status  ON lookup_log (status);

CREATE TABLE IF NOT EXISTS audit_outbox (
    id          BIGSERIAL PRIMARY KEY,
    event_id    UUID UNIQUE NOT NULL,
    payload     JSONB NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    sent_at     TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_audit_outbox_unsent
    ON audit_outbox(id) WHERE sent_at IS NULL;
