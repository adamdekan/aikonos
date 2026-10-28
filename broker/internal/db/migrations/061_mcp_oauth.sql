-- 061_mcp_oauth.sql
-- AG-09: per-user OAuth for MCP connections. auth_type 'oauth' means each user
-- links their own account on the MCP server; 'bearer' stays as the shared
-- service mode. Discovery (RFC 9728 + RFC 8414) runs when an admin saves the
-- connection, and its result is pinned here so a user's connect and a tool
-- call's refresh never re-discover. The client secret (when there is one)
-- lives in Vault; per-user tokens live in Vault under the user's workspace.

ALTER TABLE mcp_connections DROP CONSTRAINT IF EXISTS mcp_connections_auth_type_check;
ALTER TABLE mcp_connections ADD CONSTRAINT mcp_connections_auth_type_check
    CHECK (auth_type IN ('none','bearer','oauth'));

ALTER TABLE mcp_connections
    ADD COLUMN IF NOT EXISTS oauth_client_id TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS oauth_scopes    TEXT[] NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS oauth_issuer    TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS oauth_auth_url  TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS oauth_token_url TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS oauth_resource  TEXT NOT NULL DEFAULT '';

INSERT INTO schema_migrations (version, description)
VALUES ('061', 'mcp_connections oauth auth type (AG-09)')
ON CONFLICT (version) DO NOTHING;
