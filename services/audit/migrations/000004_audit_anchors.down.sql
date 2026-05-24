DROP TRIGGER IF EXISTS audit_anchors_no_mutation ON audit_anchors;
DROP FUNCTION IF EXISTS audit_anchors_block_mutations();
DROP INDEX IF EXISTS idx_audit_anchors_computed;
DROP INDEX IF EXISTS idx_audit_anchors_range;
DROP TABLE IF EXISTS audit_anchors;
