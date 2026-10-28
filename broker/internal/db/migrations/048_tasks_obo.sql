-- 048_tasks_obo.sql
-- AG-17: explicit on-behalf-of chain on the task row. All nullable; NULL means
-- "not this kind of run" (no delegating sender, no external client, no
-- standing grant). Read by Task.OnBehalfOf; producers fill them in later
-- slices. Audit-only: no access decision reads these yet.
--
-- Idempotent (ADD COLUMN IF NOT EXISTS): CI applies the migration set twice
-- to prove that.

ALTER TABLE tasks
    ADD COLUMN IF NOT EXISTS delegated_by_user_id TEXT,
    ADD COLUMN IF NOT EXISTS external_client_id   UUID,
    ADD COLUMN IF NOT EXISTS grant_id             UUID;

INSERT INTO schema_migrations (version, description)
VALUES ('048', 'tasks: on-behalf-of chain columns (delegated_by_user_id, external_client_id, grant_id)')
ON CONFLICT (version) DO NOTHING;
