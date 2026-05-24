DROP TRIGGER IF EXISTS audit_entries_no_delete ON audit_entries;
DROP TRIGGER IF EXISTS audit_entries_no_update ON audit_entries;
DROP FUNCTION IF EXISTS audit_entries_block_modify();
DROP INDEX IF EXISTS idx_audit_correlation;
DROP INDEX IF EXISTS idx_audit_subject;
DROP INDEX IF EXISTS idx_audit_actor;
DROP INDEX IF EXISTS idx_audit_occurred;
DROP TABLE IF EXISTS audit_entries;
