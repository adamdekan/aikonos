-- 004_scheduled_runs.sql
-- Scheduler: users schedule agentic runs (one-off or recurring) that the
-- agent-gateway fires unattended. The broker owns the durable schedule; the
-- gateway ticker claims due rows (ClaimDue) and reports outcomes (ReportResult).
--
-- approved_tools is the standing human-consent allowlist: when an unattended run
-- hits a tool that would normally need human approval, the gateway approver
-- grants it iff its tool id is in this list. It never overrides an OPA DENY or a
-- capability-scope failure.

CREATE TABLE IF NOT EXISTS scheduled_runs (
    scheduled_run_id  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id         UUID NOT NULL,
    owner_user_id     TEXT NOT NULL,
    prompt            TEXT NOT NULL,
    schedule_kind     TEXT NOT NULL CHECK (schedule_kind IN ('CRON','ONCE')),
    -- Set for CRON schedules; NULL for ONCE.
    cron_expr         TEXT,
    -- Next time this run is due. NULL once a ONCE run has fired, or while PAUSED.
    next_fire_at      TIMESTAMPTZ,
    approved_tools    JSONB NOT NULL DEFAULT '[]'::jsonb,
    state             TEXT NOT NULL DEFAULT 'ACTIVE'
                      CHECK (state IN ('ACTIVE','PAUSED','COMPLETED','FAILED')),
    last_fire_at      TIMESTAMPTZ,
    last_status       TEXT,
    last_summary      TEXT,
    run_count         INTEGER NOT NULL DEFAULT 0,
    created_by        TEXT NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Drives the ClaimDue query (state='ACTIVE' AND next_fire_at <= now).
CREATE INDEX IF NOT EXISTS idx_scheduled_runs_due ON scheduled_runs (state, next_fire_at);
CREATE INDEX IF NOT EXISTS idx_scheduled_runs_tenant_owner ON scheduled_runs (tenant_id, owner_user_id);

ALTER TABLE scheduled_runs ENABLE ROW LEVEL SECURITY;
CREATE POLICY scheduled_runs_tenant_isolation ON scheduled_runs
    USING (tenant_id::text = current_setting('app.current_tenant', true));

INSERT INTO schema_migrations (version, description)
VALUES ('004', 'scheduled_runs for user-scheduled agentic runs')
ON CONFLICT (version) DO NOTHING;
