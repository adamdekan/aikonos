-- broker/internal/db/migrations/001_initial_schema.sql
-- Aikonos Broker — initial database schema
-- Run via: psql -U aikonos -d aikonos -f 001_initial_schema.sql
-- Or automatically via the broker's migration runner on startup.
--
-- Design notes:
--   - tenant_id on EVERY table — Row Level Security enforced from day one
--   - UUIDs everywhere — sortable via UUIDv7 where time ordering matters
--   - JSONB for flexible context fields — indexed where queried
--   - Audit trail goes to MinIO (immutable), not this DB — DB is operational state only

BEGIN;

-- ── Extensions ────────────────────────────────────────────────────────────────
CREATE EXTENSION IF NOT EXISTS "uuid-ossp";
CREATE EXTENSION IF NOT EXISTS "pgcrypto";

-- ── RLS helper ───────────────────────────────────────────────────────────────
-- All queries must set this: SET app.current_tenant = '<tenant_id>';
-- The RLS policies below enforce it.

-- ── Tasks ─────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS tasks (
    task_id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    parent_task_id  UUID REFERENCES tasks(task_id) ON DELETE SET NULL,
    tenant_id       UUID NOT NULL,
    owner_user_id   TEXT NOT NULL,
    -- State machine values match TaskStatus proto enum
    state           TEXT NOT NULL DEFAULT 'CREATED'
                    CHECK (state IN (
                        'CREATED','PLANNING','VALIDATING','AWAITING_APPROVAL',
                        'APPROVED','EXECUTING','COMPLETED','FAILED',
                        'DENIED','CANCELLED','TERMINATED','TIMEOUT'
                    )),
    prompt          TEXT NOT NULL,
    cost_budget     BIGINT NOT NULL DEFAULT 1000,
    cost_consumed   BIGINT NOT NULL DEFAULT 0,
    replan_count    SMALLINT NOT NULL DEFAULT 0,
    trace_id        TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deadline        TIMESTAMPTZ,
    completed_at    TIMESTAMPTZ
);

CREATE INDEX idx_tasks_tenant_state ON tasks (tenant_id, state);
CREATE INDEX idx_tasks_tenant_owner ON tasks (tenant_id, owner_user_id);
CREATE INDEX idx_tasks_parent ON tasks (parent_task_id) WHERE parent_task_id IS NOT NULL;
CREATE INDEX idx_tasks_updated ON tasks (updated_at DESC);

ALTER TABLE tasks ENABLE ROW LEVEL SECURITY;
CREATE POLICY tasks_tenant_isolation ON tasks
    USING (tenant_id::text = current_setting('app.current_tenant', true));

-- ── Plan steps ────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS plan_steps (
    step_id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    task_id             UUID NOT NULL REFERENCES tasks(task_id) ON DELETE CASCADE,
    tenant_id           UUID NOT NULL,
    seq                 SMALLINT NOT NULL,
    tool_id             TEXT NOT NULL,
    -- args stored only as a hash here; full args in audit log (MinIO)
    args_hash           TEXT,
    effect_class        TEXT NOT NULL,
    justification       TEXT,
    estimated_cost      BIGINT NOT NULL DEFAULT 0,
    actual_cost         BIGINT,
    policy_decision     TEXT CHECK (policy_decision IN ('ALLOW','DENY','APPROVAL_REQUIRED','STEP_UP_REQUIRED')),
    policy_rule_id      TEXT,
    capability_token_id UUID,
    state               TEXT NOT NULL DEFAULT 'PENDING'
                        CHECK (state IN ('PENDING','APPROVED','DENIED','EXECUTING','COMPLETED','FAILED')),
    started_at          TIMESTAMPTZ,
    ended_at            TIMESTAMPTZ,
    error_message       TEXT,
    UNIQUE (task_id, seq)
);

CREATE INDEX idx_plan_steps_task ON plan_steps (task_id);
CREATE INDEX idx_plan_steps_tenant ON plan_steps (tenant_id);

ALTER TABLE plan_steps ENABLE ROW LEVEL SECURITY;
CREATE POLICY plan_steps_tenant_isolation ON plan_steps
    USING (tenant_id::text = current_setting('app.current_tenant', true));

-- ── Capability tokens ─────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS capability_tokens (
    token_id        UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    task_id         UUID NOT NULL REFERENCES tasks(task_id) ON DELETE CASCADE,
    step_id         UUID REFERENCES plan_steps(step_id) ON DELETE SET NULL,
    tenant_id       UUID NOT NULL,
    -- SPIFFE ID of the workload this token was issued to
    issued_to       TEXT NOT NULL,
    -- Structured scope (domain:action:resource[:constraints])
    scope           JSONB NOT NULL,
    ttl_seconds     INT NOT NULL DEFAULT 300,   -- 5 min default per architecture doc
    -- Single-use nonce — checked and marked used at tool proxy
    nonce           TEXT NOT NULL UNIQUE,
    nonce_used      BOOLEAN NOT NULL DEFAULT FALSE,
    nonce_used_at   TIMESTAMPTZ,
    revoked         BOOLEAN NOT NULL DEFAULT FALSE,
    revoked_at      TIMESTAMPTZ,
    revoke_reason   TEXT,
    issued_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at      TIMESTAMPTZ NOT NULL
);

