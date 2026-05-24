-- Anchor log — append-only record of Merkle roots over consecutive
-- ranges of the audit chain. The audit service periodically computes
-- the Merkle root over (last anchored entry_id + 1) .. (current max
-- entry_id) and inserts a row here.
--
-- An operator may then publish `merkle_root` to an external witness
-- (consortium ledger, Sigstore Rekor, public blockchain) and store the
-- witness's receipt in `external_proof`. Once that's done, a verifier
-- holding any entry + the inclusion proof for it can prove to a third
-- party that the entry was on our chain at that anchor's time, without
-- trusting GUVA — they only need to trust the external witness.
--
-- The table is append-only via trigger AND via role grants (writer
-- gets INSERT only, no UPDATE/DELETE). external_proof is the one
-- exception: an operator must be able to record the witness receipt
-- *after* the row is inserted. We handle this via an UPDATE allowance
-- targeted at that single column from a dedicated `guva_audit_operator`
-- role; the writer (the periodic job) never sees that role.

CREATE TABLE IF NOT EXISTS audit_anchors (
    anchor_id        BIGSERIAL PRIMARY KEY,
    range_from_id    BIGINT      NOT NULL,
    range_to_id      BIGINT      NOT NULL,
    leaf_count       BIGINT      NOT NULL,
    merkle_root      TEXT        NOT NULL,  -- hex SHA-256
    algorithm        TEXT        NOT NULL DEFAULT 'sha256-binary-merkle-v1',
    computed_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- External witness receipt — free-text, populated by an operator
    -- after publishing merkle_root to a Rekor/blockchain/consortium
    -- ledger. Empty until then.
    external_proof   JSONB,
    CHECK (range_from_id <= range_to_id),
    CHECK (leaf_count > 0)
);

CREATE INDEX IF NOT EXISTS idx_audit_anchors_range
    ON audit_anchors (range_from_id, range_to_id);
CREATE INDEX IF NOT EXISTS idx_audit_anchors_computed
    ON audit_anchors (computed_at DESC);

-- Append-only enforcement: writer role lacks UPDATE/DELETE, but the
-- trigger is defence in depth — if a future migration relaxes the
-- grant by accident, the trigger still bites. external_proof updates
-- are routed through a separate role (see grant migration below).
CREATE OR REPLACE FUNCTION audit_anchors_block_mutations()
    RETURNS TRIGGER AS $$
BEGIN
    -- Allow only updates that change external_proof and nothing else,
    -- and only when the operator role explicitly performed them. The
    -- check on session_user keeps the writer from accidentally
    -- discovering this path.
    IF TG_OP = 'UPDATE' AND session_user = 'guva_audit_operator' THEN
        IF NEW.anchor_id      = OLD.anchor_id
           AND NEW.range_from_id = OLD.range_from_id
           AND NEW.range_to_id   = OLD.range_to_id
           AND NEW.leaf_count    = OLD.leaf_count
           AND NEW.merkle_root   = OLD.merkle_root
           AND NEW.algorithm     = OLD.algorithm
           AND NEW.computed_at   = OLD.computed_at THEN
            -- only external_proof changed; allow
            RETURN NEW;
        END IF;
    END IF;
    RAISE EXCEPTION 'audit_anchors is append-only (only operator may set external_proof)';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER audit_anchors_no_mutation
    BEFORE UPDATE OR DELETE ON audit_anchors
    FOR EACH ROW EXECUTE FUNCTION audit_anchors_block_mutations();
