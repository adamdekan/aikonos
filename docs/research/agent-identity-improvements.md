# Agent identity and governance — improvement plan

> Date: 2026-10-27, against `main` at `2b76b1d`.
> Scope: how aikonOS identifies, bounds, delegates for and revokes AI agents,
> and how agents reach downstream systems. Facts were checked in code and docs
> on `main`; file references are to that commit.
> Purpose: a ranked list of improvements for later implementation. Items are
> numbered `AG-xx`, in the style of `docs/17-security-findings.md`.

## 0. Summary

aikonOS governs agents well at the **tool-call layer**: every call passes task
binding, OPA, OpenFGA, a task-bound capability token, budgets and rate limits,
and risky calls wait for a human.

It is thinner at the **identity layer**, which decides who an agent is, who
owns it, whether it is active, and how far its authority reaches:

- **An agent is not bounded by its user.** For an agent-bound task the broker
  checks only the agent's grants, so anyone who can reach the agent gets all of
  its authority. The target is *effective access = user ∩ agent ∩ tool*.
- **Agents have no lifecycle.** No staged/active/suspended state, no owner
  rule, no kill switch. The incident runbook tells admins to click
  "Admin → Agents → Disable", which has no code behind it.
- **Downstream calls drop the user.** Each MCP connection uses one static
  bearer shared by every user and agent, so the MCP server cannot tell users
  apart.
- **External agents hold non-expiring API keys.**
- **Nothing revokes early.** No user block, no revoke-all, no deprovisioning
  feed; tokens live until they expire.

### Tracking

| ID | Item | Priority | Effort | Related | Status |
|---|---|---|---|---|---|
| AG-01 | Bind every task to a registered agent | High | S | H-05 | open |
| AG-02 | Bound every agent by its user | High | S–M | H-05 | open |
| AG-03 | Agent lifecycle and a working kill switch | High | M | runbook | open |
| AG-04 | Mandatory owners, orphan detection | High | S–M | AG-06 | open |
| AG-05 | Key-pair authentication for external agents | High | M | L-03 | open |
| AG-06 | Revocation epoch, user block, SCIM | High / Medium | S / M | M-02 | open |
| AG-07 | Asynchronous out-of-band approval | High | M | H-06, M-09, M-28, L-25 | open |
| AG-08 | Per-agent tool ceilings, admin-set MCP effect classes | Medium | M | H-11 | open |
| AG-09 | Per-user OAuth to MCP servers | Medium | M–L | accepted risk in `SECURITY.md` | open |
| AG-10 | Identity-assertion grant (ID-JAG) client | Medium, conditional | L | ROADMAP "Delegation interop" | open |
| AG-11 | Delegation envelopes that carry their limits | Medium | S | M-17 | open |
| AG-12 | Delegation chain in every tool audit event | Low | S | H-07 | open |
| AG-13 | Step-up authentication for risky approvals | Low | S–M | — | open |
| AG-14 | Setup guide for further IdPs; agent inventory export | Low | S | ROADMAP "Agent inventory" | open |

Effort: S = days, M = one to two weeks, L = longer.
Suggested order: AG-01 → AG-04 together with H-05 (one change set about agent
identity), then AG-05 → AG-07, then the rest. AG-10 only when a customer needs
it.

## 1. Current state

| Area | Today | Gap |
|---|---|---|
| Agent identity | `agents` row plus FGA `agent:<id>`, admin CRUD (`broker/internal/broker/agents.go`). Interactive chat uses a pseudo-id `<localpart>-agent` (`agent-gateway/src/broker/agent-identity.ts`); north `CreateTask` carries no agent | Chat tasks are unbound; pseudo-ids are not registered agents |
| Owners | FGA `owner_user` = the creator (`agents.go:256`) | No owner rule, no orphan detection |
| Lifecycle | Create, update, delete. `gateway_enabled` gates only the external API | No suspend. `docs/incident-response-runbook.md:47,187` describe a Disable action that does not exist (the "agents disabled" errors in `agents.go` mean the subsystem is unconfigured) |
| Agent credentials | Gateway↔broker: SPIFFE mTLS. External agents: static `tk_` keys without expiry (`api_keys.go`, L-03) | External agents use bearer secrets |
| Acting for a user | Owner bound from the verified token or the HMAC owner grant. Google Drive and OneDrive use the owner's own OAuth token, or Entra on-behalf-of (`broker/internal/connector/obo.go`) | MCP and web search use shared credentials |
| Effective authority | Agent-bound task: agent grants only (`service_plan.go:169-183`, `service_invoke.go:364-418`) | **Agent not bounded by user** |
| Agent ceiling | Agent skills, MCP grant per whole connector, allowed LLM providers, capability-token attenuation | Per connector, not per tool. MCP effect class taken from the plan (H-11) |
| Downstream tokens | Vault holds per-user OAuth tokens for Google and Microsoft with refresh; tenant-wide keys for web search and MCP | No per-user MCP auth, no generic OAuth provider |
| MCP server auth | Static bearer or none (`broker/internal/toolproxy/mcp.go`) | No MCP OAuth |
| Cross-system delegation | aikonOS mints its own grants; ROADMAP lists "Delegation interop" under consideration | No standard protocol |
| Human approval | n-of-m approvals, four-eyes for step-up, separation of duty. Interactive approval held in gateway memory; unattended runs pre-approve allowlists | No asynchronous channel; H-06 |
| Revocation | Delete agent, revoke API key or role. Tokens live to expiry (`docs/user-lifecycle.md:73-75`) | No block flag, no revoke-all, no SCIM |
| Audit | Hash-chained, signed per-tenant log with decision records and replay | Stream readable by every member (H-07); no delegation chain per event |

