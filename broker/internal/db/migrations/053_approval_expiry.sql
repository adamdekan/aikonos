-- 053_approval_expiry.sql
-- AG-07 (M-09): approval_requests.expires_at is enforced. The broker's expiry
-- sweep moves PENDING rows past expires_at to EXPIRED and their task to
-- TIMEOUT. Finding them is one cross-tenant read with no single tenant to
-- scope app.current_tenant to, so (as with 045's prune) it goes through a
-- SECURITY DEFINER function; the writes stay tenant-scoped in the broker.
-- search_path is pinned empty and the table schema-qualified. migrate.sh
-- grants EXECUTE to aikonos_app.

CREATE INDEX IF NOT EXISTS idx_approvals_pending_expiry ON approval_requests (expires_at)
    WHERE state = 'PENDING';

CREATE OR REPLACE FUNCTION public.approval_requests_expired()
    RETURNS TABLE(tenant_id UUID, approval_id UUID, task_id UUID)
    LANGUAGE sql
    SECURITY DEFINER
    SET search_path = ''
AS $$
    SELECT a.tenant_id, a.approval_id, a.task_id
    FROM public.approval_requests a
    WHERE a.state = 'PENDING' AND a.expires_at <= pg_catalog.now()
    ORDER BY a.expires_at
    LIMIT 500
$$;

REVOKE ALL ON FUNCTION public.approval_requests_expired() FROM PUBLIC;

INSERT INTO schema_migrations (version, description)
VALUES ('053', 'approval_requests: expiry sweep carve-out (AG-07)')
ON CONFLICT (version) DO NOTHING;
