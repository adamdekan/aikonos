-- 036_agent_skill_keywords.sql
-- Admin-editable keyword list per agent_skill bundle, used by the gateway's
-- parent-side auto-load matcher to
-- trigger activation before the model sees the prompt. Empty list (default)
-- means the bundle never auto-loads — matching is opt-in per bundle.
ALTER TABLE agent_skill ADD COLUMN IF NOT EXISTS keywords JSONB NOT NULL DEFAULT '[]'::jsonb;

INSERT INTO schema_migrations (version, description)
VALUES ('036', 'agent_skill: admin-editable keywords for auto-load matching')
ON CONFLICT (version) DO NOTHING;
