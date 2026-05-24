-- idempotency_keys: caches a response for replay when the same caller
-- retries with the same Idempotency-Key header. Implements the safety
-- net for the "I sent POST /consumers, network died before I got the
-- 201" case — without this, the retry creates a duplicate Keycloak
-- client (or fails 409 because the previous attempt did).
--
-- Fingerprint protects against the same key being reused for a different
-- payload (which would be a client bug); we reject that as 422.

CREATE TABLE idempotency_keys (
    key                   VARCHAR(255) PRIMARY KEY,
    request_fingerprint   CHAR(64) NOT NULL,        -- hex-encoded SHA-256 of canonical request bytes
    response_status       INTEGER NOT NULL,
    response_body         BYTEA NOT NULL,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at            TIMESTAMPTZ NOT NULL
);

CREATE INDEX idx_idempotency_keys_expires_at ON idempotency_keys(expires_at);

-- Old keys can be pruned by a periodic job:
--   DELETE FROM idempotency_keys WHERE expires_at < NOW();
