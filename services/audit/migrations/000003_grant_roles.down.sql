-- Revoke the audit role grants. The roles themselves remain (created
-- at DB-bootstrap by 02-roles.sql); only the privileges on this
-- service's tables are stripped.

REVOKE SELECT, INSERT          ON audit_entries FROM guva_audit_writer;
REVOKE SELECT                  ON audit_entries FROM guva_audit_reader;
REVOKE SELECT, INSERT, UPDATE  ON audit_outbox  FROM guva_audit_writer;

REVOKE USAGE, SELECT ON SEQUENCE audit_entries_entry_id_seq FROM guva_audit_writer;
REVOKE USAGE, SELECT ON SEQUENCE audit_outbox_id_seq         FROM guva_audit_writer;

REVOKE USAGE ON SCHEMA public FROM guva_audit_writer;
REVOKE USAGE ON SCHEMA public FROM guva_audit_reader;
