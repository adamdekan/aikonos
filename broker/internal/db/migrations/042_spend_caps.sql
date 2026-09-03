-- 042_spend_caps.sql
-- Monthly LLM spend caps + accumulation.
-- spend_caps: admin-set budgets scoped org/user/agent, one row per
-- (tenant, scope, subject_id) — subject_id is '' for scope='org'.
-- llm_spend_counters: durable per-period accumulator, keyed
-- (tenant, user, agent, period_start); period_start is the UTC calendar-month
-- start, computed in Go so the DB stays timezone-agnostic.
-- llm_providers gains flat per-provider token pricing for cost fallback.

CREATE TABLE IF NOT EXISTS spend_caps (
    id           UUID        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id    UUID        NOT NULL,
    scope        TEXT        NOT NULL CHECK (scope IN ('org', 'user', 'agent')),
    subject_id   TEXT        NOT NULL DEFAULT '', -- '' for scope='org'
    cap_micros   BIGINT      NOT NULL CHECK (cap_micros > 0),
    created_by   TEXT        NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (id)
);

CREATE UNIQUE INDEX IF NOT EXISTS spend_caps_scope
    ON spend_caps(tenant_id, scope, subject_id);

ALTER TABLE spend_caps ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS spend_caps_tenant_isolation ON spend_caps;
CREATE POLICY spend_caps_tenant_isolation ON spend_caps
    USING (tenant_id::text = current_setting('app.current_tenant', true));

CREATE TABLE IF NOT EXISTS llm_spend_counters (
    tenant_id    UUID        NOT NULL,
    user_id      TEXT        NOT NULL DEFAULT '',
    agent_id     TEXT        NOT NULL DEFAULT '', -- pseudo agent ids allowed, TEXT not UUID
    period_start DATE        NOT NULL,
    cost_micros  BIGINT      NOT NULL DEFAULT 0,
    tokens_in    BIGINT      NOT NULL DEFAULT 0,
    tokens_out   BIGINT      NOT NULL DEFAULT 0,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, user_id, agent_id, period_start)
);

ALTER TABLE llm_spend_counters ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS llm_spend_counters_tenant_isolation ON llm_spend_counters;
CREATE POLICY llm_spend_counters_tenant_isolation ON llm_spend_counters
    USING (tenant_id::text = current_setting('app.current_tenant', true));

ALTER TABLE llm_providers ADD COLUMN IF NOT EXISTS price_in_micros_per_mtok BIGINT NOT NULL DEFAULT 0;
ALTER TABLE llm_providers ADD COLUMN IF NOT EXISTS price_out_micros_per_mtok BIGINT NOT NULL DEFAULT 0;

INSERT INTO schema_migrations (version, description)
VALUES ('042', 'spend_caps + llm_spend_counters: monthly LLM budget caps and accumulation; llm_providers pricing columns')
ON CONFLICT (version) DO NOTHING;
