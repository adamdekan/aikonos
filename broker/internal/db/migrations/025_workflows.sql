-- 025_workflows.sql
-- Versioned workflow persistence. Every save creates a new immutable version row
-- (append-only, no UPDATE on definition) so history is always recoverable. The
-- lineage_id groups all versions of the same logical workflow; forks introduce a
-- new lineage_id with parent_lineage_id pointing at the fork source.
--
-- workflow_version_pins lets a user "downgrade" to an earlier version — the
-- ResolveVersionForUser repo helper returns the pinned version when set, otherwise
-- falls back to the highest (current) version. Pins never block new versions from
-- being created.

CREATE TABLE IF NOT EXISTS workflows (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    lineage_id        UUID NOT NULL,            -- groups all versions of one workflow
    version           INTEGER NOT NULL,         -- 1-based; all versions preserved
    tenant_id         UUID NOT NULL,
    owner_user_id     TEXT NOT NULL,
    name              TEXT NOT NULL,
    description       TEXT NOT NULL DEFAULT '',
    status            TEXT NOT NULL DEFAULT 'private'
                      CHECK (status IN ('private','published')),
    visibility_kind   TEXT NOT NULL DEFAULT 'private'
                      CHECK (visibility_kind IN ('private','shared')),
    visibility_groups JSONB NOT NULL DEFAULT '[]'::jsonb,
    definition        JSONB NOT NULL,           -- canonical Workflow JSON (CP1 ToJSON)
    parent_lineage_id UUID,                     -- fork source; NULL if not a fork
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (lineage_id, version)
);

CREATE INDEX IF NOT EXISTS idx_workflows_tenant_lineage   ON workflows (tenant_id, lineage_id);
CREATE INDEX IF NOT EXISTS idx_workflows_tenant_owner     ON workflows (tenant_id, owner_user_id);

ALTER TABLE workflows ENABLE ROW LEVEL SECURITY;
CREATE POLICY workflows_tenant_isolation ON workflows
    USING (tenant_id::text = current_setting('app.current_tenant', true));

CREATE TABLE IF NOT EXISTS workflow_version_pins (
    tenant_id  UUID NOT NULL,
    user_id    TEXT NOT NULL,
    lineage_id UUID NOT NULL,
    version    INTEGER NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, user_id, lineage_id)
);

ALTER TABLE workflow_version_pins ENABLE ROW LEVEL SECURITY;
CREATE POLICY workflow_version_pins_tenant_isolation ON workflow_version_pins
    USING (tenant_id::text = current_setting('app.current_tenant', true));

INSERT INTO schema_migrations (version, description)
VALUES ('025', 'workflows: versioned workflow storage + version pins')
ON CONFLICT (version) DO NOTHING;
