-- 059_task_envelope_limits.sql
-- AG-11: a task spawned from (or started on) a delegation envelope carries the
-- envelope id and the sender's attenuated scopes, so the sender's narrowing is
-- enforced at SubmitPlan instead of being advisory. delegated_scopes NULL =
-- not a delegation (no limit); an empty array = a delegation that granted no
-- scope (every scoped tool is refused).
--
-- Idempotent (ADD COLUMN IF NOT EXISTS): CI applies the migration set twice.

ALTER TABLE tasks ADD COLUMN IF NOT EXISTS envelope_id UUID;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS delegated_scopes TEXT[];

INSERT INTO schema_migrations (version, description)
VALUES ('059', 'tasks: envelope_id, delegated_scopes (AG-11)')
ON CONFLICT (version) DO NOTHING;
