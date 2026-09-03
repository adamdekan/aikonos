-- 040_drop_workflow_bound_agent_index.sql
-- Drops idx_workflows_bound_agent_id, added by migration 039. Review found the
-- index has zero consumers: no query anywhere filters workflows by
-- bound_agent_id (agent binding is read per-row off an already-fetched
-- WorkflowRow, never queried by it). An unused index only costs write
-- amplification on every workflow insert/update, so remove it.
-- Idempotent: IF EXISTS guards re-runs (and a DB that never applied 039).
DROP INDEX IF EXISTS idx_workflows_bound_agent_id;

INSERT INTO schema_migrations (version, description)
VALUES ('040', 'workflows: drop unused idx_workflows_bound_agent_id (added by 039, zero consumers)')
ON CONFLICT (version) DO NOTHING;
