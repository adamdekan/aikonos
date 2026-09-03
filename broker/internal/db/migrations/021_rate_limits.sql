CREATE TABLE IF NOT EXISTS rate_limit_policies (
    id          UUID        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id   UUID        NOT NULL,
    agent_id    UUID        NULL,
    provider    TEXT        NULL,
    rpm_limit   INT         NULL CHECK (rpm_limit IS NULL OR rpm_limit >= 0),
    tpm_limit   INT         NULL CHECK (tpm_limit IS NULL OR tpm_limit >= 0),
    created_by  TEXT        NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (id)
);

CREATE UNIQUE INDEX IF NOT EXISTS rate_limit_policies_scope
    ON rate_limit_policies(tenant_id, COALESCE(agent_id::text,''), COALESCE(provider,''));

ALTER TABLE rate_limit_policies ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS rate_limit_policies_tenant_isolation ON rate_limit_policies;
CREATE POLICY rate_limit_policies_tenant_isolation ON rate_limit_policies
    USING (tenant_id::text = current_setting('app.current_tenant', true));

INSERT INTO schema_migrations (version, description)
VALUES ('021', 'rate_limit_policies: per-agent/provider/tenant rate limiting')
ON CONFLICT (version) DO NOTHING;
