-- 058_agent_mcp_tools.sql
-- AG-08: per-agent MCP tool allowlist. Attaching a connector no longer grants
-- every tool it exposes; the admin picks tools ("mcp:<conn>:<tool>" ids).
-- Default empty: an existing agent with connectors attached gets no MCP tool
-- until an admin selects them (least privilege, no backfill).

ALTER TABLE agents ADD COLUMN IF NOT EXISTS mcp_tools JSONB NOT NULL DEFAULT '[]'::jsonb;

INSERT INTO schema_migrations (version, description)
VALUES ('058', 'agents.mcp_tools: per-agent MCP tool allowlist (AG-08)')
ON CONFLICT (version) DO NOTHING;
