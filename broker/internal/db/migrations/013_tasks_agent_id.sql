-- 013_tasks_agent_id.sql
-- Binds a task to a named agent so the broker can enforce the skill boundary
-- at SubmitPlan independent of gateway claims (CP2).
-- Nullable: human north tasks have no agent binding; only gateway tasks created
-- by the external-agent surface (F3) or the scheduler carry agent_id.

ALTER TABLE tasks ADD COLUMN IF NOT EXISTS agent_id UUID REFERENCES agents(id) ON DELETE SET NULL;

INSERT INTO schema_migrations (version, description)
    VALUES ('013', 'tasks: agent_id binding for skill-boundary enforcement')
    ON CONFLICT (version) DO NOTHING;
