-- 046_llm_provider_catalog.sql
-- LLM provider catalog.
--
-- Two additions:
--
-- 1. llm_providers.config — family-specific connection fields that do not
--    deserve a column each (aws-bedrock's region today). The per-model `mode`
--    and `pricing` records ride the existing models JSONB, so they need no
--    schema change here.
--
-- 2. llm_provider_defaults — one row per (tenant, capability) naming the
--    provider that serves it. This replaces the three singleton boolean flags
--    (is_default / is_default_vision / is_fallback), each of which needed its
--    own column, partial unique index and RPC; a fourth capability would have
--    replicated the whole pattern again. The three columns are deliberately NOT
--    dropped: they are frozen (never written again) and their values are
--    backfilled below as chat/vision/fallback rows. List/Get recompute the
--    booleans from this table, so the gateway's candidate chains keep working
--    unchanged. A later cleanup migration removes the columns.
--
-- Idempotent throughout (IF NOT EXISTS / DROP-then-CREATE policy / ON CONFLICT):
-- CI applies the migration set twice to prove that.

ALTER TABLE llm_providers ADD COLUMN IF NOT EXISTS config JSONB NOT NULL DEFAULT '{}';

CREATE TABLE IF NOT EXISTS llm_provider_defaults (
    tenant_id   UUID        NOT NULL,
    capability  TEXT        NOT NULL, -- chat | vision | fallback | embedding | image_generation | audio_speech | audio_transcription | rerank | ocr
    provider_id TEXT        NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, capability),
    -- Deleting a provider must not leave a capability pointing at a ghost row;
    -- the composite FK also makes a cross-tenant default unrepresentable.
    FOREIGN KEY (tenant_id, provider_id) REFERENCES llm_providers(tenant_id, id) ON DELETE CASCADE
);

ALTER TABLE llm_provider_defaults ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS llm_provider_defaults_tenant_isolation ON llm_provider_defaults;
CREATE POLICY llm_provider_defaults_tenant_isolation ON llm_provider_defaults
    USING (tenant_id::text = current_setting('app.current_tenant', true));

-- Backfill the three frozen flags. ON CONFLICT DO NOTHING so a re-run (or a
-- deployment where an admin already set a default through the new RPC) never
-- overwrites the live value with the stale boolean.
INSERT INTO llm_provider_defaults (tenant_id, capability, provider_id)
SELECT tenant_id, 'chat', id FROM llm_providers WHERE is_default
ON CONFLICT DO NOTHING;

INSERT INTO llm_provider_defaults (tenant_id, capability, provider_id)
SELECT tenant_id, 'vision', id FROM llm_providers WHERE is_default_vision
ON CONFLICT DO NOTHING;

INSERT INTO llm_provider_defaults (tenant_id, capability, provider_id)
SELECT tenant_id, 'fallback', id FROM llm_providers WHERE is_fallback
ON CONFLICT DO NOTHING;

INSERT INTO schema_migrations (version, description)
VALUES ('046', 'llm_provider_catalog: llm_providers.config + llm_provider_defaults (default-per-capability) + boolean-flag backfill')
ON CONFLICT (version) DO NOTHING;
