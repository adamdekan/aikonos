-- 016_user_directory.sql
-- Passive user directory: records the (subject, email, display name) of every
-- authenticated north caller. Upserted on each verified RPC — the broker never
-- relies on this for authz; it is a display-label seam only.

CREATE TABLE IF NOT EXISTS user_directory (
    tenant_id  UUID    NOT NULL,
    subject    TEXT    NOT NULL,
    email      TEXT    NOT NULL DEFAULT '',
    name       TEXT    NOT NULL DEFAULT '',
    last_seen  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, subject)
);

ALTER TABLE user_directory ENABLE ROW LEVEL SECURITY;
CREATE POLICY user_directory_tenant_isolation ON user_directory
    USING (tenant_id::text = current_setting('app.current_tenant', true));

INSERT INTO schema_migrations (version, description)
VALUES ('016', 'user_directory: passive display-name cache for OIDC subjects')
ON CONFLICT (version) DO NOTHING;
