# Agent & MCP Server Deployment Checklist

**Version:** 1.0  
**Status:** Active  
**Owner:** Security Team  
**Effective:** 2026-06-19

All items must be checked before an agent or MCP server is promoted to production. Incomplete checklists block deployment.

---

## New Agent Checklist

**Agent name / ID:** ___________________________  
**Reviewer:** ___________________________  
**Date:** ___________________________

### Skill Bundle Review

- [ ] Skill bundle uploaded to broker via Admin → Agents → Upload Bundle.
- [ ] `allowed_tools` list reviewed: contains only tools the agent requires for its stated purpose. No wildcards.
- [ ] Each tool in `allowed_tools` has an `effect_class` declared in the skill manifest (`read`, `write`, `destructive`, `external`). No tool has an undeclared or missing effect class.
- [ ] Any tool with `effect_class=destructive` is explicitly documented with justification. If no destructive tools are needed, none are present in the bundle.
- [ ] Skill bundle does not include `mcp-echo` or any dev/test-only tool IDs.
- [ ] Bundle version is pinned; floating/latest references are not permitted.
- [ ] Auto-load keywords (Admin → Agents → Skill Bundles) are reviewed: each keyword is specific enough not to false-positive on common chat phrasing, since a keyword match auto-activates the bundle for that turn without the model calling `load_skill`. Leave keywords empty if the bundle should only ever load on explicit `/command` or model-driven invocation.

### Effect Class Audit

- [ ] All `write_external` tools (connectors, `email.draft` with send, `web.fetch` POST) are listed and justified.
- [ ] Plan templates (if any) do not combine `reads_sensitive` + `write_external` steps without `has_dlp_attestation` — if they do, a DLP attestation mechanism is in place and tested.
- [ ] HITL requirement documented for all `destructive` and `external` write steps; confirm the gateway `selectRunApprover` will engage HITL for these.

### Rate-Limit Policy

- [ ] A rate-limit policy is set for this agent in Admin → Settings → Rate Limits before the agent goes live. No agent may operate without an explicit RPM/TPM ceiling.
- [ ] RPM and TPM ceilings are set to the agent's expected operational baseline, not to platform maximum.
- [ ] The policy has been verified in broker logs (`rate_limit_policy applied: agent_id=<id>`).

### FGA Grants Review

- [ ] FGA relations for `agent:<id>` reviewed: only the `can_invoke` relations required for the declared `allowed_tools` are present.
- [ ] No group-level wildcard grants that would allow this agent to invoke tools beyond its bundle.
- [ ] If the agent acts on behalf of users (delegated mode), the delegation group and scope attenuation have been reviewed against `policies/opa/envelope_send.rego`.

### Process Isolation

- [ ] `AIKONOS_GATEWAY_CHILD_KEYING=per-user` confirmed in the running gateway config. The default is `single` (all users share one child process — **not** a cross-user isolation boundary); production multi-user agents must set `per-user` so each (tenant, user, agent) gets its own forked child. Valid values are `single` and `per-user`.
- [ ] Child pool cap (`AIKONOS_GATEWAY_MAX_CHILDREN`, default 32) is appropriate for this agent's concurrency needs; documented if changed.
- [ ] Agent has a dedicated API key minted via Admin → Agents → Keys. The key is not shared with any other agent or service account. Key is stored in a secrets manager, not in source control or environment files.

### Persona (soul)

- [ ] The agent's persona/soul (Admin → Agents → Personality) has been reviewed: it scopes the agent to its stated purpose and contains no instructions that attempt to override platform policy, broker governance, or HITL gating. Persona text is advisory — it never grants capability; FGA and OPA remain authoritative.
- [ ] The persona does not embed secrets, credentials, or internal hostnames.
- [ ] Operator note: a persona edit takes effect on the next **idle** reuse of the agent's pooled gateway child (within ~10s), not mid-conversation. Restart `agent-gateway` to apply immediately (see `docs/OPS-RUNBOOK.md` → Service lifecycle).

### Smoke Test

- [ ] `task compose:verify` returns all checks green against the stack with the new agent configured.
- [ ] At least one test invocation of the agent has been run; the resulting audit events are visible in `/api/audit/stream` and contain the correct `agent_id`.
- [ ] HITL flow tested for any `destructive` or `external` write steps: approval appears in Inbox, task proceeds only after approval, denial halts execution.

