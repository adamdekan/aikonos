-- 049_scheduled_runs_delegation.sql
-- AG-17 S4: a schedule row is a standing, visible, revocable delegation from
-- its owner (subject) to an agent (actor). ClaimDue skips a row whose
-- delegation is expired or revoked. Old rows get deploy time + 90 days (not
-- created_at: that would park every schedule older than 90 days on deploy).
-- Deleting the actor agent deletes its schedules: a dead actor ends the delegation.
--
-- ponytail: actor_agent_id is nullable (spec says NOT NULL). There is no tenant
-- default agent yet (AG-01) to backfill from; AG-01 backfills it and sets NOT NULL.
--
-- Idempotent: CI applies the migration set twice to prove that.

ALTER TABLE scheduled_runs
    ADD COLUMN IF NOT EXISTS actor_agent_id        UUID REFERENCES agents(id) ON DELETE CASCADE,
    ADD COLUMN IF NOT EXISTS delegation_expires_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS delegation_revoked_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS delegation_revoked_by TEXT;

UPDATE scheduled_runs
SET delegation_expires_at = NOW() + INTERVAL '90 days'
WHERE delegation_expires_at IS NULL;

ALTER TABLE scheduled_runs
    ALTER COLUMN delegation_expires_at SET DEFAULT NOW() + INTERVAL '90 days',
    ALTER COLUMN delegation_expires_at SET NOT NULL;

INSERT INTO schema_migrations (version, description)
VALUES ('049', 'scheduled_runs: delegation columns (actor_agent_id, delegation_expires_at/revoked_at/revoked_by)')
ON CONFLICT (version) DO NOTHING;