## 2. Recommendations

### AG-01 Bind every task to a registered agent (High, S)

- **Why:** every tool call should be attributable to a registered agent, so
  per-agent controls (AG-02, AG-03, AG-08) apply to chat as well.
- **Today:** north `CreateTask` has no agent field, so interactive tasks are
  unbound. The MCP check then falls back to the agent id the caller claims
  (`service_invoke.go:377-382`), and the gateway invents `<localpart>-agent`
  ids that are not registered agents.
- **Change:** add `agent_id` to north `CreateTask`, check FGA `can_use`, store
  it on the task row. Give interactive chat a real per-tenant default agent
  instead of pseudo-ids, then remove the fallback. Fix H-05 (`/agui` has no
  `can_use` check) in the same change.

### AG-02 Bound every agent by its user (High, S–M)

- **Why:** an agent should never do more than the person it acts for. Then a
  hijacked or prompt-injected agent still cannot exceed that person's rights.
- **Today:** for agent-bound tasks `SubmitPlan` skips the user-skill check
  (`service_plan.go:169-183`), and `InvokeTool` checks MCP access for the agent
  only (`service_invoke.go:364-418`). A user who may use an agent inherits all
  of its skills and connectors, including ones the user could not reach
  directly. With H-05 open, that is every tenant member.
- **Change:** when the task owner is a human (not a `svc-` principal), require
  both grants. Run `userSkillDecision` for agent-bound tasks too, and add
  `CheckFGA(user:<owner>, can_access, mcp_connector:<id>)` next to the agent
  check. The model needs no change: `mcp_connector#can_access` already resolves
  `permitted_group` membership for users (`policies/fga/model.fga:53-58`).
- **Trade-off:** some admins may use an agent on purpose as a controlled door
  to a connector its users cannot reach directly. Keep that as an explicit,
  audited per-agent "service agent" flag, which scheduled and external agents
  with `svc-` owners need anyway.

### AG-03 Agent lifecycle and a working kill switch (High, M)

- **Why:** suspending an agent must stop it everywhere at once, including
  tasks already running.
- **Today:** no state; deletion is the only stop; the runbook's Disable action
  is unbacked.
- **Change:** add `agents.status` = `staged | active | suspended | retired`.
  New agents start staged; activation needs an owner (AG-04). Enforce the state
  where every path converges: `CreateGatewayTask`, `SubmitPlan`,
  `ResolveAgentApiKey`, and per call in `InvokeTool`. The per-call check means
  a suspension stops in-flight tasks at their next tool call instead of waiting
  out the 30-minute capability token. The gateway supervisor already
  re-resolves and respawns children; have it evict them on a status change.
  Audit `agent.activated`, `agent.suspended` and `agent.retired`, and update
  the runbook.

### AG-04 Mandatory owners and orphan detection (High, S–M)

- **Why:** every agent needs accountable people. With a single owner, the agent
  is orphaned when that person leaves. Require at least one owner; recommend
  two people or a group.
- **Change:** allow several `owner_user` tuples plus an `owner_group` relation
  in `model.fga`. Require at least one owner to activate. In `ListAgents`, flag
  agents whose owners are all blocked or gone (needs AG-06), and suspend them
  automatically after a grace period. Audit owner changes.

### AG-05 Key-pair authentication for external agents (High, M)

- **Why:** a static bearer key is a standing credential that never expires and
  works from anywhere. A private key never leaves the agent, and each signed
  assertion lives minutes.
- **Today:** external agents on port 8090 present a static `tk_` key with no
  expiry (L-03), resolved to a `svc-<agent>` principal.
- **Change:** register one or more public keys per external agent. The agent
  sends a short-lived signed JWT client assertion (RFC 7523: `iss` = `sub` =
  agent id, `aud` = the gateway, `exp` at most five minutes ahead, `jti`
  checked against a replay cache). Keys carry a `kid`, a status and an expiry;
  rotation registers the new key before retiring the old. Keep `tk_` keys only
  with a mandatory expiry. When an external agent acts for a specific person,
  such as an IDE agent calling aikonOS for its user, accept that person's IdP
  access token with the aikonOS audience instead of a shared key.

