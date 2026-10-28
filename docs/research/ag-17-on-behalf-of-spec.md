# AG-17 — Explicit on-behalf-of context: implementation work package

> Date: 2026-10-28, against `main` at `e219bba`. Companion to
> `docs/research/agent-identity-improvements.md` (AG-17) and ADR-013.
> File:line references were checked on that commit. Anything not checked is
> marked *unverified*.

## 1. Goal and non-goals

**Goal.** Make both parties of every agent action explicit and broker-held:

- `subject` — the human the work is done for (none for a pure service run);
- `actor` — the registered agent doing it, with an optional chain for
  delegation and subagents (modelled on the RFC 8693 `act` claim).

Every task row, every owner grant and every capability token carries both.
Every tool call is checked as *subject ∩ actor ∩ tool*. Every audit event names
the chain. Downstream tokens are minted per call for the subject; the agent
(the Pi child) never holds the subject's token or any downstream token.

**Non-goals.**

- No new service, no new token format. The Biscuit capability token, the HMAC
  owner grant and the `tasks` row are extended, not replaced.
- Not the enforcement of user ∩ agent on MCP connectors itself (AG-02), not
  agent binding of north tasks (AG-01), not per-user MCP OAuth (AG-09), not
  ID-JAG (AG-10). AG-17 defines the context those items read.
- No nested actor chain deeper than one hop in v1 (`actor` + `delegated_by`).
- No change to the FGA model beyond what AG-02 and AG-04 already plan.

## 2. Current state (verified)

| Where | Today | Problem |
|---|---|---|
| North `CreateTaskRequest` (`proto/broker.proto:601-610`) | `tenant_id`, `user_id`, no agent | Actor unknown for every interactive run; `governance.ts:161-172` routes interactive calls north whenever an OIDC token is present |
| South `CreateGatewayTaskRequest` (`proto/broker.proto:1726-1734`) | `agent_id` (5) optional; `owner_grant` (6) required | Agent id is a caller claim; non-UUID ids coerced to NULL (`gateway_tasks.go:164-173`) |
| Owner grant (`broker/internal/gatewaygrant/gatewaygrant.go:10,46-57,61`) | HMAC payload `v1\|tenant\|owner\|exp`, TTL default 1 h (`cmd/broker/main.go:619-622`) | Names the subject only; no actor, no grant id, no reason |
| Grant mint sites | `api_keys.go:283` (`svc-<agentId>` for `tk_` keys), `scheduler.go:548` (schedule owner, per claim), `service_workflows.go:614` (user, agent-bound workflow) | Scheduled runs re-mint a fresh 1 h grant at every claim with no standing, visible, revocable delegation behind it |
| Gateway pseudo-ids | `agent-identity.ts:5-12` (`<localpart>-agent`), env override `:18-39`; used by `scheduler/ticker.ts:90,94-95,282-287` and `/agui` | Not registered agents; dropped by the broker |
| External REST | owner `svc-<agentId>` (`external/rest.ts:123`); broker rejects `svc-` subjects on north (`auth/oidc.go:156-158`, `auth/interceptors.go:56-61`) | No way for an external agent to act for a named person |
| Task row (`broker/internal/db/tasks.go:66-93`) | `owner_user_id` (NOT NULL), `agent_id` (nullable, migration `013_tasks_agent_id.sql`) | Subject and actor exist implicitly; no delegation or client fields |
| `SubmitPlan` (`service_plan.go:170-184`) | user-skill check skipped when `task.AgentID != nil` and for every `mcp:` tool | Agent-bound task: subject never checked |
| `InvokeTool` (`service_invoke.go:203-250`) | identity normalised from the row: owner (`:226-229`), bound agent (`:232-242`), `userID = task.OwnerUserID` (`:248`); MCP FGA check agent-only with caller-claim fallback (`:372-418`, fallback `:377-382`) | Subject never checked for MCP; fallback trusts the caller |
| Capability token (`capability/capability.go:124-146,198-224`) | facts `expires`, `holder` (audit only, `:111-113`), `task`, `right`; `Authorize(token, scope, taskID)`; TTL 30 min | No subject or actor fact |
| Tool proxy `Request` (`toolproxy/toolproxy.go:34-47`) | `TaskID`, `TenantID`, `UserID`, `AgentID`, `EffectClass` | Connector tools require `UserID` (`connector_token.go:40-47`); a `svc-` owner reaches Vault as a fake user |
| Downstream | Entra OBO: `Bootstrap(tenant, user, assertion)` with the caller's north bearer (`m365_obo.go:57-61`, `connector/obo.go:207`), `FreshAccessToken(tenant, user)` (`obo.go:245`) cached in Vault per user; MCP static bearer (`toolproxy/mcp.go:216-217`); web search tenant key (`web_search.go:186`) | OBO already keyed by subject; MCP and web search carry no identity at all |
| Delegation envelope (`service_envelopes.go:182,411,470-491`) | spawned task: owner = recipient, prompt = intent, budget; no agent, no envelope id, no sender | Chain lost; scopes dropped (AG-11) |
| Subagent branches (`agent-gateway/src/subagent/run.ts:234-237`) | branch children spawn under the caller's identity verbatim | Same subject and actor; fine, must stay so |
| Audit event (`proto/audit.proto:12-70`) | `actor_spiffe_id`, `actor_user_id`, `actor_email`, free-form `context` | No agent, no chain; `tool.denied` sets `ActorUserId = req.UserId` (`service_invoke.go:51`) |
| `agents` row (`db/agents.go:23-38`) | no status, no service flag | AG-02's `service_agent` and AG-03's `status` are missing |

