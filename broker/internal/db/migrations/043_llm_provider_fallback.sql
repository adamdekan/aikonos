ALTER TABLE llm_providers ADD COLUMN IF NOT EXISTS is_fallback BOOLEAN NOT NULL DEFAULT false;
CREATE UNIQUE INDEX IF NOT EXISTS idx_llm_providers_one_fallback
    ON llm_providers (tenant_id) WHERE is_fallback;
INSERT INTO schema_migrations (version, description)
VALUES ('043', 'llm_providers: is_fallback for tenant fallback provider selection')
ON CONFLICT (version) DO NOTHING;
