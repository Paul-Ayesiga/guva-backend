-- Grants for the anchor table. The periodic anchor job in the audit
-- service runs as guva_audit_writer; the operator role
-- (guva_audit_operator) is separate and used only by humans/automation
-- updating external_proof after publishing the root externally.
--
-- The reader role gets SELECT — anchors are publicly readable so
-- external verifiers can fetch one and verify a Merkle proof against
-- the same root they got from the external witness.

-- Operator role: created only if missing so re-running this migration
-- in environments where the role exists already is safe.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'guva_audit_operator') THEN
        CREATE ROLE guva_audit_operator WITH LOGIN PASSWORD 'audit-operator-dev';
    END IF;
END
$$;

GRANT CONNECT ON DATABASE guva_audit TO guva_audit_operator;
GRANT USAGE ON SCHEMA public TO guva_audit_operator;

GRANT SELECT, INSERT ON audit_anchors TO guva_audit_writer;
GRANT SELECT         ON audit_anchors TO guva_audit_reader;
-- Operator can SELECT and UPDATE — the trigger restricts UPDATEs to
-- the external_proof column only (see 000004_audit_anchors.up.sql).
GRANT SELECT, UPDATE ON audit_anchors TO guva_audit_operator;

GRANT USAGE, SELECT ON SEQUENCE audit_anchors_anchor_id_seq TO guva_audit_writer;
