ALTER TABLE agents ADD COLUMN IF NOT EXISTS soul TEXT NOT NULL DEFAULT '';
INSERT INTO schema_migrations (version, description)
VALUES ('018', 'agents: per-agent SOUL / personality markdown')
ON CONFLICT (version) DO NOTHING;