## 3. Design

### 3.1 Data model

One Go value derived from the task row, used by every enforcement point:

```go
// broker/internal/db/tasks.go
type OnBehalfOf struct {
    Subject          string // human user id; "" for a service run
    Actor            string // agent UUID; "" only until AG-01 lands
    ServiceRun       bool   // Subject == "" (owner is svc-<agent>)
    DelegatedBy      string // sender user of an accepted envelope, else ""
    EnvelopeID       string // AG-11 writes it; "" otherwise
    ExternalClientID string // api_keys.id for an external REST run, else ""
    GrantID          string // scheduled_runs.id for a scheduled run, else ""
}
func (t *Task) OnBehalfOf() OnBehalfOf
```

`Subject` = `owner_user_id` unless it has the `svc-` prefix, in which case
`Subject = ""` and `ServiceRun = true`. `Actor` = `agent_id`.

`tasks` gains three nullable columns (one migration, `048_tasks_obo.sql`):
`delegated_by_user_id text`, `external_client_id uuid`, `grant_id uuid`.
`envelope_id` is AG-11's column; AG-17 only reads it. `owner_user_id` and
`agent_id` are kept as they are: renaming them touches every query for no
gain.

> ponytail: `svc-` prefix stays the wire encoding of "no subject" in v1; it
> is already the FGA principal for API keys (`api_keys.go:112-117`). Upgrade
> path: a nullable `subject_user_id` column when a second subject-less producer
> appears.

Scheduled runs are the only standing delegation today, so the schedule row is
the grant. `scheduled_runs` gains `actor_agent_id uuid NOT NULL` (FK agents),
`delegation_expires_at timestamptz NOT NULL`, `delegation_revoked_at
timestamptz`, `delegation_revoked_by text`. Scope = the existing
`approved_tools`. Subject = `owner_user_id`.

> ponytail: no `delegation_grants` table; add one when a second producer of
> standing delegation exists (an external agent with offline user access under
> AG-05, or AG-09 refresh tokens).

### 3.2 Owner grant v2

