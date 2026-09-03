CREATE TABLE IF NOT EXISTS llm_providers (
    tenant_id   UUID NOT NULL,
    id          TEXT NOT NULL,
    name        TEXT NOT NULL,
    endpoint    TEXT NOT NULL,
    api         TEXT NOT NULL DEFAULT 'openai-completions',
    models      JSONB NOT NULL DEFAULT '[]'::jsonb,
    enabled     BOOLEAN NOT NULL DEFAULT true,
    is_default  BOOLEAN NOT NULL DEFAULT false,
    has_key     BOOLEAN NOT NULL DEFAULT false,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_by  TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (tenant_id, id)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_llm_providers_one_default
    ON llm_providers (tenant_id) WHERE is_default;
ALTER TABLE llm_providers ENABLE ROW LEVEL SECURITY;
CREATE POLICY llm_providers_tenant_isolation ON llm_providers
    USING (tenant_id::text = current_setting('app.current_tenant', true));
INSERT INTO schema_migrations (version, description)
VALUES ('014', 'llm_providers: per-tenant LLM provider configs')
ON CONFLICT (version) DO NOTHING;
