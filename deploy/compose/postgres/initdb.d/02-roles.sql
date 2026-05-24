-- =============================================================================
-- Per-service Postgres roles (least-privilege boundary).
--
-- The audit service runs two distinct connections against the same DB:
-- one for the chain consumer + outbox worker (INSERT path), and one for
-- the HTTP read API (/entries, /verify). Splitting these into separate
-- roles means a bug or compromise in the read path can't DROP, UPDATE,
-- or DELETE rows on the chain — the role simply lacks the privilege.
--
-- The chain also has a BEFORE UPDATE/DELETE trigger that RAISE
-- EXCEPTION at the table layer; this role separation is the second
-- of the two defenses: even if the trigger is dropped, the role still
-- can't issue UPDATE/DELETE.
--
-- Grants for the actual tables (audit_entries, audit_outbox) live in
-- the audit service's migration 000003_grant_roles.up.sql so that they
-- are re-applied alongside any schema change. This file only creates
-- the roles — no grants — so it can run before migrations.
--
-- Passwords here are dev-only; in non-local environments these roles
-- get their passwords from Vault (see tools/scripts/seed-vault.sh and
-- services/audit/cmd/server/main.go).
-- =============================================================================

CREATE ROLE guva_audit_writer WITH LOGIN PASSWORD 'audit-writer-dev';
CREATE ROLE guva_audit_reader WITH LOGIN PASSWORD 'audit-reader-dev';

GRANT CONNECT ON DATABASE guva_audit TO guva_audit_writer;
GRANT CONNECT ON DATABASE guva_audit TO guva_audit_reader;
