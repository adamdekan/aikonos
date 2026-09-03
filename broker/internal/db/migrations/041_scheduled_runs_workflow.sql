-- 041_scheduled_runs_workflow.sql
-- Workflow-bound schedules: a scheduled_runs
-- row now carries either a free-text prompt or a workflow lineage ref + input
-- values, never both. prompt stays NOT NULL (workflow-mode rows store '');
-- workflow_lineage_id/workflow_inputs are nullable and unused by prompt rows.

ALTER TABLE scheduled_runs ADD COLUMN IF NOT EXISTS workflow_lineage_id UUID;
ALTER TABLE scheduled_runs ADD COLUMN IF NOT EXISTS workflow_inputs JSONB;

-- Enforces exactly one of {prompt, workflow_lineage_id}: boolean XOR via <>.
-- Every existing row is prompt-mode (prompt <> '', workflow_lineage_id NULL),
-- which already satisfies this with no backfill.
ALTER TABLE scheduled_runs DROP CONSTRAINT IF EXISTS scheduled_runs_prompt_xor_workflow;
ALTER TABLE scheduled_runs ADD CONSTRAINT scheduled_runs_prompt_xor_workflow
    CHECK ((prompt <> '') <> (workflow_lineage_id IS NOT NULL));

INSERT INTO schema_migrations (version, description)
VALUES ('041', 'scheduled_runs: workflow_lineage_id + workflow_inputs, prompt XOR workflow-ref CHECK')
ON CONFLICT (version) DO NOTHING;