CREATE INDEX idx_tokens_task ON capability_tokens (task_id);
CREATE INDEX idx_tokens_nonce ON capability_tokens (nonce) WHERE nonce_used = FALSE AND revoked = FALSE;
CREATE INDEX idx_tokens_expires ON capability_tokens (expires_at) WHERE revoked = FALSE;

ALTER TABLE capability_tokens ENABLE ROW LEVEL SECURITY;
CREATE POLICY tokens_tenant_isolation ON capability_tokens
    USING (tenant_id::text = current_setting('app.current_tenant', true));

-- ── Task envelopes (inter-agent delegation) ───────────────────────────────────
CREATE TABLE IF NOT EXISTS envelopes (
    envelope_id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    parent_task_id      UUID REFERENCES tasks(task_id) ON DELETE SET NULL,
    tenant_id           UUID NOT NULL,
    from_user_id        TEXT NOT NULL,
    -- to_target is {type: "user"|"group"|"role", id: "..."}
    to_target           JSONB NOT NULL,
    -- task_spec mirrors EnvelopeTask proto
    task_spec           JSONB NOT NULL,
    -- delegation_spec mirrors EnvelopeDelegation proto (token stored separately)
    delegation_spec     JSONB NOT NULL,
    depth               SMALLINT NOT NULL DEFAULT 1,
    state               TEXT NOT NULL DEFAULT 'PENDING'
                        CHECK (state IN ('PENDING','DELIVERED','ACCEPTED','REJECTED','COMPLETED','EXPIRED')),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    delivered_at        TIMESTAMPTZ,
    accepted_at         TIMESTAMPTZ,
    completed_at        TIMESTAMPTZ,
    expires_at          TIMESTAMPTZ NOT NULL,
    trace_id            TEXT
);

CREATE INDEX idx_envelopes_tenant ON envelopes (tenant_id, state);
CREATE INDEX idx_envelopes_from ON envelopes (tenant_id, from_user_id);
CREATE INDEX idx_envelopes_expires ON envelopes (expires_at) WHERE state = 'PENDING';

ALTER TABLE envelopes ENABLE ROW LEVEL SECURITY;
CREATE POLICY envelopes_tenant_isolation ON envelopes
    USING (tenant_id::text = current_setting('app.current_tenant', true));

-- ── Kill switches ─────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS kill_switches (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       UUID NOT NULL,
    -- scope: 'user' | 'tenant' | 'platform'
    scope           TEXT NOT NULL CHECK (scope IN ('user','tenant','platform')),
    -- subject: user_id, tenant_id, or '*' for platform
    subject         TEXT NOT NULL,
    active          BOOLEAN NOT NULL DEFAULT TRUE,
    reason          TEXT,
    activated_by    TEXT NOT NULL,
    activated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deactivated_at  TIMESTAMPTZ,
    deactivated_by  TEXT
);

CREATE INDEX idx_kill_switches_active ON kill_switches (tenant_id, scope, subject)
    WHERE active = TRUE;

-- ── Approval requests ─────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS approval_requests (
    approval_id     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    task_id         UUID NOT NULL REFERENCES tasks(task_id) ON DELETE CASCADE,
    tenant_id       UUID NOT NULL,
    requester_id    TEXT NOT NULL,
    -- JSON summary of what needs approval (safe to show to approver)
    summary         JSONB NOT NULL,
    -- Set of user_ids who can approve (resolved from FGA at request time)
    approver_set    TEXT[] NOT NULL,
    requires_n      SMALLINT NOT NULL DEFAULT 1,   -- 2 for four-eyes
    approved_by     TEXT[],
    denied_by       TEXT[],
    state           TEXT NOT NULL DEFAULT 'PENDING'
                    CHECK (state IN ('PENDING','APPROVED','DENIED','EXPIRED')),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at      TIMESTAMPTZ NOT NULL,
    resolved_at     TIMESTAMPTZ,
    denial_reason   TEXT
);

CREATE INDEX idx_approvals_task ON approval_requests (task_id);
CREATE INDEX idx_approvals_tenant_pending ON approval_requests (tenant_id, state)
    WHERE state = 'PENDING';

ALTER TABLE approval_requests ENABLE ROW LEVEL SECURITY;
CREATE POLICY approvals_tenant_isolation ON approval_requests
    USING (tenant_id::text = current_setting('app.current_tenant', true));

-- ── Schema version tracking ───────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS schema_migrations (
    version         TEXT PRIMARY KEY,
    applied_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    description     TEXT
);

INSERT INTO schema_migrations (version, description)
VALUES ('001', 'Initial schema: tasks, plan_steps, capability_tokens, envelopes, kill_switches, approval_requests')
ON CONFLICT DO NOTHING;

-- ── Triggers: auto-update updated_at ─────────────────────────────────────────
CREATE OR REPLACE FUNCTION update_updated_at()
RETURNS TRIGGER AS $$
BEGIN NEW.updated_at = NOW(); RETURN NEW; END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER tasks_updated_at
    BEFORE UPDATE ON tasks
    FOR EACH ROW EXECUTE FUNCTION update_updated_at();

COMMIT;