`gatewaygrant` payload becomes
`v2|<tenant>|<subject>|<actor>|<kind>|<grantID>|<expiresUnix>` with
`kind ∈ {interactive, scheduled, external, workflow}`. `Mint` takes a
`Grant` struct; `Verify` returns one. `Verify` accepts `v1` only while
`gateway_grant.accept_v1 = true` (default true for one release, then removed)
and maps it to `{Subject: owner, Actor: "", Kind: "legacy"}`.

`CreateGatewayTask`: `req.AgentId` must be empty or equal `grant.Actor`;
otherwise `PermissionDenied` with audit reason `grant_actor_mismatch`. A v2
grant with a non-empty actor always binds the task; the NULL coercion goes
with AG-01.

### 3.3 Capability token facts

`MintTTL` adds authority facts `subject("<user>")` and `actor("<agentId>")`
(empty string when absent). `Authorize(token, scope, taskID)` becomes
`Authorize(token, scope, taskID, obo OnBehalfOf)` and adds
`check if subject({subject}); check if actor({actor});`. The only non-test
caller is `service_invoke.go:313`. Envelope delegation tokens
(`service_envelopes.go:~465`, `MintTTL(recipient, "", ...)`) mint
`subject = recipient`, `actor = ""`; they are never presented to `InvokeTool`
(task-less), so the checks never see them.

Holder stays audit-only (unchanged H-03 deferral).

### 3.4 Enforcement points (authority = subject ∩ actor ∩ tool)

| Point | Check | Status |
|---|---|---|
| North `CreateTask` | subject = OIDC identity (`identity.go:28-42`); actor = `req.agent_id` with FGA `can_use(user:<subject>, agent:<actor>)` | field and check are AG-01; AG-17 fixes the meaning |
| South `CreateGatewayTask` | subject, actor, kind, grant id from grant v2; `req.AgentId` must match | AG-17 slice 2 |
| `SubmitPlan` | `userSkillDecision` for the subject whenever `Subject != ""` (also for agent-bound tasks); `checkAgentSkills` for the actor; MCP connector: `can_access` for `user:<subject>` and `agent:<actor>` | AG-02 consumes the context; AG-17 provides `task.OnBehalfOf()` |
| `InvokeTool` | row normalisation as today; capability facts match `OnBehalfOf`; MCP `can_access` for both parties (AG-02); `callerAgent` fallback removed (AG-01); `agents.status` (AG-03) and epoch (AG-06) checked on the actor and subject | AG-17 slices 3 and 8 |
| Tool proxy `Request` | gains `OBO OnBehalfOf`; connector tools refuse `ServiceRun` with `connector tools require a human subject` instead of a Vault miss | AG-17 slice 5 |
| Delegation spawn | `DelegatedBy = sender`, `Actor = recipient's default agent` (AG-01) | AG-17 slice 7, AG-11 adds scopes |
| Audit | every `tool.*`, `plan.*`, `task.*`, `scheduler.*` event carries the chain (3.5) | AG-17 slice 1; AG-12 widens it |

### 3.5 Audit shape

Add one top-level field `string actor_agent_id = 15` to `AuditEvent` and put
the rest under `context.obo`:

```json
"obo": {"subject": "u-123", "actor": "a9f1…", "service_run": false,
        "delegated_by": "", "envelope_id": "", "external_client_id": "",
        "grant_id": "", "kind": "scheduled"}
```

`actor_user_id` keeps today's value (the subject, or `svc-<agent>`). The
chain canonical form is `json.Marshal` of the event with the signature
cleared (`audit/verify.go:185-191`); an unset new field encodes to nothing,
so old events verify unchanged. *Unverified*: confirm with
`audit/golden_json_test.go` that the generated struct omits empty strings.

### 3.6 Per-path behaviour

