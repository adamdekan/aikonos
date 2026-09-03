-- 002_step_args.sql
-- Server-side per-step execution (Phase 6.4) needs the actual tool args to
-- replay a step through the Tool Proxy. plan_steps previously kept only
-- args_hash (full args were destined for the audit store). Persist the args
-- here too — the row is already tenant-isolated by RLS. A stricter deployment
-- where the sandbox agent drives execution would not need this (it holds the
-- args in-pod and calls InvokeTool), but the broker-orchestrated executor does.
ALTER TABLE plan_steps ADD COLUMN IF NOT EXISTS args_json JSONB;

INSERT INTO schema_migrations (version, description)
VALUES ('002', 'plan_steps.args_json for server-side per-step execution')
ON CONFLICT (version) DO NOTHING;
