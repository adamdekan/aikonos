-- 019_skill_overlay.sql
-- Tenant-scoped skill overlay: lets tenant admins curate, annotate, and
-- enable/disable the executable tool surface without inventing executors.
-- Overlay rows are only written for tool_ids that already have an executor
-- (enforcement is at the service layer, not enforced by this schema).
--
-- executor_kind: 'builtin' | 'mcp' (derived at write time, not user-set).
-- capability_scope: derived (I3) and stored for display only.
-- effect_class: the stored authoritative class (I4); may never be weaker
--   than the built-in's compiled class for built-in tools.
-- enabled: defaults true; false makes the tool behave exactly like a
--   disabled_tools entry (deny-by-default, fail-closed).

CREATE TABLE IF NOT EXISTS skill_overlay (
    tenant_id        UUID        NOT NULL,
    tool_id          TEXT        NOT NULL,
    executor_kind    TEXT        NOT NULL,
    capability_scope TEXT        NOT NULL DEFAULT '',
    effect_class     TEXT        NOT NULL DEFAULT '',
    display_name     TEXT        NOT NULL DEFAULT '',
    description      TEXT        NOT NULL DEFAULT '',
    enabled          BOOLEAN     NOT NULL DEFAULT TRUE,
    created_by       TEXT        NOT NULL DEFAULT '',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, tool_id)
);

ALTER TABLE skill_overlay ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS skill_overlay_tenant_isolation ON skill_overlay;
CREATE POLICY skill_overlay_tenant_isolation ON skill_overlay
    USING (tenant_id::text = current_setting('app.current_tenant', true));

INSERT INTO schema_migrations (version, description)
VALUES ('019', 'skill_overlay: tenant-scoped skill curation overlay')
ON CONFLICT (version) DO NOTHING;
