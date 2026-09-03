-- 033_baseline_maintenance_fns.sql
-- SECURITY DEFINER carve-outs for the two legitimately cross-tenant baseline
-- maintenance sweeps.
--
-- Context: the broker connects as the non-superuser role `aikonos_app` (see
-- migrate.sh), so RLS now enforces on every tenant-scoped query. The baseline
-- Learner's two sweeps — discovering (tenant, agent) pairs across all tenants
-- and pruning old windows across all tenants — cannot set app.current_tenant
-- (there is no single tenant to scope to), so under enforced RLS they would
-- see/prune zero rows. These functions run with the definer's (owner `aikonos`)
-- rights, bypassing RLS for exactly these two queries and nothing else. This is
-- the only sanctioned RLS bypass; the surface is two named, grantable functions
-- rather than an ambient superuser connection.

-- search_path is pinned empty and every object is schema-qualified: a
-- SECURITY DEFINER function runs with the owner's (superuser) rights, so an
-- attacker-controlled search_path could otherwise shadow `agent_behavior_windows`
-- with a malicious object and hijack owner privileges. Empty search_path forces
-- explicit qualification and closes that vector regardless of the PUBLIC-CREATE
-- default on schema public.

-- Every (tenant, agent) pair with at least one window at or after p_since.
CREATE OR REPLACE FUNCTION public.baseline_distinct_agents(p_since TIMESTAMPTZ)
    RETURNS TABLE(tenant_id TEXT, agent_id TEXT)
    LANGUAGE sql
    SECURITY DEFINER
    SET search_path = ''
AS $$
    SELECT DISTINCT w.tenant_id, w.agent_id
    FROM public.agent_behavior_windows w
    WHERE w.window_start >= p_since
$$;

-- Delete every window strictly before p_cutoff; return the number removed.
CREATE OR REPLACE FUNCTION public.baseline_prune_windows(p_cutoff TIMESTAMPTZ)
    RETURNS BIGINT
    LANGUAGE plpgsql
    SECURITY DEFINER
    SET search_path = ''
AS $$
DECLARE
    removed BIGINT;
BEGIN
    DELETE FROM public.agent_behavior_windows WHERE window_start < p_cutoff;
    GET DIAGNOSTICS removed = ROW_COUNT;
    RETURN removed;
END;
$$;

-- Definer functions must not be executable by PUBLIC; migrate.sh grants EXECUTE
-- to aikonos_app after the app role exists.
REVOKE ALL ON FUNCTION public.baseline_distinct_agents(TIMESTAMPTZ) FROM PUBLIC;
REVOKE ALL ON FUNCTION public.baseline_prune_windows(TIMESTAMPTZ) FROM PUBLIC;

INSERT INTO schema_migrations (version, description)
VALUES ('033', 'baseline_distinct_agents + baseline_prune_windows: SECURITY DEFINER cross-tenant carve-outs for the RLS-enforcing app role')
ON CONFLICT (version) DO NOTHING;
