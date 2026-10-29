# Agent identity and governance — improvement plan

> Date: 2026-10-28, against `main` at `e219bba`.
> Scope: how aikonOS identifies, bounds, delegates for and revokes AI agents,
> and how agents reach downstream systems. Facts were checked in code and docs
> on `main`; file references are to that commit.
> Purpose: a ranked list of improvements for later implementation. Items are
> numbered `AG-xx`, in the style of `docs/17-security-findings.md`. Note that
> the changelog in `docs/17-security-findings.md:318` is stale: C-01 and
> H-01..H-04 are merged (PR #40).

## 0. Summary

aikonOS governs agents well at the **tool-call layer**: every call passes task
binding, OPA, OpenFGA, a task-bound capability token, budgets and rate limits,
and risky calls wait for a human.

It is thinner at the **identity layer**, which decides who an agent is, who
owns it, whether it is active, and how far its authority reaches:

- **An agent is not bounded by its user.** For an agent-bound task the broker
  checks only the agent's grants. For MCP tools the user is never checked on
  any path. The target is *effective access = user ∩ agent ∩ tool*.
- **Most tasks are unbound.** Interactive and scheduled runs send a pseudo
  agent id that the broker coerces to NULL, so per-agent controls do not apply
  to them.
- **Agents have no lifecycle.** No staged/active/suspended state, no owner
  rule, no kill switch. `gateway_enabled` only gates the external REST path.
  The incident runbook tells admins to click "Admin → Agents → Disable", which
  has no code behind it.
- **Downstream calls drop the user.** Each MCP connection uses one static
  bearer shared by every user and agent.
- **External agents hold non-expiring API keys.** Revoke and `last_used_at`
  exist; expiry does not.
- **Nothing revokes early.** No user block, no revoke-all, no deprovisioning
  feed; tokens live until they expire.

### Tracking

| ID | Item | Priority | Effort | Related | Status |
|---|---|---|---|---|---|
| AG-01 | Bind every task to a registered agent | High | S–M | H-05 | open |
| AG-02 | Bound every agent by its user | High | M | H-05, AG-01 | open |
| AG-03 | Agent lifecycle and a working kill switch | High | M | runbook, AG-04 | open |
| AG-04 | Mandatory owners, owner backfill, orphan detection | Medium | S | AG-06 | open |
| AG-05 | Key-pair authentication for external agents | Medium | M | AG-15 | open |
| AG-06 | Revocation epoch and user block | High | S–M | M-02, AG-04 | open |
| AG-07 | Durable, asynchronous approval | High | M | H-06, M-09, M-28, L-25 | open |
| AG-08 | Per-agent tool ceilings, admin-set MCP effect classes | Medium | M | H-11 | open |
| AG-09 | Per-user OAuth to MCP servers | Medium | M–L | accepted risk in `SECURITY.md` | open |
| AG-10 | Identity-assertion grant (ID-JAG) client | Medium, conditional | L | ROADMAP "Delegation interop" | open |
| AG-11 | Delegation envelopes that carry their limits | Medium | S | M-17 | open |
| AG-12 | Delegation chain in every tool audit event | Low | S | H-07 | open |
| AG-13 | Step-up authentication for risky approvals | Low | S–M | — | open |
| AG-14 | Setup guide for further IdPs; agent inventory export | Low | S | ROADMAP "Agent inventory" | open |
| AG-15 | Mandatory expiry for `tk_` API keys | High | S | L-03 | open |
| AG-16 | SCIM 2.0 provisioning | Medium | M | M-02, AG-06 | open |
| AG-17 | Explicit on-behalf-of context (subject + actor) | High | M | AG-01, AG-02, AG-06, AG-11, AG-12; ADR-013 | open |

Effort: S = days, M = one to two weeks, L = longer.

Changes from the 2026-10-27 draft, one line each:

- AG-01 S → S–M: also needs a per-tenant default agent row, removal of the
  `AIKONOS_AGENT_FOR_USER` override and of the NULL coercion.
- AG-02 S–M → M: the FGA model has no direct user grant on connectors, so
  enforcement needs an audit-only rollout first.
- AG-04 High → Medium, S: owner reassignment UI and multi-owner model exist;
  what remains is a backfill, `owner_group` and an activation rule.
- AG-05 High → Medium: once AG-15 gives keys an expiry, a revocable,
  expiring, usage-tracked key is an acceptable baseline; key pairs are the
  next step, not an emergency.
- AG-06 split: epoch and user block stay High; SCIM moves to AG-16 (Medium).
- AG-15 new (High, S): the quick win split out of AG-05.
- AG-17 new (High, M): the design foundation under AG-01/AG-02/AG-09/AG-12;
  work package in `ag-17-on-behalf-of-spec.md`, decision in ADR-013.

### Phases

| Phase | Items | Why this order |
|---|---|---|
| 1 — quick wins, days each | **AG-15** (first PR), AG-07 slice 1 (M-09 expiry + `ListPendingApprovals`), AG-06, AG-04 backfill | Independent of each other; each closes a finding on its own |
| 2 — identity core | **AG-17 core** (slices S1–S3: task context, grant v2, token facts), then AG-01 with H-05, then AG-02 audit-only → enforce (with AG-17 S5), then AG-03, then AG-04 activation rule and orphan sweep; AG-17 S4 (scheduled delegation) lands with AG-06 | AG-17 defines what AG-01 binds and what AG-02 checks; AG-02 needs a bound task and a human owner; AG-03 activation needs owners; the orphan sweep needs the block flag from AG-06 |
| 3 — authority and audit | AG-05 (with AG-17 S6), AG-07 slice 2, AG-08, AG-11 and AG-12 (with AG-17 S7, after H-07), AG-17 S8 enforce last | Build on the identity from phase 2 |
| 4 — integration | AG-16, AG-09, AG-13, AG-14; AG-10 only when a customer asks | Need an IdP or a customer to drive them |

First PR: AG-15. It touches one table, two RPCs and one UI list, has no
dependency, and closes L-03.

## 1. Current state

| Area | Today | Gap |
|---|---|---|
| Agent identity | `agents` row plus FGA `agent:<id>`, admin CRUD (`broker/internal/broker/agents.go`). Interactive chat and scheduled runs send the pseudo-id `<localpart>-agent` (`agent-gateway/src/broker/agent-identity.ts:11`, `scheduler/ticker.ts:90`); the broker coerces any non-UUID `agent_id` to NULL (`gateway_tasks.go:164-173`). North `CreateTask` carries no agent. Only external REST runs use a real agent id, with owner `svc-<agentId>` (`external/rest.ts:123`) | Interactive and scheduled tasks are unbound; pseudo-ids are not registered agents |
| Operator override | `AIKONOS_AGENT_FOR_USER` maps users to real agent ids gateway-side (`agent-identity.ts:14-27`) | Binds a task to an agent with no `can_use` check (H-05 by another door) |
| Owners | FGA `owner_user` = the creator, written only at creation (`agents.go:256-262`); admin UI reassigns a single owner (`webui/web/src/views/admin/AgentsTab.vue:43-75`); `agents.created_by` column exists (`db/agents.go:49`) | Older agents may have no owner tuple; no `owner_group`; no owner rule, no orphan detection |
| Lifecycle | Create, update, delete. `gateway_enabled` (migration 022) gates only the external REST path (`rest.ts:25`). A `kill_switches` table with scopes `user | tenant | platform` exists since migration 001 (`001_initial_schema.sql:148-163`) and has no Go code reading it | No suspend. `docs/incident-response-runbook.md:47,187` describe a Disable action that does not exist (the "agents disabled" errors in `agents.go` mean the subsystem is unconfigured) |
| Agent credentials | Gateway↔broker: SPIFFE mTLS. External agents: static `tk_` keys; revoke (`api_keys.go:220-245`) and `last_used_at` (`db/agent_api_keys.go:150-158`) exist, expiry does not (L-03 is partly stale on `last_used_at`) | Non-expiring bearer secrets |
| Acting for a user | Owner bound from the verified token or the HMAC owner grant. Google Drive and OneDrive use the owner's own OAuth token, or Entra on-behalf-of (`broker/internal/connector/obo.go`) | MCP and web search use shared credentials |
| Effective authority | Agent-bound task: agent grants only (`service_plan.go:179-184`, `service_invoke.go:364-418`). `mcp:` tools skip the user-skill check on every task, bound or not (`service_plan.go:182`); the per-user skill object `skill:mcp:<conn>:<tool>` is not a valid FGA id | **Agent not bounded by user; user never checked for MCP tools** |
| Agent ceiling | Agent skills, MCP grant per whole connector, allowed LLM providers, capability-token attenuation | Per connector, not per tool. MCP effect class taken from the plan (H-11) |
| Downstream tokens | Vault holds per-user OAuth tokens for Google and Microsoft with refresh; tenant-wide keys for web search and MCP | No per-user MCP auth, no generic OAuth provider |
| MCP server auth | Static bearer or none (`broker/internal/toolproxy/mcp.go`) | No MCP OAuth |
| Cross-system delegation | aikonOS mints its own grants; ROADMAP lists "Delegation interop" under consideration | No standard protocol |
| Human approval | n-of-m approvals, four-eyes for step-up, separation of duty. Records live in Postgres `approval_requests` (`001_initial_schema.sql:167`, `db/approvals.go`, `approvalsvc/`); `expires_at` is stored but never enforced (M-09). The interactive wait is a promise in gateway memory (`agui/hitl.ts`). Scheduler pre-approves `web.fetch`, `doc.read`, `doc.write` even with `approvedTools: []` (L-25, `ticker.ts:45`); `unattendedAllowed` is read only in `routes/agui.ts` (H-06) | No pending-approvals RPC, no out-of-band channel, no expiry |
| Notifiers | Webhook and SMTP (`alerting/notifier.go`); SMTP never STARTTLS (L-08) | No Teams or Slack notifier |
| Revocation | Delete agent, revoke API key or role. Tokens live to expiry (`docs/user-lifecycle.md:73-75`) | No block flag, no revoke-all, no SCIM |
| Audit | Hash-chained, signed per-tenant log with decision records and replay | Stream readable by every member (H-07); no delegation chain per event |

## 2. Recommendations

### AG-01 Bind every task to a registered agent (High, S–M)

- **Why:** every tool call should be attributable to a registered agent, so
  per-agent controls (AG-02, AG-03, AG-08) apply to chat and scheduled runs
  as well.
- **Today:** north `CreateTask` has no agent field; the gateway's pseudo-ids
  become NULL (`gateway_tasks.go:164-173`); the invoke-time MCP check falls
  back to the agent id the caller claims (`service_invoke.go:377-382`); and
  `AIKONOS_AGENT_FOR_USER` binds a user to a real agent with no `can_use`
  check.
- **Change:** add `agent_id` to north `CreateTask`, check FGA `can_use`, store
  it on the task row. Create one default agent row per tenant for interactive
  and scheduled runs and have the gateway send its UUID. Replace the
  `AIKONOS_AGENT_FOR_USER` map with a per-user default-agent setting stored
  broker-side and checked with `can_use`. Then reject non-UUID ids in
  `CreateGatewayTask` instead of coercing them, and remove the caller-claim
  fallback in `InvokeTool`. Fix H-05 (`/agui` has no `can_use` check) in the
  same change.
- **Acceptance:**
  - North `CreateTask` with an `agent_id` the caller lacks `can_use` on
    returns `PermissionDenied`; a successful call stores `tasks.agent_id`.
  - `CreateGatewayTask` with a non-UUID `agent_id` returns
    `InvalidArgument`; no new task row has `agent_id IS NULL`.
  - The `callerAgent` fallback in `service_invoke.go:377-382` is gone; a
    caller-supplied `agent_id` that differs from the bound agent is refused.
  - `AIKONOS_AGENT_FOR_USER` is removed from config, docs and tests.

### AG-02 Bound every agent by its user (High, M; after AG-01)

- **Why:** an agent should never do more than the person it acts for. Then a
  hijacked or prompt-injected agent still cannot exceed that person's rights.
- **Today:** for agent-bound tasks `SubmitPlan` skips the user-skill check
  (`service_plan.go:179-184`). For `mcp:` tools it skips that check on every
  task, bound or not (`service_plan.go:182`), and `InvokeTool` checks the
  connector for the agent only (`service_invoke.go:364-418`). So a user is
  never checked against an MCP connector on any path, and a user who may use
  an agent inherits all of its skills and connectors. With H-05 open, that is
  every tenant member. Scheduled runs are not special here: they carry the
  real schedule owner (`ticker.ts:78,94`). Only external REST runs have a
  `svc-<agentId>` owner with no person behind it (`rest.ts:123`). Some test
  comments that say scheduled runs use `svc-` owners are stale.
- **Model constraint:** `mcp_connector#can_access` is
  `permitted_agent or permitted_group` (`policies/fga/model.fga:53-58`). A
  user passes only through a group that holds `permitted_group`; there is no
  direct user grant. Connectors granted to agents only would deny every user
  the day the check is enforced. The user-side check must target the
  `mcp_connector` object; the skill object `skill:mcp:<conn>:<tool>` is not a
  valid FGA id (comment at `service_plan.go:174-180`).
- **Change:** when the task owner is a human (not `svc-`), require both
  grants: run `userSkillDecision` for agent-bound tasks too, and add
  `CheckFGA(user:<owner>, can_access, mcp_connector:<id>)` at `SubmitPlan` and
  `InvokeTool` next to the agent check. Add `permitted_user: [user]` to
  `mcp_connector` so an admin can grant a single person without a group. Keep
  an explicit, audited per-agent `service_agent` flag for agents meant as a
  controlled door to a connector its users cannot reach directly; `svc-`
  owners imply it.
- **Rollout order:** (1) ship the user check in audit-only mode, emitting
  `mcp_user_access_would_deny` with user, agent and connector; (2) admins add
  `permitted_group` or `permitted_user` tuples from that audit, or set
  `service_agent`; (3) flip a per-tenant `enforce_user_bound` flag; (4) make
  enforcement the default and remove the flag after one release.
- **Acceptance:**
  - Agent-bound task, human owner, skill the owner lacks: the plan step is
    denied with the same reason an unbound task gets today.
  - Human owner without `can_access` on the connector: `InvokeTool` emits
    `mcp_user_access_would_deny` in audit mode and denies with
    `mcp_user_access_denied` in enforce mode; both covered by tests.
  - `svc-` owners and `service_agent` agents keep today's behaviour, and each
    such call carries the flag in its audit event.
  - `permitted_user` resolves `can_access` in the FGA model tests.

### AG-03 Agent lifecycle and a working kill switch (High, M)

- **Why:** suspending an agent must stop it everywhere at once, including
  tasks already running.
- **Today:** no state; deletion is the only stop. `gateway_enabled` only
  refuses external REST calls (`rest.ts:25`); interactive and scheduled runs
  ignore it. The runbook's Disable action is unbacked. The unused
  `kill_switches` table has no `agent` scope; use it for the user and tenant
  block in AG-06, or drop it. Agent state belongs on the `agents` row.
- **Change:** add `agents.status` = `staged | active | suspended | retired`.
  New agents start staged; activation needs an owner (AG-04). Enforce the
  state where every path converges: `CreateGatewayTask`, `SubmitPlan`,
  `ResolveAgentApiKey`, and per call in `InvokeTool`. The per-call check means
  a suspension stops in-flight tasks at their next tool call instead of
  waiting out the 30-minute capability token. The gateway supervisor already
  re-resolves and respawns children; have it evict them on a status change.
  Fold `gateway_enabled` into the status or keep it as a per-channel flag
  under it. Audit `agent.activated`, `agent.suspended` and `agent.retired`,
  wire the admin UI action and update the runbook.
- **Acceptance:**
  - Setting `suspended` makes the next `InvokeTool` on a running task of that
    agent return `PermissionDenied` with reason `agent_suspended`, without
    waiting for token expiry.
  - `CreateGatewayTask`, `SubmitPlan` and `ResolveAgentApiKey` refuse any
    status other than `active`.
  - A staged agent cannot be set `active` without at least one owner.
  - "Admin → Agents → Disable" in the runbook points at a real UI action that
    emits `agent.suspended`.

### AG-04 Mandatory owners, owner backfill, orphan detection (Medium, S)

- **Why:** every agent needs accountable people. With a single owner, the agent
  is orphaned when that person leaves.
- **Today:** the FGA type already allows several `owner_user` tuples; only
  `owner_group` is missing. The admin UI reassigns a single owner
  (`AgentsTab.vue:43-75`). The tuple is written only at creation
  (`agents.go:256-262`), so agents created before that change may have none.
- **Change:** backfill `owner_user` from `agents.created_by` for rows without
  a tuple (phase 1, one migration-style sweep). Add `owner_group` to
  `model.fga`. Let the UI add and remove owners instead of swapping one.
  Require at least one owner to activate (AG-03). In `ListAgents`, flag
  agents whose owners are all blocked (AG-06) and suspend them after a grace
  period. Audit owner changes.

### AG-05 Key-pair authentication for external agents (Medium, M; after AG-15)

- **Why:** a static bearer key works from anywhere. A private key never leaves
  the agent, and each signed assertion lives minutes.
- **Today:** external agents on port 8090 present a `tk_` key resolved to a
  `svc-<agentId>` principal. After AG-15 the key expires, can be revoked and
  records its last use; that is an acceptable baseline, which is why this
  item is Medium.
- **Change:** register one or more public keys per external agent. The agent
  sends a short-lived signed JWT client assertion (RFC 7523: `iss` = `sub` =
  agent id, `aud` = the gateway, `exp` at most five minutes ahead, `jti`
  checked against a replay cache). Keys carry a `kid`, a status and an expiry;
  rotation registers the new key before retiring the old. When an external
  agent acts for a specific person, such as an IDE agent calling aikonOS for
  its user, accept that person's IdP access token with the aikonOS audience
  instead of a shared key.

### AG-06 Revocation epoch and user block (High, S–M)

- **Why:** deprovisioning a user or agent should cut access at once rather than
  at token expiry.
- **Today:** no block flag and no revoke-all; both are planned in
  `docs/user-lifecycle.md:73-75`. The `kill_switches` table (scopes `user`,
  `tenant`, `platform`, with `active`, `reason`, `activated_by`) already
  models the block and is unread.
- **Change:** a per-principal revocation epoch (a "not before" timestamp) for
  users and agents; a block is an epoch of infinity. Store user and tenant
  blocks in `kill_switches`, agent epochs on the `agents` row. Check the epoch
  in the north interceptor, in gateway-grant verification, and in
  `capability.Authorize` (add an `issued_at` fact). Revoke-all bumps the
  epoch. Cache with a short TTL. Expose block and revoke-all in the admin UI
  and audit both.
- **Acceptance:**
  - Blocking a user makes the next north RPC with a still-valid IdP token
    fail within the cache TTL (at most 60 s).
  - Revoke-all on an agent makes `capability.Authorize` reject every token
    issued before the new epoch, with a test on the boundary.
  - A blocked user's HMAC owner grant is rejected at the gateway.
  - Block and unblock emit audit events with actor and reason.

### AG-07 Durable, asynchronous approval (High, M)

- **Why:** approvals should survive a closed browser, reach the right
  approvers out of band, and let unattended runs wait for a human instead of
  pre-approving tools. OpenID CIBA describes the pattern.
- **Today:** approvals are persisted with `approver_set`, `requires_n` and
  `expires_at`, but expiry is never enforced (M-09) and no RPC lists pending
  requests, so a second approver for an n≥2 gate cannot find the request. The
  interactive wait is a promise in gateway memory (`agui/hitl.ts`). The
  approve call carries no argument digest (M-28). Unattended runs pre-approve
  allowlists (L-25, H-06; see the table above).
- **Change, slice 1 (S, phase 1):** enforce `expires_at` in both queries and
  sweep expired rows to `EXPIRED` and the task to `TIMEOUT` (M-09). Add
  `ListPendingApprovals` filtered by `approver_set` and FGA `can_approve`.
  Bind each approval to an argument digest and have the server re-read the
  arguments by `toolCallId` (M-28, the deferred `args_hash` in ADR-006).
- **Change, slice 2 (M, phase 3):** the gateway resumes from broker state
  instead of memory. Notify approvers through the existing webhook and SMTP
  notifiers with a deep link; fix L-08 (STARTTLS) before SMTP carries
  approval links. A Teams or Slack card is a new notifier, not an existing
  one. Scheduled and external runs then pause for a human instead of
  pre-approving; drop the scheduler baseline (L-25) and enforce
  `unattendedAllowed` on every path (H-06).
- **Acceptance (slice 1):**
  - An approval past `expires_at` cannot be approved; the sweep sets it
    `EXPIRED` and the task `TIMEOUT`.
  - `ListPendingApprovals` returns only rows the caller may approve, verified
    by a test with two approvers and one outsider.
  - An approve call whose digest does not match the stored arguments is
    refused.
  - A gateway restart does not lose a pending approval; the second approver
    of a 2-of-n gate can complete it from another session.

### AG-08 Per-agent tool ceilings and admin-set MCP effect classes (Medium, M)

- **Why:** granting a whole connector gives an agent tools nobody reviewed.
  Least privilege starts with an explicit tool list.
- **Today:** an agent is granted a whole MCP connector. An MCP tool's effect
  class comes from the plan, which the gateway derives from the tool's name and
  `readOnlyHint` (H-11).
- **Change:** when an admin attaches a connector to an agent, they choose its
  tools (default none, with a read-only preset). Each MCP tool gets an
  admin-confirmed effect class in the tenant tool registry when the connection
  is added, and the broker floors the plan's class with it, as it already does
  for built-in tools (`service_plan.go:163-167`). This fixes H-11 at the root.

### AG-09 Per-user OAuth to MCP servers (Medium, M–L)

- **Why:** the MCP server should see the user, so it can scope data per user
  and log who acted.
- **Today:** one static bearer per MCP connection, shared by all users and
  agents.
- **Change:** add an `oauth` auth type for MCP connections that follows the MCP
  authorization spec: discover the server's protected-resource metadata
  (RFC 9728), run authorization code with PKCE per user, bind tokens to the
  server with resource indicators (RFC 8707), and keep them in Vault per user.
  Reuse the connector flow, state store and refresh write-back in
  `broker/internal/connector/oauth.go` and `token.go`. Publish an aikonOS
  client metadata document (CIMD) so servers that support it need no
  per-server registration. Keep the static bearer as an explicitly labelled
  service mode.

### AG-10 Identity-assertion grant (ID-JAG) client (Medium, L; only when a customer needs it)

- **Why:** when an enterprise IdP mediates access between applications,
  aikonOS can reach other SaaS as the user under the IdP's central policy,
  without per-app consent or shared keys.
- **Worth it when** a customer's IdP issues ID-JAGs and the target resources
  accept them; Slack and Atlassian document resource-side support. I found no
  evidence that Microsoft Entra, aikonOS's main production IdP, issues them.
  Do not start this without a named customer and a verified IdP.
- **Change:** a connector auth type next to Entra on-behalf-of. The broker
  keeps the user's IdP refresh token server-side, as it already does for OBO,
  performs leg 1 (RFC 8693 token exchange at the IdP for an ID-JAG naming the
  resource) and leg 2 (RFC 7523 JWT-bearer grant at the resource's
  authorization server), and authenticates as aikonOS with a private-key
  client assertion. Resource tokens stay inside the broker; the agent never
  holds them. This implements the ROADMAP's "Delegation interop" item.

### AG-11 Delegation envelopes that carry their limits (Medium, S)

- **Why:** a delegation should carry its limits; otherwise the sender's
  narrowing is only advisory.
- **Today:** an accepted envelope spawns a plain task with only the intent and
  cost budget (`service_envelopes.go:470-491`). The attenuated scopes are
  dropped, so the recipient works with their own full authority. It is not an
  escalation, since the recipient only uses their own rights.
- **Change:** store the envelope id and attenuated scopes on the spawned task,
  intersect them with the recipient's authority at `SubmitPlan`, and name the
  sender and envelope in each tool event. Add the missing `envelope_send`
  policy tests (M-17).

### AG-12 Delegation chain in every tool audit event (Low, S; after H-07)

- **Why:** an auditor should see the user, the agent and any delegation behind
  every tool call, from one event.
- **Change:** fix H-07 first; the audit stream must be admin-only before it
  carries more identity data. Then add an actor chain to each tool event:
  user, agent, envelope sender if any, external client id, and the
  `service_agent` flag from AG-02. Add agent lifecycle and credential events
  from AG-03, AG-05 and AG-15.

### AG-13 Step-up authentication for risky approvals (Low, S–M)

- **Why:** approving a destructive or credential-touching step deserves a
  fresh, strong sign-in, not only a second person.
- **Today:** OPA's `require_step_up` means "two approvers". The approver's own
  sign-in strength is never checked; `acr`, `amr` and `auth_time` are not read
  anywhere.
- **Change:** for step-up approvals, require a recent MFA on the approver's
  token: `amr` includes MFA or a configured `acr`, and `auth_time` falls within
  N minutes. The web UI re-authenticates with `acr_values` (Keycloak and other
  OIDC IdPs) or an Entra conditional-access authentication-context claims
  challenge.

### AG-14 Setup guide for further IdPs; agent inventory export (Low, S)

- **Further IdPs:** the broker accepts any OIDC IdP but needs a tenant claim
  holding a UUID (`oidc.tenant_claim`, default `tenant_id`). IdPs without such
  a claim need a custom claim and an authorization server that issues the
  aikonOS audience. Write and test setup steps next to `docs/12-entra-login.md`
  for the next IdP a customer brings.
- **Inventory export:** identity-governance tools increasingly keep registries
  of AI agents. An agent inventory endpoint (id, owners, status, connectors),
  available once AG-03 and AG-04 land, would let them import aikonOS agents.
  This advances the ROADMAP's "Agent inventory and lifecycle" item.

### AG-15 Mandatory expiry for `tk_` API keys (High, S; first PR)

- **Why:** a key that never expires is a standing credential. Expiry turns a
  leaked or forgotten key into a bounded problem and forces rotation.
- **Today:** `CreateAgentApiKey` sets no expiry; `agent_api_keys` has no
  expiry column (`db/agent_api_keys.go`). Revoke works (`api_keys.go:220-245`)
  and `last_used_at` is updated on resolve (`db/agent_api_keys.go:150-158`),
  so L-03 is half done.
- **Change:** add `expires_at NOT NULL` with a tenant maximum (default 90
  days). `CreateAgentApiKey` requires an expiry at or below the maximum.
  `ResolveAgentApiKey` rejects expired keys. Backfill existing keys to
  `created_at + maximum`, and warn admins in the key list before they expire.
  Show expiry and `last_used_at` in the UI.
- **Acceptance:**
  - `CreateAgentApiKey` without an expiry, or above the tenant maximum,
    returns `InvalidArgument`.
  - `ResolveAgentApiKey` on an expired key returns the same error as a
    revoked key, and the REST path returns 401.
  - After the migration, no `agent_api_keys` row has `expires_at IS NULL`.
  - The key list shows `expires_at` and `last_used_at`.

### AG-16 SCIM 2.0 provisioning (Medium, M; after AG-06)

- **Why:** the identity directory should drive deprovisioning and group
  membership, so a leaver is blocked without an admin noticing.
- **Today:** provisioning is just-in-time from the OIDC token, with groups
  granted from an unverified email claim (M-02).
- **Change:** a minimal SCIM 2.0 endpoint for Users (`active`) and Groups
  (membership written as FGA group tuples). `active=false` sets the AG-06
  block, which feeds the AG-04 orphan sweep. For tenants on SCIM this replaces
  email-rule provisioning and removes M-02. Entra ID supports SCIM
  provisioning; test against it first.

### AG-17 Explicit on-behalf-of context (High, M; phase 2 first)

- **Why:** every other identity item asks "who is this call for, and who is
  making it?" Today the answer is implicit: the owner string on the task row,
  a nullable agent id, and a `svc-` prefix. Making subject and actor explicit
  on every task, grant and token gives AG-01, AG-02, AG-06, AG-09, AG-11 and
  AG-12 one structure to read, and keeps the broker as the only holder of the
  user's identity: the Pi child never sees the user's token or any downstream
  token.
- **Today:** the HMAC owner grant names tenant, owner and expiry only
  (`gatewaygrant.go:10`); scheduled runs re-mint it at every claim with no
  standing, visible or revocable delegation behind it (`scheduler.go:548`).
  Capability tokens carry `task` and `right` but no party (`capability.go:
  124-146`). Audit events carry `actor_user_id` and no agent. External REST
  runs are `svc-<agentId>` with no way to act for a person (`rest.ts:123`).
  Entra OBO already mints per-user downstream tokens (`connector/obo.go:245`)
  but nothing states which runs may use them.
- **Change:** a broker-side `OnBehalfOf{Subject, Actor, ServiceRun,
  DelegatedBy, EnvelopeID, ExternalClientID, GrantID}` derived from the task
  row (RFC 8693 `act` shape, one hop in v1). Owner grant v2 carries subject,
  actor, kind and grant id; `CreateGatewayTask` refuses an `agent_id` that
  differs from the grant's actor. Capability tokens gain `subject` and
  `actor` facts checked in `Authorize`. Scheduled runs become explicit
  delegations: `actor_agent_id`, `delegation_expires_at`,
  `delegation_revoked_at` on the schedule row, re-checked at each claim
  against the subject's block (AG-06) and the actor's status (AG-03).
  External agents may add the person's own IdP token (aikonOS audience) to
  their key to act with a subject; key-only runs need the AG-02
  `service_agent` flag. Every tool, plan and scheduler audit event carries
  `actor_agent_id` and `context.obo`. Downstream: OBO and (later) AG-09 and
  AG-10 mint per subject; MCP static bearer and web search log the chain.
  Consumers: AG-01 fills the actor; AG-02 reads subject and `service_agent`;
  AG-05 supplies the user-assertion path; AG-06 checks subject and actor at
  claim and in `Authorize`; AG-09/AG-10 key tokens by subject and present the
  actor as client; AG-11 writes the envelope id; AG-12 types the chain.
  Rollout audit-only first, tenant flag `identity.obo = audit | enforce`, v1
  grants accepted for one release.
- **Acceptance:**
  - `CreateGatewayTask` with an `agent_id` that differs from the grant's
    actor returns `PermissionDenied`; a v1 grant is refused once
    `gateway_grant.accept_v1=false`.
  - A capability token minted for subject X and task T fails `Authorize` when
    T's row names subject Y, in enforce mode; audit-only emits
    `obo_context_mismatch`.
  - A revoked, expired or subject-blocked scheduled delegation is skipped at
    claim with an audit event; the schedule list shows actor, expiry and
    revocation.
  - A REST run with a valid user assertion creates a task with
    `Subject=user`, `Actor=agent`; a key-only run on a non-`service_agent`
    agent is refused.
  - Every `tool.invoked` and `tool.denied` event carries `actor_agent_id` and
    `context.obo`; old audit chains still verify.
- **Spec:** `docs/research/ag-17-on-behalf-of-spec.md` (slices S1–S8, test
  plan); decision record ADR-013.

## 3. Strengths to keep

- Per-call decisions with OPA and OpenFGA, effect classes, network ACL and DLP
  rules.
- Human approval with n-of-m thresholds and separation of duty, already
  persisted in Postgres.
- Task-bound, 30-minute, attenuable capability tokens.
- Tamper-evident, signed, per-tenant audit with decision replay.
- Budgets, rate limits, spend caps and per-agent behavioural baselines.
- SPIFFE mTLS between gateway and broker.
- Self-hosting: identity, policy and audit data stay with the customer.

## 4. Deliberately not planned

- **OAuth scopes as the internal authorization model.** Coarser than per-call
  OPA and OpenFGA. Use scopes only at the boundary with external resources
  (AG-09, AG-10).
- **Long-lived cached resource tokens** for downstream systems.
- **Open dynamic client registration for agents.** Admin-issued registration is
  safer. If self-registration is ever needed, prefer CIMD with an allowlist.
- **aikonOS as an ID-JAG issuer.** That is the IdP's job; aikonOS should
  consume ID-JAGs, not mint them.
- **Inbound ID-JAG acceptance.** Dropped from AG-10 until an external agent
  with an ID-JAG-issuing IdP exists.
- **Extending `kill_switches` to agents.** Agent state lives on the `agents`
  row (AG-03); the table serves user and tenant blocks only (AG-06).

## 5. References

- [IETF draft-ietf-oauth-identity-assertion-authz-grant-04](https://datatracker.ietf.org/doc/html/draft-ietf-oauth-identity-assertion-authz-grant-04)
- [RFC 8693: OAuth 2.0 Token Exchange](https://www.rfc-editor.org/rfc/rfc8693)
- [RFC 7523: JWT Profile for OAuth 2.0 Client Authentication and Authorization Grants](https://www.rfc-editor.org/rfc/rfc7523)
- [RFC 9728: OAuth 2.0 Protected Resource Metadata](https://www.rfc-editor.org/rfc/rfc9728)
- [RFC 8707: Resource Indicators for OAuth 2.0](https://www.rfc-editor.org/rfc/rfc8707)
- [RFC 7644: SCIM 2.0 Protocol](https://www.rfc-editor.org/rfc/rfc7644)
- [OpenID Connect Client-Initiated Backchannel Authentication (CIBA)](https://openid.net/specs/openid-client-initiated-backchannel-authentication-core-1_0.html)
- [Slack: enterprise-managed authorization](https://docs.slack.dev/authentication/enterprise-managed-authorization)
- [Atlassian: enterprise-managed authorization for Rovo MCP](https://www.atlassian.com/blog/development/enterprise-managed-authorization-rovo-mcp-xaa-id-jag)
