REVOKE USAGE, SELECT ON SEQUENCE audit_anchors_anchor_id_seq FROM guva_audit_writer;
REVOKE SELECT, UPDATE ON audit_anchors FROM guva_audit_operator;
REVOKE SELECT         ON audit_anchors FROM guva_audit_reader;
REVOKE SELECT, INSERT ON audit_anchors FROM guva_audit_writer;
REVOKE USAGE ON SCHEMA public FROM guva_audit_operator;
REVOKE CONNECT ON DATABASE guva_audit FROM guva_audit_operator;
-- We don't DROP ROLE here — the role may be reused elsewhere and
-- dropping it would orphan any privileges granted out of scope.
