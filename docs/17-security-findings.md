# 17 — Security Findings (whole-codebase audit, 2026-10-27)

> Scope: every service and config domain at commit `859e060` (v0.7.4): Go broker
> (auth/identity, data/storage, policy/execution), agent-gateway (TS), webui
> (Vue SPA + Fastify proxy), office-worker + docs-mcp (Node), desktop (Rust),
> deploy/compose/CI/scripts, OPA + OpenFGA policies, bundled skills.
> Method: eight read-only domain audits, then adversarial re-verification of
> every Critical/High claim by an independent reader (file:line evidence).
> Every Critical and High below is **confirmed in code**. Nothing was changed.
> Prior audits (`docs/11-security-audit.md`, `docs/13-zero-trust-audit.md`) and
> accepted residual risks in `docs/04-threat-model.md` were not re-reported.
>
> Tally: 1 Critical, 10 High (plus H-11, High only with a non-stock gateway), 44 Medium, 46 Low, 12 Info. Status column is for
> tracking implementation; all rows start `open`.

## 0. Fix order (recommended)

The first five are one root cause and one pattern: **RPCs that still trust
request-supplied identity**, the same bug class fixed in audit 11 for the
admin/connector RPCs. Fix them together with the same `callerIdentity` /
task-row binding idiom and add the regression test pattern already used in
`roles_test.go`.

| # | Finding | Why first |
|---|---|---|
| 1 | C-01 ApproveTask forged approver | Full HITL bypass with any user token |
| 2 | H-01 SendEnvelope / ListInbox forged sender | Mint tokens over an admin's scopes |
| 3 | H-02 InvokeTool uses request user/tenant | Cross-user workspace + connector tokens |
| 4 | H-03 Capability tokens never expire / unbound | Makes every token leak permanent |
| 5 | H-04 South RPCs: no peer or task binding | Enables 3 and 4 from any in-domain SVID |
| 6 | H-08 Compose default `AIKONOS_DEV_MODE=true` | A forgotten env var = allow-all prod |
| 7 | H-05, H-06, H-07 gateway authz gaps | Agent impersonation, unattended bypass, audit leak |
| 8 | H-09 OpenFGA/Grafana exposure | Write access to the authz source of truth |
| 9 | Mediums by domain (§2–§8) | |

---

## 1. Critical

### C-01 `ApproveTask` trusts request-supplied approver and tenant
- **Status:** fixed — branch `security/identity-binding` (2026-10-27), regression tests added
- **Where:** `broker/internal/broker/service_tasks.go:132-170`; `broker/internal/approvalsvc/resolve.go:35-53`
- **What:** `ApproveTask` never calls `callerIdentity`. The FGA `can_approve` check, `approvalsvc.Resolve`, the audit event and the event payload all use `req.ApproverUserId` and `req.TenantId` verbatim. The task owner is seeded as approver (`gateway_tasks.go:111-112`), so FGA passes for a forged owner id. For n-of-m gates, `appendUnique` counts several forged ids as distinct voters.
- **Exploit:** Any user with a valid bearer who learns a task id sends `ApproveTask{task_id, approver_user_id=<owner>, approved=true}`. Task becomes APPROVED and the response returns the minted per-step capability tokens. Defeats human-in-the-loop, four-eyes and separation of duty; works cross-tenant because `tenant_id` is unchecked.
- **Fix:** First line of the handler: `tenant, approver, err := s.callerIdentity(ctx, req.TenantId, req.ApproverUserId)`; use only `tenant`/`approver` in CheckFGA, Resolve, audit and the published event. Mirror `RespondToEnvelope` (`service_envelopes.go:365`). Add a spoof regression test like the ones in `roles_test.go`.
- **Confirmed by:** two independent auditors + verifier.

---

## 2. Broker: identity, authz, capability (High)

### H-01 `SendEnvelope` and `ListInboxEnvelopes` trust request identity
- **Status:** fixed — branch `security/identity-binding` (2026-10-27), regression tests added
- **Where:** `service_envelopes.go:28-196` (lines 54, 75, 83, 98-103, 114-115, 149, 186-189); `:253-268`
- **What:** Sender = `req.FromUserId`, tenant = `req.TenantId`, inbox user = `req.UserId`. Delegable scopes and the FGA `can_delegate_to_user` check run as the spoofed sender; the Biscuit is minted over the spoofed sender's scopes (`:136`, `buildDelegationToken` → `Capability.Mint(recipient, "", senderScopes)`).
- **Exploit:** Low-privilege user sends an envelope `from_user_id=<admin>` to themselves with the admin's scopes attenuated in, accepts it, and holds a token over admin scopes. `ListInboxEnvelopes{user_id=<victim>}` reads another user's inbox cross-tenant.
- **Fix:** `callerIdentity(ctx, req.TenantId, req.FromUserId)` in `SendEnvelope`; `callerIdentity(ctx, req.TenantId, req.UserId)` in `ListInboxEnvelopes`; thread `tenant`/`from` into `sendEnvelopeToUser` and `sendEnvelopeToGroup` as parameters.

### H-02 `InvokeTool` executes as request-supplied `user_id` / `tenant_id`
- **Status:** fixed — branch `security/identity-binding` (2026-10-27), regression tests added
- **Where:** `service_invoke.go:223` (`resolveTenant` returns `req.TenantId` first, `service_plan.go:619-627`), `:283-287` (ErrNotFound only skips the budget gate), `:446-457` (`toolproxy.Request.UserID = req.UserId`)
- **What:** `UserID` is taken from the request **even when the task row loads**; it is never compared with `taskOwnerUserID`. Consumers: workspace read/write/list (`toolproxy/workspace_seam.go:63,108,137`), connector OAuth/OBO tokens for OneDrive/GDrive (`connector_token.go:59-66`, `cloudfile.go:118,166`), user-scope memory (`memory.go:64-75`), netacl principal (`web_fetch.go:98`, `web_search.go:213`). A missing task proceeds.
- **Exploit:** Any trust-domain SVID (realistically a compromised gateway or its forked child, see H-04) with any token carrying the scope (see H-03) sets `user_id`/`tenant_id` to a victim and any `task_id`: reads/writes the victim's workspace and uses the victim's Drive/OneDrive tokens, across tenants.
- **Fix:** Deny when `Tasks != nil` and `Get` returns ErrNotFound. Set `Request.UserID = taskOwnerUserID` and `TenantID = task.TenantID`; reject `req.UserId != owner`. Require task state APPROVED or EXECUTING. Also rebind `effectiveAgentID = boundAgentID` for **all** tools, not only `mcp:` (`:341,367-370`), which closes the per-agent rate-limit and baseline-poisoning bypass (M-07).