| Path | Subject | Actor | Grant / token | Downstream | Chain fields |
|---|---|---|---|---|---|
| Interactive chat (`/agui`, north) | OIDC `sub` | `req.agent_id` (AG-01; tenant default agent or named agent with `can_use`) | none; OIDC bearer; step tokens carry both facts | OBO/OAuth as subject | `kind=interactive` |
| Scheduled run | schedule owner | `scheduled_runs.actor_agent_id` | claim re-checks expiry, revocation, subject block (AG-06), actor status (AG-03), then mints grant v2 `kind=scheduled`, `grant_id=schedule id` | as subject; a blocked subject stops the next claim | `grant_id` |
| External REST, `tk_` key only | none (`svc-<agent>`) | key's agent, must be `service_agent` (AG-02 flag) | grant v2 `kind=external`, actor only | connector tools refused; MCP static bearer with chain logged; web search tenant key | `service_run=true`, `external_client_id=key id` |
| External REST, key + user assertion | user from the assertion (`aud` = aikonOS, validated by the north `oidc.Validator`) | key's agent, needs `can_use(user, agent)` | grant v2 `kind=external` with subject | as subject; OBO bootstrap can use the assertion as the Entra assertion when the IdP is Entra | `external_client_id` |
| Delegation envelope accept | recipient | recipient's default agent (AG-01) | ordinary task; AG-11 intersects scopes | as recipient | `delegated_by=sender`, `envelope_id` |
| Subagent branches | parent's | parent's | parent's identity verbatim (`subagent/run.ts:234-237`); each branch task row copies the parent's chain and sets `parent_task_id` | as parent | inherited |
| Google Drive / OneDrive / Entra OBO | required | logged | `FreshAccessToken(tenant, subject)`; per-call, short-lived, Vault-cached per subject | token names the subject; actor cannot be expressed to Graph today | `obo` on the tool event |
| MCP (static bearer) | optional | required | shared bearer unchanged; the chain is logged on the tool event and the MCP request carries no identity | AG-09 keys per-user tokens by subject; AG-10 uses the subject's IdP refresh token and the actor as the client | `obo` |
| Web search | optional | required | tenant key; chain logged | — | `obo` |

### 3.7 How later items consume the context

- **AG-01** fills `Actor` for north tasks and removes the NULL coercion.
- **AG-02** reads `Subject`/`ServiceRun` to decide whether to run the user
  checks, and `service_agent` to allow subject-less runs.
- **AG-05** produces the "key + user assertion" row above.
- **AG-06** block and epoch checks take `Subject` and `Actor` from the same
  struct; the scheduled-run claim calls them.
- **AG-09 / AG-10** key per-user tokens by `Subject` and present `Actor` as
  the OAuth client (`client_id` or ID-JAG `act`).
- **AG-11** writes `EnvelopeID` and the attenuated scopes.
- **AG-12** promotes `context.obo` into typed fields once H-07 is fixed.

## 4. Migration and rollout

1. **Audit-only first.** Slices 1, 2 (mint side), 3 (facts only) and 7 change
   no decision. A mismatch between the token's facts and the row emits
   `obo_context_mismatch` (DENY in the event, call allowed) until the flag
   below flips.
2. **Flags.** Tenant setting `identity.obo = audit | enforce` (default
   `audit`); AG-02's `enforce_user_bound` becomes an alias of `enforce`.
   Broker config `gateway_grant.accept_v1` (default true) for one release.
3. **Gateway compatibility.** A gateway that sends a v1 grant or a pseudo
   agent id keeps working while `accept_v1` is on; the broker logs
   `grant_v1_used` once per gateway SVID per hour.
4. **Backfill.** `scheduled_runs.actor_agent_id` ← tenant default agent (AG-01);
   `delegation_expires_at` ← `created_at + 90 d`; admins see the expiry in the
   schedule list and extend or let it lapse.
5. **Enforce.** Flip `identity.obo = enforce` per tenant, then default, then
   remove `accept_v1` and the pseudo-id derivation in the gateway.

## 5. Work breakdown (ordered, PR-sized)

