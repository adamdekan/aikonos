-- 037_user_workspace_prefs.sql
-- Per-user workspace-backend preference:
-- which storage backend (local disk vs the tenant's OneDrive OBO connection)
-- a user's Files explorer / composer uploads / agent file tools currently
-- route to, plus the resolved OneDrive working-folder path and Graph item
-- ids (auto-created on first use — see broker/internal/broker/workspace_backend.go).
-- A missing row means "never set" — the PrefResolver applies the default rule
-- (explicit row wins; no row + M365 configured -> onedrive/Apps/Aikonos; else
-- local), not a schema default read directly off this table.

CREATE TABLE IF NOT EXISTS user_workspace_prefs (
    tenant_id             TEXT NOT NULL,
    user_id               TEXT NOT NULL,
    backend               TEXT NOT NULL DEFAULT 'local' CHECK (backend IN ('local', 'onedrive')),
    onedrive_folder_path  TEXT NOT NULL DEFAULT 'Apps/Aikonos',
    drive_id              TEXT NOT NULL DEFAULT '',
    root_item_id          TEXT NOT NULL DEFAULT '',
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, user_id)
);

ALTER TABLE user_workspace_prefs ENABLE ROW LEVEL SECURITY;
CREATE POLICY user_workspace_prefs_tenant_isolation ON user_workspace_prefs
    USING (tenant_id = current_setting('app.current_tenant', true));

INSERT INTO schema_migrations (version, description)
VALUES ('037', 'user_workspace_prefs: per-user workspace-backend preference (tenant-onedrive-obo CP5)')
ON CONFLICT (version) DO NOTHING;
