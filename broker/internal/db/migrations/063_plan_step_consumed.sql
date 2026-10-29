-- 063_plan_step_consumed.sql
-- H-03: InvokeTool binds each call to an approved plan step (tool, args hash)
-- and runs it once. consumed_at is stamped atomically when a call claims the
-- step; a replan of the same seq clears it (InsertPlanSteps).

ALTER TABLE plan_steps ADD COLUMN IF NOT EXISTS consumed_at TIMESTAMPTZ;

INSERT INTO schema_migrations (version, description)
VALUES ('063', 'plan_steps.consumed_at: single-use step binding for InvokeTool (H-03)')
ON CONFLICT (version) DO NOTHING;
