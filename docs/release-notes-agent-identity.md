# Release notes: agent identity (AG-01 to AG-17)

This release makes every task name a human subject and a registered agent
(on-behalf-of, AG-17) and tightens keys, approvals and schedules around that.
GitHub release notes are generated from commits; this page is the upgrade
guide that goes with them. Read it before you upgrade a running deployment.

## Upgrade order

1. **Upgrade every audit-chain verifier first.** Audit events now carry a new
   top-level field, `actor_agent_id`, which is part of the canonical bytes that
   the chain hash and the HMAC signature cover. A broker or tool built before
   this release decodes such an event without the field, so it computes a
   different hash. It reports a chain break and a signature failure for every
   new event, although the trail is intact. Old events verify unchanged on both
   versions.
   - Upgrade all broker replicas together. Do not run a mixed fleet in which an
     old replica serves `VerifyAuditChain` while a new one writes events.
   - Rebuild any offline or SIEM-side verifier that embeds
     `broker/internal/audit` before the first new broker writes.
   - Do not roll a broker back past this release once it has written events.
     The older version reports the new events as tampered.
2. **Apply the database migrations** (`048` onwards) with the normal migrate
   job, then start the new broker, then the agent-gateway, webui and desktop
   client. The gateway, webui and desktop client must all be from this release
   (see the approvals section below).
3. **Keep `identity.obo = audit`** (the default) until the audit trail shows
   no tool denials with reason `obo_context_mismatch` and no
   `aikonos.scheduler.run.skipped.actor_missing` events. Then switch the tenant
   to `enforce` in Admin, Settings, Identity. Broker config
   `gateway_grant.accept_v1` stays `true` for this release so older gateways'
   v1 grants still work in audit mode. Enforce refuses them.

## Agent API keys now expire (AG-15)

Every `tk_` key has a mandatory expiry (tenant cap `api_keys.max_ttl_days`,
default and ceiling 90). Migration `052` backfills existing keys to
`created_at + 90 days`, but never sooner than 14 days after the migration ran.
That gives every key at least a 14-day grace to rotate. An expired key gets the
same response as a revoked one (REST 401). Before the grace ends, check the
admin key list (it warns at 7 days left) and mint replacement keys for
external integrations.

## Approvals require the argument digest (AG-07)

`SubmitPlan` returns `args_digest`. `ApproveTask`, `ApproveGatewayTask` and the
gateway's `POST /approve/:id` must echo the digest. A missing or different
digest is refused (`FailedPrecondition`, HTTP 409) and the approval keeps
waiting. A denial needs no digest. As a result:

- A gateway, webui or desktop client from before this release cannot approve
  anything. Upgrade them in the same window as the broker.
- An approval modal that was open across the upgrade has no digest. Reload it,
  or approve from Inbox, which reads the digest from the broker.
- External clients that call the approve RPC directly must send the digest
  they received from the approval request.

## Approvals resume from the broker (AG-07 slice 2)

- A waiting tool call polls the broker (`GetGatewayApproval`, south) every
  2 s, so a decision made anywhere ends the wait: the chat dialog, the Inbox,
  another session. The dialog closes on `aikonos.approval.resolved`.
- Separation of duty: on an n >= 2 gate the requester cannot vote, even when
  the approver set is empty. The chat shows "Waiting for N more approvers"
  with only Withdraw; the others decide from their Inbox.
- Scheduled and external runs no longer deny an unlisted gated call; they
  wait for a human until the run's timeout (`AIKONOS_SCHEDULER_RUN_TIMEOUT_MS`,
  180 s for external calls). The scheduler baseline (web.fetch, doc.read,
  doc.write) is gone: add those tools to a schedule's approved tools if it
  needs them unattended.
- The org switch "Allow unattended (auto-approve) mode" now covers scheduled,
  external and subagent runs, and an unreadable setting counts as off.
- New approval requests are sent through the alerting webhook and SMTP sinks
  with a link to `<alerting.inbox_url>?approval=<task id>`
  (`AIKONOS_ALERTING_INBOX_URL`; unset = no approval notifications). SMTP
  mails the approvers whose email the user directory knows. SMTP now requires
  STARTTLS unless the relay is on loopback.
