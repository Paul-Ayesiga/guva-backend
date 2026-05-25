-- Consent service tables.
--
-- A consent_grant is a citizen's authorisation for a specific
-- consumer to verify a specific set of attributes against a specific
-- upstream agency, for a stated purpose, until a stated expiry.
-- Every grant carries an Ed25519-signed assertion (JWT-like) so that
-- an external auditor can prove the platform issued it, even if the
-- platform's database is later compromised.
--
-- PII discipline (mirrors verification service): the citizen is
-- referenced by SHA-256(NIN), not the raw NIN. Same hashing recipe
-- as services/verification/internal/store.HashSubject so cross-
-- service joins (consent grants for the same citizen) work via the
-- same hash space without ever putting the NIN on disk.

CREATE TABLE IF NOT EXISTS consent_grants (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    citizen_subject_type  VARCHAR(32) NOT NULL,    -- "nin"
    citizen_subject_hash  CHAR(64)    NOT NULL,    -- SHA-256 of the NIN
    consumer_id           VARCHAR(128) NOT NULL,   -- e.g. "acacia-onboarding"
    upstream              VARCHAR(32) NOT NULL,    -- "NIRA" / "URSB" / ...
    purpose               TEXT        NOT NULL,    -- "loan-application", "tenancy-check"
    allowed_attributes    TEXT[]      NOT NULL,    -- {"nin","given_name","surname"} or {"*"}
    granted_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at            TIMESTAMPTZ NOT NULL,
    revoked_at            TIMESTAMPTZ,
    revocation_reason     TEXT,

    -- Signed assertion: a small JWT signed by the consent service
    -- using its Ed25519 key. The verification service includes the
    -- assertion in the audit chain entry so future regulators can
    -- verify the grant existed without contacting GUVA.
    assertion_jwt         TEXT NOT NULL,
    signing_key_id        VARCHAR(64) NOT NULL,

    CHECK (expires_at > granted_at),
    CHECK (array_length(allowed_attributes, 1) > 0)
);

CREATE INDEX IF NOT EXISTS idx_consent_citizen
    ON consent_grants(citizen_subject_hash, granted_at DESC);
CREATE INDEX IF NOT EXISTS idx_consent_consumer
    ON consent_grants(consumer_id, expires_at);

-- Append-only enforcement on the historical record. Revocation is
-- the one allowed mutation; everything else raises. The trigger lets
-- through an UPDATE only when revoked_at goes from NULL to a value
-- and revocation_reason fills in — i.e. a single revoke transition,
-- never a re-write of granted fields.
CREATE OR REPLACE FUNCTION consent_grants_block_mutations()
    RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'consent_grants is append-only; DELETE is forbidden';
    END IF;
    IF TG_OP = 'UPDATE' THEN
        IF OLD.revoked_at IS NOT NULL THEN
            RAISE EXCEPTION 'consent grant % is already revoked; cannot mutate', OLD.id;
        END IF;
        IF NEW.id              <> OLD.id
           OR NEW.citizen_subject_type <> OLD.citizen_subject_type
           OR NEW.citizen_subject_hash <> OLD.citizen_subject_hash
           OR NEW.consumer_id          <> OLD.consumer_id
           OR NEW.upstream             <> OLD.upstream
           OR NEW.purpose              <> OLD.purpose
           OR NEW.allowed_attributes   <> OLD.allowed_attributes
           OR NEW.granted_at           <> OLD.granted_at
           OR NEW.expires_at           <> OLD.expires_at
           OR NEW.assertion_jwt        <> OLD.assertion_jwt
           OR NEW.signing_key_id       <> OLD.signing_key_id THEN
            RAISE EXCEPTION 'consent_grants is append-only; only revoked_at/revocation_reason may change';
        END IF;
        IF NEW.revoked_at IS NULL THEN
            RAISE EXCEPTION 'consent grant update must set revoked_at (this is the revoke transition)';
        END IF;
        RETURN NEW;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER consent_grants_no_mutation
    BEFORE UPDATE OR DELETE ON consent_grants
    FOR EACH ROW EXECUTE FUNCTION consent_grants_block_mutations();

-- Audit outbox — every grant / revoke / verify lands a chain event
-- in the same transaction as the table mutation.
CREATE TABLE IF NOT EXISTS audit_outbox (
    id          BIGSERIAL PRIMARY KEY,
    event_id    UUID UNIQUE NOT NULL,
    payload     JSONB NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    sent_at     TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_audit_outbox_unsent
    ON audit_outbox(id) WHERE sent_at IS NULL;