---

## New MCP Server Checklist

**MCP server name / connector ID:** ___________________________  
**Reviewer:** ___________________________  
**Date:** ___________________________

### Container Isolation

- [ ] MCP server runs in its own container, not co-located with broker, gateway, or other MCP servers.
- [ ] Container applies the standard hardening anchor: `no-new-privileges: true`, `cap_drop: ALL`, `pids_limit: 512`, `mem_limit` set, `read_only: true`, non-root user. Verified in `compose.yaml` via `<<: *hardening`.
- [ ] Container is on the `mesh` network only; it does not have `backend` network access unless it directly dials a Aikonos datastore (documented with justification).

### SSRF / Private Host Access

- [ ] `AIKONOS_TOOLPROXY_MCP_ALLOW_PRIVATE=false` is set (or not set; the production default is `false`). This must never be `true` in production.
- [ ] MCP server does not bind to or require access to loopback, RFC1918, link-local, or cloud-metadata IP ranges.

### Authentication

- [ ] MCP server requires authentication on all endpoints: OAuth 2.0 bearer token, API key header, or mTLS client certificate. No unauthenticated HTTP endpoints exposed on the mesh network.
- [ ] Authentication method documented: ___________________________
- [ ] Credentials stored in Vault KV-v2 (`secret/data/connectors/<tenant>/<connector_id>/`). Not in environment variables or compose files.
- [ ] OAuth consent / credential linkage completed via Connections popover; token stored and refresh tested.

### Tool List Review

- [ ] All tools exposed by the MCP server have been reviewed in Admin → Access Control → Skills.
- [ ] Each tool has a declared `effect_class`. Tools with `destructive` or `write_external` effect class are explicitly justified.
- [ ] No tool exposes raw shell execution, arbitrary code evaluation, or filesystem access outside the workspace volume.
- [ ] Tool IDs follow the `mcp:<server_id>:<tool_name>` naming convention as registered in the broker's toolregistry.

### FGA Relations

- [ ] FGA `can_invoke` relations have been created for the MCP server's tools only for the principals that require them. No wildcard group grants unless explicitly reviewed and approved.
- [ ] `mcp:<server_id>` tool namespace is included in the `allowed_tools` list of the agent(s) that will use it. Agents not authorized to use this server cannot invoke its tools (FGA deny-by-default).

### Smoke Test

- [ ] `task compose:verify` returns all checks green with the MCP server connected.
- [ ] At least one tool call to the MCP server has been made via an authorized agent; the `InvokeTool` audit event is present with the correct `tool_id` and `connector_id`.
- [ ] Unauthorized agent (one without `can_invoke` for this server's tools) was tested: attempt was denied with FGA error, not allowed.
- [ ] SSRF guard tested if the MCP server calls external URLs: private-host requests are blocked (`AllowPrivateHosts=false` confirmed in toolproxy config).

---

## Decommission Notes

When an agent or MCP server is decommissioned:

- Disable via Admin console before deleting config — this stops new task submissions and child spawns gracefully.
- Revoke the API key (Admin → Agents → Keys → Revoke) before removing the agent record.
- Remove all FGA relations for the agent/connector ID via Admin → Access Control.
- Remove Vault secrets for the connector: `vault kv delete secret/connectors/<tenant>/<connector_id>/`.
- Retain the WORM audit records for the retention period (365 days); do not delete MinIO objects.

---

## Change log

### 2026-06-27 — Correct process-isolation facts; add persona review

**What changed:** Fixed the Process Isolation section to match the gateway config: child keying defaults to `single` (not `per-user`), the pool-cap env var is `AIKONOS_GATEWAY_MAX_CHILDREN` (default 32, not `AIKONOS_GATEWAY_CHILD_POOL_CAP`/8), and the only valid keying values are `single`/`per-user` (no `shared`). Added a Persona (soul) review subsection, including the operator note that persona edits propagate on the next idle child reuse.

**Why:** The stated defaults and env var name diverged from `agent-gateway/src/ipc/supervisor.ts`; agents now carry an author-provided persona that needs a governance review gate.

### 2026-06-19 — Initial version

First publication. Addresses Domain 10 (AI Governance) Foundation-blocking gap identified in `docs/13-zero-trust-audit.md`.
