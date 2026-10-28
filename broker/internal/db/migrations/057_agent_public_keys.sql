-- 057_agent_public_keys.sql
-- AG-05: key-pair authentication for external agents. An agent registers one
-- or more public keys and authenticates on the external surface with a
-- short-lived signed JWT client assertion (RFC 7523) instead of a tk_ bearer
-- key. The private key never reaches aikonOS.
--
-- public_key is the SPKI PEM, normalised at registration whatever the admin
-- pasted (PEM or JWK). alg is fixed per key (ES256 | EdDSA): an assertion
-- signed with another alg is refused. Rotation registers the new kid, then
-- retires the old one; a retired kid stays in the table (and blocks reuse of
-- its kid) so audit events keep pointing at a row. expires_at is mandatory and
-- bounded by api_keys.max_ttl_days, as for tk_ keys (AG-15).
--
-- The broker resolves an assertion with the tenant the gateway is configured
-- for, so lookups are tenant-scoped and need no RLS bypass.
--
-- Idempotent: CI applies the migration set twice.

CREATE TABLE IF NOT EXISTS agent_public_keys (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    UUID NOT NULL,
    agent_id     UUID NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    kid          TEXT NOT NULL CHECK (kid <> '' AND length(kid) <= 128),
    alg          TEXT NOT NULL CHECK (alg IN ('ES256', 'EdDSA')),
    public_key   TEXT NOT NULL,            -- SPKI PEM
    status       TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'retired')),
    created_by   TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at   TIMESTAMPTZ NOT NULL,
    last_used_at TIMESTAMPTZ,
    retired_at   TIMESTAMPTZ,
    UNIQUE (agent_id, kid)
);

ALTER TABLE agent_public_keys ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON agent_public_keys;
CREATE POLICY tenant_isolation ON agent_public_keys
    USING (tenant_id::text = current_setting('app.current_tenant', true));

INSERT INTO schema_migrations (version, description)
VALUES ('057', 'agent_public_keys: key-pair auth for external agents (AG-05)')
ON CONFLICT (version) DO NOTHING;
