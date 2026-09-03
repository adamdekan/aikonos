-- 039_workflow_bound_agent_index.sql
-- Index for workflows.bound_agent_id (2026-07 review remediation CP5.4):
-- migration 038 added the column but no index, so any agent-bound workflow
-- lookup (e.g. "workflows bound to agent X") would force a sequential scan.
-- Partial: the column is nullable and every lookup filters on a concrete
-- agent id (never IS NULL), so rows with no bound agent (the common case —
-- personal, unbound workflows) don't bloat the index.
CREATE INDEX IF NOT EXISTS idx_workflows_bound_agent_id ON workflows (bound_agent_id)
    WHERE bound_agent_id IS NOT NULL;

INSERT INTO schema_migrations (version, description)
VALUES ('039', 'workflows: index on bound_agent_id (partial, non-null)')
ON CONFLICT (version) DO NOTHING;
