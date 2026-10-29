-- 066_alerts_retention.sql
-- Retention sweep for the alerts table (F34): nothing pruned it, so a denial
-- flood grew it without bound. Same carve-out shape as 045's
-- llm_usage_prune_events: the sweep is one cross-tenant DELETE with no single
-- tenant to scope app.current_tenant to, so under the NOBYPASSRLS aikonos_app
-- role RLS would filter it to zero rows. search_path is pinned empty and the
-- table schema-qualified so an attacker-controlled search_path cannot shadow
-- alerts and inherit the definer's rights.
CREATE OR REPLACE FUNCTION public.alerts_prune(p_before TIMESTAMPTZ)
    RETURNS BIGINT
    LANGUAGE plpgsql
    SECURITY DEFINER
    SET search_path = ''
AS $$
DECLARE
    removed BIGINT;
BEGIN
    DELETE FROM public.alerts WHERE created_at < p_before;
    GET DIAGNOSTICS removed = ROW_COUNT;
    RETURN removed;
END;
$$;

-- Definer functions must not be executable by PUBLIC; migrate.sh grants EXECUTE
-- to aikonos_app after the app role exists.
REVOKE ALL ON FUNCTION public.alerts_prune(TIMESTAMPTZ) FROM PUBLIC;

INSERT INTO schema_migrations (version, description)
VALUES ('066', 'alerts_prune retention carve-out')
ON CONFLICT (version) DO NOTHING;