### H-03 Capability tokens have no expiry and no task/holder binding
- **Status:** fixed (TTL + task binding; nonce/holder/args_hash deferred) — branch `security/identity-binding` (2026-10-27), regression tests added
- **Where:** `broker/internal/capability/capability.go:83-106` (Mint adds `holder`,`task`,`right`), `:144-182` (Authorize checks only `right($s), operation($s)`); `service_invoke.go:299-332`; `docs/adr/DECISIONS.md:~147-154`
- **What:** No `time()` fact, no expiry block, no nonce store, no DB record of consumed tokens. `task`/`holder` facts are minted but never checked. Task state is read only to drive APPROVED→EXECUTING; COMPLETED/CANCELLED/DENIED tasks are never refused. ADR-006 says tokens are "short-lived"; they have no lifetime. One root key serves all tenants.
- **Exploit:** A token for scope S (from logs, IPC, an envelope) works forever, for any task, any args, any tenant. `web:read` approved for URL A fetches URL B; `onedrive:write` approved for one path writes any path. Delegation tokens (`service_envelopes.go:436`) carry no task at all.
- **Fix:** In `Mint` add `check if time($t), $t < {exp}` (minutes TTL). `Authorize(token, scope, taskID, now)` adds `time({now}); task({id});` and `check if task($t), $t == {id}`; pass `req.TaskId`. Deny terminal task states. Add a consumed-nonce table (`INSERT … ON CONFLICT DO NOTHING`) at InvokeTool. Optionally mint `args_hash` and compare with `argsHash(req.Args)` (`service.go:451`). Define a binding for delegation tokens before enforcing task checks on them.
- **Fixed (TTL + task binding):** `Mint` embeds `check if time($t), $t < exp` (TTL `capability.ttl`, default 30 min); `Authorize(token, scope, taskID)` adds `time(now)` and `check if task(taskID)`; `InvokeTool` passes `req.TaskId` and audits `capability_expired` / `capability_task_mismatch`. Delegation tokens stay task-less (an envelope record never presented to `InvokeTool`; the spawned task gets fresh step tokens at approval), live as long as the envelope (72 h) and are rejected by `Authorize`. **Still deferred:** single-use nonce / consumed-token table (replay within TTL on the same task), holder enforcement (needs the sandbox pool bound to the verified peer), `args_hash` binding, per-tenant root keys.

### H-04 South surface: `SubmitPlan`, `EmitStatus`, `InvokeTool` accept any trust-domain SVID with no peer or task binding
- **Status:** fixed (peer + task-row binding; sandbox_path_prefix default unchanged) — branch `security/identity-binding` (2026-10-27), regression tests added
- **Where:** none of the three call `requireGatewayPeer`; `auth/interceptors.go:108` checks the sandbox path only when `spiffe.sandbox_path_prefix` is set, default `""` (`cmd/broker/main.go:164`, `compose.yaml:473`); `service_plan.go:53,104,198,364,393` (`req.SandboxSpiffeId` used as actor/holder); `service_invoke.go:568-607` (`EmitStatus`: tenant from request, no ownership check); `policies/opa/tool_invocation.rego:74-80`
- **What:** The OPA "infrastructure cannot come from a sandbox" rule keys on a client-supplied SPIFFE string. `EmitStatus` can drive legal edges (APPROVED→FAILED, EXECUTING→COMPLETED/FAILED/PLANNING) on any task in any tenant; EXECUTING→PLANNING lets an attacker re-submit a plan on a victim's task. No HITL bypass (APPROVED is unreachable), but cross-tenant integrity damage and DoS. A gateway child (`agent-gateway/src/ipc/supervisor.ts:959`, same uid, `/tls` readable) can read the gateway key and present as the gateway, so "compromised gateway" includes any RCE in the untrusted child.
- **Fix:** `requireGatewayPeer(ctx)` at the top of all three. Replace `req.SandboxSpiffeId` with `auth.IdentityFromContext(ctx).SpiffeID` (reject a differing request value). Store peer SPIFFE id / tenant on the task row and require a match in `EmitStatus`/`SubmitPlan`. Default `spiffe.sandbox_path_prefix` to `/sandbox/`. Make `requireGatewayPeer` fail closed when `GatewaySpiffeID==""` unless dev mode (`scheduler.go:504-513`).

### H-10 `StreamTaskEvents` and gRPC reflection bypass OIDC/tenant/audit interceptors
- **Status:** open
- **Where:** `cmd/broker/main.go:906,1020` (only `ChainUnaryInterceptor`), `:1010,1066` (reflection); `service_tasks.go:225-265`
- **What:** The only streaming RPC runs with no `Identity` on the context, so `callerIdentity` takes the dev branch and trusts `req.TenantId`. No ownership check on the task either.
- **Exploit:** Any client with a bundle mTLS cert streams any task's lifecycle events for any tenant without a token.
- **Fix:** Add `grpc.ChainStreamInterceptor` with stream versions of the OIDC/tenant/audit interceptors (wrap `ServerStream.Context()`). Require `task.OwnerUserID == user` or FGA. Register reflection only in dev mode.

