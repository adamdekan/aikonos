-- F22: client-supplied idempotency key on task creation. Nullable/empty means
-- "no idempotency requested" (today's behavior); the partial unique index
-- only constrains rows that actually opted in.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS client_request_id TEXT;
CREATE UNIQUE INDEX IF NOT EXISTS idx_tasks_tenant_client_request_id
    ON tasks (tenant_id, client_request_id)
    WHERE client_request_id IS NOT NULL AND client_request_id <> '';
INSERT INTO schema_migrations (version, description)
VALUES ('031', 'tasks: client_request_id idempotency key + partial unique index')
ON CONFLICT (version) DO NOTHING;
