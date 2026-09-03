-- Add DISMISSED to the envelopes.state CHECK constraint.
-- Drop the existing constraint (if it exists under either historic name) then
-- re-add it with the expanded value set. Idempotent: both DROP and ADD are
-- guarded so re-running the migration is safe.
DO $$
BEGIN
  ALTER TABLE envelopes DROP CONSTRAINT IF EXISTS envelopes_state_check;
  ALTER TABLE envelopes ADD CONSTRAINT envelopes_state_check
    CHECK (state IN ('PENDING','DELIVERED','ACCEPTED','REJECTED','COMPLETED','EXPIRED','DISMISSED'));
END
$$;

INSERT INTO schema_migrations (version, description)
VALUES ('024', 'envelopes: add DISMISSED state')
ON CONFLICT (version) DO NOTHING;
