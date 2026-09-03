-- 006_connector_oauth_states.sql
-- Server-only state for an in-flight connector OAuth round-trip (auth-code +
-- PKCE). The opaque `state` is the lookup key; `verifier` is the PKCE secret
-- that must never reach the browser. Promoted from the broker's in-memory map
-- so BeginConnectorAuth and CompleteConnectorAuth can land on different broker
-- replicas (multi-replica safe). Single-use: CompleteConnectorAuth deletes the
-- row atomically (DELETE ... RETURNING). Rows are short-lived (TTL ~10 min) and
-- swept on write.

CREATE TABLE IF NOT EXISTS connector_oauth_states (
    state       TEXT PRIMARY KEY,
    tenant_id   UUID NOT NULL,
    user_id     TEXT NOT NULL,
    provider    TEXT NOT NULL,
    verifier    TEXT NOT NULL,
    scopes      TEXT[] NOT NULL DEFAULT '{}',
    expires_at  TIMESTAMPTZ NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_connector_oauth_states_expires ON connector_oauth_states (expires_at);

ALTER TABLE connector_oauth_states ENABLE ROW LEVEL SECURITY;
CREATE POLICY connector_oauth_states_tenant_isolation ON connector_oauth_states
    USING (tenant_id::text = current_setting('app.current_tenant', true));

INSERT INTO schema_migrations (version, description)
VALUES ('006', 'connector_oauth_states for multi-replica connector OAuth')
ON CONFLICT (version) DO NOTHING;
