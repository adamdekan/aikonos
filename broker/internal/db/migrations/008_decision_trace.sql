-- Migration 008: per-step policy reason + structured decision trace (CP1).
-- Backward-compatible: existing rows keep policy_reason='' and decision_trace=NULL.
-- Idempotent: IF NOT EXISTS so `task db:migrate` can re-apply safely.
ALTER TABLE plan_steps ADD COLUMN IF NOT EXISTS policy_reason  TEXT NOT NULL DEFAULT '';
ALTER TABLE plan_steps ADD COLUMN IF NOT EXISTS decision_trace JSONB;

INSERT INTO schema_migrations (version, description)
VALUES ('008', 'plan_steps: policy_reason + decision_trace')
ON CONFLICT (version) DO NOTHING;
