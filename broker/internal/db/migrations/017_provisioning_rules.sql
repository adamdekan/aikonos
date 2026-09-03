-- 017_provisioning_rules.sql
-- Email-based JIT user provisioning: maps email patterns to FGA groups so new
-- users auto-receive skill grants at first login, without the admin needing to
-- know their opaque oid ahead of time.
--
-- matcher: lowercase exact address ('alice@contoso.com') or domain wildcard
-- ('*@contoso.com'). UNIQUE per tenant — one rule per pattern.
-- groups: JSONB array of group names (no 'group:' prefix); applied as
--   user:<subject> member group:<g> in OpenFGA at claim time.
-- provisioned_at on user_directory: set atomically on first claim; re-running
-- ClaimProvisioning WHERE provisioned_at IS NULL is the idempotent race-safe gate.

CREATE TABLE IF NOT EXISTS provisioning_rules (
    tenant_id  UUID NOT NULL,
    id         UUID NOT NULL,
    matcher    TEXT NOT NULL,
    groups     JSONB NOT NULL,
    created_by TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, matcher)
);

ALTER TABLE provisioning_rules ENABLE ROW LEVEL SECURITY;
CREATE POLICY provisioning_rules_tenant_isolation ON provisioning_rules
    USING (tenant_id::text = current_setting('app.current_tenant', true));

ALTER TABLE user_directory ADD COLUMN IF NOT EXISTS provisioned_at TIMESTAMPTZ;

INSERT INTO schema_migrations (version, description)
VALUES ('017', 'provisioning_rules: email→group JIT provisioning + user_directory.provisioned_at')
ON CONFLICT (version) DO NOTHING;
