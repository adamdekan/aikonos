-- Raise the agent_skill bundle size cap from 256 KiB to 5 MiB.
-- Mirrors the gateway-layer cap (SKILL_UPLOAD_MAX_BYTES, spec Risks row 3): the
-- DB CHECK guards against direct writes that bypass the gateway, so both layers
-- must move together. Drop the existing constraint then re-add it with the
-- larger ceiling. Idempotent: DROP IF EXISTS + re-ADD is safe to re-run.
DO $$
BEGIN
  ALTER TABLE agent_skill DROP CONSTRAINT IF EXISTS agent_skill_size_cap;
  ALTER TABLE agent_skill ADD CONSTRAINT agent_skill_size_cap CHECK (
    octet_length(body) + octet_length(extras::text) <= 5242880
  );
END
$$;

INSERT INTO schema_migrations (version, description)
VALUES ('028', 'agent_skill: raise bundle size cap to 5 MiB')
ON CONFLICT (version) DO NOTHING;
