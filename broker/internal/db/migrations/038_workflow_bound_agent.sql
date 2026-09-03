ALTER TABLE workflows ADD COLUMN IF NOT EXISTS bound_agent_id UUID;
INSERT INTO schema_migrations (version, description)
VALUES ('038', 'workflows: bound_agent_id — agent-bound workflows (F9)')
ON CONFLICT (version) DO NOTHING;
