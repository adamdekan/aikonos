ALTER TABLE agents ADD COLUMN IF NOT EXISTS allowed_providers JSONB NOT NULL DEFAULT '[]'::jsonb;
ALTER TABLE agents ADD COLUMN IF NOT EXISTS preferred_provider TEXT NOT NULL DEFAULT '';
INSERT INTO schema_migrations (version, description)
VALUES ('015', 'agents: allowed_providers + preferred_provider')
ON CONFLICT (version) DO NOTHING;
