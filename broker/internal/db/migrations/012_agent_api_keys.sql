-- 012_agent_api_keys.sql
-- Per-agent API keys for external (service-principal) access.
-- key_hash stores HMAC-SHA256(pepper, sha256(raw_key)); the raw key is never
-- persisted. RLS mirrors agents (011): app.current_tenant must be set.

CREATE TABLE IF NOT EXISTS agent_api_keys (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    UUID NOT NULL,
    agent_id     UUID NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    key_hash     TEXT NOT NULL,            -- HMAC-SHA256(pepper, sha256(key))
    key_prefix   TEXT NOT NULL,            -- first 8 chars of tk_… for display
    label        TEXT NOT NULL DEFAULT '',
    created_by   TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_used_at TIMESTAMPTZ,
    revoked_at   TIMESTAMPTZ,
    UNIQUE (key_hash)
);

CREATE INDEX IF NOT EXISTS agent_api_keys_agent_id_idx ON agent_api_keys (agent_id);
CREATE INDEX IF NOT EXISTS agent_api_keys_key_hash_idx  ON agent_api_keys (key_hash);

ALTER TABLE agent_api_keys ENABLE ROW LEVEL SECURITY;

-- Tenant isolation: the broker sets app.current_tenant to the UUID string.
CREATE POLICY tenant_isolation ON agent_api_keys
    USING (tenant_id::text = current_setting('app.current_tenant', true));

INSERT INTO schema_migrations (version, description) VALUES ('012', 'agent_api_keys: per-agent API keys') ON CONFLICT (version) DO NOTHING;
