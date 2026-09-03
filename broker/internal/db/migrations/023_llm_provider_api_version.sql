ALTER TABLE llm_providers ADD COLUMN IF NOT EXISTS api_version TEXT NOT NULL DEFAULT '';
INSERT INTO schema_migrations (version, description)
VALUES ('023', 'llm_providers: api_version for Azure Foundry classic deployment route')
ON CONFLICT (version) DO NOTHING;
