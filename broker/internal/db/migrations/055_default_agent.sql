-- 055_default_agent.sql
-- AG-01: every task binds a registered agent.
--
-- agents.is_default marks the tenant default agent: the agent a chat or a
-- schedule runs as when it names none. At most one per tenant. The broker
-- creates it on first use and at startup (EnsureDefault), since the tenants
-- are not known here. It carries the user's own grants (skills, memory), so
-- binding it adds attribution, not authority.
--
-- user_default_agents is the per-user default-agent setting that replaces the
-- gateway's former per-user env override. No row = the tenant default agent. The
-- broker checks can_use when it is set and again on every use.
--
-- Idempotent: CI applies the migration set twice.

ALTER TABLE agents ADD COLUMN IF NOT EXISTS is_default BOOLEAN NOT NULL DEFAULT FALSE;

CREATE UNIQUE INDEX IF NOT EXISTS agents_one_default ON agents (tenant_id) WHERE is_default;

CREATE TABLE IF NOT EXISTS user_default_agents (
    tenant_id  UUID NOT NULL,
    user_id    TEXT NOT NULL,
    agent_id   UUID NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, user_id)
);

ALTER TABLE user_default_agents ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS user_default_agents_tenant_isolation ON user_default_agents;
CREATE POLICY user_default_agents_tenant_isolation ON user_default_agents
    USING (tenant_id::text = current_setting('app.current_tenant', true));

INSERT INTO schema_migrations (version, description)
VALUES ('055', 'agents.is_default tenant default agent and user_default_agents setting (AG-01)')
ON CONFLICT (version) DO NOTHING;