### Broker identity/authz — Medium
| ID | Finding | Where | Fix |
|---|---|---|---|
| M-01 | `GetTaskState`/`CancelTask` have no per-task ownership check (any tenant member with a task id can cancel a colleague's task) | `service_tasks.go:108-127,189-220` | Require owner or FGA like `authorizeRunMutation`. **Fixed for `CancelTask`:** only the task owner (the subject, also on a delegated task; the delegator has no standing) or a tenant admin may cancel; anyone else gets `PermissionDenied`, audited `aikonos.broker.task.cancel_denied` (a cancel is audited `aikonos.broker.task.cancelled`); an FGA error fails closed. Open: `GetTaskState` still checks only the tenant |
| M-02 | JIT provisioner grants groups from unverified `email` / `preferred_username` claim (no `email_verified`) | `auth/oidc.go:139-152`, `provisioning/provisioner.go:76-142`, `broker/provisioning_admin.go:118-143` | Require `email_verified`, or match on immutable `oid`+UPN; never fall back to `preferred_username`. **Fixed for SCIM tenants (AG-16):** under `provisioning.mode=scim` the identity provider grants groups through SCIM, keyed by `externalId` = `oid`; email-rule grants are off and new rules are refused. Open for `jit` tenants (the default) |
| M-03 | Admin gate pinned to the single configured tenant object, not the token's tenant | `roles.go:26-28`, `identity.go:28-42,80-92` | Reject token tenants outside the configured set, or build the FGA tenant object from the resolved tenant |
| M-04 | Policy bundle HTTP endpoint unauthenticated, cleartext, published on all interfaces; OPA fetches without bundle signing | `cmd/broker/policy_serving.go:55-61`, `compose.yaml:536`, `deploy/compose/opa.yaml:11-22` | Bind to internal iface / mTLS, enable OPA bundle signing, drop host port |
| M-05 | Insecure defaults: empty FGA store = allow-all, empty issuer = no OIDC, empty sandbox prefix = any workload; only a WARN | `main.go:147,158,164,565`, `policy/engine.go:373-381`, `guard.go` | Fail startup unless explicit dev flag (see H-08) |
| M-06 | `ApproveGatewayTask` self-approves with the owner identity for any gateway task (design intent unclear) | `gateway_tasks.go:195-232` | Confirm intent; restrict to the agent's pre-approved allowlist or require an owner-signed grant |
| M-07 | Per-agent RPM limit and baseline keyed on request `agent_id` for non-MCP tools — **fixed with H-02** (keyed on the task's bound agent) | `service_invoke.go:341,410-417,525` | Done; see H-02 |

### Broker identity/authz — Low / Info
- L-01 OIDC: token with no `exp` accepted; JWKS refreshed only on unknown `kid`; refresh holds the write lock across HTTP (DoS via random kids); discovery `issuer` not compared (`auth/oidc.go:128-137,192-270`).
- L-02 Subject-claim fallback (`oid` → `sub`) silently changes the principal (`oidc.go:139-150`). Reject instead.
- L-03 API keys: no expiry, `last_used_at` not updated on resolve (`api_keys.go:249-300`). **Fixed (AG-15):** `expires_at` is mandatory (migration 052, tenant cap `api_keys.max_ttl_days`, default and ceiling 90), expired keys resolve like revoked ones (401), and `last_used_at` is updated on resolve and shown in the key list. **Follow-up (AG-05):** external agents can drop the bearer key for a registered public key and five-minute signed client assertions with single-use `jti`.
- L-04 Gateway grant not bound to agent/session; 1h multi-use (`gatewaygrant/`). Design note.

---

## 3. Broker: policy and execution

### H-11 (Medium in stock deployment) MCP tools take the plan-declared effect class
- **Status:** fixed (AG-08). `AuthoritativeEffectClass` returns `destructive` for every `mcp:` id, so `Stricter` floors the plan's class; a tenant admin confirms the real class per tool (Admin → MCP → Tools, stored as a `skill_overlay` row, which then wins). Agents also need each MCP tool in `agents.mcp_tools` (SubmitPlan and InvokeTool). The gateway heuristic is left in place but no longer decides routing.
- **Where:** `service_plan.go:151-155` (`Stricter` only when `authKnown`), `toolregistry/authoritative.go:58-72` (ok=false for `mcp:`), `overlay.go:93-102,150-152`, `policies/opa/tool_invocation.rego:8,24-28`; class set by `agent-gateway/src/broker/mapping.ts:121-134` (name-prefix heuristic + server `readOnlyHint` from `pi/mcp-tools.ts:78`)
- **What:** No broker-side floor for unknown MCP tools. The gateway classifies READ_ONLY (auto-approved, no human) when the third-party MCP server sets `readOnlyHint=true` or the tool name starts with get/list/search/read/fetch/describe/lookup/query/find. Nothing re-derives the class at execution.
- **Exploit:** A malicious/compromised MCP server advertises a destructive tool as read-only, or names it `query_…`; it runs with no approval. A compromised gateway/sandbox can declare any class.
- **Fix:** In `AuthoritativeEffectClass`, return `(WRITE_EXTERNAL, true)` for any `mcp:` id without a baseline/overlay row so `Stricter` floors it. If MCP reads must stay auto, add an admin-attested per-tool read-only overlay. In the gateway, drop the name heuristic and ignore the child-relayed `readOnlyHint` in `GovernanceBridge.gate` (`ipc/bridge-server.ts:177-183`); resolve hints parent-side from the south tool listing as `resolveMcpReadOnlyHints` does.

### Policy/execution — Medium
| ID | Finding | Where | Fix |
|---|---|---|---|
| M-08 | `plan.has_dlp_attestation` is a bool from the plan submitter; turns the hard exfil deny into a normal approval | `proto/plan.proto:45`, `service_plan.go:200`, `policy/engine.go:600,628`, `plan_validation.rego:46-53`, `tool_invocation.rego:65-72` | Pass `false` / drop from policy input until a broker-verified DLP stage exists |
| M-09 | Approval `expires_at` written but never enforced; no EXPIRED sweep | `approvalsvc/build.go:158`, `db/approvals.go:349-372,378-451` | `AND expires_at > NOW()` in both queries; resolve expired rows to EXPIRED, task to TIMEOUT. **Fixed (AG-07):** both queries filter `expires_at > NOW()`; a broker sweep (every minute, migration 053 carve-out) sets expired rows `EXPIRED`, the task `TIMEOUT`, and audits `aikonos.broker.approval.expired` |
| M-10 | Replanning leaves stale `plan_steps`; approval of plan N mints tokens for non-DENY rows of earlier plans | `db/approvals.go:113-124,241-247`, `approvalsvc/resolve.go:85-94` | Add `plan_id`, supersede old rows on `SubmitPlan`, mint from the approved plan only |
| M-11 | Empty approver set after separation-of-duty = "anyone may vote" (legacy bypass) | `approvalsvc/build.go:93-125`, `db/approvals.go:456-461` | Empty set stays PENDING; don't seed owner as approver when n≥2. **Fixed:** an empty set admits nobody (`MayVote`, `ListPendingApprovalsFor`). For n≥2 the broker resolves the eligible approvers at `SubmitPlan` (task approvers plus the tenant's `tenant#approver`, admins included, minus the requester, each checked for FGA `can_approve`); fewer than n refuses the plan with a reason the user sees, audited `aikonos.broker.approval.insufficient_approvers` (DENY), so no undecidable request is created. The requester can withdraw a pending request (`CancelTask` closes it, audited `aikonos.broker.approval.withdrawn`) |
| M-12 | Admin rate-limit edit replaces the global limiter policy slice with one tenant's rows | `rate_limits.go:114-121`, `ratelimit/limiter.go:95-99`, `main.go:829` | Use `ListAll` in `reloadRateLimitPolicies` (Low while single-tenant) |
| M-13 | Cost/budget gate racy and self-declared: pre-check then post-hoc increment; user picks own `cost_budget`; negative `estimated_cost` accepted; memory calls cost 0 | `service_invoke.go:289,535-546`, `db/tasks.go:246-268`, `service_tasks.go:62-65`, `plan_validation.rego:30-36` | Atomic reserve (`UPDATE … WHERE consumed+est <= budget`), tenant ceiling, reject negatives |
| M-14 | `TestLlmProvider` sends the stored Vault key to a request-supplied endpoint (tenant-admin key exfil) | `providers_test_conn.go:53-59,75-81,205-268` | Force endpoint/api family from the stored row when using the stored key |
| M-15 | Network ACL `ASK` enforced only at plan time; fetch-time only blocks DENY; token is scope-only so URL may differ from plan | `toolproxy/web_fetch.go:92-102,258-270`, `route_step.go:158-175`, `netacl/eval.go:134-137` | Treat non-ALLOW as block at execution; or bind host/URL into the token |
| M-16 | `web.fetch` is READ_ONLY → auto-allowed; exfil DLP rule covers only write_external/network_egress; ACL default-allow with no rules | `skills/web.fetch/skill.yaml:10-15`, `toolproxy/web_fetch.go:65`, `tool_invocation.rego:8,65-72` | Classify as NETWORK_EGRESS (or when URL has a query); ship default-deny host ACL; extend DLP rule |
| M-17 | OPA rules skip checks when input fields are undefined (`plan_validation` allows `{}`; cost/risk/sensitivity/shell rules dormant); no tests for `envelope_send` or malformed input; several documented rules never receive input (`risk_score`, `is_on_call`, `resource.sensitivity`, `is_approved`; envelope `relationship`/`depth`/`effect_class` hardcoded) | `policies/opa/plan_validation.rego:10-43`, `tool_invocation.rego:45-72`, `envelope_send.rego:29-37`, `route_step.go:140-149`, `engine.go:613-632` | Explicit violations for undefined required fields; `object.get` with fail-closed defaults; add tests; feed real signals or delete dead rules. **Partly fixed (AG-11):** `policies/opa/tests/envelope_send_test.rego` covers `envelope_send` (attenuation, FGA, depth, cost, risk, auto-accept, empty and malformed input deny). Open: `plan_validation` undefined-field checks, the dormant rules and the hardcoded envelope `relationship`/`depth`/`effect_class` |

### Policy/execution — Low
- L-05 MCP client: no response body cap, fail-open when tool not in advertised list, no injection scan on MCP content, connector URL leaked to agent (`toolproxy/mcp.go:229-263`, `mcp_handler.go:128-166`).
- L-06 LLM spend accounting trusts gateway-reported tokens/cost; negative/NaN accepted; 30s cache + check-before-record overshoot (`providers_south.go:36-110`, `spend_caps.go:48,170-192`).
- L-07 Metrics labels from request-controlled `tool_id`/`tenant` (cardinality) (`service_invoke.go:459`, `metrics/task.go:73-74`).
- L-08 SMTP alert notifier never STARTTLS; `normalizeHost` mangles IPv6 literals (`alerting/notifier.go:121-139`, `netacl/eval.go:45-51`). **Fixed (AG-07 slice 2):** the notifier upgrades with STARTTLS (TLS 1.2+, server name verified) and refuses a server without it unless the host is loopback; `normalizeHost` uses `net.SplitHostPort` and keeps bare IPv6 literals whole.

---

## 4. Broker: data, storage, audit (no High)

### Medium
| ID | Finding | Where | Fix |
|---|---|---|---|
| M-18 | Audit `Emit` advances the chain head before persistence; drops on full queue (1024/100ms), gives up after 3 retries, loses tail on exit; returns nil in all cases → silent loss + chain break indistinguishable from tampering | `audit/emitter.go:206-222,251-253,335-347,361-407` | Local outbox/WAL before returning; chain inside the single worker in dequeue order; return error for mutations/denials |
| M-19 | Chain order (lock order) ≠ verifier order (event_id sort); `event_id` minted before lock → spurious breaks under concurrency; restart may resume from a non-head | `emitter.go:206-222,304-331`, `verify.go:69-71`, `files.go:317` | Assign `EventId` under the lock; verify by `prior_event_hash` linkage |
| M-20 | Query/Verify see only the **oldest** 5000 objects per tenant; Query has no truncation flag | `audit/reader.go:72-87,142-151,256-259` | Date-prefixed listing, newest-first pagination, `truncated` in response, incremental verify with checkpoints |
| M-21 | Silent downgrade to env key / unsigned when Vault read fails at startup; unsigned chain is forgeable with bucket write | `cmd/broker/main.go:1444-1454`, `emitter.go:217-221`, `verify.go:158-169` | Fail or retry-refuse when Vault is configured; unsigned = explicit dev opt-in; alert metric |
| M-22 | No per-user/tenant byte, file-count or depth quota on workspace RPCs; unbounded `ListDir`; no explicit `grpc.MaxRecvMsgSize` found | `workspacefs/store.go:34,326-411,505-534,662-672`, `files.go:133-238` | Quotas in `Store.Write`/`Mkdir`; cap listing; set `MaxRecvMsgSize` |

### Low / Info
- L-09 Verifier can't detect tail truncation; `Skipped>0` still `OK`; WORM is Governance mode with default retention 0 (`verify.go:125-145`, `emitter.go:170-176`). Signed checkpoints to a separate sink; Compliance mode in prod.
- L-10 Subject `group-<gid>` aliases a group memory dir via file RPCs (only `svc-` rejected in `identity.go:56-60`); `SanitizeSeg` is many-to-one. Reject `group-` subjects; collision-free per-user segment.
- L-11 Session-record HMAC sidecar fails open when absent / key missing; signature covers content only; orphan `.sig` on delete (`workspacefs/store.go:461-481,568-591`).
- L-12 Internal absolute paths and Graph error bodies leaked in `Internal` errors (`files.go:113`, `onedrivefs/store.go:239`, `rooted.go:221,326`, `store.go:422`).
- L-13 No symlink defense in local workspace store (lexical only). Use `os.OpenRoot`.
- L-14 Vault client: no re-login on 403, `http://` accepted silently, redirects followed with `X-Vault-Token` (`secrets/vault.go:99,106,178-212`). Set `CheckRedirect = ErrUseLastResponse`.
- L-15 File mutations audited only after success; `.agent/` forgery attempts and failed deletes not audited; raw unbounded path in audit context (`files.go:224-330`).
- L-16 OneDrive `@odata.nextLink` followed with bearer and no host check / page cap (`onedrivefs/rooted.go:72-87`).
- L-17 Personal-skill replace install is delete-then-copy (non-atomic) (`personalskill/store.go:239-267`).
- I-01 Connector config dirs `0o755` vs `0o750` elsewhere (`connectorstore/store.go:140`). I-02 `*.sig` hidden everywhere, `.upload-*` temp files orphaned (`workspacefs/store.go:389-393,509`).

---

## 5. agent-gateway (TypeScript)

### H-05 `/agui` lets any tenant member run as any agent (no `can_use` check)
- **Status:** fixed — branch `feat/ag-identity` (AG-01, 2026-10-28). `/agui` resolves the agent through the south RPC `ResolveRunAgent`, which requires `can_use` (tenant-admin fallback) and answers 403 otherwise; north `CreateTask` re-checks it, `CreateGatewayTask` checks it for the grant owner when the grant names no actor, and scheduled runs re-check it at every fire. Regression tests: `default_agent_test.go`, `north_actor_test.go`, `agent-gateway/test/agui-history-validation.test.ts`. AG-02 (2026-10-28) bounds the agent by its user: agent-bound tasks with a human subject also take the owner's skill gate, and MCP calls check `user:<subject> can_access` at plan and invoke time (`identity.enforce_user_bound`, audit by default; `user_bound_test.go`).
- **Where:** `agent-gateway/src/routes/agui.ts:537-558,602-620`; broker `agent_spec.go:19-63` (gated only by `requireGatewayPeer`, no user); `service_invoke.go:372-402` (FGA subject is the agent); `service_plan.go:495-500` (agent-bound tasks skip the owner's skill gate)
- **What:** `x-aikonos-agent` / `body.agentId` is used verbatim; `getAgentSpec` found=true is treated as sufficient. Nothing downstream ties the user to the agent. Agent UUIDs leak via the audit stream (`resource_ref`, H-07) and the UI.
- **Exploit:** A user runs under a colleague's or a service agent's `approval_mode:auto`, skills, MCP connectors and agent-scope memory.
- **Fix:** After resolving the id in `startChatTurn`, require it in `north.listMyAgents({tenantId,userId}, token)` or add a south RPC `CheckFGA(user:<sub>, can_use, agent:<id>)`; 403 otherwise. Validate as UUID string. Apply the same to the scheduler's agent path.

### H-06 Org-wide `unattendedAllowed=false` enforced only on interactive chat
- **Status:** fixed (AG-07 slice 2): `unattendedAllowed()` (`scheduler/ticker.ts`) is read once per run and fails closed; with the master off nothing is pre-approved on the scheduler prompt and workflow paths, the external surface, subagent branches, or `auto`-mode chat. Gated calls wait for a human (the broker-held approval in the Inbox) instead of being denied. Interactive chat no longer fails open on a settings error.
- **Where:** only `routes/agui.ts:96-107,739-751` (fails open on `getOrgSettings` error). Not in `scheduler/ticker.ts:121,313`, `external/core.ts:101-103`, `external/rest.ts:112`. Broker stores only (`org_settings.go:98,126`); no OPA reference.
- **Exploit:** Admin disables unattended execution; a user's cron schedule still fires with `preAuthApprover(SCHEDULER_BASELINE_TOOLS + approvedTools)` (web.fetch, doc.read, doc.write always pre-approved, `ticker.ts:41-45`), and an agent API-key call still auto-approves.
- **Fix:** One `resolveUnattendedApprover()` helper (cached ~30s) used in `tick()`, `runViaChild`, `runViaWorkflow`, `runAgentInvocation`; deny-all approver when false; fail closed on settings error. Consider enforcing in the broker on `CreateScheduledRun`/`ResolveAgentApiKey` too, but existing schedules need the fire-time check.

### H-07 Tenant-wide audit stream readable by every tenant member
- **Status:** fixed (AG-12): `/api/audit` and `/api/audit/stream` give the whole tenant only to a tenant admin (probed via the admin-gated `GetObservabilityInfo`); anyone else, and anyone whose probe fails, sees only events they acted in or that ran on their behalf (`context.obo.subject`). At most 3 open streams per principal (429 above). The webui guards every `/admin/*` route on the admin probe and reads the stream over fetch with the bearer (an `EventSource` sent none). No auditor role exists, so none was added.
- **Where:** `agent-gateway/src/audit/stream.ts:31-33,71-106` (`requireUser` + tenant filter only); `webui/server.mjs:192-211` forwards verbatim; events carry `actor_user_id`, `actor_email`, `resource_ref`, full `context` (`audit/emitter.go:196-197,243-246`)
- **Exploit:** Any member streams every colleague's tool calls, approvals, emails and agent ids (feeds H-05). Unlimited SSE connections per user.
- **Fix:** Tenant-admin gate on both routes (reuse the admin check from `routes/admin.ts`), or filter to `actor_user_id === principal.sub`. Add per-principal connection cap. Add `meta.admin` guard on the `/admin/*` routes in `webui/web/src/router.js:38-104` (UI only hides nav today).

### Gateway — Medium
| ID | Finding | Where | Fix |
|---|---|---|---|
| M-23 | Per-run LLM call budget reset by every concurrent run on the shared child; threat-model D-01 cap defeated | `ipc/supervisor.ts:317-322,490`, `llm/egress-proxy.ts:235-257` | Key budget by `runId`; don't reset shared counter; cap concurrent runs per user |
| M-24 | Child shares parent uid; can read `/proc/<ppid>/environ` (LLM key) and `/tls` SVID key → full gateway identity (enables H-02/H-04) | `ipc/supervisor.ts:959-963`, `child-entry.ts:8-13`, `compose.yaml:~665` | Fork under a distinct uid with no read on `/tls`; `PR_SET_DUMPABLE=0`; Node `--permission` |
| M-25 | External API rate limit per source IP only, no `trustProxy`, no per-key limit; unauthenticated requests cost a south RPC | `external/server.ts:32-38` | `keyGenerator` on hashed Authorization; `trustProxy` to the proxy hop only; low limit for failed auth |

### Gateway — Low / Info
- L-18 No rate limit / SSE cap / disconnect abort on internal :8080 (`app.ts:54-121`, `routes/workflows.ts:294-322`).
- L-19 JWT issuer/audience checks silently skipped when env is empty (jose treats falsy as "don't check"); no `algorithms` allowlist (`config.ts:179,181`, `auth/verify.ts:44-50`). Refuse to start unless explicit insecure-dev flag.
- L-20 `cfg.defaultTenantId` used for chat/scheduler runs while REST uses `principal.tenant` (`routes/agui.ts:418-420,605-606,647-658,766-771`, `scheduler/ticker.ts:93,284,362`). Assert equality.
- L-21 Legacy demo UI at `/` has an `innerHTML` DOM-XSS sink and no CSP (`public/index.html:160-163`). Remove route or fix.
- L-22 Error text leaks on workflow routes and provider errors (`routes/workflows.ts:319`, `broker/governance.ts:401,449`, `llm/reason.ts:137-141`, `llm/embed.ts:108-113`).
- L-23 Provider `fetch` follows redirects and forwards `api-key`/`x-api-key`; accepts `http://` endpoints (`llm/reason.ts:127`, `vision.ts:149`, `embed.ts:98`). `redirect:'error'`, require https.
- L-24 `/delegate` demo defaults (`to`, `scopes`, `maxCost=50`) and unvalidated `maxCost`/`scopes` (`routes/delegation.ts:30`).
- L-25 Scheduler baseline pre-approves web.fetch/doc.read/doc.write for every schedule even with `approvedTools: []` (`ticker.ts:41-45`). Make explicit at creation. **Fixed (AG-07 slice 2):** the baseline is gone; only the schedule's own `approvedTools` are pre-approved, and any other gated call pauses the run for a human.
- I-03 Forged assistant turns in client history are possible (roles user/assistant), accepted class L-01. I-04 Wildcard ACAO on SSE responses, not exploitable (bearer-only, no credentials).

---

## 6. webui (Vue SPA + proxy)

### Medium
| ID | Finding | Where | Fix |
|---|---|---|---|
| M-26 | No CSP, X-Frame-Options, nosniff, Referrer-Policy, HSTS anywhere; markdown from LLM rendered via `vue-stream-markdown` (beta) with katex/mermaid/shiki → clickjacking of approval modal, zero-click `img-src` exfil, no backstop for any sanitizer gap | `webui/server.mjs:93-119`, `webui/web/index.html:1-56`, `components/MarkdownMessage.vue:12` | `onSend` hook: `default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self' <idp>; frame-ancestors 'none'; base-uri 'none'; object-src 'none'` + nosniff, `X-Frame-Options: DENY`, `Referrer-Policy: no-referrer`; hash/externalize the two inline scripts |
| M-27 | Access/ID/refresh tokens in `sessionStorage`; any XSS = account takeover (compounds M-26) | `web/src/auth/oidc.js:57-69` | Ship CSP; no `offline_access`; short lifetimes + rotation; longer term BFF httpOnly cookie via the existing proxy |
| M-28 | Approval modal collapses nested args by default; "reviewed" toggle doesn't require opening; approve POST sends only `{approved}`+`toolCallId` (no args hash); `risk`/`stepUp` from stream payload; `.arg-val` lacks `pre-wrap` | `components/ApprovalModal.vue:25-29,127-129,196-203`, `ArgsView.vue:45-56`, `api/agui.js:129-130` | `<details open>` or full `JSON.stringify` pre-wrap; gate on nested args; server re-reads args by `toolCallId` and optionally requires args hash. **Fixed (AG-07):** nested args render open with `pre-wrap`; SubmitPlan returns `args_digest` (sha256 of the canonical step args), the dialog echoes it, and the gateway and the broker (recomputing from the stored plan-step args) refuse an approval without the matching digest. Slice 2: `stepUp`, `requiresN` and whether the requester may vote come from the broker's approval row (`GetGatewayApproval`), not the plan outcome or stream; the gateway emits no `risk` field, so the modal's `risk` branch stays inert. The modal is replaced by an inline chat card (`components/ApprovalCard.vue`) that opens the arguments of a step-up call and keeps Approve locked until reviewed |

### Low / Info
- L-26 Chat history in `localStorage`, not keyed by `sub`, cleared only by Settings sign-out (`store/chat.js:41-165`, `store/user.js:26-35`). Key by `sub`, clear on `logout()`.
- L-27 Proxy: no upstream abort on client close, no backpressure, no timeouts, no proxy-side auth on `/agui`/`/audit/stream`, 15 MiB body limit (`server.mjs:146-271`). `AbortController` on `raw.close`, `AbortSignal.timeout`, 401 without Authorization.
- L-28 `/healthz` and 502 bodies leak internal URLs / `String(err)` (`server.mjs:121,185,209,238,273`).
- L-29 `/api/*` upstream URL by raw concatenation; dot segments reach any gateway path (`server.mjs:219,243-245`). Reject `..`, allowlist prefix.
- L-30 `window.location.assign(data.authorizeUrl)` with no scheme check (`views/Connections.vue:43-44`).
- I-05 Connector callback forwards `code`/`state` with no client nonce; CSRF binding depends on broker (`state` is user-bound and single-use per audit 11: OK); scrub query via `router.replace`. I-06 Admin UI gated client-side only (OK iff server enforces; see H-07). I-07 Static dotfiles/sourcemaps: defaults safe.

---

## 7. office-worker and docs-mcp (Node)

### Medium
| ID | Finding | Where | Fix |
|---|---|---|---|
| M-29 | office-worker has no authentication; protection is only the `internal: true` `office` network; several ops execute caller-supplied scripts (RCE by design) | `office-worker/src/server.js:39-95`, `compose.yaml:98-100,459,565-588` | Per-deployment bearer/HMAC header checked before `semaphore.acquire()`; broker sends it |
| M-30 | Model-authored scripts run same uid, shared `/tmp/office-job-*` readable across concurrent jobs; `setsid` double-fork escapes `killAll`; symlink in job dir makes `readJobFile` return any readable file | `handlers/run-script.js:94-157`, `job.js:21-60` | `mkdtemp` + `chmod 700` + per-slot uid, or bwrap/nsjail with private `/tmp`, no netns, rlimits; `realpath` re-check in `readJobFile` |
| M-31 | Semaphore slot taken before multipart parse; no `requestTimeout`/`headersTimeout`/`maxConnections`; unbounded acquire queue → slowloris starves all 4 slots | `server.js:73-75`, `multipart.js:54-153`, `job.js:62-77` | Parse first (inside a timeout), then acquire; set server timeouts; bound queue, 503 when full |
| M-32 | Uploads fully buffered (16×11 MiB) then copied; output read whole + `Buffer.concat` (2× peak); `max_output_bytes` only if caller supplies it; `zip_edit.py:47` / `zip_xml_query.py:35` read members with no uncompressed-size check (zip bomb) | `multipart.js:31-39,94-112`, `size-check.js:29`, `scripts/zip_*.py` | Server-side default cap (~50 MiB), stat before read, `ZipInfo.file_size` cap, per-job `prlimit`/cgroup |
| M-33 | docs-mcp published `0.0.0.0:8060`, no auth, no Host/Origin validation, no DNS-rebinding protection on `StreamableHTTPServerTransport` | `compose.yaml:771-772`, `docs-mcp/src/server.js:19-28,104,142` | `127.0.0.1:8060:8060` or drop ports; `enableDnsRebindingProtection: true` + `allowedHosts`; optional bearer (opt-in profile, so Medium) |

### Low / Info
- L-31 `params` JSON shape unvalidated (`null` → 500); `dpi`/`max_pages`/`pages`/`angle` unclamped to pdftoppm/qpdf (`dpi:200000` OOM; `--`-prefixed page range = argument injection); user regex in Python `re` (ReDoS bounded by timeout); `ops` unbounded (`multipart.js:141`, `handlers/pptx.js:62-81`, `pdf.js:96-126`, `zip-edit.js:14-26`). Assert plain object; clamp ints; regex-validate page ranges.
- L-32 docs-mcp: `X-Powered-By` on; sync `readFileSync` over whole corpus per call; unbounded `query` length (`corpus.js:52-91,184-250`). `z.string().max(256)`, cache corpus.
- L-33 Dockerfiles: tag-only base images, `apt-get upgrade` (non-reproducible); office-worker `chown -R node /app` (mitigated by `read_only`) (`office-worker/Dockerfile:4,13,58-59`, `docs-mcp/Dockerfile:4,15-23`).
- I-08 OOXML edits are raw text replace: can insert `TargetMode="External"` rels / `vbaProject.bin`; outputs not scanned (`scripts/zip_edit.py:19-49`, `soffice.js:34-57`). Post-check if outputs reach humans.
- I-09 `image-size` override pins 2.0.4 (past advisory): OK. I-10 `test/mcp-echo-server` is `profiles:[dev]` only; add CI check that prod env templates never include `dev`.

---

## 8. desktop (Rust)

### Medium
| ID | Finding | Where | Fix |
|---|---|---|---|
| M-34 | Server-supplied OIDC `authority` and discovery endpoints used without https/issuer/host validation; refresh token POSTed to whatever `token_endpoint`/`end_session_endpoint` says | `desktop/crates/client/src/server.rs:145-165`, `auth.rs:39-59,166-177,236-255,293-294` | Require https (unless loopback↔loopback), `metadata.issuer == authority`, endpoints same scheme+host as authority; show IdP host before opening browser |
| M-35 | Plain `http://` to non-loopback accepted; the only warning appears after the browser already opened; bearer then sent in cleartext on every call | `server.rs:68-106`, `connection.rs:52-57,214`, `signin.rs:116-124,201-207` | Reject http unless loopback, or explicit "allow insecure" confirmation before `discover` |

### Low / Info
- L-34 `host.starts_with("127.")` string test → `127.evil.com` defaults to http (`server.rs:93-95`). Parse as IP, `is_loopback()`.
- L-35 Unbounded response buffering: `read_file` base64, `response.text()`, SSE `pending` with no newline, `unbounded_channel` (`api/files.rs:117-131`, `connection.rs:231-299,340-351`, `sse.rs:43`). Cap and bounded channel.
- L-36 `SystemTime + Duration::from_secs(expires_in)` panics on huge `expires_in` from a rogue IdP (`auth.rs:198`). `checked_add` + clamp.
- L-37 `aikonos.exe` not Authenticode-signed; only SHA256SUMS cosign-signed (`.github/workflows/release.yml:17-20,358`). Add signtool / Azure Trusted Signing.
- L-38 Loopback sign-in listener serial accept; local process can stall callback ~10s per connection (`auth.rs:352-440`).
- L-39 `save_bytes` predictable temp name, follows pre-planted symlink, remove-then-rename window (`local_files.rs:106-119`). `create_new(true)` + random suffix; rename replaces on Windows.
- I-11 ID token not validated, `nonce` never compared (display-only; gateway validates bearer) (`auth.rs:88-93,302`). I-12 `TokenSet` derives `Debug` (`auth.rs:114-120`); implement redacting `Debug`.
- Reviewed OK: tokens in memory only; PKCE S256 + state enforced; `127.0.0.1` random-port listener; no `danger_accept_invalid_certs`; `open_link` restricted to http/https with tests; no auto-update; downloads to user-picked path only; `--locked` builds, pinned toolchain.

---

## 9. Deploy, supply chain, infra, policy files

### H-08 Compose defaults to `AIKONOS_DEV_MODE=true` with an empty OpenFGA store → silent allow-all
- **Status:** open
- **Where:** `compose.yaml:464,475`; `broker/internal/policy/fga.go:57`, `engine.go:374-381` (stub returns `(true, nil)`); `cmd/broker/guard.go:25-37` (fatal only when dev mode is false); `compose.azure.yaml` / `compose.onprem.yaml` do not override; `scripts/compose-seed-openfga.sh` (store id written to env, broker recreate is a manual step)
- **What:** The startup guard is the only thing stopping a degraded broker and the compose default switches it off. Every fresh deploy runs FGA allow-all until an operator seeds **and** recreates the broker. With OIDC also unset, every RPC trusts request-body identity and every admin gate passes.
- **Fix:** `AIKONOS_DEV_MODE: ${AIKONOS_DEV_MODE:-false}` and `AIKONOS_OIDC_ISSUER: ${AIKONOS_OIDC_ISSUER:?}` in `compose.yaml` (local example env already sets dev mode true explicitly). Broker refuses to start with empty FGA store / issuer / capability key unless an explicit dev flag. Seed script runs `docker compose up -d broker` or prints a loud blocker.

### H-09 Authorization source of truth reachable from low-trust networks
- **Status:** open
- **Where:** OpenFGA has no `OPENFGA_AUTHN_METHOD` anywhere (`compose.yaml:162-180`, `compose.onprem.yaml:38`); on-prem adds OpenFGA to `mesh` (`compose.onprem.yaml:55-60`) beside agent-gateway, webui, keycloak, nats and any attached MCP container; Grafana (`profiles:[obs]`) on `[obs, backend]` with admin password fallback `admin`, anonymous Viewer on, `3030:3000` on 0.0.0.0 (`compose.yaml:954-973`); `obs` network is **not** internal despite the comment (`compose.yaml:101-102`); Grafana Postgres datasource role has RLS `USING (true)` on six tables incl. `user_directory`, `agents`, `llm_usage_events` (`deploy/compose/migrate.sh:125-142`)
- **Exploit:** Any compromised mesh container (on-prem) POSTs `/stores/<id>/write` and grants itself tenant admin / `can_invoke`. With the obs profile, default Grafana admin + a proxied datasource reaches `http://openfga:8080`; anonymous Viewer reads all tenants' rows via `/api/ds/query` (plausible, unproven).
- **Fix:** `OPENFGA_AUTHN_METHOD=preshared` + key on OpenFGA and broker. Remove OpenFGA from `mesh` (use `docker compose exec` for admin). `GF_SECURITY_ADMIN_PASSWORD: ${GF_ADMIN_PASSWORD:?}`, `GF_AUTH_ANONYMOUS_ENABLED=false`, `127.0.0.1:3030:3000`, mark `obs` internal or put Grafana behind Traefik, drop Grafana from `backend`.

### Deploy — Medium
| ID | Finding | Where | Fix |
|---|---|---|---|
| M-36 | Azure/on-prem overlays leave dev-grade services published on all interfaces: Keycloak `start-dev` (H2, admin console 18080, realm import), NATS 4222/8222 (azure), broker 9090-9092, otel 4317/4318/8889, loki 3100, tempo 3200, prometheus 9095 (`auth_enabled: false`, remote-write receiver) | `compose.yaml:293-295,312,331-332,533-536,941-944,996,1015,1039`, `compose.azure.yaml:65-72`, `compose.onprem.yaml:69-70` | `ports: !reset []` or loopback binds in both overlays; gate Keycloak behind a profile off for Entra; publish nothing in obs except Grafana behind Traefik |
| M-37 | Keycloak realm: `sslRequired: none`, public client `redirectUris: ["*"]`, committed client secret with `directAccessGrantsEnabled`, seeded users with trivial password, no brute-force protection; imported on every variant | `deploy/compose/keycloak-realm.json:4,6,13,111-118,145-178` | Localhost-only redirect URIs; remove smoke client from shipped realm (dev profile); `sslRequired: external`, `bruteForceProtected: true`; don't import outside dev |
| M-38 | Internal mTLS uses the dev CA in every variant: 10-year certs, leaf keys `chmod 0644`, whole `tls/` dir (incl. `ca.key`) mounted into broker and gateway | `scripts/compose-dev-ca.sh:49,113,131`, `compose.yaml:518-529,552-553,665` | Mount only the files each service needs; CA key out of the tree; short-lived certs/SPIRE in azure/onprem; `0644` only behind a dev opt-in |
| M-39 | Git deploy hook fails open without an allowed-signers file and runs `docker compose build` from source as the deploy user (bypasses signed release images) | `scripts/git-deploy-hook.sh:60-79,220,243` | Default `AIKONOS_DEPLOY_REQUIRE_SIGNED=true`; deploy tags only; pull release images by digest |
| M-40 | Digest pinning is an optional overlay; Dockerfile `FROM` lines tag-only; `dev-ca-mint` is `alpine/openssl:latest` in the base file; `mcp-grafana` unpinned | `deploy/compose/compose.digests.yaml:3-48`, `compose.yaml:376,794`, all Dockerfiles | Pin in base files or make the digest overlay required in azure/onprem `COMPOSE_FILE`; digest `FROM` lines |
| M-41 | Vault AppRole `secret_id_ttl=0`, `num_uses=0`, written to `.env` and visible in `docker inspect`; Vault listener `tls_disable=1` in all variants; hardcoded root-token fallback literal in the seed script and README | `scripts/compose-vault-seed.sh:47,53-56,114-137`, `deploy/compose/vault/vault.hcl:27-30`, `deploy/compose/README.md:199` | TTL + rotation or response wrapping; TLS on the listener for azure/onprem; root token via stdin; remove the literal |
| M-42 | Backups world-readable, unencrypted, inside the repo tree; dev vault-data includes unseal key + root token; silent fallback to default DB/MinIO passwords | `scripts/compose-backup.sh:105-112,125-139,186-195`, `vault-init.sh:64` | `umask 077`, 0700 dir, age/gpg encryption, exclude `init.env`, fail on unset creds |
| M-43 | OTel collector runs as root with all container logs mounted; redaction not applied to the SIEM metrics pipeline; log-body redaction unverified; SIEM endpoint may be `http://` with Authorization in clear; OTLP 4317/4318 published unauthenticated | `compose.yaml:901,940`, `deploy/compose/obs/otel-collector-config-siem.yaml:61-78,103-105,119-122` | Transform/redact log bodies; redaction on metrics; non-root collector; require https; loopback-bind receivers |
| M-44 | CI: `ci.yml` and `desktop.yml` have no top-level `permissions:` and pin actions by mutable tag (release.yml is strict) | `.github/workflows/ci.yml:1-22,88,327`, `desktop.yml:6-64` | `permissions: contents: read`; SHA-pin as `release.yml` does |

### Deploy — Low
- L-40 Postgres/OpenFGA/Grafana DB connections `sslmode=disable`; DB password embedded in `OPENFGA_DATASTORE_URI` (`compose.yaml:153,172`, `obs/grafana/provisioning/datasources/datasources.yaml:63-65`).
- L-41 NATS unauthenticated on `mesh`; carries the audit stream (`compose.yaml:281-302,645`). Enable user/token auth with split pub/sub perms.
- L-42 Traefik (azure) mounts docker.sock read-only; `trustForwardHeader=true` (`compose.azure.yaml:105-108,164`). Socket proxy; drop trustForwardHeader at the edge.
- L-43 `provision.sh` hardcodes subscription/RG defaults and decides the SSH allow /32 from an `ipify` lookup (`deploy/azure/provision.sh:13-14,29,34`). Explicit inputs.
- L-44 `aikonosctl` f-string SQL into `psql -c`; fixed `/tmp/aikonosctl-plan-input.json`; default MinIO creds fallback (`scripts/aikonosctl:156,191,267-268,299-300`). `psql -v`, `NamedTemporaryFile`.
- L-45 Root `.dockerignore` excludes only `.env.local`; broker builder stage `COPY . .` pulls `.env*`, `deploy/compose/tls/*.key`, `backups/` into build context/cache (`.dockerignore:14`, `broker/Dockerfile:26`).
- L-46 Vault broker policy grants `update` on the four signing-key paths (broker compromise can rotate audit/Biscuit keys); wildcards span all tenants (`deploy/compose/vault-broker-policy.hcl:18-75`). Drop `update` if cas=0 create-then-read suffices.
- Reviewed OK: `release.yml` (`permissions: {}`, SHA-pinned, keyless cosign, SLSA, SBOM, vuln gate); `x-hardening` anchor on every first-party service; `backend`/`office` internal; non-root everywhere; `vault-init.sh` umask 077; `migrate.sh` `NOSUPERUSER NOBYPASSRLS` + `-v` binding; OPA rules default-deny with required `fga_decision`; `model.fga` `can_invoke` only via `permitted_group`; demo tuples opt-in; `doc.write`/`email.draft` skills correctly scoped; OPA decision log disabled; cloud-init writes no secrets; NSG 80/443 + SSH /32.

---

## 10. Cross-cutting patterns (for the fix PRs)

1. **Request-supplied identity** (C-01, H-01, H-02, H-04, H-10, M-01, M-07): every north handler must start with `callerIdentity`; every south handler must bind to the task row and the verified peer SPIFFE id. Add a lint/test that greps `req\.(UserId|FromUserId|ApproverUserId|TenantId|SandboxSpiffeId|AgentId)` outside `callerIdentity`/`resolveTenant`.
2. **Fail-open on missing config** (H-08, M-05, M-21, L-11, L-19, M-39): empty issuer / FGA store / signing key / allowed-signers must refuse to start outside an explicit dev flag.
3. **Trust in self-declared risk signals** (H-11, M-08, M-13, M-16, M-17, L-06): effect class, DLP attestation, cost, read-only hints and budgets are all chosen by the party being governed. Derive server-side or floor.
4. **Capability tokens are scope-only** (H-03, M-10, M-15): TTL + task + args binding turns most Mediums in §3 into non-issues.
5. **Durable audit** (M-18 to M-21, L-09, L-15): WAL/outbox, chain in worker order, verify newest-first, signed checkpoints, Compliance-mode WORM.
6. **Network exposure in non-dev variants** (H-09, M-33, M-36, M-38, M-41, M-43, L-40 to L-42): the azure/onprem overlays should `!reset` every dev port and enable authn on OpenFGA, NATS, Vault TLS.

## 11. Audit process notes

- Reader agents did not open any `.env*` file. One verification grep matched `deploy/compose/.env.*.example` and printed its `AIKONOS_DEV_MODE` lines only (no secrets); `GF_ADMIN_PASSWORD` overrides in those templates were therefore left unverified.
- No secret values appear in this document; where a hardcoded credential exists only its location is named.
- Not read in depth: `broker/internal/workflow/workflow.go` execution semantics, `connector/oauth.go`, `notify/nats_bus.go` subject authz, `config/store.go`, `office_*` broker handlers, `desktop` `cron.rs`/`transcript.rs`/`chat/*`, `vue-stream-markdown` internals (node_modules absent). These are candidates for a follow-up pass.

## 12. Implementation log

- **2026-10-27, branch `security/identity-binding` (uncommitted):** C-01, H-01, H-02, H-03, H-04 and M-07 fixed with regression tests; `go test ./broker/...` green (37 packages).
  - Follow-up surfaced by H-02: interactive tasks are never bound to an agent (north `CreateTaskRequest` has no agent field), so for an *unbound* task the MCP `can_access` subject is still the caller's `agent_id` (pre-existing; marked with a `ponytail:` comment in `service_invoke.go`). Closing it needs the agent bound at task creation (proto change).
    Closed by AG-01 (2026-10-28): every task binds a registered agent (the tenant default agent when none is named), and `InvokeTool` checks the MCP connector for the bound agent only; a caller agent claim that differs is refused.
  - H-04: `spiffe.sandbox_path_prefix` left at `""` because the interceptor applies the prefix to every south caller including the gateway. `requireGatewayPeer` now fails closed when `scheduler.gateway_spiffe_id` is empty outside dev mode (viper default already set). SubmitPlan now refuses a `sandbox_spiffe_id` that differs from the verified peer; the gateway's default, the broker's default and the dev CA leaf all agree on `spiffe://aikonos.com/agent-gateway`.
  - H-03: tokens minted before this change carry no `expires` fact and are now refused (no migration needed; they were never persisted server-side).
  - Pre-existing: `gofmt -l broker/` lists ~40 files unformatted at HEAD; only files touched here were formatted.
