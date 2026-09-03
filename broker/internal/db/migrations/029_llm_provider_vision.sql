ALTER TABLE llm_providers ADD COLUMN IF NOT EXISTS vision_capable BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE llm_providers ADD COLUMN IF NOT EXISTS is_default_vision BOOLEAN NOT NULL DEFAULT false;
CREATE UNIQUE INDEX IF NOT EXISTS idx_llm_providers_one_default_vision
    ON llm_providers (tenant_id) WHERE is_default_vision;
INSERT INTO schema_migrations (version, description)
VALUES ('029', 'llm_providers: vision_capable + is_default_vision for dedicated vision routing')
ON CONFLICT (version) DO NOTHING;
