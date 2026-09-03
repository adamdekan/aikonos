-- 020_agent_skill.sql
-- Tenant-scoped agent-skill bundle storage. Each row is an admin-uploaded
-- SKILL.md bundle: a system prompt (body) + narrowing tool allowlist
-- (allowed_tools). The bundle references no new executor and grants no new
-- invocation capability — it is context + constraints layered onto a session.
--
-- allowed_tools: JSONB array of validated tool_ids; [] means no restriction
--   beyond the user's existing grants.
-- extras: JSONB map of non-executable companion files
--   {references: {path: content}, assets: {path: content}}; size-capped.
-- Size guard: octet_length(body) + octet_length(extras::text) must not exceed
--   256 KiB (262144 bytes). This mirrors the gateway-layer cap to guard against
--   direct DB writes that bypass the gateway.

CREATE TABLE IF NOT EXISTS agent_skill (
    tenant_id                  UUID        NOT NULL,
    id                         UUID        NOT NULL DEFAULT gen_random_uuid(),
    name                       TEXT        NOT NULL,
    description                TEXT        NOT NULL DEFAULT '',
    body                       TEXT        NOT NULL DEFAULT '',
    allowed_tools              JSONB       NOT NULL DEFAULT '[]'::jsonb,
    context_fork               BOOLEAN     NOT NULL DEFAULT FALSE,
    disable_model_invocation   BOOLEAN     NOT NULL DEFAULT FALSE,
    extras                     JSONB       NOT NULL DEFAULT '{}'::jsonb,
    created_by                 TEXT        NOT NULL DEFAULT '',
    created_at                 TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at                 TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, name),
    -- DB-layer 256 KiB bundle size guard (Risks row 3: guard against direct
    -- DB writes bypassing the gateway). octet_length counts bytes, not chars.
    CONSTRAINT agent_skill_size_cap CHECK (
        octet_length(body) + octet_length(extras::text) <= 262144
    )
);

ALTER TABLE agent_skill ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS agent_skill_tenant_isolation ON agent_skill;
CREATE POLICY agent_skill_tenant_isolation ON agent_skill
    USING (tenant_id::text = current_setting('app.current_tenant', true));

INSERT INTO schema_migrations (version, description)
VALUES ('020', 'agent_skill: tenant-scoped SKILL.md bundle storage')
ON CONFLICT (version) DO NOTHING;
