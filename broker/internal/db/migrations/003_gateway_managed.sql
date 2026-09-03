-- 003_gateway_managed.sql
-- Phase 7: a task may be driven by an external agent harness (the Pi
-- agent-gateway) rather than the broker's own executor. For such tasks the
-- gateway calls InvokeTool itself, so the broker must NOT spawn a sandbox or
-- run the server-side executor on APPROVED (that would double-execute). This
-- flag is set at CreateTask from a reserved skill_hint sentinel
-- ("aikonos:execution=gateway") and consulted in SubmitPlan / ApproveTask before
-- launchTask.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS gateway_managed BOOLEAN NOT NULL DEFAULT false;

INSERT INTO schema_migrations (version, description)
VALUES ('003', 'tasks.gateway_managed for agent-gateway-driven execution')
ON CONFLICT (version) DO NOTHING;
