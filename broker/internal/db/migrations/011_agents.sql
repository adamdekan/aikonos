-- 011_agents.sql
-- Named per-tenant agent configs. Each row is an agent the admin has configured
-- with a model, approval mode, and tool/MCP selection. The row is config only —
-- it does not grant any capability; FGA usable_by tuples control who may use it.

CREATE TABLE IF NOT EXISTS agents (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     UUID NOT NULL,
    name          TEXT NOT NULL,
    llm_model     TEXT NOT NULL DEFAULT '',
    approval_mode TEXT NOT NULL DEFAULT 'needs_approval' CHECK (approval_mode IN ('auto','needs_approval')),
    skills        JSONB NOT NULL DEFAULT '[]'::jsonb,
    mcp_servers   JSONB NOT NULL DEFAULT '[]'::jsonb,
    created_by    TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_agents_tenant ON agents (tenant_id);

ALTER TABLE agents ENABLE ROW LEVEL SECURITY;
CREATE POLICY agents_tenant_isolation ON agents
    USING (tenant_id::text = current_setting('app.current_tenant', true));

INSERT INTO schema_migrations (version, description)
VALUES ('011', 'agents: named per-tenant agent configs')
ON CONFLICT (version) DO NOTHING;
