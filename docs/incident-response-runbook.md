# Aikonos Incident Response Runbook

**Version:** 1.0  
**Status:** Active  
**Owner:** Security Team  
**Effective:** 2026-06-19

---

## Severity Tiers

| Tier | Description | SLA (detect → contain) | Examples |
|------|-------------|------------------------|---------|
| **S1** | Active exfiltration, credential compromise, audit chain break | 30 min | Prompt injection exfil confirmed, capability key stolen, audit HMAC mismatch |
| **S2** | Suspected exfil, MCP server compromise, runaway cost spike | 2 hours | Unusual `write_external` pattern, MCP tool abuse, billing spike >10× baseline |
| **S3** | Policy violation without confirmed impact, single user locked out | 8 hours | AUP violation, FGA misconfiguration, rate limit bypass attempt |
| **S4** | Anomaly under investigation, no active harm | 24 hours | Unexpected tool call pattern, single audit warning |

All S1 and S2 incidents: export the WORM audit chain within 1 hour of detection. See [Escalation](#escalation).

---

## Scenario 1 — Prompt Injection / Exfiltration Attempt

**Threat IDs:** L-02, I-02 (threat model `docs/04-threat-model.md`)

### Detect

Query the WORM audit via the read-side API (tenant-admin bearer required):

```
GET /admin/audit/query?event_type=InvokeTool&decision=2&limit=100
```

`decision=2` filters for DENY decisions. There is no server-side filter for `reads_sensitive` or `write_external` — query by `event_type` and/or `actor`, then narrow client-side by inspecting the `tool_id` and argument fields in the returned events. Use `cursor` from `nextCursor` to page.

Or via the live stream (`/api/audit/stream` SSE) — filter for events where `tool_id` includes a doc/web read step followed by `email.draft` (send), `web.fetch` (POST), or any connector write in the same `task_id`.

Indicators:
- `plan_validation.rego` violation logged but plan was resubmitted with altered step ordering.
- `write_external` step in the same task as a `reads_sensitive` workspace read.
- Unusual data volume in MinIO `workspace-data` writes.

### Isolate

1. Identify the `agent_id` or `user_sub` from the audit event.
2. Disable the agent: Admin → Agents → (agent) → Disable. This stops new child spawns; in-flight tasks drain naturally (the child pool evicts on next request).
3. If a personal user session: revoke all FGA tuples for the user via Admin → Access Control → Users → Remove all grants.
4. Block further task submission: set a rate-limit policy of 0 RPM/TPM for the subject via Admin → Settings → Rate Limits.

### Gather Evidence

Export audit events for the affected actor and time window:

```
GET /admin/audit/query?actor=<user_sub>&start=<ISO8601>&end=<ISO8601>&event_type=InvokeTool&limit=200
```

`task_id` filtering is not available server-side; filter the returned `events` array client-side by matching the `task_id` field. To verify chain integrity for the export window, call:

```
GET /admin/audit/verify
```

Download the MinIO object-lock objects for offline forensics:

```bash
mc cp --recursive minio/aikonos-audit/<tenant>/<date>/ ./evidence/
```

Record: task IDs, user subject, tool invocations, timestamps, plan steps, HITL decisions.

### Recover

1. Review all FGA grants for the affected user/agent and remove any over-permissioned relations.
2. If the capability root key may have been observed (e.g., by a compromised broker process):
   - Rotate the key: delete from Vault (`secret/data/broker/capability-root-key`) + restart broker; `main.go` will generate and persist a new key via `cas=0`.
   - All in-flight Biscuit tokens minted with the old key become invalid immediately.
3. Review the `allowed_tools` list in the implicated skill bundle; re-upload with reduced scope if warranted.
4. File a follow-up security review request with the security team.

---

## Scenario 2 — Capability Key Compromise

**Threat IDs:** E-08, T-04

### Detect

- Broker log lines: `capability key: minting token for tool=<X>` for tools not in any known agent's `allowed_tools`.
- `InvokeTool` audit events for tool IDs that no active task plan contains.
- Vault audit log shows unexpected reads of `secret/data/broker/capability-root-key`.

### Isolate

1. Stop the broker immediately:
   ```bash
   docker compose stop broker
   ```
2. Revoke the Vault AppRole secret ID:
   ```bash
   # From vault container or CLI with admin token
   vault write auth/approle/role/broker/secret-id-accessor/destroy accessor=<accessor>
   ```
3. No new Biscuit tokens can be minted while the broker is down. In-flight tokens expire on their TTL (check `AIKONOS_CAPABILITY_TOKEN_TTL`, default 5 min).

### Gather Evidence

- Extract broker logs from the Docker log driver:
  ```bash
  docker compose logs --no-log-prefix broker > evidence/broker.log
  ```
- Export all `InvokeTool` audit events for the 24-hour window before detection.
- Record: which tools were invoked, which tenants, which task IDs, timestamps.

### Recover

1. Re-seed Vault (AppRole + new capability root key):
   ```bash
   bash scripts/compose-vault-seed.sh
   ```
2. Restart broker:
   ```bash
   docker compose up -d broker
   ```
3. The broker generates a new capability root key on startup (`cas=0` write); all previously minted Biscuit tokens are now invalid.
4. Monitor audit stream for 30 minutes post-restart for unexpected `InvokeTool` events.
5. Escalate to S1 if key compromise is confirmed; export WORM chain immediately.

---

## Scenario 3 — Audit Chain Break

**Threat IDs:** T-02, R-01, R-02

### Detect

- `VerifyAuditChain` RPC returns a broken-chain error.
- Grafana alert on `aikonos_audit_chain_break_total` counter (if wired).
- MinIO object-lock object unexpectedly missing or modified (requires object-lock bypass — indicates storage-level tampering).

Run the chain verifier manually:

```bash
# gRPC call to broker's audit verification endpoint
grpcurl -plaintext localhost:9090 aikonos.broker.v1.AuditService/VerifyAuditChain
```

Or query the reader directly — `broker/internal/audit/reader.go` loads the chain and checks each HMAC against the prior event's hash.

### Investigate

1. Identify the first broken hash: the reader returns the event ID where `prev_hash` does not match the previous event's `SHA-256(event_bytes)`.
2. Check MinIO object metadata for the objects around that event ID — look for missing `x-amz-meta-chain-hash` or a modified object without a new object-lock version.
3. Determine whether the break is:
   - A software bug (hash algorithm mismatch after a broker upgrade) — check git log for audit changes.
   - A storage-level deletion or modification — check MinIO access logs and object-lock governance records.

### Recover

- **New events continue unaffected.** The broker's `loadChainHeads` seeds from the latest intact chain head on restart; new events form a valid sub-chain from that point.
- Broken chain segments must be treated as potentially compromised for forensic purposes — do not discard.
- Escalate to forensics: export the full MinIO bucket for the affected tenant + date range.
- If tampering is confirmed: treat as S1, notify affected tenant(s), preserve all evidence, do not overwrite.

---

## Scenario 4 — Rate Limit Bypass / Cost Spike

**Threat IDs:** D-01, D-02

### Detect

- Grafana dashboard: `aikonos_rate_limit_exceeded_total` counter spiking, or LLM billing spike >10× 7-day baseline.
- Broker log: `CheckRateLimit: transport error — failing open` repeated for the same subject — indicates the fail-open condition is being exploited (Domain 4 known gap).
- NATS message queue depth alert.

### Isolate

1. Identify the subject (user sub or agent ID) from broker logs or Grafana.
2. Delete the rate-limit policy for that subject to force a re-evaluation, then set a restrictive policy:
   - Admin → Settings → Rate Limits → Delete existing policy → Add policy with `rpm=0, tpm=0` for the subject.
3. If the fail-open condition is the cause (broker gRPC unavailable): restart broker to restore connectivity.
   ```bash
   docker compose restart broker
   ```
4. If a runaway agent loop: disable the agent (Admin → Agents → Disable) to stop new child spawns.

### Gather Evidence

- Export broker logs for the spike window.
- Export `InvokeTool` and `SubmitPlan` audit events for the affected subject over the spike window.
- Record: total token consumption, tool call sequence, task_id chain depth (check for circular `parent_task_id` chains indicating a loop).

### Recover

1. Review agent plan depth limits and loop detection configuration.
2. Set appropriate RPM/TPM ceilings for the subject via Admin → Settings → Rate Limits.
3. If loop detection failed: file a bug; add a `parent_task_id` chain depth check to the OPA `plan_validation.rego` if not already present.
4. Revoke the agent API key if the agent is deemed malicious: Admin → Agents → Keys → Revoke.

---

## Scenario 5 — MCP Server Compromise

**Threat IDs:** S-06, I-06, T-03

### Detect

- `InvokeTool` audit events for `mcp:*` tool IDs with unexpected argument shapes or destination tenants.
- MCP server returning tool results that trigger the plan-level exfil guard (`reads_sensitive + write_external`).
- Connector OAuth token used outside normal hours or from unexpected IP (check connector provider's audit log).

### Isolate

1. Remove the MCP server connection: Admin → Connections → (server) → Disconnect.
   - This removes the MCP server from the broker's live toolregistry; new plans cannot include its tools.
   - In-flight tasks using those tools: the broker will fail `InvokeTool` with "tool not found" after toolregistry reload.
2. Revoke the connector's OAuth token immediately via the provider's developer console (Google, Microsoft, etc.).
3. Revoke the Vault-stored connector token:
   ```bash
   # vault path: secret/data/connectors/<tenant>/<connector_id>/token
   vault kv delete secret/connectors/<tenant>/<connector_id>/token
   ```

### Gather Evidence

- Export all `InvokeTool` audit events for `mcp:*` tool IDs over the past 7 days.
- Pull MCP server access logs from the container:
  ```bash
  docker compose logs --no-log-prefix <mcp-service>
  ```
- Record: which tool IDs were called, arguments, results (if logged), task IDs.

### Recover

1. Rotate the connector OAuth credentials: re-link the connector via the Connections popover, which triggers a new OAuth consent flow and stores a fresh token in Vault.
2. Audit the tool list in the MCP server's toolregistry entry; remove any tools not explicitly needed.
3. Review FGA relations for the `mcp:<server_id>` skill namespace; remove over-permissioned grants.
4. Before re-enabling: verify the MCP server requires authentication (`auth_required: true`); do not reconnect unauthenticated endpoints in production.

---

## Escalation

| Condition | Action |
|-----------|--------|
| Any S1 incident | Notify security team lead within 15 min; export WORM audit chain within 1 hour; preserve all evidence before any recovery steps |
| Any S2 incident | Notify security team within 30 min; export WORM audit chain within 1 hour |
| Confirmed data exfiltration | Notify legal and DPO; initiate breach assessment per applicable regulation (GDPR 72-hour notification, HIPAA 60-day) |
| Audit chain break (suspected tampering) | Treat as S1 regardless of other indicators; escalate to forensics |
| Capability key compromise confirmed | Treat as S1; key rotation invalidates all in-flight tasks — coordinate with affected users before rotation in production |

**WORM audit chain export:**

```bash
mc cp --recursive minio/aikonos-audit/<tenant>/ ./evidence/audit-export-$(date +%Y%m%d-%H%M%S)/
# Verify object-lock retention on each object before export
mc stat minio/aikonos-audit/<tenant>/<object>
```

---

## Change log

### 2026-06-19 — Initial version

First publication. Addresses Domain 10 (AI Governance) Foundation-blocking gap identified in `docs/13-zero-trust-audit.md`. Five scenarios derived from top risks in `docs/04-threat-model.md`.
