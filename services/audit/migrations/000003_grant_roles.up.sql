-- Grant the least-privilege roles created in
-- deploy/compose/postgres/initdb.d/02-roles.sql their working set on
-- the audit tables. Re-applying is idempotent (GRANT is idempotent
-- by default in Postgres — re-issuing the same grant is a no-op).
--
-- Writer:
--   audit_entries  — SELECT (needed for FOR UPDATE chain lookup) + INSERT
--   audit_outbox   — SELECT + INSERT + UPDATE (set sent_at)
--   sequences      — USAGE + SELECT (so BIGSERIAL DEFAULT works)
--
-- Reader:
--   audit_entries  — SELECT only
--   audit_outbox   — no access (read API never touches it)

GRANT USAGE ON SCHEMA public TO guva_audit_writer;
GRANT USAGE ON SCHEMA public TO guva_audit_reader;

GRANT SELECT, INSERT ON audit_entries TO guva_audit_writer;
GRANT SELECT          ON audit_entries TO guva_audit_reader;

GRANT SELECT, INSERT, UPDATE ON audit_outbox TO guva_audit_writer;

GRANT USAGE, SELECT ON SEQUENCE audit_entries_entry_id_seq TO guva_audit_writer;
GRANT USAGE, SELECT ON SEQUENCE audit_outbox_id_seq         TO guva_audit_writer;
