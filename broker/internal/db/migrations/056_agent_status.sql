-- 056_agent_status.sql
-- AG-03: agent lifecycle and kill switch.
--
-- agents.status is staged | active | suspended | retired. Only an active
-- agent runs: CreateGatewayTask, north CreateTask, SubmitPlan,
-- ResolveAgentApiKey, the scheduled-run claim and every InvokeTool refuse any
-- other status, so a suspension stops in-flight tasks at their next tool call.
-- Existing agents (the tenant default agent included) become active; new
-- agents start staged and need an owner to activate. status_reason keeps the
-- admin's (or the orphan sweep's) reason for the last change.
-- gateway_enabled stays a per-channel flag under the status: it only opens
-- the external surface for an agent that is active.
--
-- Idempotent: CI applies the migration set twice.

ALTER TABLE agents ADD COLUMN IF NOT EXISTS status TEXT NOT NULL DEFAULT 'active'
    CHECK (status IN ('staged', 'active', 'suspended', 'retired'));
ALTER TABLE agents ALTER COLUMN status SET DEFAULT 'staged';
ALTER TABLE agents ADD COLUMN IF NOT EXISTS status_reason TEXT NOT NULL DEFAULT '';

INSERT INTO schema_migrations (version, description)
VALUES ('056', 'agents.status lifecycle and status_reason (AG-03)')
ON CONFLICT (version) DO NOTHING;
