-- 044_rate_limit_policies_all_fn.sql
-- SECURITY DEFINER carve-out for the cross-tenant rate-limit policy preload the
-- broker runs once at startup.
--
-- Context: the in-process limiter holds one flat policy set covering every
-- tenant (ratelimit.Policy carries its own TenantID), and SetPolicies replaces
-- that set wholesale. At startup there is no single tenant to scope to — the
-- broker has not served a request yet, so no OIDC `tid` claim exists — which
-- means the preload cannot set app.current_tenant and under enforced RLS would
-- arm the limiter with zero policies. The runtime path is unaffected: policy
-- mutations hot-reload through the ordinary tenant-scoped List.
--
-- Same pattern and rationale as 033's baseline sweeps and 034's api-key resolve:
-- a single named, grantable function rather than an ambient superuser
-- connection. search_path is pinned empty and the table is schema-qualified so
-- an attacker-controlled search_path cannot shadow rate_limit_policies and
-- inherit the definer's rights.

CREATE OR REPLACE FUNCTION public.rate_limit_policies_all()
    RETURNS TABLE(
        id          UUID,
        tenant_id   UUID,
        agent_id    UUID,
        provider    TEXT,
        rpm_limit   INTEGER,
        tpm_limit   INTEGER,
        created_by  TEXT,
        created_at  TIMESTAMPTZ,
        updated_at  TIMESTAMPTZ
    )
    LANGUAGE sql
    SECURITY DEFINER
    SET search_path = ''
AS $$
    SELECT p.id, p.tenant_id, p.agent_id, p.provider, p.rpm_limit, p.tpm_limit,
           p.created_by, p.created_at, p.updated_at
    FROM public.rate_limit_policies p
    ORDER BY p.created_at
$$;

-- Definer functions must not be executable by PUBLIC; migrate.sh grants EXECUTE
-- to aikonos_app after the app role exists.
REVOKE ALL ON FUNCTION public.rate_limit_policies_all() FROM PUBLIC;

INSERT INTO schema_migrations (version, description)
VALUES ('044', 'rate_limit_policies_all: SECURITY DEFINER cross-tenant carve-out for the startup limiter preload')
ON CONFLICT (version) DO NOTHING;