### AG-06 Revocation epoch, user block, SCIM (High for the epoch: S; Medium for SCIM: M)

- **Why:** deprovisioning a user or agent should cut access at once rather than
  at token expiry, and the identity directory should drive it.
- **Today:** no block flag and no revoke-all; both are already planned in
  `docs/user-lifecycle.md:73-75`. Provisioning is just-in-time only.
- **Change:**
  - A per-principal revocation epoch (a "not before" timestamp) for users and
    agents. Check it in the north interceptor, in gateway-grant verification,
    and in `capability.Authorize` (add an `issued_at` fact). Revoke-all bumps
    the epoch; block sets it to infinity. Cache it with a short TTL.
  - A minimal SCIM 2.0 endpoint for Users (`active`) and Groups (membership
    written as FGA group tuples). The IdP then pushes deprovisioning and group
    changes; `active=false` blocks the user and feeds AG-04. For tenants on
    SCIM this replaces email-rule provisioning and removes M-02 (groups granted
    from an unverified email).

### AG-07 Asynchronous, out-of-band approval (High, M)

- **Why:** approvals should survive a closed browser, reach the right
  approvers out of band, and let unattended runs wait for a human instead of
  pre-approving tools. OpenID CIBA (Client-Initiated Backchannel
  Authentication) describes the pattern.
- **Today:** an interactive approval is a promise held in gateway memory
  (`agent-gateway/src/agui/hitl.ts`). The broker has no pending-approvals RPC,
  so a second approver for an n≥2 gate has no way to find the request.
  Scheduled and external runs avoid waiting by pre-approving allowlists (H-06,
  L-25).
- **Change:** add `ListPendingApprovals`, filtered by FGA `can_approve`, over
  the approval records the broker already stores. Notify approvers through the
  existing webhook and SMTP notifiers plus a Teams or Slack card with a deep
  link. Show the tool, its arguments and an argument digest, and bind the
  approval to that digest (M-28 and the deferred `args_hash` in ADR-006).
  Enforce expiry (M-09). The gateway resumes from the broker's state instead
  of memory. Scheduled and external runs can then pause for a human instead of
  pre-approving, and `unattendedAllowed` can be enforced on every path.

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
  for built-in tools. This fixes H-11 at the root.

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
  Verify before planning.
- **Change:** a connector auth type next to Entra on-behalf-of. The broker
  keeps the user's IdP refresh token server-side, as it already does for OBO,
  performs leg 1 (RFC 8693 token exchange at the IdP for an ID-JAG naming the
  resource) and leg 2 (RFC 7523 JWT-bearer grant at the resource's
  authorization server), and authenticates as aikonOS with a private-key
  client assertion. Resource tokens stay inside the broker; the agent never
  holds them. This implements the ROADMAP's "Delegation interop" item.
- **Later, optional:** accept ID-JAGs inbound, so external agents reach aikonOS
  under the enterprise IdP's policy instead of aikonOS-issued keys.

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

### AG-12 Delegation chain in every tool audit event (Low, S)

- **Why:** an auditor should see the user, the agent and any delegation behind
  every tool call, from one event.
- **Change:** fix H-07 first; the audit stream must be admin-only. Then add an
  actor chain to each tool event: user, agent, envelope sender if any, external
  client id. Add agent lifecycle and credential events from AG-03 and AG-05.

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

## 3. Strengths to keep

- Per-call decisions with OPA and OpenFGA, effect classes, network ACL and DLP
  rules.
- Human approval with n-of-m thresholds and separation of duty.
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

## 5. References

- [IETF draft-ietf-oauth-identity-assertion-authz-grant-04](https://datatracker.ietf.org/doc/html/draft-ietf-oauth-identity-assertion-authz-grant-04)
- [RFC 8693: OAuth 2.0 Token Exchange](https://www.rfc-editor.org/rfc/rfc8693)
- [RFC 7523: JWT Profile for OAuth 2.0 Client Authentication and Authorization Grants](https://www.rfc-editor.org/rfc/rfc7523)
- [RFC 9728: OAuth 2.0 Protected Resource Metadata](https://www.rfc-editor.org/rfc/rfc9728)
- [RFC 8707: Resource Indicators for OAuth 2.0](https://www.rfc-editor.org/rfc/rfc8707)
- [OpenID Connect Client-Initiated Backchannel Authentication (CIBA)](https://openid.net/specs/openid-client-initiated-backchannel-authentication-core-1_0.html)
- [Slack: enterprise-managed authorization](https://docs.slack.dev/authentication/enterprise-managed-authorization)
- [Atlassian: enterprise-managed authorization for Rovo MCP](https://www.atlassian.com/blog/development/enterprise-managed-authorization-rovo-mcp-xaa-id-jag)
