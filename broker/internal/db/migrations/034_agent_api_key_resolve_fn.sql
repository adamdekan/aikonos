-- 034_agent_api_key_resolve_fn.sql
-- SECURITY DEFINER carve-out for the one legitimately tenant-agnostic lookup on
-- agent_api_keys: resolving a raw API key to its row by its globally-unique hash.
--
-- Context: the broker connects as the non-superuser role `aikonos_app` (see
-- migrate.sh), so the tenant_isolation RLS policy on agent_api_keys (migration
-- 012) enforces on every query. External-agent authentication resolves an
-- inbound key by HMAC hash BEFORE any tenant is known — there is no tenant to
-- scope to at that point — so under enforced RLS the SELECT sees zero rows and
-- every minted key authenticates as "revoked or not found". This is the same
-- shape of problem migration 033 solved for the baseline sweeps: a genuinely
-- cross-tenant read that cannot set app.current_tenant.
--
-- This function runs with the definer's (owner `aikonos`) rights, bypassing RLS
-- for exactly this key_hash lookup and nothing else. The caller (broker
-- ResolveAgentApiKey RPC) still enforces tenant correctness against the returned
-- tenant_id after resolution, so the trust boundary is unchanged — the hash is
-- an unguessable bearer secret and the row it returns is self-describing.

-- search_path is pinned empty and every object is schema-qualified: a
-- SECURITY DEFINER function runs with the owner's (superuser) rights, so an
-- attacker-controlled search_path could otherwise shadow `agent_api_keys` with a
-- malicious object and hijack owner privileges. Empty search_path forces
-- explicit qualification and closes that vector.

CREATE OR REPLACE FUNCTION public.resolve_agent_api_key(p_key_hash TEXT)
    RETURNS TABLE(
        id           UUID,
        tenant_id    UUID,
        agent_id     UUID,
        key_hash     TEXT,
        key_prefix   TEXT,
        label        TEXT,
        created_by   TEXT,
        created_at   TIMESTAMPTZ,
        last_used_at TIMESTAMPTZ,
        revoked_at   TIMESTAMPTZ
    )
    LANGUAGE sql
    SECURITY DEFINER
    SET search_path = ''
AS $$
    SELECT k.id, k.tenant_id, k.agent_id, k.key_hash, k.key_prefix, k.label,
           k.created_by, k.created_at, k.last_used_at, k.revoked_at
    FROM public.agent_api_keys k
    WHERE k.key_hash = p_key_hash AND k.revoked_at IS NULL
$$;

-- Definer functions must not be executable by PUBLIC; migrate.sh grants EXECUTE
-- to aikonos_app after the app role exists.
REVOKE ALL ON FUNCTION public.resolve_agent_api_key(TEXT) FROM PUBLIC;

INSERT INTO schema_migrations (version, description)
VALUES ('034', 'resolve_agent_api_key: SECURITY DEFINER carve-out for the tenant-agnostic API-key hash lookup under the RLS-enforcing app role')
ON CONFLICT (version) DO NOTHING;
