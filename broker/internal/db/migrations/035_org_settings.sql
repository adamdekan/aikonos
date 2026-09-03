-- 035_org_settings.sql
-- Org governance control plane (A-series). One tenant-scoped settings row per
-- tenant, stored as a single JSONB document so new governance fields can be
-- added without a migration per feature — the typed proto OrgSettings message
-- is the schema view over this blob. Partial updates merge with `settings || $patch`.
--
-- Secrets never live here (e.g. A9 OTLP auth headers go to Vault); this table
-- holds non-sensitive configuration only.

CREATE TABLE IF NOT EXISTS org_settings (
    tenant_id  TEXT PRIMARY KEY,
    settings   JSONB NOT NULL DEFAULT '{}',
    updated_by TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE org_settings ENABLE ROW LEVEL SECURITY;
CREATE POLICY org_settings_tenant_isolation ON org_settings
    USING (tenant_id = current_setting('app.current_tenant', true));

INSERT INTO schema_migrations (version, description)
VALUES ('035', 'org_settings: tenant-scoped org governance control plane (A-series)')
ON CONFLICT (version) DO NOTHING;
