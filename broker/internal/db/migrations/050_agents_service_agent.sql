-- 050_agents_service_agent.sql
-- AG-17 S5: an agent may run subject-less (external tk_ key, no user) only
-- when an admin marks it a service agent. Default false: existing agents are
-- not service agents; identity.obo=audit records the gap, enforce refuses.
--
-- Idempotent (ADD COLUMN IF NOT EXISTS): CI applies the migration set twice.

ALTER TABLE agents ADD COLUMN IF NOT EXISTS service_agent BOOLEAN NOT NULL DEFAULT false;

INSERT INTO schema_migrations (version, description)
VALUES ('050', 'agents: service_agent flag')
ON CONFLICT (version) DO NOTHING;
