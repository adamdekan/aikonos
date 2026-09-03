-- 045_llm_usage_events.sql
-- Per-call LLM usage events.
--
-- llm_spend_counters (migration 042) stays the authoritative cap meter: it is a
-- monthly rollup keyed (tenant, user, agent, period_start), so it carries no
-- day/model/run/session/group dimension. This table is the analytics twin —
-- one row per billable LLM call, written on the same single accounting path
-- (recordLlmUsage), with cost_micros taken from the same costMicrosFor result
-- the counter accumulates. provider/model are free-form TEXT so a future model
-- needs no schema change.
--
-- user_groups is an FGA membership snapshot at emit time, fail-open to empty:
-- it is a reporting dimension only and is never consulted for authorization.

CREATE TABLE IF NOT EXISTS llm_usage_events (
    id           BIGSERIAL   NOT NULL,
    ts           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    tenant_id    UUID        NOT NULL,
    user_id      TEXT        NOT NULL DEFAULT '',
    agent_id     TEXT        NOT NULL DEFAULT '', -- pseudo agent ids allowed, TEXT not UUID
    run_id       TEXT        NOT NULL DEFAULT '',
    session_id   TEXT        NOT NULL DEFAULT '',
    source       TEXT        NOT NULL DEFAULT '', -- chat | reason | vision | test
    provider     TEXT        NOT NULL DEFAULT '',
    model        TEXT        NOT NULL DEFAULT '',
    tokens_in    BIGINT      NOT NULL DEFAULT 0,
    tokens_out   BIGINT      NOT NULL DEFAULT 0,
    cache_read   BIGINT      NOT NULL DEFAULT 0,
    cache_write  BIGINT      NOT NULL DEFAULT 0,
    cost_micros  BIGINT      NOT NULL DEFAULT 0,
    user_groups  TEXT[]      NOT NULL DEFAULT '{}',
    PRIMARY KEY (id)
);

-- Every dashboard query filters (tenant, time range) first; the group dimension
-- reaches rows via = ANY(user_groups)/unnest off this same index scan.
CREATE INDEX IF NOT EXISTS llm_usage_events_tenant_ts
    ON llm_usage_events(tenant_id, ts);

ALTER TABLE llm_usage_events ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS llm_usage_events_tenant_isolation ON llm_usage_events;
CREATE POLICY llm_usage_events_tenant_isolation ON llm_usage_events
    USING (tenant_id::text = current_setting('app.current_tenant', true));

-- Retention sweep carve-out. Same rationale and shape as 033's baseline prune
-- and 044's limiter preload: the sweep is one cross-tenant DELETE with no
-- single tenant to scope app.current_tenant to, so under the NOBYPASSRLS
-- aikonos_app role RLS would filter it to zero rows. search_path is pinned empty
-- and the table schema-qualified so an attacker-controlled search_path cannot
-- shadow llm_usage_events and inherit the definer's rights.
CREATE OR REPLACE FUNCTION public.llm_usage_prune_events(p_before TIMESTAMPTZ)
    RETURNS BIGINT
    LANGUAGE plpgsql
    SECURITY DEFINER
    SET search_path = ''
AS $$
DECLARE
    removed BIGINT;
BEGIN
    DELETE FROM public.llm_usage_events WHERE ts < p_before;
    GET DIAGNOSTICS removed = ROW_COUNT;
    RETURN removed;
END;
$$;

-- Definer functions must not be executable by PUBLIC; migrate.sh grants EXECUTE
-- to aikonos_app after the app role exists.
REVOKE ALL ON FUNCTION public.llm_usage_prune_events(TIMESTAMPTZ) FROM PUBLIC;

INSERT INTO schema_migrations (version, description)
VALUES ('045', 'llm_usage_events: per-call LLM usage events + llm_usage_prune_events retention carve-out')
ON CONFLICT (version) DO NOTHING;
