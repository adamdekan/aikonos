-- 051_tasks_obo_kind.sql
-- AG-17: how the task's delegation arose, stamped by the producer
-- (interactive | workflow | ...). Interactive chat and gateway workflow tasks
-- look alike on the other columns (gateway_managed, no grant/client id), so
-- the kind is recorded rather than derived. NULL = producer did not say;
-- Task.OnBehalfOf then derives scheduled/external from grant_id /
-- external_client_id as before. Audit-only.
--
-- Idempotent (ADD COLUMN IF NOT EXISTS): CI applies the migration set twice.

ALTER TABLE tasks ADD COLUMN IF NOT EXISTS obo_kind TEXT;

INSERT INTO schema_migrations (version, description)
VALUES ('051', 'tasks: obo_kind')
ON CONFLICT (version) DO NOTHING;
