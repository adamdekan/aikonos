-- 052_agent_api_keys_expiry.sql
-- AG-15 (L-03): every tk_ API key expires. Existing keys are backfilled to
-- created_at + the default tenant maximum (api_keys.max_ttl_days = 90; the
-- setting is new, so no tenant has overridden it yet), but never sooner than
-- 14 days after the migration, so old keys get a rotation window.
--
-- resolve_agent_api_key (034) now skips expired rows, so an expired key
-- resolves exactly like a revoked one. Its return type gains expires_at, which
-- CREATE OR REPLACE cannot change, hence the DROP. migrate.sh re-grants
-- EXECUTE to aikonos_app after every run.
--
-- Idempotent: CI applies the migration set twice.

ALTER TABLE agent_api_keys ADD COLUMN IF NOT EXISTS expires_at TIMESTAMPTZ;
UPDATE agent_api_keys SET expires_at = GREATEST(created_at + INTERVAL '90 days', now() + INTERVAL '14 days') WHERE expires_at IS NULL;
ALTER TABLE agent_api_keys ALTER COLUMN expires_at SET NOT NULL;

DROP FUNCTION IF EXISTS public.resolve_agent_api_key(TEXT);
CREATE FUNCTION public.resolve_agent_api_key(p_key_hash TEXT)
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
        revoked_at   TIMESTAMPTZ,
        expires_at   TIMESTAMPTZ
    )
    LANGUAGE sql
    SECURITY DEFINER
    SET search_path = ''
AS $$
    SELECT k.id, k.tenant_id, k.agent_id, k.key_hash, k.key_prefix, k.label,
           k.created_by, k.created_at, k.last_used_at, k.revoked_at, k.expires_at
    FROM public.agent_api_keys k
    WHERE k.key_hash = p_key_hash AND k.revoked_at IS NULL AND k.expires_at > pg_catalog.now()
$$;

REVOKE ALL ON FUNCTION public.resolve_agent_api_key(TEXT) FROM PUBLIC;

INSERT INTO schema_migrations (version, description)
VALUES ('052', 'agent_api_keys: mandatory expires_at (AG-15)')
ON CONFLICT (version) DO NOTHING;
