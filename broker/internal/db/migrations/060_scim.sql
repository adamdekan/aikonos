-- 060_scim.sql
-- AG-16: SCIM 2.0 provisioning (RFC 7643/7644). The identity directory drives
-- deprovisioning and group membership: Users.active=false sets the AG-06 user
-- block, group membership is written as FGA `member` tuples on group:<fga_group>.
--
-- scim_tokens: per-tenant bearer tokens. Only HMAC-SHA256(pepper,
-- sha256(raw)) is stored (same scheme as agent API keys); the raw token is
-- shown once at creation. The broker resolves a token with the tenant the
-- gateway is configured for, so lookups are tenant-scoped and need no RLS bypass.
--
-- scim_users.external_id is the user's OIDC subject (Entra: objectId), the
-- principal every block and tuple is keyed by. userName is unique per tenant,
-- case-insensitively (RFC 7643 §4.1.1: caseExact false).
--
-- scim_groups.fga_group is derived from displayName at creation and never
-- changes, so a rename in the IdP does not orphan tuples. members holds
-- scim_users ids.
--
-- Idempotent: CI applies the migration set twice.

CREATE TABLE IF NOT EXISTS scim_tokens (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    UUID NOT NULL,
    token_hash   TEXT NOT NULL UNIQUE,
    token_prefix TEXT NOT NULL,
    created_by   TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_used_at TIMESTAMPTZ,
    revoked_at   TIMESTAMPTZ,
    revoked_by   TEXT
);

CREATE TABLE IF NOT EXISTS scim_users (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    UUID NOT NULL,
    user_name    TEXT NOT NULL CHECK (user_name <> '' AND length(user_name) <= 256),
    external_id  TEXT NOT NULL CHECK (external_id <> '' AND length(external_id) <= 256),
    display_name TEXT NOT NULL DEFAULT '',
    emails       JSONB NOT NULL DEFAULT '[]',
    active       BOOLEAN NOT NULL DEFAULT TRUE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (tenant_id, external_id)
);
CREATE UNIQUE INDEX IF NOT EXISTS scim_users_user_name_ci
    ON scim_users (tenant_id, lower(user_name));

CREATE TABLE IF NOT EXISTS scim_groups (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    UUID NOT NULL,
    display_name TEXT NOT NULL CHECK (display_name <> '' AND length(display_name) <= 256),
    external_id  TEXT NOT NULL DEFAULT '',
    fga_group    TEXT NOT NULL,
    members      UUID[] NOT NULL DEFAULT '{}',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (tenant_id, fga_group)
);

ALTER TABLE scim_tokens ENABLE ROW LEVEL SECURITY;
ALTER TABLE scim_users  ENABLE ROW LEVEL SECURITY;
ALTER TABLE scim_groups ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON scim_tokens;
CREATE POLICY tenant_isolation ON scim_tokens
    USING (tenant_id::text = current_setting('app.current_tenant', true));
DROP POLICY IF EXISTS tenant_isolation ON scim_users;
CREATE POLICY tenant_isolation ON scim_users
    USING (tenant_id::text = current_setting('app.current_tenant', true));
DROP POLICY IF EXISTS tenant_isolation ON scim_groups;
CREATE POLICY tenant_isolation ON scim_groups
    USING (tenant_id::text = current_setting('app.current_tenant', true));

INSERT INTO schema_migrations (version, description)
VALUES ('060', 'scim_tokens, scim_users, scim_groups: SCIM 2.0 provisioning (AG-16)')
ON CONFLICT (version) DO NOTHING;
