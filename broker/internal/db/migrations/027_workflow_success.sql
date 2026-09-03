-- 027_workflow_success.sql
-- Adds the success-rating marker to workflows. Set by RateWorkflowRun(SUCCESS).
-- Rating is INFORMATIONAL ONLY: the original publish gate on success_rated_at
-- was removed 2026-07-21 (see  change log) — Publish no
-- longer requires a success rating, and no code reads this column as a gate.
-- The column is retained as an audit/analytics marker of a run rated SUCCESS.
--
-- Idempotent: IF NOT EXISTS guards re-runs.

ALTER TABLE workflows ADD COLUMN IF NOT EXISTS success_rated_at TIMESTAMPTZ;

INSERT INTO schema_migrations (version, description)
VALUES ('027', 'workflows: success_rated_at marker for publish gate')
ON CONFLICT (version) DO NOTHING;
