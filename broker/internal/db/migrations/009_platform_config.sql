-- 009_platform_config.sql
-- Tenant-scoped runtime config store. Values are text; the application schema
-- (internal/config/schema.go) knows each key's type and validates on write.
-- RLS mirrors 005_network_rules.sql: rows visible/writable only for the
-- session tenant set via withTenant (app.current_tenant).

CREATE TABLE IF NOT EXISTS platform_config (
    tenant_id  UUID        NOT NULL,
    key        TEXT        NOT NULL,
    value      TEXT        NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_by TEXT        NOT NULL DEFAULT '',
    PRIMARY KEY (tenant_id, key)
);

ALTER TABLE platform_config ENABLE ROW LEVEL SECURITY;
CREATE POLICY platform_config_tenant_isolation ON platform_config
    USING (tenant_id::text = current_setting('app.current_tenant', true));

INSERT INTO schema_migrations (version, description)
VALUES ('009', 'platform_config for runtime tenant config')
ON CONFLICT (version) DO NOTHING;
