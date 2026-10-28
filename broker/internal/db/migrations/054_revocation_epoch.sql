-- 054_revocation_epoch.sql
-- AG-06: per-principal revocation epoch and user block.
--
-- A revocation epoch ("not before") is the instant before which every
-- credential issued to the principal is void: IdP tokens (iat), capability
-- tokens (issued_at fact) and owner grants. Revoke-all moves it to now. Users
-- keep theirs on user_directory, agents on the agents row.
--
-- A block is an infinite epoch. User and tenant blocks live in the
-- kill_switches table from 001 (scope 'user' with subject = principal id,
-- scope 'tenant' for the whole tenant), which nothing read until now. At most
-- one active row per (tenant, scope, subject); older duplicates (aikonosctl
-- wrote without a unique key) are deactivated so the index can be built.
--
-- kill_switches gains the same tenant-isolation RLS policy as every other
-- tenant-scoped table now that the broker reads it as aikonos_app.
--
-- Idempotent: CI applies the migration set twice.

ALTER TABLE user_directory ADD COLUMN IF NOT EXISTS revoked_before TIMESTAMPTZ;
ALTER TABLE agents ADD COLUMN IF NOT EXISTS revoked_before TIMESTAMPTZ;

UPDATE kill_switches k
SET active = FALSE, deactivated_at = NOW(), deactivated_by = 'migration-054'
WHERE k.active AND EXISTS (
    SELECT 1 FROM kill_switches o
    WHERE o.active AND o.tenant_id = k.tenant_id AND o.scope = k.scope AND o.subject = k.subject
      AND (o.activated_at, o.id) > (k.activated_at, k.id)
);

CREATE UNIQUE INDEX IF NOT EXISTS kill_switches_one_active
    ON kill_switches (tenant_id, scope, subject) WHERE active;

ALTER TABLE kill_switches ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS kill_switches_tenant_isolation ON kill_switches;
CREATE POLICY kill_switches_tenant_isolation ON kill_switches
    USING (tenant_id::text = current_setting('app.current_tenant', true));

INSERT INTO schema_migrations (version, description)
VALUES ('054', 'revocation epoch (user_directory/agents.revoked_before) and kill_switches user/tenant block (AG-06)')
ON CONFLICT (version) DO NOTHING;
