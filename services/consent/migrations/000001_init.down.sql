DROP INDEX IF EXISTS idx_audit_outbox_unsent;
DROP TABLE IF EXISTS audit_outbox;
DROP TRIGGER IF EXISTS consent_grants_no_mutation ON consent_grants;
DROP FUNCTION IF EXISTS consent_grants_block_mutations();
DROP INDEX IF EXISTS idx_consent_consumer;
DROP INDEX IF EXISTS idx_consent_citizen;
DROP TABLE IF EXISTS consent_grants;
