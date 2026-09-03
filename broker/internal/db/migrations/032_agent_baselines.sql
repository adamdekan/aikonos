-- 032_agent_baselines.sql
-- Automated agent behavioral baseline learning (Enterprise Domain 4,
-- ). Two tables: the raw rolling
-- per-window observations the Recorder flushes and the Learner reads/prunes,
-- and the materialized learned envelope the Detector compares against.

CREATE TABLE IF NOT EXISTS agent_behavior_windows (
    tenant_id    TEXT NOT NULL,
    agent_id     TEXT NOT NULL,
    window_start TIMESTAMPTZ NOT NULL,
    tool_id      TEXT NOT NULL,
    invocations  BIGINT NOT NULL DEFAULT 0,
    cost_units   BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (tenant_id, agent_id, tool_id, window_start)
);

CREATE INDEX IF NOT EXISTS idx_agent_behavior_windows_tenant_window
    ON agent_behavior_windows (tenant_id, window_start);

ALTER TABLE agent_behavior_windows ENABLE ROW LEVEL SECURITY;
CREATE POLICY agent_behavior_windows_tenant_isolation ON agent_behavior_windows
    USING (tenant_id = current_setting('app.current_tenant', true));

CREATE TABLE IF NOT EXISTS agent_baselines (
    tenant_id      TEXT NOT NULL,
    agent_id       TEXT NOT NULL,
    tool_set       JSONB NOT NULL DEFAULT '[]',
    rpm_p95        DOUBLE PRECISION NOT NULL DEFAULT 0,
    cost_p95       DOUBLE PRECISION NOT NULL DEFAULT 0,
    sample_windows INT NOT NULL DEFAULT 0,
    first_seen     TIMESTAMPTZ NOT NULL DEFAULT now(),
    computed_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, agent_id)
);

ALTER TABLE agent_baselines ENABLE ROW LEVEL SECURITY;
CREATE POLICY agent_baselines_tenant_isolation ON agent_baselines
    USING (tenant_id = current_setting('app.current_tenant', true));

INSERT INTO schema_migrations (version, description)
VALUES ('032', 'agent_behavior_windows + agent_baselines: automated behavioral baseline learning')
ON CONFLICT (version) DO NOTHING;