| # | Slice | Files | Tests | Acceptance | Depends on |
|---|---|---|---|---|---|
| S1 | `OnBehalfOf` on the task row + audit chain | `db/tasks.go`, migration `048_tasks_obo.sql`, `service_invoke.go` (`emitToolDenied`, `tool.invoked`), `service_plan.go` (plan events), `scheduler.go:643`, `proto/audit.proto` (+`actor_agent_id`) | `db/tasks_obo_test.go` (table: owner, `svc-`, agent, delegated); `audit_denial_test.go` and `golden_json_test.go` extended; verify-chain test with old and new events | every `tool.*` event carries `actor_agent_id` and `context.obo`; old chains verify | — |
| S2 | Grant v2 | `gatewaygrant/gatewaygrant.go` (+`_test.go`), `gateway_tasks.go`, mint sites `api_keys.go:283`, `scheduler.go:548`, `service_workflows.go:614`, `cmd/broker/main.go` (`accept_v1`) | `gatewaygrant_test.go` (v2 round-trip, v1 accepted only with flag, tamper); `gateway_tasks_test.go` (`grant_actor_mismatch`) | `CreateGatewayTask` with `agent_id ≠ grant.Actor` → `PermissionDenied`; v1 refused when `accept_v1=false` | S1 |
| S3 | Capability facts | `capability/capability.go` (+`_test.go`), `service_invoke.go:313`, approval minting path (`approvalsvc/`, *unverified* exact file) | `capability_test.go`: facts present, `Authorize` denies subject or actor mismatch, empty facts accepted in audit mode; `service_invoke_test.go` mismatch case | token minted for task A with subject X cannot pass for the same task with subject Y | S1 |
| S4 | Scheduled runs as explicit delegation | migration `049_scheduled_runs_delegation.sql`, `db/scheduled_runs.go`, `scheduler.go` (Create/Update/Claim/List), `proto/broker.proto` (`ScheduledRun`, `CreateScheduledRunRequest`), `webui` schedule list (expiry, revoke), `agent-gateway/src/scheduler/ticker.ts` (drop `agentForUser`) | `scheduler_test.go`: expired, revoked, blocked subject, suspended actor each skip the claim with an audit event; UI unit test for revoke | a revoked or expired delegation never fires; `ListScheduledRuns` shows actor, expiry, revocation | S2; AG-06 for the block check (soft: hook exists, no-op until AG-06) |
| S5 | Service runs explicit | migration `050_agents_service_agent.sql`, `db/agents.go`, `agents.go` (admin CRUD + audit), `api_keys.go` (mint only for `service_agent` or with assertion), `toolproxy/toolproxy.go` (`Request.OBO`), `connector_token.go:40-47` | `api_keys_test.go`: key for a non-service agent without assertion refused; `toolproxy` test: connector tool on a service run → clear error | `ServiceRun` tasks carry `service_run=true` in audit; connector tools refuse them | S1 |
| S6 | External agent acting for a person | `proto/broker.proto` (`ResolveAgentApiKeyRequest.user_assertion`), `api_keys.go:249-300`, `auth/oidc.go` (reuse `Validate`, audience check), `external/rest.ts` (read `X-Aikonos-Subject-Token`), FGA `can_use` check | `api_keys_test.go`: assertion with wrong audience, `svc-` subject, or no `can_use` refused; good assertion yields grant with subject | REST run with assertion creates a task with `Subject=user`, `Actor=agent`, `external_client_id=key` | S2, S5; AG-05 later swaps the key for a JWT assertion |
| S7 | Delegation chain and downstream logging | `service_envelopes.go:470-491` (`DelegatedBy`, parent id), `toolproxy/mcp.go`, `web_search.go` (log chain at call), `subagent/run.ts` (no change expected; test only) | `dismiss_envelope_test.go`/`send_envelope_group_test.go` extended: spawned task has `delegated_by`; tool-proxy log test | an accepted envelope's task names the sender; MCP and web-search calls log subject, actor, task | S1; AG-11 for scopes and `envelope_id` |
| S8 | Enforce | `service_plan.go:170-184`, `service_invoke.go:372-418`, `capability.Authorize` strict, remove `accept_v1`, `agent-identity.ts` deleted, `AIKONOS_AGENT_FOR_USER` removed | `service_submitplan_test.go`, `service_invoke_test.go`: bound task + subject lacking skill → denied; MCP user lacking `can_access` → denied; mismatch facts → denied | `identity.obo=enforce` is the default; no pseudo-ids anywhere | AG-01, AG-02, S2–S6 |

