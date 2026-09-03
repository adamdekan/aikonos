-- 007_mcp_connections.sql
-- Admin-managed remote MCP server registry. Each row is a tenant-scoped MCP
-- connection. The bearer token is stored in Vault only; this table holds the
-- Vault path reference (auth_secret_ref) so the broker can fetch it JIT.
-- auth_secret_ref is NULL until the broker layer writes the Vault secret and
-- back-fills it (set at Add time; never exposed via List/Read responses).

CREATE TABLE IF NOT EXISTS mcp_connections (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       UUID NOT NULL,
    name            TEXT NOT NULL,
    url             TEXT NOT NULL,
    transport       TEXT NOT NULL DEFAULT 'streamable_http' CHECK (transport IN ('streamable_http','sse')),
    auth_type       TEXT NOT NULL DEFAULT 'none' CHECK (auth_type IN ('none','bearer')),
    auth_secret_ref TEXT,
    created_by      TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_mcp_connections_tenant ON mcp_connections (tenant_id);

ALTER TABLE mcp_connections ENABLE ROW LEVEL SECURITY;
CREATE POLICY mcp_connections_tenant_isolation ON mcp_connections
    USING (tenant_id::text = current_setting('app.current_tenant', true));

INSERT INTO schema_migrations (version, description)
VALUES ('007', 'mcp_connections for remote MCP server registry')
ON CONFLICT (version) DO NOTHING;