- Not yet: a run that was waiting when the gateway restarted is gone; its
  approval stays in the Inbox until it expires, and approving it runs nothing.

## Scheduled runs

Each schedule is a standing delegation from its owner to an agent. The
delegation expires 90 days after creation; migration `049` gave existing rows
90 days from the time it ran. When it expires, the schedule is parked and the
owner sees "Delegation expired" in Schedules. The owner can extend it by
another 90 days (the extend button; audited as
`aikonos.scheduler.run.delegation_extended`) and then resume it. Admins can
revoke a delegation but cannot extend one in another user's name.

Under `identity.obo = enforce`, a schedule with no actor agent is parked at its
next claim, and resuming it is refused with `FailedPrecondition`. The owner sets
an agent ("Acts as") in the edit form first, then resumes.

## Per-agent MCP tools and confirmed effect classes (AG-08)

- Attaching an MCP connector to an agent no longer grants its tools. Each
  agent lists the tools it may call (`agents.mcp_tools`, migration 058).
  Existing agents start with none: after the upgrade, open each agent with
  connectors under Admin → Agents and tick its tools (or *Read-only preset*).
  Until then its MCP plan steps are denied ("not in agent ...'s allowed MCP
  tools"); InvokeTool denies with `mcp_tool_not_allowed`. This applies to the
  tenant default agent too.
- An MCP tool routes as `destructive` (approval plus step-up) until an admin
  confirms its class under Admin → MCP → *Tools* → *Confirm*. Confirm the
  read-only tools first, or the read-only preset selects nothing. Existing
  MCP rows in Admin → Skills keep their stored class; a class confirmed
  there is no longer raised to `write_external`.

## Per-user OAuth to MCP servers (AG-09)

- New MCP auth type `oauth`: the MCP server sees the user. When an admin saves
  the connection the broker discovers the server's authorization server (RFC
  9728 protected-resource metadata, then RFC 8414 / OIDC metadata) and refuses
  to save one that lacks PKCE S256 or names a different resource. The client is
  the admin's pre-registered client id (secret optional, in Vault), else the
  aikonOS client metadata document when the server supports it, else dynamic
  registration (RFC 7591).
- Users link their account under Connections → *MCP servers* → *Connect*
  (authorization code + PKCE, resource indicator RFC 8707 on every authorize
  and token request). Tokens are per user in Vault and refreshed with
  write-back, like the Drive connectors; *Disconnect* deletes them.
- Tool calls carry the task subject's token. Service runs and users who have
  not linked the server are refused; session tool discovery skips the server
  for them.
- The existing `bearer` type is unchanged and labelled *service (shared
  bearer)* in the admin form.
- Migration `061` adds the oauth columns to `mcp_connections` and allows
  `auth_type = 'oauth'`.
- Config: oauth needs Vault and `connectors.redirect_uri`. Optional
  `connectors.mcp_client_metadata_url` (broker) =
  `<AIKONOS_EXTERNAL_BASE_URL>/oauth/client-metadata.json`; the gateway serves
  that document on the external surface when `AIKONOS_EXTERNAL_BASE_URL` and
  `AIKONOS_CONNECTORS_REDIRECT_URI` are set.
- Gateway and broker must both be from this release (new
  `BeginConnectorAuthRequest.mcp_connection_id`).

## SCIM 2.0 provisioning (AG-16)

- New, opt-in: a SCIM 2.0 endpoint on the external surface (`/scim/v2`, port
  8090) for Users and Groups. Entra deactivation blocks the user; Entra group
  membership becomes FGA group tuples. Setup: `docs/18-scim-provisioning.md`.
- Migration `060` adds `scim_tokens`, `scim_users` and `scim_groups`.
- New tenant setting `provisioning.mode` = `jit` (default, unchanged behaviour)
  or `scim`. Under `scim` email rules grant nothing and new rules are refused;
  under `jit` the SCIM endpoint answers 403. Nothing changes until an admin
  switches.
- New gateway env `AIKONOS_EXTERNAL_BASE_URL` (optional): the public URL of
  the external surface, shown as the SCIM tenant URL in the admin page.
- The gateway and broker must both be from this release: the gateway forwards
  SCIM calls over the new south RPC `ScimCall`.