Phase placement: S1–S3 in phase 2 before AG-01; S4 with AG-06; S5 with
AG-02; S6 with AG-05 and S7 with AG-11/AG-12 in phase 3; S8 closes phase 3.

## 6. Risks and open questions

- **Grant TTL vs standing delegation.** The 1 h HMAC grant is a run ticket;
  the schedule row is the standing grant. If the gateway caches a grant past
  a revocation, the run finishes within the hour. Acceptable; AG-06's epoch in
  `capability.Authorize` cuts it to the next tool call.
- **Audit canonical form.** Adding `actor_agent_id` must not change bytes of
  old events. Covered by S1's chain test; if it does, the field moves into
  `context.obo` only.
- **`svc-` as "no subject".** Cheap, but `svc-` strings exist in FGA tuples and
  tests. S5 must not rename them.
- **Subagent task rows.** *Unverified* whether each branch creates its own
  task via `gate()` or reuses the parent's; if it creates one, S1 must copy
  the chain fields.
- **Approval-time minting.** The exact function that mints step tokens at
  approval (`approvalsvc/`) was not read; S3 must find every `MintTTL` call.
- **Scheduled-run actor for old rows.** Backfill needs the tenant default agent
  from AG-01; until then S4 can land with the column nullable and a startup
  warning, which weakens the "every actor explicit" guarantee for one release.
- **External assertion replay.** A user access token presented to the REST
  endpoint is a bearer; it is bounded by its own `exp` and the aikonOS
  audience. AG-05's `jti` replay cache applies to the agent assertion, not
  to this one.
- **Open:** should `kind=workflow` be its own kind or `interactive` with an
  actor? Default: own kind, since `BeginWorkflowRun` already gates on agent.

## 7. Test plan (regression per enforcement point)

| Test | Where | Asserts |
|---|---|---|
| `TestOnBehalfOf_Derivation` | `db/tasks_obo_test.go` | owner/`svc-`/agent/delegated → expected struct |
| `TestGatewayGrant_V2RoundTrip`, `_V1Flag`, `_Tamper` | `gatewaygrant_test.go` | fields survive; v1 only with flag; MAC failure |
| `TestCreateGatewayTask_GrantActorMismatch` | `gateway_tasks_test.go` | `PermissionDenied`, audit reason |
| `TestCapability_SubjectActorFacts`, `_MismatchDenied` | `capability_test.go` | facts minted; `Authorize` denies |
| `TestInvokeTool_OBOMismatchAuditOnly`, `_Enforce` | `service_invoke_test.go` | audit event then denial by flag |
| `TestSubmitPlan_BoundTaskSubjectSkillDenied` | `service_submitplan_test.go` | subject lacking skill denied on bound task |
| `TestClaimDue_SkipsExpiredRevokedBlocked` | `scheduler_test.go` | three skips, three audit events |
| `TestResolveAgentApiKey_UserAssertion` | `api_keys_test.go` | audience, `svc-`, `can_use` cases |
| `TestConnectorTool_ServiceRunRefused` | `toolproxy/connector_token_test.go` | clear error, no Vault read |
| `TestAcceptEnvelope_DelegatedBy` | `service_envelopes` tests | spawned task has sender |
| `TestAudit_ChainWithActorAgentId` | `audit/verify_test.go` | old and new events verify |
| `ticker.test.ts` scheduled run | `agent-gateway` | no `agentForUser`; actor from the due run |
| `rest.test.ts` subject header | `agent-gateway` | header forwarded as `user_assertion` |
