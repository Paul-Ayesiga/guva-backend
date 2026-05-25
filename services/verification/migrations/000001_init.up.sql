-- Verification service tables.
--
-- We keep TWO things here:
--   1. An operational log of every verification call (rate-limit
--      analysis, abuse detection, who-asked-about-whom forensics).
--      The platform-wide audit chain has its own copy keyed by the
--      same verification_id; this is the service-local view that
--      doesn't need a Kafka roundtrip to consult.
--   2. A short-lived idempotency cache. Repeating the exact same
--      verification within the TTL returns the cached canonical
--      response — saves a NIRA call and keeps response semantics
--      stable when a consumer retries on network error.
--
-- PII handling: the subject identifier (NIN) is hashed before storage.
-- The actual NIRA response is also NOT persisted here — only the
-- canonical match summary (per-attribute boolean) and the resulting
-- status. The audit chain holds the same summary; together they let
-- regulators reconstruct "consumer X verified subject Y at time Z"
-- without ever storing the citizen's attributes a second time.

CREATE TABLE IF NOT EXISTS verification_log (
    id                   BIGSERIAL PRIMARY KEY,
    verification_id      UUID UNIQUE NOT NULL DEFAULT gen_random_uuid(),
    consumer_id          VARCHAR(128) NOT NULL,   -- JWT azp claim
    subject_type         VARCHAR(32)  NOT NULL,   -- "nin", "passport", ...
    subject_hash         CHAR(64)     NOT NULL,   -- SHA-256 of subject identifier
    consent_reference    VARCHAR(128),            -- stub today; consent service ships separately
    upstream             VARCHAR(32)  NOT NULL,   -- "NIRA" / "URSB" / ...
    status               VARCHAR(32)  NOT NULL,   -- verified | mismatch | not_found | deceased | revoked | error
    requested_attributes TEXT[]       NOT NULL,   -- which fields the caller asked us to check
    match_count          INT          NOT NULL DEFAULT 0,  -- how many matched
    mismatch_count       INT          NOT NULL DEFAULT 0,
    upstream_latency_ms  INT,
    correlation_id       UUID,
    requested_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_vlog_consumer ON verification_log (consumer_id, requested_at DESC);
CREATE INDEX IF NOT EXISTS idx_vlog_subject  ON verification_log (subject_hash, requested_at DESC);
CREATE INDEX IF NOT EXISTS idx_vlog_status   ON verification_log (status);

-- The cache. (consumer_id, subject_type, subject_hash, request_fingerprint)
-- is the key; request_fingerprint is SHA-256 of the canonical request
-- body so callers who change ANY claimed attribute get a fresh look.
CREATE TABLE IF NOT EXISTS verification_cache (
    consumer_id         VARCHAR(128) NOT NULL,
    subject_type        VARCHAR(32)  NOT NULL,
    subject_hash        CHAR(64)     NOT NULL,
    request_fingerprint CHAR(64)     NOT NULL,
    response_body       JSONB        NOT NULL,
    cached_at           TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    expires_at          TIMESTAMPTZ  NOT NULL,
    PRIMARY KEY (consumer_id, subject_type, subject_hash, request_fingerprint)
);

CREATE INDEX IF NOT EXISTS idx_vcache_expires ON verification_cache (expires_at);
