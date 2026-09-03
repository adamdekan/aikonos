-- 005_network_rules.sql
-- Network/web access-list for agents. Each row is a host rule scoped to the
-- tenant, a group, or a single user. The Tool Proxy web.fetch handler evaluates
-- these against the destination host (the unbypassable enforcement point), and
-- SubmitPlan evaluates them at plan time for a governed deny/ask + a clear reason.
--
-- action: ALLOW | DENY | ASK (ASK routes an unlisted host to human approval).
-- host_pattern: exact ("api.example.com"), suffix wildcard ("*.example.com"),
--   or catch-all ("*"). The catch-all row is the per-tenant default — shipped
--   ALLOW (no disruption); admins switch it to DENY for a true allow-list.
-- Most-specific scope wins (USER > GROUP > TENANT), then most-specific host;
-- DENY beats ASK beats ALLOW on a tie.

CREATE TABLE IF NOT EXISTS network_rules (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     UUID NOT NULL,
    scope_kind    TEXT NOT NULL CHECK (scope_kind IN ('TENANT','GROUP','USER')),
    scope_value   TEXT NOT NULL DEFAULT '',  -- group name / user email / '' for tenant
    action        TEXT NOT NULL CHECK (action IN ('ALLOW','DENY','ASK')),
    host_pattern  TEXT NOT NULL,
    note          TEXT NOT NULL DEFAULT '',
    created_by    TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_network_rules_tenant_scope ON network_rules (tenant_id, scope_kind);

ALTER TABLE network_rules ENABLE ROW LEVEL SECURITY;
CREATE POLICY network_rules_tenant_isolation ON network_rules
    USING (tenant_id::text = current_setting('app.current_tenant', true));

INSERT INTO schema_migrations (version, description)
VALUES ('005', 'network_rules for the agent web access-list')
ON CONFLICT (version) DO NOTHING;
