# 20 — Security Audit and Fixes (whole-codebase audit, 2026-10-29)

> Scope: every service and config domain at baseline `0ef97ce` (v0.7.4 plus the
> agent-identity work): Go broker, agent-gateway (TS), webui (Vue
> SPA + Fastify proxy), office-worker + docs-mcp (Node), desktop (Rust),
> deploy/compose/CI/scripts, OPA + OpenFGA policies, bundled skills. Open items
> of the previous audit ([17-security-findings.md](17-security-findings.md),
> 2026-10-27 at `859e060`) were re-checked; where one is still open or regressed
> it appears here with its old id in the `prior` column, and its status in
> docs/17 is updated.
> Fixes: integration branch `security/audit-2026-10-29`. Waves 1 and 2 are
> described at `86c86a9`; the statuses below are final after the wave-3 review
> fixes (`376a115`) and the merge-seam fixes (`6728508`), see
> [Wave 3 review fixes](#wave-3-review-fixes); ships in v0.7.5. Operators: read
> [OPS-RUNBOOK "Upgrading to 0.7.5"](OPS-RUNBOOK.md#upgrading-to-075) before the
> first `up`.
>
> Tally by severity (241 findings): 0 Critical, 7 High, 81 Medium, 144 Low, 9 Info.
> Tally by fix status (`ledger.json`): 164 fixed, 20 partial, 46 deferred, 9 refuted, 2 skipped.
> Audit verification: 217 confirmed, 15 contested (split vote; 2 fixed anyway, 13 deferred), 9 refuted.
> No secret values appear in this document; where a source quotes one, only its
> location is named or it is written `[REDACTED]`.

| severity | fixed | partial | deferred | refuted | skipped | total |
|---|---|---|---|---|---|---|
| critical | 0 | 0 | 0 | 0 | 0 | 0 |
| high | 5 | 1 | 0 | 1 | 0 | 7 |
| medium | 54 | 10 | 16 | 0 | 1 | 81 |
| low | 100 | 9 | 28 | 6 | 1 | 144 |
| info | 5 | 0 | 2 | 2 | 0 | 9 |
| **total** | 164 | 20 | 46 | 9 | 2 | 241 |

## Method

1. **Audit.** Eight read-only domain audits by independent readers, covering
   broker identity/auth, broker workflows + scheduler, broker plan/invoke/policy,
   broker tool proxy, audit chain, limits/workspace, agent-gateway runs,
   agent-gateway IPC/external API, webui, office-worker + docs-mcp, compose/env,
   scripts/CI/policies, and desktop. Prior audits
   ([11](11-security-audit.md), [13](13-zero-trust-audit.md)) and the accepted
   risks in [04-threat-model.md](04-threat-model.md) were not re-reported.
2. **Verification.** Every finding went to independent refuters, one per lens,
   scaled by claimed severity: High to three (code reality, exploitability,
   threat-model fit), Medium to two (code reality, exploitability), Low and Info
   to one (code reality). The majority of the recorded votes decides; a 1–1
   split is `contested`. Duplicates were merged (53 source findings
   folded into survivors). 241 findings remain; the 9 refuted ones are kept for
   traceability. Ids are `F*` and `G*` and are not contiguous (merged ids are gone).
3. **Fix, wave 1.** 13 file-disjoint packages (p1…p8), each implemented by an
   independent implementer in its own git worktree with regression tests, then
   merged into the integration branch.
4. **Fix, wave 2.** Three packages that needed proto or cross-package changes:
   w2-p9 (owner grants on south reads and gateway votes), w2-p10 (compose
   integration of the wave-1 settings), w2-p11 (broker leftovers).
5. **Wave 3.** One adversarial reviewer per package plus a merge-seam reviewer
   re-verified every fix on the merged branch at `86c86a9`; implementers applied
   the review fixes. Statuses were then re-decided from the reviewer's verdict
   (Confidentiality > Integrity > Availability, never claiming `fixed` while the
   reviewer's gap is open). Results: [Wave 3 review fixes](#wave-3-review-fixes).
6. **Tests at `86c86a9`.** Go: `go test ./broker/...` 39 packages; agent-gateway
   typecheck + 1495 tests; webui 1334 + proxy 33; docs-mcp 38; office-worker 80
   (9 skipped, no soffice); OPA 82/82; OpenFGA 25/25; `docker compose config`
   for local, azure and onprem; env-drift and compose-posture checks; release
   scripts 78; deploy hook 34; replay 4; aikonosctl 17; `cargo check` + 65
   desktop client tests. DB-backed Go tests skip without `TEST_DATABASE_URL`.
   Wave-3 test runs are per package (`wave3.json`); see the Decisions under
   [Wave 3 review fixes](#wave-3-review-fixes) for what did not run locally.

| pkg | wave | scope | commit | wave-3 fix | fixed | partial | skipped |
|---|---|---|---|---|---|---|---|
| p1 | 1 | broker identity: OIDC, SCIM, revocation, grants, connectors, Vault client | `544c65e` | `79dada9` | 25 | 2 | 0 |
| p2a | 1 | broker workflows and scheduler | `1ee5d65` | `f5a200e` | 9 | 0 | 0 |
| p2b | 1 | broker plan, invoke, policy engine, providers, org settings | `93ac0f8` | `2ef76f9` | 16 | 2 | 0 |
| p2c | 1 | broker tool proxy: web.fetch, MCP client, OneDrive, workspace seam | `99add62` | `d028499` | 8 | 0 | 0 |
| p3a | 1 | audit chain, verifier, admin audit | `4257ea7` | `4f24d20` | 5 | 1 | 0 |
| p3b | 1 | limits, workspace store, alerting, baselines, startup guard | `d41f0f7` | `23ac6f1` | 13 | 5 | 0 |
| p4a | 1 | gateway runs: /agui, subagents, scheduler ticker, workflows | `b64e157` | `ae973c2` | 7 | 0 | 0 |
| p4b | 1 | gateway IPC, external API, audit stream, skills | `71e8085` | `73fe67e` | 15 | 4 | 1 |
| p5 | 1 | webui SPA and proxy | `adff2cf` | `2eb0b34`, `4581d34` | 12 | 0 | 0 |
| p6 | 1 | office-worker, docs-mcp, broker office client | `12cb43f` | `4a1525a` | 8 | 2 | 0 |
| p7a | 1 | compose, env templates, Dockerfiles, obs config | `3c5e10d` | `f236c51` | 10 | 4 | 1 |
| p7b | 1 | scripts, CI, Vault policy, migrate.sh, OPA/FGA tests | `70b3689` | `f57e9ab` | 10 | 0 | 0 |
| p8 | 1 | desktop client | `072ae54` | `ce48372`, `c3af867` | 12 | 0 | 0 |
| w2-p9 | 2 | proto: owner grants on south reads and gateway votes, MintSessionGrant, memory hash, SCIM expiry | `a7649ed` | `49d7744` | 9 | 0 | 0 |
| w2-p10 | 2 | compose integration of wave-1 settings, OPS-RUNBOOK upgrade section | `3c2f621` | `b0a5e5f` | 0 | 0 | 0 |
| w2-p11 | 2 | broker leftovers: negative cost, Vault 403 race, audit error text, admin audit values, web-search config | `36fa728` | `57d8a9f` | 5 | 0 | 0 |

Schema and API changes: migrations `062_scheduled_runs_pin_claim` (F18, F172,
F242), `063_plan_step_consumed` (F16), `064_scim_token_expiry` (F13),
`065_alerts_retention` (F34). Proto: `owner_grant` on the south read requests
and `ApproveGatewayTaskRequest`, north RPC `MintSessionGrant`, `content_sha256`
on memory concept get/verify, `ScimToken.expires_at`;
`ClaimDueScheduledRunsRequest.now` is deprecated and ignored.

## Operator-visible changes

The runbook section [Upgrading to 0.7.5](OPS-RUNBOOK.md#upgrading-to-075) has
the step-by-step list. What an operator must know:

- **Deploy broker and gateway together, broker first.** Outside dev mode the
  broker refuses south reads and gateway approval votes without an owner grant,
  and the gateway calls the new north RPC `MintSessionGrant` (w2-p9). Owner
  grants are v3; a 0.7.4 broker refuses them, so upgrade every broker replica
  at once.
- **`AIKONOS_TENANT_ID` is required on the broker** (compose `:?`) and must equal
  the tenant claim in users' tokens (Entra `tid`), or every north call is denied
  as a foreign tenant (F10).
- **New required secrets:** `AIKONOS_NATS_BROKER_PASSWORD` and
  `AIKONOS_NATS_GATEWAY_PASSWORD` (two NATS users with subject permissions since
  wave 3; compose builds each client's URL, `AIKONOS_NATS_URL` is no longer read
  from `.env`) and `AIKONOS_OFFICE_WORKER_TOKEN` (same value for broker and
  office-worker; office-worker refuses to start without it unless dev mode).
  Outside dev mode the broker refuses a placeholder or short NATS password or
  office token.
- **Dev mode is off by default** on broker and gateway. Outside dev mode:
  the gateway needs `AIKONOS_OIDC_ISSUER` and `AIKONOS_OIDC_JWKS_URL`; the broker
  refuses to start with a placeholder or short (<32 B) audit signing key,
  gateway grant key, API key pepper or session key, without a durable audit
  store (`AIKONOS_AUDIT_MINIO_ENDPOINT`, `AIKONOS_AUDIT_BUCKET`,
  `AIKONOS_AUDIT_ACCESS_KEY`, `AIKONOS_AUDIT_SECRET_KEY`), without a signing key,
  or when Vault is configured but fails. Unseal Vault before the broker starts.
- **Network exposure.** azure/onprem publish only Traefik 80/443 and Grafana on
  `127.0.0.1:3030`; OpenFGA is backend-only (`docker compose run --rm fga-cli`).
  The policy bundle binds loopback unless `AIKONOS_POLICY_BUNDLE_HTTP_ADDR` is set
  (compose sets `:9092`; an OPA outside this compose file needs the same).
  docs-mcp listens on `HOST` (compose `0.0.0.0` in the mesh, published on
  `127.0.0.1:8060` in local) behind a Host/Origin allowlist (`ALLOWED_HOSTS`,
  `ALLOWED_ORIGINS`).
- **External API `:8090`.** `AIKONOS_EXTERNAL_TRUST_PROXY` is the number of
  proxies (or their IPs/CIDRs) in front of it: `0` in local, `1` behind the
  overlays' Traefik; `true` is refused. A per-agent bucket applies after auth.
- **webui CSP.** A bundle that bakes `VITE_OIDC_AUTHORITY` must also set
  `AIKONOS_WEBUI_OIDC_AUTHORITY` at runtime, or the CSP blocks the IdP; silent
  renew frames `AIKONOS_WEBUI_OIDC_REDIRECT_URI` (default `/auth/callback`).
- **Recovery switches** (all default off, each logged; turn off again after use):
  `AIKONOS_WORKSPACE_ALLOW_UNSIGNED_SESSIONS` (sessions saved before 0.7.5 are
  refused until re-saved), `AIKONOS_VAULT_ALLOW_INSECURE_HTTP` (http Vault on a
  routable host; compose's `http://vault:8200` is unaffected),
  `AIKONOS_TOOLPROXY_MCP_ALLOW_PRIVATE` (http or private MCP servers; stored
  `http://` MCP connections otherwise fail until edited to https),
  `AIKONOS_DEPLOY_REQUIRE_SIGNED=false` (first-time hook setup),
  `AIKONOS_DEPLOY_ALLOW_ROLLBACK=1` (deliberate rollback),
  `AIKONOS_DESKTOP_ALLOW_HTTP=1` (per PC). `AIKONOS_DEV_MODE=true` is the last
  resort and relaxes every guard at once.
- **Added in wave 3** (runbook has the steps): re-run
  `scripts/compose-vault-seed.sh` so Vault gets the create/read-only policy
  (F115); re-install the on-prem deploy hook and set
  `receive.denyNonFastForwards`/`denyDeletes` (F228); find or clear legacy
  schedule pre-approvals with the runbook query (F211); the broker caps gRPC at
  64 connections per IP and 1000 streams per connection
  (`AIKONOS_BROKER_GRPC_MAX_CONNS_PER_IP`, `AIKONOS_BROKER_GRPC_MAX_STREAMS`, not
  passed by compose); `AIKONOS_HOST_UID`/`AIKONOS_HOST_GID` must be the checkout
  owner for the dev-CA mint; with `AIKONOS_WORKSPACE_ALLOW_UNSIGNED_SESSIONS` on,
  each start re-signs legacy sessions whose old sidecar verifies.
- **New tunables:** `AIKONOS_WORKSPACE_QUOTA_BYTES` (2 GiB) and
  `AIKONOS_WORKSPACE_QUOTA_ENTRIES` (20000; `0` = off),
  `AIKONOS_ALERTING_DENIAL_COOLDOWN_SECONDS` (60) and
  `AIKONOS_ALERTING_RETENTION_DAYS` (90; neither is passed by compose),
  `OFFICE_SCRIPT_MAX_AS_MB` (1024; not compose-substituted).
- **Behaviour users will notice.**
  - Each approved plan step runs once; a transient tool failure needs a replan
    or a new task (F16).
  - Publishing a workflow version parks every schedule pinned to it as
    `workflow_changed`; the owner edits the schedule to re-confirm (F18).
    Revoke-all parks the user's schedules as `subject_revoked` (extend the
    delegation, then resume; F242); losing `skill:scheduler` parks them too (F192).
  - New schedules carry no pre-approved tools unless ticked; existing schedules
    keep any stored baseline, now visible per row (F211).
  - SCIM tokens expire after 365 days (existing ones 365 days after the upgrade);
    SCIM no longer adopts a group that already has members or grants (409).
  - Memory verify is bound to the reviewed content hash; a legacy concept with
    more than 32 tags or a tag longer than 64 characters cannot be verified or
    deprecated (delete or rewrite it).
  - An interactive session grant lives `AIKONOS_GATEWAY_GRANT_TTL` (default 1 h);
    a single chat run longer than that loses its south reads mid-run.
  - An IdP outage longer than 10 min + 1 h makes every token fail validation
    until the IdP is back (F248, intended).
  - `aikonosctl kill-switch user` requires `--tenant <uuid>`.
  - The Windows app refuses plain http to a non-loopback server or IdP and
    refuses split-host IdPs (same scheme and host required).

## Findings

One table per verification domain, rows sorted by severity then id. `prior` is
the docs/17 id from the mapping; `package` is the fixing package (`—` for
deferred and refuted); `note` summarises the fix ledger.

### Broker: identity, authentication, grants (`broker-identity`)

42 findings: 30 fixed, 2 partial, 7 deferred, 3 refuted.

| id | sev | prior | title | package | status | note |
|---|---|---|---|---|---|---|
| F2 | medium |  | client_request_id idempotent replay writes FGA owner+approver tuples for the caller on another user's task | p1 | fixed | A client_request_id held by another owner returns AlreadyExists with an audited DENY; no victim ids leak and no FGA tuples are written. |
| F3 | medium |  | External-agent user assertion skips the revocation epoch: a revoked user's old token still mints an owner grant | p1 | fixed | User assertions now honour the user's revocation epoch and block; a failed state read fails closed (Unavailable). |
| F4 | medium |  | SCIM group creation can claim an existing admin-managed FGA group name and add arbitrary members to it | p1 | fixed | SCIM refuses (409, audited) a group whose slug already has FGA tuples; an FGA read error counts as in use. |
| F5 | medium | M-06 | ApproveGatewayTask lets the gateway SVID vote as the task owner on any task (no gateway-managed or grant check) | w2-p9 | fixed | ApproveGatewayTask requires a gateway-managed task and an owner grant naming the task owner; refusals are audited. |
| F6 | medium | M-02 | JIT provisioning grants groups from the unverified email / preferred_username claim | — | deferred | JIT email-rule grants need email_verified or oid-keyed rules; SCIM mode already closes it. |
| F136 | medium | H-10 | StreamTaskEvents and gRPC reflection still bypass OIDC, tenant and revocation interceptors | p1 | fixed | Stream interceptor chain added; StreamTaskEvents uses the GetTaskState readability check; gRPC reflection only in dev mode. |
| F157 | medium |  | External subject assertion is the user's full north bearer: replayable by the agent holder against the whole north API (self-approval) | — | deferred | The external user assertion is the full north bearer by design (AG-17); scoped assertions are a follow-up. |
| F171 | medium |  | South read RPCs (personal skills, memory, user/agent skills, MCP OAuth tool listing) act as a gateway-asserted user_id with no owner grant | w2-p9 | fixed | South reads take tenant and user from a verified owner grant (MintSessionGrant for sessions, TTL capped at the bearer's); agent memory recall needs can_use; ResolveRunAgent still trusts user_id (next proto wave). |
| F242 | medium |  | User 'Revoke sessions' (revoke-all) does not park the user's standing schedules, so a stolen-session persistence mechanism survives the documented incident-response step | p2a | fixed | Revoke-all parks the user's schedules as subject_revoked (delegated_at vs revoked_before), audited. |
| G45 | medium |  | User assertion for external agents ignores the user's revoke-all epoch, so RevokeUserSessions does not cut a stolen IdP token on the ResolveAgentApiKey path | p1 | fixed | Duplicate of F3; same fix. |
| F8 | low | L-01 | OIDC validator accepts tokens without exp and holds the JWKS write lock across HTTP (DoS during IdP outage) | p1 | fixed | Tokens without exp are rejected; the JWKS fetch no longer holds the validator lock and failed fetches back off. |
| F10 | low | M-03 | Admin gate pinned to the configured tenant object instead of the token's tenant | p1 | fixed | TenantInterceptor compares the token tenant with AIKONOS_TENANT_ID; a mismatch is denied and audited. |
| F11 | low |  | Identity hardening switches default to audit (identity.obo, identity.enforce_user_bound) | — | refuted | Audit-mode defaults are a documented, tenant-configurable rollout setting, not a vulnerability. |
| F12 | low |  | ClaimDueScheduledRuns trusts a gateway-supplied clock for delegation expiry and due-time | w2-p9 | fixed | ClaimDueScheduledRunsRequest.now is deprecated and ignored; the broker clock decides (behaviour since wave 1). |
| F13 | low |  | SCIM bearer tokens never expire and every bad token writes an audit event with attacker-controlled path | w2-p9 | fixed | SCIM tokens expire after 365 days (migration 064); ScimToken.expires_at is shown in the admin UI. |
| F14 | low |  | Assertion jti replay cache is in-process only | — | deferred | jti replay cache is per process; run one broker replica. |
| F133 | low |  | SCIM externalId is not normalised, so a case/format mismatch with the token subject makes deprovisioning fail open | p1 | partial | SCIM acts on the canonical lower-case GUID and the stored form; a block written before the upgrade covers only the stored form until the next SCIM write. |
| F134 | low |  | SCIM reactivation can silently lift an admin's manual block (block ownership is first-writer-wins) | p1 | fixed | An admin block now replaces a SCIM block, so SCIM reactivation cannot lift it. |
| F140 | low |  | Per-user MCP OAuth: begin is not gated by connector access, tokens are not revoked upstream or purged on connection delete | p1 | partial | MCP OAuth begin needs can_access and tokens bind to their token URL; upstream revoke and purge on delete remain. |
| F165 | low |  | Delegation envelopes can be accepted after expiry (and after the sender is blocked) | p1 | fixed | Expired envelopes cannot be accepted, and an envelope from a since-blocked sender is refused (audited). |
| F172 | low |  | ReportScheduledRunResult lets the gateway terminate any one-off run and plant a broker-signed session record in any run owner's workspace | p2a | fixed | Run reports need a broker-stamped claim; session records are JSON- and size-checked (1 MiB). |
| F173 | low |  | JIT provisioning interceptor runs before the tenant and revocation interceptors, so blocked, session-revoked and tenant-less callers still get group grants | p1 | fixed | North chain is now OIDC, tenant, revocation, provisioner, audit; tenant-less identities are not provisioned. |
| F174 | low |  | ResolveApproval returns 'already resolved' before the approver-set check, and Resolve then mints capability tokens for whoever wins the Transition | p1 | fixed | The approver-set check runs before the resolved-state check; a lost race mints no tokens. |
| F175 | low | L-10 | L-10 still open: principal can alias the group memory segment via 'group-' subjects and SanitizeSeg is many-to-one | p1 | fixed | SanitizeSeg is one-to-one and group-* principals, externalIds and assertion subjects are rejected. |
| F176 | low | L-14 | L-14 still open: Vault client follows redirects carrying X-Vault-Token; login holds the client mutex across HTTP; http:// accepted silently | p1 | fixed | Vault client refuses redirects, re-logs in on 403, and refuses http:// to routable hosts unless opted in. |
| F178 | low |  | Revocation cache can store a pre-block state after reset(), so a block/revoke-all does not land immediately on the writing replica | p1 | fixed | Revocation cache uses a generation counter, so a load racing a block is not cached. |
| F179 | low |  | Unbounded string inputs on task RPCs (prompt, client_request_id, reason) are stored and published without length limits | p3b | fixed | prompt, client_request_id and reason are length-capped on the shared task paths. |
| F180 | low |  | Non-admin RPCs reflect raw backend error text (FGA URL/store id, pgx errors) to the caller | p1 | fixed | Member-reachable handlers return a fixed Internal message and log the backend error. |
| F181 | low |  | BeginConnectorAuth lets the caller choose OAuth scopes beyond the plugin defaults | — | refuted | Only the caller's own account is affected and the shipped client sends no scopes. |
| F182 | low | L-02 | L-02 still open: principal derivation silently falls back oid -> sub and PrincipalID() -> email -> SPIFFE id | — | deferred | oid -> sub fallback chain is a design choice; SCIM mode keys on oid. |
| F183 | low |  | Owner grants are stateless 1 h multi-use credentials that are not voided by revoking the API key, public key, or schedule that produced them | — | deferred | 1 h stateless owner grants accepted; F244 fixed the TTL comparison. |
| F184 | low |  | Gateway grant key falls back to an ephemeral per-process key outside the dev guard, and a Vault-loaded key is not length-checked | — | deferred | Covered outside dev mode by the weak-secret refusal (F199). |
| F244 | low |  | Owner-grant revocation compares against 'issued = expiry - current GatewayGrantTTL', so a TTL reduction lets pre-revocation grants outlive a revoke-all | p1 | fixed | Owner grants are v3 with a signed issue time; v1/v2 grants are dated conservatively. |
| F248 | low |  | Broker OIDC validator caches the JWKS forever: a rotated-out or compromised signing key stays trusted until restart | p1 | fixed | JWKS has a 10 min MaxAge and a 1 h stale grace, then validation fails closed. |
| G7 | low |  | M365 entra_tenant_id and client_id stored without validation and interpolated into the token endpoint path | p1 | fixed | M365 tenant and client ids are charset-checked and path-escaped, also for stored values. |
| G10 | low | M-03 | Admin gate is pinned to the single configured tenant object, not the token's tenant - M-03 still open | p1 | fixed | Duplicate of F10; same fix. |
| G22 | low |  | Token refresh write-back is an unsynchronised read-modify-write: concurrent refresh can kill rotating-refresh-token connections, and a refresh racing Disconnect resurrects the deleted Vault secret | p1 | fixed | A per-(tenant, user, connector) lock serialises token refresh, write-back and revoke. |
| G23 | low |  | Disconnect does not revoke the access token when no refresh token is stored, cannot revoke at Microsoft, uses an unbounded default HTTP client, and audits success regardless of upstream outcome | p1 | fixed | Disconnect revokes the access token when no refresh token exists, bounds the call to 10 s, and audits upstream failure. |
| G27 | low |  | OBO bootstrap forwards the caller's raw bearer to Entra without local iss/aud/tid binding or validation of the admin-supplied Entra tenant id | — | deferred | Entra validates the bearer; local iss/aud/tid binding is a follow-up. |
| G32 | low | L-10 | regression: connector reference store maps distinct users to one directory (SanitizeSeg many-to-one, L-10), cross-user status/metadata disclosure and overwrite | p1 | fixed | One-to-one SanitizeSeg; legacy directories move once at startup to their single known owner, shared ones are quarantined. |
| G43 | low |  | No default-deny method table in interceptors: every new RPC is open to any OIDC member (north) or any SVID (south) unless the handler remembers to gate itself | — | refuted | Missing defence-in-depth table, no exploitable RPC at HEAD. |
| G47 | low |  | JIT provisioner runs before the tenant and revocation interceptors, so blocked, revoked or tenant-less identities still get group grants | p1 | fixed | Duplicate of F173; same fix. |

### Broker: plan, invoke, policy, workflows (`broker-policy-exec`)

31 findings: 26 fixed, 1 partial, 4 deferred.

| id | sev | prior | title | package | status | note |
|---|---|---|---|---|---|---|
| F15 | high |  | Any skill:workflows user can overwrite or take over another user's workflow lineage (no ownership check on SaveWorkflow edit path) | p2a | fixed | SaveWorkflow's edit path is owner-only; denials are audited and suggest a fork. |
| F16 | high | H-03 | Capability token is scope-only: invoke is not bound to the approved tool, step or args, and a connector-wide MCP scope lets an approved read-only call run any other tool on the connector | p2b | fixed | InvokeTool claims the approved plan_steps row atomically (tool, args_hash, decision) and marks it consumed, so a step runs once (migration 063). |
| F17 | medium |  | GetWorkflow / schedule creation apply no ownership or visibility check, exposing any user's private workflow definition by lineage id | p2a | fixed | One gate (loadAccessible) serves Get, Pin and schedule creation; missing and denied both answer NotFound, audited. |
| F18 | medium |  | Workflow-mode schedule pre-approves tool steps of whatever version resolves at fire time, not the version the owner reviewed | p2a | fixed | Schedules pin workflow version and definition hash (migration 062); a changed version parks the schedule as workflow_changed. |
| F21 | medium | M-12 | regression: rate-limit policy reload replaces the global limiter policy set with one tenant's rows (M-12 still open) | p2b | fixed | Rate-limit reload uses ListAll, so one tenant's edit no longer replaces the global policy set. |
| F22 | medium | H-04 | regression: SubmitPlan, InvokeTool and EmitStatus still accept any trust-domain SVID; H-04 gateway-peer fix not applied | p2b | fixed | SubmitPlan, InvokeTool and EmitStatus start with requireGatewayPeer. |
| F186 | medium |  | Per-user MCP OAuth tokens are not bound to the connection URL, issuer or resource; changing the URL sends every user's token to the new server | — | deferred | Contested; binding per-user MCP tokens to the connection URL is a follow-up (F140 binds the token URL). |
| F187 | medium |  | Tenant admin can fully rewrite another user's scheduled run (prompt, approved_tools, actor) and it fires under the owner's grant and pre-approvals | p2a | fixed | Only the owner can fully edit a schedule; admins keep pause, resume, revoke and delete; denials audited. |
| G1 | medium | M-14 | Stored LLM provider key can be sent to an admin-chosen endpoint (TestLlmProvider and Upsert endpoint swap) - M-14 still open | p2b | fixed | The stored provider key is used only for the saved destination; retargeting is refused and audited. |
| G4 | medium |  | Agent allowed_providers and preferred_provider are never enforced, and GetAgentSpec drops them | p2b | fixed | GetAgentSpec returns allowed and preferred providers; the gateway enforces them (F203). |
| G5 | medium |  | Org connector allowlist (A7) enforced only when starting OAuth, not at completion or token use | p2b | fixed | The org connector allowlist is checked at Begin, Complete and on every gdrive/onedrive InvokeTool call, fail closed. |
| G21 | medium | H-03 | regression: InvokeTool does not bind call args to the approved plan step (H-03 args_hash still deferred) - gdrive/onedrive write and read run with any args | p2b | fixed | Duplicate of F16; call args are bound to the approved step's args_hash. |
| G42 | medium | H-04 | regression: EmitStatus accepts any trust-domain SVID, no gateway-peer or task binding to the caller (H-04 residual) | p2b | fixed | EmitStatus starts with requireGatewayPeer. |
| G44 | medium |  | Client-request-id idempotency key is tenant-wide, so a replay by another user returns the victim's task id and writes owner and approver FGA tuples for the replayer | p1 | fixed | Duplicate of F2; same fix. |
| G46 | medium | M-06 | ApproveGatewayTask can approve any task of the tenant as its owner: no gateway-managed check, no owner grant, no unattended policy in the broker (M-06 still open, wider than documented) | w2-p9 | fixed | Duplicate of F5 (M-06); same fix. |
| F24 | low |  | web.fetch SSRF guard omits Azure WireServer and other special-use ranges | p2c | fixed | SSRF guard adds WireServer, CGNAT, documentation, benchmarking, reserved and NAT64/6to4 ranges. |
| F25 | low |  | Skill overlay cache can silently miss an admin disable: stale-load race and fail-open on load error | p2b | fixed | Overlay cache uses a per-tenant generation and never caches a failed or raced load; InvokeTool fails closed. |
| F26 | low | L-05 | regression: MCP client reads unbounded response bodies and fails open when the tool is not in the server's advertised list (L-05 still open) | p2c | fixed | MCP responses are capped at 8 MiB and a tool missing from tools/list is refused. |
| F27 | low | L-06 | regression: LLM spend accounting trusts gateway-reported tokens/cost; negative or non-finite values reduce the spend meter (L-06 still open) | p2b | fixed | EmitLlmUsage rejects negative, non-finite and oversized values; cost is clamped at zero. |
| F188 | low |  | InvokeTool re-checks user skill grants and agent skills only for mcp: and memory-group calls; built-in tools run on the plan-time decision for the token's 30-minute reusable lifetime | p2b | fixed | Built-in tools re-check agent skills and the subject's can_invoke on every call, fail closed. |
| F189 | low |  | MCP connection URL is not required to be https; bearer and per-user OAuth tokens can be sent in cleartext | p2c | fixed | MCP URLs must be https (http only for loopback or AllowPrivate), checked on save and before every request. |
| F190 | low |  | RateWorkflowRun and ListWorkflowVersions apply no ownership or visibility check (audit forgery, version metadata disclosure) | p2a | fixed | Rate and ListVersions use the same visibility gate as Get. |
| F191 | low |  | SendSkillTransfer lets any skill:personal-skills holder push a skill envelope to any holder, bypassing the delegation graph | p2a | fixed | Direct skill transfers need a delegation edge or a shared delegatable group; denials audited. |
| F192 | low |  | Schedules keep firing, and can be edited and re-extended, after skill:scheduler is revoked | p2a | fixed | Losing skill:scheduler parks the owner's schedules and blocks extend, resume and edit. |
| F240 | low |  | Approver-set expansion recurses through nested groups with no visited set or depth cap | p2b | fixed | Approver-set expansion keeps a visited set and caps nesting depth at 5. |
| F251 | low |  | Injection-pattern scan covers only office *.extract and memory reads; web.fetch, MCP, cloud-drive and workspace reads return external content unscanned and unaudited | — | deferred | Injection-scan coverage for web.fetch, MCP and drive results is a design follow-up. |
| G6 | low |  | UpdateOrgSettings accepts unvalidated and unbounded input; preamble silently truncated at render, effect classes and allowlist unchecked | w2-p11 | fixed | Org settings validated in wave 1; UpsertWebSearchConfig rejects unknown engines and max_results above 50, and stored values are clamped. |
| G9 | low |  | Tenant config keys documented as 'can only tighten' can be lowered by a single admin PUT | — | deferred | 'Can only tighten' config keys need a schema-level rule. |
| G24 | low |  | Org connector allowlist is enforced only at BeginConnectorAuth, not at Complete, at token use, or for the managed OneDrive OBO path; denials are not audited | p2b | partial | Complete and InvokeTool are covered by G5; the managed OneDrive OBO bootstrap and freshAccessToken are not gated. |
| G25 | low |  | Connector OAuth scopes are caller-supplied at Begin, stored as requested (not granted), and one combined read+write token serves read-only tools | — | deferred | Requested-vs-granted OAuth scopes and split read/write tokens are a follow-up. |
| G28 | low |  | OneDrive/Drive path and ID arguments pass '.' and '..' segments into provider URLs | p2c | fixed | Graph path segments go through onedrivefs.ValidateSegment (no `.`, `..`, `:`, `\` or controls); provider ids are charset-checked. |

### Broker: audit, data, limits, workspace (`broker-data-audit`)

44 findings: 26 fixed, 5 partial, 10 deferred, 3 refuted.

| id | sev | prior | title | package | status | note |
|---|---|---|---|---|---|---|
| F28 | medium | M-18 | regression: audit Emit is lossy (chain head advanced before persistence, silent drops, always returns nil) | p3a | fixed | The audit worker chains, signs and stores each event in order, moves the head only after a put, and still logs and publishes dropped events; Emit returns errors. |
| F29 | medium | M-21 | regression: audit signing/durability silently downgrades (Vault read failure, no signing key, no MinIO) and is not covered by the dev-mode guard | p3a | fixed | Outside dev mode the broker refuses to start without a durable audit store or signing key, or when Vault fails. |
| F30 | medium | M-20 | regression: VerifyAuditChain covers only the oldest 5000 objects and the truncation/skip signals are dropped before reaching the admin, so 'Chain intact' is shown for unverified tails | p3a | partial | A truncated, skipped or out-of-time walk shows 'incomplete', never 'intact'; with no checkpoint, a large trail is verified only for its oldest prefix. |
| F31 | medium |  | Chain verifier returns the expected HMAC for any tampered event (signature oracle) | p3a | fixed | A verify failure reports a fixed 'HMAC mismatch' and compares in constant time. |
| F33 | medium | M-22 | regression: no per-user/tenant quota or listing cap on workspace storage (shared volume DoS) | p3b | partial | Per-user quota (2 GiB, 20000 entries) that also charges the sender's skill-transfer staging; the legacy no-user tool-proxy path is not quotaed. |
| F34 | medium |  | Alert pipeline can be flooded and silenced: unthrottled per-DENY alert, synchronous notify in the engine loop, unbounded persist goroutines and alerts table | p3b | fixed | Denial alerts get a per-actor cooldown and tenant cap, one bounded worker, and 90-day retention (migration 065). |
| F35 | medium |  | Alert webhook URL (a bearer secret) is logged at startup and in every delivery error | p3a | fixed | Only the webhook host is logged; the URL never appears in error strings. |
| F36 | medium |  | Broker never verifies its Postgres role enforces RLS; default db.user is the owner role and many queries rely on RLS alone | — | deferred | Contested; a startup RLS-enforcement check is a DB-role design item. |
| F194 | medium |  | South InvokeTool denial audit is written before task binding with request-supplied tenant_id, user_id and tool_id (forged, signed audit events attributed to any user) | p2b | fixed | Pre-binding InvokeTool denial audits use server-side identity; claimed ids go into sanitised context. |
| F195 | medium |  | Audit actor_email is taken from an unverified email/preferred_username claim and signed into the chain; UI shows it in preference to the stable id | — | deferred | Contested; actor_email is display-only and the principal is oid/sub. |
| G14 | medium |  | Skill-transfer staging snapshots are never garbage-collected unless the recipient acts, with no per-sender or global quota | — | deferred | Contested; staging GC is housekeeping. |
| G35 | medium |  | Drift detection checks only a partial window: traffic flushed before the window closes is never evaluated (rate and unknown-tool evasion) | — | deferred | Contested; window coverage is detection quality. |
| G36 | medium |  | Baseline learner has no poisoning resistance: tool_set and p95 are re-learned from the agent's own unfiltered recent windows, with no clean-period, approval, exclusion of drifted windows, or absolute ceiling | — | deferred | Contested; baseline poisoning resistance is a design follow-up. |
| G48 | medium | M-22 | No per-method size, deadline or rate limits on the north surface: any member can store 16 MiB per call in tasks and envelopes and hold unbounded concurrent handlers | p3b | fixed | Per-field caps on north task and envelope inputs, a 4 MiB north request cap, stream and keepalive limits. |
| F37 | low |  | Policy bundle HTTP listener is unauthenticated, plaintext, bound to all interfaces and published on the host in on-prem | p7a | fixed | Bundle host port removed; the listener is reachable only on the backend network (posture check). |
| F40 | low | L-11 | regression: session HMAC sidecar fails open and gives no integrity against the workspace owner | p3b | fixed | Session MACs bind tenant, user and path; missing or legacy sidecars fail closed unless AIKONOS_WORKSPACE_ALLOW_UNSIGNED_SESSIONS. |
| F41 | low | L-12 | regression: internal paths and backend error bodies returned to callers in Internal errors | w2-p11 | fixed | Audit RPCs return fixed messages and log the store error; file-RPC error text remains (see residuals). |
| F42 | low | L-15 | File-RPC denials (.agent/ writes) and failed mutations are not audited | — | refuted | AuditInterceptor already records every failed RPC as DENY. |
| F43 | low |  | Store.resolve reserved-segment check is case-sensitive while the Router and UnderAgentDir are case-insensitive | p3b | fixed | Reserved-segment check is case-insensitive. |
| F44 | low | L-13 | regression: no symlink defense in local workspace store | p3b | fixed | Paths resolve through EvalSymlinks containment and Lstat; a check-then-use race remains. |
| F45 | low | L-16 | regression: OneDrive pagination follows @odata.nextLink with the bearer token and no host or page limit | p2c | fixed | Duplicate of G29; same fix. |
| F46 | low |  | Audit object key and chain key are built from unsanitized tenant_id/event_id | p3a | fixed | Audit object and chain keys path-escape tenant and event ids. |
| F47 | low |  | SMTP alert notifier has no I/O deadline after the TCP connect | p3b | fixed | SMTP notifier has a 30 s I/O deadline that also covers STARTTLS. |
| F48 | low |  | Fire-and-forget DB goroutines use context.Background with no bound | p3b | fixed | last_used_at stamps run on 8 bounded slots with a 5 s timeout. |
| F49 | low |  | Grafana read-only DB role gets cross-tenant SELECT on agents (system prompts) and user_directory (emails) | p7b | fixed | The Grafana role loses SELECT on agent prompts and user emails (column grants, CI-checked). |
| F139 | low |  | Delegation envelope id lets a recipient stamp arbitrary tasks as 'delegated_by' the sender (audit attribution forgery) | p3b | partial | Each envelope stamps at most 50 tasks as delegated_by its sender (fail-closed count, audited); wave 3 replaced the ACCEPTED rule, which broke the webui flow. |
| F196 | low |  | Alert detail and ResourceRef carry sandbox-controlled strings unsanitised into SMTP bodies and webhook JSON (alert content injection) | — | deferred | Escaping alert bodies in notifier templates is a follow-up. |
| F197 | low |  | Broker's Postgres, MinIO audit store and OTLP links are plaintext with no TLS knob; DSN built by string concatenation without escaping | — | deferred | In-network TLS to Postgres, MinIO and OTLP is a deployment design item. |
| F199 | low |  | No strength or placeholder validation of HMAC secrets (audit signing key, workspace session key, gateway-grant key from Vault); sample env files ship publicly known keys | p3b | fixed | Outside dev mode, placeholder or short (<32 B) HMAC secrets refuse startup. |
| F200 | low |  | gRPC servers have no pre-auth message/stream limits, keepalive policy or bounded shutdown (any in-mesh mTLS client can force 16 MiB allocations per request before authentication) | p3b | partial | 1000 streams per connection, 64 connections per IP, keepalive and bounded shutdown; grpc-go still decodes up to 16 MiB before auth. |
| F201 | low |  | Audit hot path is coupled to Postgres: email resolver queries the whole tenant directory on the request context inside Emit, with no timeout or singleflight | — | deferred | Email-resolver coupling is performance, not security. |
| F250 | low |  | Delegation envelopes have no length cap, no per-sender quota, a constant 'remaining budget' and group fan-out amplification; the inbox shows only the newest 100 | p3b | partial | Field caps and a 50-member group fan-out cap; no per-sender quota, pending cap, budget counter or keyset paging. |
| G8 | low |  | Admin config mutations record only new values or key names, so downgrades and prompt-injection-relevant edits are not forensically reconstructable | w2-p11 | fixed | Admin config audits record new non-secret values, the preamble as a digest, and the provider endpoint host. |
| G12 | low |  | DELETE /skills/{".." + space} wipes the caller's whole workspace, including .agent/, because ValidSkillName disagrees with workspacefs.CleanRel | p3b | fixed | Skill names with surrounding whitespace are rejected and subtree deletes stay inside Skills/. |
| G16 | low | L-17 | L-17 still open: personal-skill replace install is delete-then-copy and non-atomic | p3b | fixed | Personal-skill replace installs through a staged rename with rollback. |
| G17 | low |  | Personal skill scan reads each SKILL.md in full before applying the 256 KiB cap | w2-p9 | fixed | Oversized SKILL.md and skill files are refused from their listed size without being read; an unreadable or truncated listing fails closed. |
| G19 | low |  | VerifyMemoryConcept is not bound to the content the human reviewed | w2-p9 | fixed | VerifyMemoryConcept requires the content_sha256 the reviewer saw; a changed file is refused. |
| G26 | low |  | Linked provider account identity is never captured or restricted (no hd/tid/ID-token check); 'common' Microsoft authority accepts personal accounts | — | deferred | Restricting linked accounts by hd/tid is a follow-up. |
| G29 | low | L-16 | regression: OneDrive @odata.nextLink followed with bearer and no host or page cap (L-16 still open) | p2c | fixed | nextLink is followed only to the Graph host and capped at 100 pages. |
| G30 | low | L-12 | regression: Graph error bodies read unbounded and embedded in returned errors (L-12 still open) | p2c | fixed | Graph error bodies are read up to 512 bytes, logged, and not returned. |
| G33 | low |  | Connector lifecycle audit events carry no connector/provider/scope context and failures are not audited | — | refuted | AuditInterceptor records failed connector RPCs as DENY. |
| G37 | low |  | Detector ignores the learned cost_p95: cost drift is never detected despite being learned and documented as part of the envelope | — | deferred | Cost drift detection is a feature gap. |
| G39 | low |  | Silent fail-open in the detect loop: a GetBaseline error skips drift evaluation of the closed window with no log or metric; new agents have no coverage until a baseline exists | p3b | fixed | GetBaseline errors are logged and counted instead of skipped silently. |
| G41 | info |  | SECURITY DEFINER baseline functions accept an arbitrary cutoff/since: any caller of aikonos_app can wipe or enumerate all tenants' windows | — | refuted | The definer functions give aikonos_app no capability it does not already have. |

### agent-gateway (`agent-gateway`)

34 findings: 24 fixed, 4 partial, 5 deferred, 1 skipped.

| id | sev | prior | title | package | status | note |
|---|---|---|---|---|---|---|
| F51 | high |  | spawn_subagents branches auto-approve every tool the caller holds, bypassing HITL in ordinary chats | p4a | fixed | Subagent branches inherit only the run's preApproved list, which is empty for HITL runs or when the org master is off. |
| F52 | high |  | Model-driven workflow_save + workflow_schedule creates a standing unattended approval with no human decision | p4a | fixed | workflow_schedule needs a single-use human approval tied to the shown version, and the broker pins that version (F18); a narrow pre-create TOCTOU remains. |
| F53 | medium |  | Parent trusts the shape of every child IPC message: a non-object message crashes the gateway for all users | p4b | fixed | Malformed child IPC messages are dropped or answered 'malformed request'; handler throws are contained. |
| F54 | medium |  | Child-relayed LLM usage is booked verbatim, so a compromised child can zero or poison spend caps | p4b | partial | Relayed usage is clamped to bounded non-negative values; a child can still report zero tokens for real calls (needs gateway-side metering). |
| F55 | medium | M-24 | regression: child process shares the parent's uid and can read the gateway SVID key and parent memory (M-24 still open) | p4b | skipped | A separate uid needs CAP_SETUID and node --permission breaks the tsx loader; children run with TSX_DISABLE_CACHE=1 and the residual is in SECURITY.md. |
| F56 | medium | M-25 | regression: external :8090 rate limit is per source IP behind Traefik, so one anonymous client locks out every agent and SCIM (M-25 still open) | p4b | fixed | trustProxy comes from AIKONOS_EXTERNAL_TRUST_PROXY, the global limit now covers external routes, and a per-agent bucket applies. |
| F57 | medium | L-18 | Body parsing happens before authentication: unauthenticated 14 MiB buffers and no rate limit on the internal :8080 API (L-18 still open) | p4b | partial | /files, /skills/import and the admin upload routes verify the bearer before the body; no rate limit or connection cap on :8080. |
| F58 | medium | L-18 | One member can exhaust the child pool: no per-user concurrency cap, and spawn_subagents branches each take a pool slot | p4a | fixed | Every live child of a user (pooled, branch, reserved) counts against a per-user share of half the pool (HTTP 429). |
| F59 | medium |  | Audit stream authentication and admin scope are fixed at connect time and never re-evaluated | p4b | fixed | Audit streams end at token exp and re-probe the broker every 60 s. |
| F202 | medium |  | One member's memory bundle can pin the shared gateway event loop: auto-recall compiles and runs an uncapped number of regexes against the prompt | w2-p9 | fixed | Memory tags are capped broker-side; the gateway matcher uses a per-prompt word set and a phrase budget (adversarial input about 20 ms). |
| F203 | medium |  | Agent-level provider/model pinning is not applied to interactive chat or scheduled runs, and allowedProviders is enforced nowhere | p4a | fixed | Agent provider pinning applies to chat, scheduled and external runs; a pinned agent never falls back to the env provider, fail closed. |
| F204 | medium |  | Audit SSE bridge accepts unauthenticated NATS messages as audit events and trusts tenant/actor from the payload | — | deferred | Contested; NATS now requires a token (F114). |
| G2 | medium |  | Provider endpoint is admin-supplied with only a scheme check; the gateway egress path and the reason/vision/embed paths do no https, private-range or redirect checks (SSRF and credential exposure) | — | deferred | Contested; the admin-supplied provider endpoint is the admin trust boundary. |
| G11 | medium |  | Skill ZIP parser is O(files x entries): one 14 MiB upload stalls the gateway event loop | p4b | fixed | Skill ZIP parsing is single-pass with a 1000-entry cap and duplicate-name refusal. |
| G13 | medium |  | Skill catalog injects unflattened names and descriptions into the system prompt, and the transfer preview hides them from the recipient | p4b | partial | Skill names and descriptions are flattened and clipped (300); a one-line instruction still reaches the prompt and the transfer preview hides it (needs proto). |
| F60 | low | M-23 | Per-run LLM call budget is reset by every new run on a shared child (M-23 still open) | p4a | fixed | The per-run LLM budget resets only when a child goes from idle to busy. |
| F61 | low |  | Egress proxy buffers unbounded request bodies and forwards almost all child-chosen headers upstream | p4b | fixed | The egress proxy forwards an allowlist of headers and caps bodies at 32 MiB. |
| F62 | low |  | Client-chosen runId is used as a global key for approval draining and branch eviction | p4a | fixed | runId is minted by the server; draining and eviction act only on the caller's own entries. |
| F63 | low | L-19 | regression: JWT issuer check is silently disabled when AIKONOS_OIDC_ISSUER is empty and there is no algorithm allowlist (L-19 still open) | p4b | fixed | The gateway refuses to start without OIDC issuer and JWKS URL unless dev mode; only asymmetric JWT algorithms, exp required. |
| F64 | low | L-20 | regression: gateway uses cfg.defaultTenantId for chat, scheduler and south reads while REST uses principal.tenant (L-20 still open) | p4a | fixed | /agui and the user skill routes use principal.tenant everywhere. |
| F65 | low | L-21 | regression: legacy demo UI served unauthenticated at / with no CSP and an innerHTML re-parse of textContent (L-21 still open) | p4b | fixed | Legacy demo UI at / removed. |
| F66 | low | L-22, L-23 | Raw upstream and gRPC error text reaches users and the model; provider fetches follow redirects and accept http endpoints (L-22 and L-23 still open) | p4b | partial | Error text to users and the model is trimmed and redacted, gate reasons included; http provider endpoints are still accepted. |
| F67 | low |  | The delegate tool sends envelopes (recipient, intent and scopes chosen by the model) with no approval card | — | deferred | An approval card for the delegate tool is a product decision on envelope UX. |
| F129 | low |  | Blocked / session-revoked users can still start /agui chat runs and read memory until the IdP token expires (revocation is only enforced on north and tool paths) | w2-p9 | fixed | Blocked users are refused at ResolveRunAgent, memory reads and the per-turn MintSessionGrant. |
| F205 | low |  | agent.external.invoked audit records are keyed with camelCase fields the scope filter never matches, so no reader ever sees them, and they are not durable | p4b | fixed | agent.external.invoked records use AuditEvent field names; they are still in-memory only. |
| F206 | low |  | Output credential redaction runs per streamed delta, so it does not catch secrets split across deltas, and tool args and results are never redacted | — | deferred | Per-delta redaction is a best-effort control. |
| F207 | low |  | IPC tool-membership backstop fails open on an empty set (and the tool list fails open on an FGA list error); gate-then-bridge-direct ops do not require a prior gate decision | p4b | fixed | Tool membership fails closed on an empty set and on a skill-list error. |
| F208 | low |  | External invoke never aborts the run when the SSE client disconnects, so unattended runs and token spend continue for up to 180 s | p4b | fixed | External invoke aborts the run when the client disconnects. |
| F209 | low |  | Unauthenticated /readyz returns internal dependency errors (NATS and broker connection details) | p4b | fixed | /readyz reports only ok or not-ready. |
| G3 | low | L-23 | Provider fetch() follows redirects and forwards api-key / x-api-key to the redirect target - L-23 still open | p4b | fixed | Provider fetches use redirect 'manual', so keys never reach a redirect target. |
| G15 | low |  | POST /skills/import 'never overwrite' check is blind past 50 skills and silently clobbers an existing skill | p4b | fixed | Skill import probes the target directory directly and never overwrites. |
| G18 | low |  | Memory recall preamble puts the derived trust tier in-band after attacker-controlled text | — | deferred | Trust-tier ordering in the recall preamble is a prompt-design follow-up. |
| F68 | info | H-11 | Child-relayed readOnlyHint is still accepted by the parent IPC handler | p4b | fixed | The parent ignores the child-relayed readOnlyHint. |
| G20 | info |  | extras[relPath] on a plain object silently drops a bundle file named \_\_proto\_\_ and sets the object's prototype | p4b | fixed | Bundle extras use a null-prototype map and refuse \_\_proto\_\_, constructor and prototype. |

### webui (`webui`)

14 findings: 12 fixed, 1 deferred, 1 refuted.

| id | sev | prior | title | package | status | note |
|---|---|---|---|---|---|---|
| F69 | medium | M-26 | M-26 still open: webui sends no CSP, frame-ancestors, nosniff or Referrer-Policy | p5 | fixed | CSP, frame-ancestors, nosniff, X-Frame-Options and Referrer-Policy on every response; IdP origin from AIKONOS_WEBUI_OIDC_AUTHORITY. |
| F70 | medium | M-26 | Markdown renderer auto-loads attacker-chosen remote images and auto-runs fenced html in a scripted iframe | p5 | fixed | The markdown renderer loads no remote or data images and runs no html previews; mermaid is strict. |
| F71 | medium | L-27 | Gateway run keeps going after the browser aborts or disconnects: proxy never cancels the upstream fetch | p5 | fixed | The proxy aborts the upstream fetch when the browser disconnects. |
| F73 | medium |  | CSV exports write attacker-influenced cells without neutralising formula prefixes (CSV injection against admins) | p5 | fixed | CSV exports neutralise formula prefixes through one shared csvEscape. |
| F211 | medium | L-25 | regression: L-25 baseline re-created client-side, every new schedule silently carries standing pre-approvals for web.fetch, doc.read and doc.write | p5 | fixed | New schedules (web and desktop) carry no pre-approvals unless ticked; legacy ones can be unticked or cleared with the runbook query. |
| F72 | low | M-28 | Regular (non step-up) approvals show collapsed arguments and allow Approve without review | p5 | fixed | Approval arguments are always expanded. |
| F74 | low | L-26 | L-26 still open: chat history persisted in localStorage, not keyed by user, and not cleared on session expiry | p5 | fixed | Chat history is bound to its owner sub and cleared on user change or session end. |
| F75 | low | L-30 | L-30 still open: window.location.assign(authorizeUrl) with no scheme check, now fed by MCP-server metadata | — | refuted | The broker requires https for MCP OAuth endpoints outside the dev-only allow_private flag. |
| F76 | low | L-27, L-28 | L-27/L-28 still open: unauthenticated /healthz leaks internal URLs; proxy has no timeouts and forwards unauthenticated 15 MiB bodies | p5 | fixed | /healthz returns only ok; bearer required before body read; timeouts; generic 502 bodies. |
| F77 | low | M-27 | M-27 still open: OIDC access, ID and refresh tokens live in sessionStorage with no revocation on sign-out | p5 | fixed | Sign-out revokes tokens where the IdP has a revocation endpoint; tokens stay in sessionStorage. |
| F212 | low |  | Delegation confirm dialog: a global Enter listener confirms the delegation even when Cancel is focused or Enter auto-repeats | p5 | fixed | The global Enter listener is gone; only the focused Confirm button confirms. |
| F213 | low |  | Approval args rendering does not neutralise bidi or control characters in keys and values, so an LLM-controlled argument can visually mislead the approver | p5 | fixed | Every invisible, bidi and control code point in approval args and reasons is shown as an escape with a badge (web and desktop). |
| F78 | info |  | Dockerfile hygiene: no .dockerignore, and the build and runtime stages are not pinned or reproducible | — | deferred | Dockerfile hygiene (info). |
| G34 | info | I-05 | Connect callback leaves OAuth code and state in the browser URL (I-05 still open) | p5 | fixed | The callback scrubs code and state from the URL before the exchange. |

### office-worker and docs-mcp (`office-worker-docs-mcp`)

16 findings: 11 fixed, 2 partial, 2 deferred, 1 skipped.

| id | sev | prior | title | package | status | note |
|---|---|---|---|---|---|---|
| F79 | high | M-30 | M-30 (still open, upgrade to high): concurrent model-authored scripts share uid and /tmp; leftover background processes survive jobs; any job can read or tamper with other tenants' in-flight documents | p6 | partial | Minimal tagged env, /proc sweep, O_NOFOLLOW reads; concurrent jobs share one uid and can read each other's files and the worker token. |
| F80 | medium | L-31 | pdf.transform split: qpdf @argfile injection via range.pages gives arbitrary file read (embedded into returned PDF) without any script op | p6 | fixed | qpdf page ranges must match a strict allowlist, so @argfile and option injection are refused. |
| F81 | medium |  | Unbounded stdout/stderr buffering of child processes in the worker parent can OOM-kill the whole worker (all in-flight jobs) | p6 | fixed | Child stdout and stderr keep only the last 16 KiB. |
| F82 | medium | M-32 | M-32 (still open): worker-side resource limits missing: zip bomb / regex-expansion / pdftoppm dpi / xlsx range OOM the shared container; output cap is optional | p6 | fixed | Zip reads bounded by bytes read, expansion checked before it runs, query echo capped, document parsers under memory limits. |
| F85 | medium | M-33 | M-33 (still open, not fixed): docs-mcp published on 0.0.0.0:8060 with no auth and no Host/Origin validation (DNS rebinding); corpus includes the open vulnerability register | p7a | fixed | Published on 127.0.0.1:8060 only, HOST bind and Host/Origin allowlist (p6, wired by w2-p10); no authentication on the mesh by decision. |
| F215 | medium |  | Job-dir cleanup failure is swallowed: undeletable dirs leave tenant documents readable on the shared tmpfs and permanently fill /tmp (persistent DoS) | — | deferred | Contested; covered by the F79 containment work. |
| F216 | medium |  | office-worker runs node as PID 1 with no init: orphaned/killed grandchildren become unreaped zombies and exhaust pids_limit 512 (persistent DoS) | p7a | fixed | init: true in compose (and tini in the image) reaps orphans. |
| F217 | medium |  | Per-job memory ceiling for model scripts is ineffective: node --max-old-space-size does not cap Buffers, python has no limit, shared 512m tmpfs has no per-job quota | p6 | partial | Model scripts and fixed helpers run under address-space and file-size limits; one job can still fill the shared /tmp tmpfs. |
| F83 | low | M-31 | M-31 (still open): job slot taken before request body is parsed; no server timeouts or queue bound | p6 | fixed | The body is parsed under a timeout before a slot is taken; bounded queue; server timeouts. |
| F84 | low | M-29 | M-29 (still open): office-worker job API has no authentication; isolation relies solely on the internal `office` network | p7a | fixed | Compose requires AIKONOS_OFFICE_WORKER_TOKEN; the worker checks it before parsing (p6) and the broker refuses a placeholder outside dev mode. |
| F86 | low | L-32 | L-32 (still open): docs-mcp synchronous whole-corpus scans per call, unbounded query, X-Powered-By enabled | p6 | fixed | Query length capped, list_docs paged, X-Powered-By off. |
| F87 | low | L-31 | L-31 (still open): unvalidated params shape and argument values reach qpdf/pdftoppm/pandoc | p6 | fixed | params must be a JSON object; ops, regex and xml-query inputs are bounded. |
| F88 | low |  | soffice profile forces recalculation on load but does not disable external link/file-URL evaluation; no network or filesystem sandbox for LibreOffice | p6 | fixed | The soffice profile disables macros, active content and link updates. |
| F89 | low | L-33 | L-33 (still open): Dockerfiles use tag-only base images, `apt-get upgrade`, and chown of /app | — | deferred | Base-image digest pinning (low). |
| F218 | low |  | Model-authored scripts can reach the broker over the `office` network: unauthenticated policy-bundle listener :9092 and mTLS ports :9090/:9091 are open to the 'zero-authority' worker | p7a | skipped | Compose cannot isolate broker ports on the shared office network; needs a bundle bearer in the broker. |
| F249 | low |  | Compromised or malicious office-worker can exhaust broker memory with an unbounded number of multipart response parts | p6 | fixed | The broker caps office multipart responses (64 parts, about 88 MiB). |

### desktop (`desktop`)

16 findings: 12 fixed, 3 deferred, 1 refuted.

| id | sev | prior | title | package | status | note |
|---|---|---|---|---|---|---|
| F90 | medium | M-34 | M-34 still open: server-supplied OIDC authority and discovery endpoints are trusted without https, issuer match, or host check | p8 | fixed | Authority must be https, issuer must match, endpoints share its scheme and host; the IdP host is confirmed before the browser opens. |
| F91 | medium | M-35 | M-35 still open: plain http:// to a non-loopback server is accepted and the warning only shows after the browser has opened | p8 | fixed | http:// to a non-loopback server is refused unless AIKONOS_DESKTOP_ALLOW_HTTP=1; the warning shows before the browser opens. |
| F219 | medium | M-28 | regression: M-28 fix never reached the desktop approval dialog; nested arguments are collapsed and Approve does not require opening them | p8 | fixed | Nested approval arguments start expanded. |
| F220 | medium |  | Save to this PC: server-controlled file name is not sanitised and the saved file carries no Mark-of-the-Web | p8 | fixed | Saved file names are sanitised and get a Mark-of-the-Web on Windows. |
| F92 | low | L-34 | L-34 still open: string test `host.starts_with("127.")` treats 127.evil.com as the local machine and defaults to http | p8 | fixed | Loopback is decided by parsing the host, not a '127.' prefix. |
| F93 | low | L-35 | L-35 still open: unbounded response buffering and an unbounded event channel | p8 | fixed | Response bodies and SSE events are size-capped and the event channel is bounded. |
| F94 | low | L-36 | L-36 still open: huge expires_in from the IdP panics the client (SystemTime overflow) | p8 | fixed | expires_in is clamped to 24 h. |
| F95 | low | L-37 | L-37 still open: aikonos.exe is not Authenticode-signed; update link is an arbitrary server-supplied URL with no integrity check | — | deferred | Authenticode needs a code-signing certificate. |
| F96 | low | L-38 | L-38 still open: loopback sign-in listener accepts connections serially, so a local process can stall the callback | — | deferred | Serial loopback listener is a local-process DoS only. |
| F97 | low | L-39 | L-39 still open: save_bytes uses a predictable temp name that follows pre-planted symlinks, with a remove-then-rename gap | p8 | fixed | save_bytes uses a random create_new temp file and an atomic rename. |
| F221 | low |  | No request or read timeout; a stalled token refresh holds the token mutex and freezes every call and sign-out | p8 | fixed | Read and auth timeouts; the token mutex is not held across a refresh. |
| F222 | low |  | Credentials typed in the server address are kept and written in clear text to desktop.json | p8 | fixed | Credentials in the server address are refused and scrubbed from saved prefs. |
| F223 | low |  | Agent-written markdown links open in the browser on one click with no destination shown or confirmation | — | deferred | Link confirmation in the transcript is a product decision. |
| F224 | low |  | Dev mock server listens on all interfaces and accepts any bearer for read/write of its file store and any redirect_uri | p8 | fixed | The dev mock server binds 127.0.0.1 by default. |
| F98 | info | I-11, I-12 | I-11/I-12 still open: ID token is never validated and nonce never compared; TokenSet derives Debug and would print tokens | p8 | fixed | The ID-token nonce is checked and TokenSet's Debug prints [REDACTED]. |
| F99 | info |  | Assistant markdown is rendered with no image_source override; remote image exfiltration is blocked only because gpui has no HTTP client | — | refuted | No HTTP client is registered with gpui, so remote images cannot load. |

### Deploy, CI, supply chain (`deploy-ci-supplychain`)

33 findings: 17 fixed, 5 partial, 11 deferred.

| id | sev | prior | title | package | status | note |
|---|---|---|---|---|---|---|
| F100 | high | H-09 | OpenFGA (authorization source of truth) is unauthenticated and, on-prem, sits on the `mesh` network beside the gateway and webui | p7a | fixed | OpenFGA is on the backend network only with no host port in every variant; preshared authn not added. |
| F101 | medium | H-08 | Compose still defaults the broker to AIKONOS_DEV_MODE=true and does not require the OIDC issuer | p7a | fixed | Compose defaults AIKONOS_DEV_MODE to false and requires AIKONOS_OIDC_ISSUER. |
| F103 | medium | M-36 | Azure and on-prem overlays still publish dev-grade services on all host interfaces (Keycloak, broker 9090-9092, NATS on Azure, OTLP, Loki, Tempo, Prometheus, docs-mcp, webui/gateway on Azure) | p7a | fixed | Azure and on-prem overlays publish only Traefik and loopback Grafana; a CI posture check enforces it. |
| F104 | medium | M-37 | Keycloak realm shipped to every variant: open redirect URIs on a public client without PKCE, ROPC smoke client with committed secret, trivial-password users, sslRequired none | — | deferred | Contested; dev realm hardening for the local variant. |
| F105 | medium | M-38 | Dev CA private key is mounted into broker and gateway, leaf keys are 0644 and certs live 10 years in every variant | p7a | partial | Broker and gateway mount only their own leaf and ca.crt; leaf keys stay 0644 and certs live 10 years. |
| F106 | medium | M-39 | Git deploy hook fails open without an allowed-signers file and builds from source on the host | p7b | fixed | The deploy hook requires signed commits by default. |
| F108 | medium | M-41 | Vault AppRole secret_id never expires or is single-use-limited, Vault listener has TLS disabled, root-token literal fallback remains | — | deferred | Contested; AppRole secret_id TTL versus restarts needs an ops decision. |
| F109 | medium | M-42 | Backups are plaintext, umask-default, written inside the repo tree that does not git-ignore backups/, and include Vault init material | p7b | fixed | Backups are umask 077, git-ignored, exclude Vault init material and require credentials; not encrypted. |
| F110 | medium | M-43 | OTel collector runs as root with all container logs mounted, SIEM pipeline leaves metrics and log bodies unredacted, OTLP receivers are unauthenticated | p7a | partial | SIEM metrics are redacted and an https SIEM endpoint documented; the collector stays root and receivers are unauthenticated in-network. |
| F226 | medium |  | Grafana MCP profile hands any agent user a cross-tenant SQL and log reader (grafana_api_request + operator-wide Postgres datasource + Loki) | — | deferred | Contested; the Grafana MCP profile is opt-in and admin-attached. |
| F227 | medium |  | Shipped `<...-CHANGE-ME>` placeholder secrets are accepted at startup; nothing rejects a forgotten value | p3b | partial | The broker rejects placeholder secrets; Postgres, Keycloak, MinIO and Grafana placeholders are not checked. |
| F107 | low | M-40 | Image pinning is optional: base images are tag-only, digest overlay is not in COMPOSE_FILE and omits mcp-grafana | — | deferred | Digest pinning is a release-process choice; the digest overlay exists. |
| F112 | low |  | Release workflow publishes from any pushed v* tag with no branch, protection or environment gate | — | deferred | Tag protection and release environments are repository settings. |
| F113 | low |  | Audit WORM bucket is written with the MinIO root credential in azure/onprem | — | deferred | A scoped MinIO user for the WORM bucket needs provisioning work. |
| F114 | low | L-41 | NATS carries the audit stream unauthenticated on the mesh network | p7a | partial | Two NATS users with subject permissions (gateway read-only on aikonos.audit.>); a same-uid gateway child can still read every tenant's audit stream. |
| F115 | low | L-46 | Vault broker policy grants `update` on the four signing-key paths and tenant-wide wildcards | w2-p11 | fixed | Signing-key paths are create/read only (re-seed on upgrade); a 403 on the first-boot create race re-reads the winner's key and is logged. |
| F116 | low | L-45 | Root .dockerignore excludes only .env.local; broker builder stage `COPY . .` pulls secrets and key material into the build context and layers | p7a | fixed | Ignore files keep env files, keys, TLS dirs and backups out of every build context. |
| F117 | low | L-44 | aikonosctl interpolates arguments into psql -c SQL and uses a fixed /tmp file and default MinIO credentials | p7b | fixed | aikonosctl passes SQL values as psql variables, validates ids, uses a private temp file and has no default MinIO credentials. |
| F210 | low |  | Gateway image build is not reproducible or minimal: unpinned base, lifecycle scripts on npm ci, full toolchain shipped in the runtime image | p7a | partial | Gateway image is two-stage with --ignore-scripts; the base image is not digest-pinned. |
| F225 | low | M-44 | regression/open: M-44 desktop.yml has no permissions block and pins actions by mutable tag | p7b | fixed | desktop.yml has read-only permissions and SHA-pinned actions. |
| F228 | low |  | Signed-deploy hook accepts any previously signed commit: rollback or replay by a pusher who has no signing key | p7b | fixed | The deploy hook refuses non-fast-forward deploys from the last deployed commit or the bare HEAD unless AIKONOS_DEPLOY_ALLOW_ROLLBACK=1. |
| F229 | low | L-42 | Internet-facing Traefik runs as root with the Docker socket mounted and forwards client X-Forwarded-* to the auth gate | — | deferred | A docker-socket proxy for Traefik is an ops follow-up. |
| F230 | low |  | scripts/replay-decision.sh evaluates untrusted Rego from the evidence file with every OPA builtin enabled (http.send, opa.runtime) | p7b | fixed | replay-decision.sh evaluates evidence Rego with network and runtime builtins removed. |
| F231 | low | L-44 | aikonosctl kill-switch silently targets the dev tenant UUID and reports success | p7b | fixed | kill-switch requires an explicit --tenant UUID and names the tenant it wrote. |
| F232 | low |  | dev CA root is minted inside a mutable `alpine/openssl:latest` container that has internet egress (all variants) | p7a | fixed | dev-ca-mint is digest-pinned and runs with no network. |
| F233 | low |  | Third-party infra images and hand-pinned CI tooling have no update feed and sit outside the release vulnerability gate | — | deferred | An update feed for third-party images and CI tools is an ops follow-up. |
| F234 | low |  | AG-13 Keycloak step-up flow lets a user with no enrolled OTP satisfy acr=mfa by self-enrolling (dev realm shipped to every variant) | — | deferred | OTP self-enrolment affects the local dev realm only. |
| F236 | low |  | agent-gateway runtime image keeps the full build toolchain (devDependencies, protoc, tsx, ts-proto) in the container that runs untrusted model-driven child processes | p7a | fixed | The runtime gateway image drops protoc, TypeScript and type packages. |
| G31 | low | L-14 | regression: Vault client follows redirects carrying X-Vault-Token, accepts http://, no re-login on 403 (L-14 still open); connector tokens traverse it | p1 | fixed | Duplicate of F176; same fix. |
| G38 | low |  | Baseline config is not validated: NaN/Inf/<=1 DriftMultiplier, retention < min samples, or non-positive window/retention silently disable detection or crash the broker | p3b | fixed | Invalid baseline config is fatal when baselines are enabled. |
| G40 | low |  | Grafana drift alert misses the first drift per (tenant, agent, kind) series, and the in-process alert engine only sees drift as a generic DENY | — | deferred | The Grafana alert series gap is observability. |
| F119 | info | L-43 | Azure provisioning script hardcodes a real subscription id and resource group defaults and derives the SSH allow-list from a third-party lookup | p7b | fixed | provision.sh requires explicit subscription, resource group and admin IP; no third-party IP lookup. |
| F235 | info |  | Release vulnerability gate only blocks Critical findings that already have a fix; High and known-exploited findings pass | — | deferred | The release gate threshold is a policy decision. |

### OPA/FGA policies and skills (`policies-skills`)

11 findings: 6 fixed, 1 partial, 3 deferred, 1 refuted.

| id | sev | prior | title | package | status | note |
|---|---|---|---|---|---|---|
| F237 | high |  | memory.write tool lets any group member (and any user of a shared agent) write shared memory that the management RPCs reserve to group managers / tenant admins; auto-approved WRITE_LOCAL and auto-recalled into peers' turns | — | refuted | Member writes through memory.write are the documented design; the manager rule governs verify, deprecate and delete only. |
| F23 | medium |  | Agent-writable Skills/ tree is read back as trusted skill catalog with no provenance or injection scan (persistent prompt-injection channel) | p2c | fixed | The workspace write seam refuses paths under Skills/; served skills are not yet injection-scanned. |
| F120 | medium | M-08 | Exfil/DLP rules (OPA) are unreachable in production: reads_sensitive is derived from the same step's class and the stock gateway sends one-step plans with both flags false | p2b | partial | reads_sensitive and has_dlp_attestation are no longer taken from the plan, but the DLP rule cannot fire until a broker-derived flag exists. |
| F121 | medium | M-16 | regression/open: web.fetch is declared and routed READ_ONLY, so URL-based exfil is auto-allowed; network ACL is default-allow | — | deferred | Reclassifying web.fetch needs the task-scoped sensitive_read_seen flag. |
| F122 | medium | M-17 | Documented OPA rules are dormant or keyed on values the broker never produces: infra-from-sandbox, risk, on-call, production sensitivity, envelope depth/relationship/group | — | deferred | Contested; dormant rules are documentation and cleanup. |
| F238 | medium |  | Network access-list resolves user groups from direct member tuples only; nested groups the FGA model allows are ignored, so group-scoped DENY/ASK rules can be bypassed | p2b | fixed | Network ACL groups come from ListObjects, so nested groups count; errors fail closed. |
| F123 | low | M-17 | plan_validation fails open on missing/malformed input and does not bound or validate plan contents (negative cost nets out) | w2-p11 | fixed | Negative or malformed costs and non-object plan steps are denied in Go and Rego. |
| F124 | low |  | plan_validation duplicate-step rule is O(n^2) and no step cap runs before OPA, enabling shared OPA CPU exhaustion | p2b | fixed | Plans over 256 steps are refused before OPA, audited. |
| F126 | low |  | FGA wildcard grant user:* usable_by on the default agent cannot be revoked or narrowed through the admin API | — | deferred | The user:* grant on the default agent is by design. |
| F127 | low |  | FGA model has no tenant boundary and ships unused, unconstrained types; 'fga model test' is not run in CI | p7b | fixed | CI runs fga model test with a checksum-pinned CLI; separation of duty on skill approval; one tenant per store documented. |
| F239 | low |  | OpenFGA client treats any 400 write_failed/invalid_input as success, so an atomic batch revoke or grant is silently skipped | p2b | fixed | Only a single-tuple 'already exists'/'does not exist' 400 counts as success; batches retry per tuple. |

## Deferred

46 findings are deferred. Reason as recorded at deferral; "pick up
when" is the trigger to reopen. F5, G46 (M-06) and F171 were deferred in wave 1
and fixed in wave 2 (w2-p9); they are not listed.

| id | sev | title | reason | pick up when |
|---|---|---|---|---|
| F6 | medium | JIT provisioning grants groups from the unverified email / preferred_username claim | M-02 for jit tenants: design decision (requires email_verified or oid-keyed rules); SCIM mode already closes it. Deferred, documented. | A jit tenant uses email rules in production: require email_verified or oid-keyed rules. |
| F36 | medium | Broker never verifies its Postgres role enforces RLS; default db.user is the owner role and many queries rely on RLS alone | contested; RLS enforcement check at startup is an ops/DB-role design item. | A deployment uses a DB role other than the shipped NOBYPASSRLS aikonos_app: add a startup check. |
| F104 | medium | Keycloak realm shipped to every variant: open redirect URIs on a public client without PKCE, ROPC smoke client with committed secret, trivial-password users, sslRequired none | contested; dev realm hardening for the local variant only. | Keycloak is used outside local dev (azure/onprem use Entra), or the realm is reused for a customer. |
| F108 | medium | Vault AppRole secret_id never expires or is single-use-limited, Vault listener has TLS disabled, root-token literal fallback remains | contested; AppRole secret_id TTL/uses trade-off against broker restarts (availability) needs an ops decision. | Ops decides secret_id rotation (TTL plus restart tooling, or response wrapping), or Vault leaves the compose network. |
| F121 | medium | regression/open: web.fetch is declared and routed READ_ONLY, so URL-based exfil is auto-allowed; network ACL is default-allow | M-16 web.fetch class: reclassifying as NETWORK_EGRESS makes the tool need approval on every call; needs the task-scoped sensitive_read_seen flag (F120 follow-up) to do it precisely. Deferred with F120's TODO. | The task-scoped sensitive_read_seen flag (F120 `TODO(M-08)`) lands. |
| F122 | medium | Documented OPA rules are dormant or keyed on values the broker never produces: infra-from-sandbox, risk, on-call, production sensitivity, envelope depth/relationship/group | contested; dormant OPA rules are documentation/cleanup. | With F120/F121: wire or delete the dormant rules. |
| F157 | medium | External subject assertion is the user's full north bearer: replayable by the agent holder against the whole north API (self-approval) | user assertion = full bearer: design of the external OBO flow (AG-17); scoped assertions are a follow-up. | Scoped (audience- or purpose-bound) user assertions are designed for external agents. |
| F186 | medium | Per-user MCP OAuth tokens are not bound to the connection URL, issuer or resource; changing the URL sends every user's token to the new server | contested; per-user MCP OAuth token binding to URL is a follow-up. | MCP connection URLs become editable by non-admins, or with the F140 remainder. |
| F195 | medium | Audit actor_email is taken from an unverified email/preferred_username claim and signed into the chain; UI shows it in preference to the stable id | contested; actor_email is display-only, principal is the oid/sub. | actor_email is used for any authorization or forensic decision. |
| F204 | medium | Audit SSE bridge accepts unauthenticated NATS messages as audit events and trusts tenant/actor from the payload | contested; NATS now requires a token (F114). | NATS gets publishers beyond broker and gateway, or per-subject permissions (F114 residual). |
| F215 | medium | Job-dir cleanup failure is swallowed: undeletable dirs leave tenant documents readable on the shared tmpfs and permanently fill /tmp (persistent DoS) | contested; cleanup-failure handling is covered by the F79 containment work. | With the F79 per-job sandbox. |
| F226 | medium | Grafana MCP profile hands any agent user a cross-tenant SQL and log reader (grafana_api_request + operator-wide Postgres datasource + Loki) | contested; Grafana MCP profile is opt-in and admin-attached. | Before the Grafana MCP profile is offered to non-admin agents: scope the datasource per tenant. |
| G2 | medium | Provider endpoint is admin-supplied with only a scheme check; the gateway egress path and the reason/vision/embed paths do no https, private-range or redirect checks (SSRF and credential exposure) | contested; admin-supplied provider endpoint is an admin trust-boundary item. | Provider endpoints become editable by non-admins, or a hosted multi-tenant offering. |
| G14 | medium | Skill-transfer staging snapshots are never garbage-collected unless the recipient acts, with no per-sender or global quota | contested; staging GC is housekeeping. | Skill transfers are used at scale: add a staging TTL sweep. |
| G35 | medium | Drift detection checks only a partial window: traffic flushed before the window closes is never evaluated (rate and unknown-tool evasion) | contested; baseline window coverage is detection quality, not a vulnerability. | Baseline drift detection is relied on as a control, not only a signal. |
| G36 | medium | Baseline learner has no poisoning resistance: tool_set and p95 are re-learned from the agent's own unfiltered recent windows, with no clean-period, approval, exclusion of drifted windows, or absolute ceiling | contested; baseline poisoning resistance is a design follow-up. | Same as G35. |
| F14 | low | Assertion jti replay cache is in-process only | jti replay cache in-process: documented limitation in SECURITY.md (run one broker replica). | Before running more than one broker replica: move the jti cache to Postgres. |
| F67 | low | The delegate tool sends envelopes (recipient, intent and scopes chosen by the model) with no approval card | delegate tool without approval card: product decision on envelope UX; deferred. | The envelope UX is revisited, or delegate can reach recipients or scopes beyond the user's own. |
| F89 | low | L-33 (still open): Dockerfiles use tag-only base images, `apt-get upgrade`, and chown of /app | Dockerfile base pinning (low, cosmetic). | Together with F107 when the release process pins digests. |
| F95 | low | L-37 still open: aikonos.exe is not Authenticode-signed; update link is an arbitrary server-supplied URL with no integrity check | Authenticode signing needs a code-signing certificate (ops). | A code-signing certificate (or Azure Trusted Signing) is available; before any auto-update. |
| F96 | low | L-38 still open: loopback sign-in listener accepts connections serially, so a local process can stall the callback | serial loopback listener: local-process DoS only. | The app is supported on shared multi-user PCs, or the listener code changes. |
| F107 | low | Image pinning is optional: base images are tag-only, digest overlay is not in COMPOSE_FILE and omits mcp-grafana | image pinning by digest is a release-process design choice (digest overlay exists). | The release process makes the digest overlay mandatory for azure/onprem. |
| F112 | low | Release workflow publishes from any pushed v* tag with no branch, protection or environment gate | GitHub branch/tag protection and environments are repository settings, not code. | Before the next release: repository admin sets tag protection and a release environment. |
| F113 | low | Audit WORM bucket is written with the MinIO root credential in azure/onprem | MinIO root credential for the WORM bucket: needs a provisioning change for a scoped user (ops). | azure/onprem provisioning creates a scoped MinIO user for the audit bucket. |
| F126 | low | FGA wildcard grant user:* usable_by on the default agent cannot be revoked or narrowed through the admin API | FGA wildcard on the default agent is by design (tenant default agent). | A tenant needs to restrict the default agent to a subset of users. |
| F182 | low | L-02 still open: principal derivation silently falls back oid -> sub and PrincipalID() -> email -> SPIFFE id | L-02 principal fallback chain: design; SCIM mode keys on oid. | With F6, or when an IdP without oid is supported. |
| F183 | low | Owner grants are stateless 1 h multi-use credentials that are not voided by revoking the API key, public key, or schedule that produced them | stateless owner grants vs revocation: 1 h ceiling accepted; F244 fixed the TTL-comparison bug. | The owner-grant TTL is raised above 1 h, or grants get server-side revocation. |
| F184 | low | Gateway grant key falls back to an ephemeral per-process key outside the dev guard, and a Vault-loaded key is not length-checked | gateway grant key fallback: covered by the placeholder/weak-secret refusal (F199) outside dev mode. | The broker runs replicated, or a deployment runs outside dev mode with no configured grant key. |
| F196 | low | Alert detail and ResourceRef carry sandbox-controlled strings unsanitised into SMTP bodies and webhook JSON (alert content injection) | alert body sanitisation: low; notifier templates escape in a follow-up. | Notifier templates change, or alerts go to an HTML-rendering sink. |
| F197 | low | Broker's Postgres, MinIO audit store and OTLP links are plaintext with no TLS knob; DSN built by string concatenation without escaping | TLS to Postgres/MinIO/OTLP inside the compose network: deployment design (service mesh/sidecars) rather than code. | Postgres, MinIO or OTLP leave the single-host compose network. |
| F201 | low | Audit hot path is coupled to Postgres: email resolver queries the whole tenant directory on the request context inside Emit, with no timeout or singleflight | audit email resolver coupling: performance, not security. | Audit latency or DB load shows in profiling. |
| F206 | low | Output credential redaction runs per streamed delta, so it does not catch secrets split across deltas, and tool args and results are never redacted | per-delta redaction: best-effort control, low. | Tool args or results are streamed to the UI or logs where a split secret matters. |
| F223 | low | Agent-written markdown links open in the browser on one click with no destination shown or confirmation | link confirmation UX in the desktop transcript: product decision. | The desktop transcript renderer is revised. |
| F229 | low | Internet-facing Traefik runs as root with the Docker socket mounted and forwards client X-Forwarded-* to the auth gate | Traefik with the Docker socket: standard compose pattern; docker-socket-proxy is an ops follow-up. | The azure overlay is hardened further: add a docker-socket proxy and drop trustForwardHeader. |
| F233 | low | Third-party infra images and hand-pinned CI tooling have no update feed and sit outside the release vulnerability gate | third-party infra image update feed: ops/dependabot config follow-up. | Dependabot or Renovate config is added for compose and CI images. |
| F234 | low | AG-13 Keycloak step-up flow lets a user with no enrolled OTP satisfy acr=mfa by self-enrolling (dev realm shipped to every variant) | Keycloak dev realm OTP self-enrolment: local dev variant only. | The Keycloak realm is used outside local dev. |
| F251 | low | Injection-pattern scan covers only office *.extract and memory reads; web.fetch, MCP, cloud-drive and workspace reads return external content unscanned and unaudited | injection scan coverage for web.fetch/MCP results: design follow-up (prompt-injection is contained, not prevented, per README). | The prompt-injection containment work resumes: scan or label web.fetch, MCP and drive content. |
| G9 | low | Tenant config keys documented as 'can only tighten' can be lowered by a single admin PUT | 'can only tighten' config keys: schema design follow-up. | The tenant config schema next changes. |
| G18 | low | Memory recall preamble puts the derived trust tier in-band after attacker-controlled text | in-band trust tier ordering: prompt-design follow-up. | The recall preamble format next changes. |
| G25 | low | Connector OAuth scopes are caller-supplied at Begin, stored as requested (not granted), and one combined read+write token serves read-only tools | connector OAuth scopes as requested vs granted: follow-up. | Connector scopes are reworked (read-only tools on read-only tokens). |
| G26 | low | Linked provider account identity is never captured or restricted (no hd/tid/ID-token check); 'common' Microsoft authority accepts personal accounts | linked provider account identity (hd/tid) restriction: follow-up. | A tenant needs linked accounts restricted to its own domain or tenant. |
| G27 | low | OBO bootstrap forwards the caller's raw bearer to Entra without local iss/aud/tid binding or validation of the admin-supplied Entra tenant id | OBO bootstrap token validation: Entra validates the bearer; local binding is a follow-up. | The OBO bootstrap is next touched, or non-Entra OBO is added. |
| G37 | low | Detector ignores the learned cost_p95: cost drift is never detected despite being learned and documented as part of the envelope | cost drift detection: feature gap. | Same as G35. |
| G40 | low | Grafana drift alert misses the first drift per (tenant, agent, kind) series, and the in-process alert engine only sees drift as a generic DENY | Grafana alert series gap: observability. | With the G35-G37 detection work. |
| F78 | info | Dockerfile hygiene: no .dockerignore, and the build and runtime stages are not pinned or reproducible | Dockerfile hygiene (info). | Together with F89/F107 when base images are digest-pinned. |
| F235 | info | Release vulnerability gate only blocks Critical findings that already have a fix; High and known-exploited findings pass | release gate severity threshold: policy decision (High with fix would block releases on upstream base-image CVEs). | The release policy is revisited (block High-with-fix and known-exploited). |

## Partial and skipped

What remains after the fix, as of `376a115`. The status follows the ledger
(after wave 3).

| id | sev | status | pkg | done | remains |
|---|---|---|---|---|---|
| F79 | high | partial | p6 | Minimal env tagged per job, a `/proc` sweep (sequential since wave 3) kills leftovers, `O_NOFOLLOW` + realpath job-file reads, qpdf password via file. | Concurrent jobs share uid 1000 and the `/tmp` tmpfs: a script can read or alter another tenant's in-flight files and read `AIKONOS_OFFICE_WORKER_TOKEN` from the server's `/proc/<pid>/environ`; a process that execs with an empty environment or makes itself non-dumpable escapes the sweep. Needs a per-job PID and mount namespace (bwrap/nsjail/gVisor). |
| F30 | medium | partial | p3a | The walk covers the trail oldest-first; a truncated, skipped or out-of-time walk shows "incomplete", never "intact". Wave 3 stops 2 s before the caller's deadline with 16 parallel GETs, so the admin gets a verdict, not an error. | No persisted, signed checkpoint: a real tenant's trail is verified only for the oldest prefix reachable in about 28 s. A truncated prefix of a legacy chain whose chain order differs from key order can show a "missing predecessor" break. |
| F33 | medium | partial | p3b | Per-(tenant, user) quota (2 GiB, 20000 entries), listing up to the entry quota; wave 3 charges skill-transfer staging to the sender and sweeps it hourly. | The legacy no-user tool-proxy per-task path is not quotaed; no tenant aggregate (accepted); with `quota_entries=0`, listings above 5000 fail closed and webui/gateway ignore `x-aikonos-list-truncated`. |
| F54 | medium | partial | p4b | Relayed usage is clamped to finite, non-negative values with per-call caps; provider, model and runId are clipped. | A child that reports zero tokens for real proxied calls is not booked, so spend caps and TPM miss it; provider and model are child-chosen. Needs metering in the gateway or the egress proxy. |
| F57 | medium | partial | p4b | `/files`, `/skills/import` and (wave 3) the admin upload routes verify the bearer before reading the body; `requestTimeout` 120 s. | No rate limit or connection cap on `:8080`; authenticated users can still send 14 MiB bodies. |
| F105 | medium | partial | p7a | Broker and gateway mount only their own leaf and `ca.crt`; `ca.key` stays out of every container. | Leaf keys are 0644, certs live 3650 days with no re-mint, and azure/onprem reuse the dev CA unless the operator supplies certs. |
| F110 | medium | partial | p7a | SIEM metrics pipeline redacted (log bodies were already redacted); https SIEM endpoint documented. | Collector runs as root (docker json logs are root 0640); OTLP and Loki receivers are unauthenticated in-network; https is not enforced. |
| F120 | medium | partial | p2b | `reads_sensitive` is no longer derived from the step's own class and `has_dlp_attestation` is forced false, so a client cannot switch the exfil rule off. | The exfil/DLP rule (M-08) cannot fire on the enforcement path until a task-scoped, broker-derived `sensitive_read_seen` flag exists (`TODO(M-08)`); the DLP stage (F121) is deferred. |
| F217 | medium | partial | p6 | Model scripts run under `prlimit` (address space, 64 MiB per file); wave 3 drops `--nproc` and puts fixed helpers, markitdown and pandoc under memory limits. | `RLIMIT_FSIZE` is per file, so one script can fill the shared 512 MiB `/tmp` and every concurrent job gets ENOSPC; per-job limits over 4 slots exceed `mem_limit` 2g (the cgroup is the backstop). |
| F227 | medium | partial | p3b | The broker refuses the shipped `<…-CHANGE-ME>` values for every secret it reads, since wave 3 also the office-worker token and the NATS credential. | No compose `:?` or pre-flight check for Postgres, Keycloak, MinIO and Grafana placeholder passwords. |
| G13 | medium | partial | p4b | Catalog names and descriptions are flattened and clipped (300 runes each since wave 3, so long personal skill names load). | A 300-character single-line instruction still lands in every recipient's system prompt; the transfer preview hides description and keywords from the recipient (needs proto). |
| F66 | low | partial | p4b | Error text to users and the model is trimmed and redacted, gate `reason` and the `/agui` command-skill error included (wave 3); provider redirects are refused (G3). | http provider endpoints are accepted (documented deviation; G2 deferred). |
| F114 | low | partial | p7a | Since wave 3 two NATS users: `broker` (publish and subscribe on `aikonos.>`) and `gateway` (subscribe to `aikonos.audit.>` only), so the gateway and its children cannot forge events; NATS errors are redacted. | A same-uid gateway child (F55) can read the gateway password from `/proc/<ppid>/environ` and then every tenant's audit stream; the gateway does not check its password for placeholders; monitoring port 8222 is unauthenticated and published in local dev. |
| F133 | low | partial | p1 | GUID externalIds are stored canonical; since wave 3, deactivate, reactivate, membership and delete act on both the stored and the canonical form of older rows. | A SCIM block written before the upgrade on an upper-case GUID still covers only that form until the next SCIM write for the user: restart provisioning (or re-sync) in the IdP after the upgrade. |
| F139 | low | partial | p3b | Each envelope stamps at most 50 tasks as `delegated_by` its sender; the count fails closed and refusals are audited as `delegation_ignored` (wave-3 decision replacing the ACCEPTED rule). | The recipient can still attribute up to 50 chats to the sender during the envelope's 72 h; concurrent creates can pass 50 slightly; the webui still shows the delegation banner past the cap. |
| F140 | low | partial | p1 | MCP OAuth begin needs `can_access` (audited); tokens bind to the token URL they were issued at. | No RFC 7009 upstream revoke for MCP tokens; per-user `mcp-<id>` Vault entries are not purged on connection delete (unusable, the id is never reused). |
| F200 | low | partial | p3b | Keepalive policy, connection timeout and bounded shutdown; wave 3: 1000 streams per connection and 64 connections per remote IP. | grpc-go decodes up to 16 MiB before the size interceptor, so pre-auth memory is bounded only by IPs × connections × streams × message size; compose passes neither limit; refusals are not logged. |
| F210 | low | partial | p7a | Gateway image is two-stage, `npm ci --ignore-scripts`, no toolchain at runtime. | Base image not pinned by digest; `apt-get upgrade` keeps builds non-reproducible. |
| F250 | low | partial | p3b | Envelope field caps; group fan-out capped at 50 members (refused and audited above). | No per-sender quota, no cap on pending envelopes per recipient, constant `SenderRemainingCost`, inbox still `LIMIT 100` with in-memory paging. |
| G24 | low | partial | p2b | Allowlist at Complete and on every gdrive/onedrive InvokeTool call (G5), audited. | `ensureOneDriveOBO` (managed OneDrive bootstrap), the managed-OneDrive workspace backend (`onedrivefs`) and toolproxy `freshAccessToken` are not gated, so `onedrive.*` tools are blocked while managed OneDrive workspace I/O is not. |
| F55 | medium | skipped | p4b | Comment in `child-entry.ts` documents the limit; wave 3 sets `TSX_DISABLE_CACHE=1` for children and adds M-24 to `SECURITY.md`. | The child shares the parent uid: it can read the SVID key under `/tls`, `/proc/<ppid>/environ` and the shared `/tmp`. `fork({uid})` needs CAP_SETUID; `node --permission` breaks the tsx loader. Real fix: separate container or uid for the child. |
| F218 | low | skipped | p7a | F37 removed the host publish of `:9092`. | The bundle listener binds every broker interface, so scripts on the `office` network (and every backend and mesh peer) can fetch the unauthenticated bundle and reach the TLS handshake of `:9090/:9091`; a Docker bridge has no per-port ACL. Needs a bearer token on the bundle endpoint plus OPA credentials (broker code). |

## Refuted

| id | sev | title | why refuted |
|---|---|---|---|
| F237 | high | memory.write lets any group member write shared memory reserved to managers | No create RPC exists; the manager-only rule covers verify/deprecate/delete. Member writes through `memory.write` are the documented design (`docs/00-aikonos-architecture.md:83`); trust is anchored in the human verify. All three refuters agreed. |
| F11 | low | Identity hardening switches default to audit | Documented, tenant-configurable rollout default (`SECURITY.md`, admin toggle) that emits `would_deny` events; enforcing by default would break existing tenants. Posture note, not a bug. |
| F42 | low | File-RPC denials and failed mutations are not audited | `AuditInterceptor` emits an `aikonos.broker.rpc` DENY for every north RPC that returns an error, including the `.agent/` guard; only the path detail is missing. |
| F75 | low | `window.location.assign(authorizeUrl)` with no scheme check (L-30) | The broker requires https for every MCP OAuth endpoint unless the dev-only `allow_private` flag is set, so a `javascript:` or `data:` URL never reaches the SPA. |
| F181 | low | BeginConnectorAuth lets the caller choose OAuth scopes | The only client sends `scopes: []`; a direct caller affects only their own provider account behind the consent screen. |
| G33 | low | Connector lifecycle failures are not audited | `AuditInterceptor` records every failed connector RPC as DENY in the WORM chain. |
| G43 | low | No default-deny method table in interceptors | Missing defence in depth with no exploitable RPC at HEAD; `ListConnectorProviders` is member-visible on purpose. |
| F99 | info | Desktop markdown has no `image_source` override | gpui has no HTTP client registered, so remote images cannot load; hypothetical until one is added. |
| G41 | info | SECURITY DEFINER baseline functions accept any cutoff | `aikonos_app` already has DELETE/SELECT on all tables and sets `app.current_tenant` itself; the functions add no capability. |

## Residual risks

Deduplicated from the 16 implementer reports and the wave-3 reviews and fixes,
restated for `376a115` (residuals closed by a later package or by wave 3 are
dropped). Deferred findings are in [Deferred](#deferred). This list feeds
`SECURITY.md` "Known limitations".

**Broker: identity, grants, sessions**
- A compromised gateway, or its same-uid child (F55), still acts for any user
  whose live bearer it sees (it can mint that user's session grant, valid at
  most as long as the bearer) and for the scheduled and external runs it holds
  grants for. Grants are not bound to a task id and their kind is not matched
  to the task's OBO kind; gateway votes stay limited to n=1 owner votes and
  step-up is refused (F5, F171).
- `GetGatewayApproval` mints step tokens for an APPROVED gateway-managed task
  without an owner grant (gateway SVID only; covers only voted steps).
- `ResolveRunAgent` takes `user_id` from the request (gateway peer and block
  check only; it discloses whether the user may use agents and the default
  agent): `ResolveRunAgentRequest.owner_grant` is deferred to the next proto
  wave. It and the dev-mode fallback check the block but not the revocation
  epoch (no `iat` on south requests); `/agui` gets the epoch check from
  `MintSessionGrant`.
- Org `unattendedAllowed` and pre-approval are enforced only in the gateway; the
  broker does not check where an approval came from (H-06).
- Owner grants are stateless and multi-use for their TTL (F183). v1/v2 grants
  are dated `expiry - max(TTL, 24h)`, so during rollout a revoke-all can also void
  live v2 grants.
- Per-process state assumes one broker replica: jti replay cache (F14),
  connector token lock (G22), skill-overlay cache invalidation (F25).
- `callerIdentity` still trusts request fields when no identity is present; the
  startup dev-mode guard is what prevents OIDC-off outside dev (F136).
  StreamTaskEvents has no request-size gate, and a stream of a user blocked
  mid-stream stays open until its next event or a disconnect, delivering
  nothing.
- SCIM: a block written before the upgrade on an upper-case GUID externalId
  covers only that form until the next SCIM write for the user, so restart
  provisioning in the IdP after the upgrade (F133). The group-in-use check is
  read-then-create and can race an admin grant (F4); an expired token shows only
  as 401 + audit `expired_token`; denial audits are bounded per event, not
  sampled per source.
- Vault over plain http stays allowed to loopback and single-label hosts because
  the compose Vault listener is plaintext (F108); login holds the client mutex
  across HTTP (availability).
- Workspace directory migration (G32/F175): legacy directories move once at
  startup to their single known owner; one that several identities shared is
  quarantined under `.legacy-<name>` (logged), and one whose subject is in
  neither the user directory nor the SCIM users stays unreachable (delete
  `.f175-migrated` and restart to rerun).
- Connector tokens: per-user MCP tokens are neither revoked upstream nor purged
  on connection delete (F140); MCP tokens stored before the change bind to the
  current endpoint on first refresh (trust on first use); OneDrive tokens cannot
  be revoked upstream (about 1 h access, up to 90 days refresh; G23).

**Broker: plan, invoke, policy, workflows**
- APPROVAL_REQUIRED/STEP_UP steps stay PENDING after approval; the step claim
  relies on the capability token, minted only on approval, as proof. A step is
  consumed before dispatch, so a dispatch failure burns it.
- The exfil/DLP rule (M-08) is unreachable until a task-scoped, broker-derived
  `sensitive_read_seen` flag and a broker-verified DLP stage exist (F120, F121).
- OpenFGA duplicate/missing detection relies on OpenFGA v1.5.3 message text; a
  batch retried tuple by tuple is not atomic (F239).
- Spend cap reads a 30 s cache before recording, so spend can overshoot (F27,
  accepted). The duplicate-step Rego rule is O(n²); Go caps plans at 256 steps.
- `requireGatewayPeer` now refuses south calls without a SPIFFE identity even in
  dev mode; recovery is dev mode with `GatewaySpiffeID` unset.
- The `Skills/` write guard covers only the tool-proxy seam (sandbox shell or
  code-exec writes were not checked); south personal-skill reads are not
  injection-scanned and skills written before the fix are still served (F23).
- MCP results are not injection-scanned, are bounded only by the 8 MiB body cap,
  still include the server URL, and a slow SSE stream can hold a goroutine up to
  30 s (F26). With `AIKONOS_TOOLPROXY_MCP_ALLOW_PRIVATE`, http MCP is allowed to
  private IPs and single-label names, which are trusted as in-mesh (F189).
  OneDrive `sessionUpload` PUTs to Graph's `uploadUrl` without a host check (no
  bearer is sent; F45). IPv4-compatible `::/96` is not blocked (F24).
- Managed OneDrive (the OBO bootstrap and the `onedrivefs` workspace backend)
  ignores the org connector allowlist while `onedrive.*` tools honour it (G24).
- Strict Drive/Graph id charset may reject a real id containing `.` or `:` (G28).
- Scheduler: the run claim is a timestamp, not a per-claim token, so a
  compromised gateway can report on any run it or the broker claimed (F172).
  A claim holds the next fire for a fixed 1 h, so a gateway run timeout above
  1 h brings overlapping fires back; a one-off run skipped in the claim loop
  stays ACTIVE with no next fire. A publish between the gateway's version
  re-check and `CreateScheduledRun` is not caught (F52, needs proto). With FGA
  disabled (dev) the scheduler and transfer gates allow everything (F191,
  F192). A lineage with no approved version answers NotFound to everyone, its
  owner included (F17).
- The webui web-search form has no `max_results` upper bound; the server rejects
  values above 50 (G6).

**Broker: audit, limits, workspace**
- Callers ignore audit-emit errors (`RecordEmitFailure`, the auth interceptor),
  so mutations succeed while the audit store is down (M-18 class, deliberately
  not changed). Dropped events are still logged and published (wave 3). Emit
  waits for MinIO on audited mutations (latency; up to about 3×10 s when MinIO
  is down). No spool/WAL: queued events are lost on a crash (threat-model D-07).
- Verify has no persisted, signed checkpoint: it stops 2 s before the caller's
  deadline and reports "incomplete", so a large trail is verified only for the
  oldest prefix reachable in about 28 s (F30).
- A never-signed chain verifies `ok=true, signed=false`; only the
  `audit_chain_unsigned_total` metric fires.
- The tenant claim is not validated as a UUID at the OIDC verifier; it is
  escaped in audit keys (F46).
- Admin audit: `llm.provider.key_retarget_denied` records no attempted host;
  M365, web-search and org-settings events record new values only (old value is
  in the previous event) (G8).
- Workspace quota is per (tenant, user), skill-transfer staging included: no
  tenant aggregate or volume cap; the quota walks the user dir on every write;
  the legacy no-user tool-proxy write path is not covered; with the entry quota
  off, listings above 5000 entries fail closed, and webui/gateway ignore
  `x-aikonos-list-truncated` (F33).
- Symlink containment is check-then-use (F44); `os.OpenRoot` would close it.
- grpc-go decodes a request before interceptors run, so an mTLS peer can make
  the broker allocate up to 16 MiB per stream; connections are capped per IP
  (64) and streams per connection (1000), and compose passes neither setting;
  no per-user north RPC rate limiter (F200, G48).
- Delegation attribution: a recipient can stamp up to 50 tasks per envelope as
  `delegated_by` the sender during its 72 h life, and concurrent creates can
  pass 50 slightly (F139; decision in [Wave 3](#decisions)).
- The broker's policy bundle listener on `:9092` is unauthenticated plaintext and
  binds every broker interface, so every backend, mesh and office peer
  (including attached MCP containers on `aikonos_mesh` and model-authored scripts
  in office-worker) can fetch it. It holds only the Rego policy (F37, F218).
- Denial alerts are suppressed by cooldown (high-severity rules are not);
  dropped alerts are logged, not exported as a metric (F34). Compose passes no
  `AIKONOS_ALERTING_*`, so alerting stays off unless an overlay wires a sink.
- Skill and memory south reads (`ListUserSkills`, `ListUserAgentSkills`,
  `ListPersonalSkillsSouth`, memory) return empty or NotFound rather than
  PermissionDenied on an FGA miss.

**agent-gateway**
- Same-uid child (F55, M-24): reads the SVID key under `/tls`, the parent's
  environment (including its NATS password, F114) and the shared `/tmp`.
  Children run with `TSX_DISABLE_CACHE=1` (wave 3); the real fix is a separate
  container or uid.
- The child can under-report LLM usage (zero tokens) to evade spend caps;
  provider/model in the relay are child-reported (F54).
- `analyze_image` and `spawn_subagents` need no prior gate decision; while
  `ListUserSkills` fails, interactive sessions get no registry tools (F207).
- `agent.external.invoked` records live only in a 1000-entry in-memory ring
  (F205).
- `AIKONOS_EXTERNAL_TRUST_PROXY=1` trusts one X-Forwarded-For hop from any peer:
  anything that reaches `agent-gateway:8090` directly (a mesh member, or a
  `traefik_net` member on onprem) picks its own per-IP bucket; the onprem README
  shows how to pin Traefik's address instead. SCIM shares the per-IP bucket; no
  per-agent concurrency cap (F56).
- `:8080` has no rate limit or connection cap; authenticated users can send
  14 MiB bodies (F57).
- http provider endpoints are accepted (F66, G2).
- A skill description (one line, up to 300 characters) reaches every
  recipient's system prompt, and the skill-transfer preview hides description
  and keywords from the recipient (needs proto); the broker does not reject
  control characters in skill dir names (G13).
- Two users at their per-user share (half the pool each) can fill the child
  pool, and one owner's large batch of scheduled runs can be refused (F58). A
  busy pooled child picks up a tightened provider pin only at its next idle
  reuse (F203).
- The gateway does not check that a NATS audit event's tenant matches its
  subject (F114, F204).

**webui**
- Tokens stay in `sessionStorage` (M-27); revocation happens only on explicit
  sign-out and only where the IdP has a revocation endpoint (not Entra).
- CSP not verified in a signed-in browser with shiki, katex and markdown
  rendering; an IdP whose token or JWKS endpoints sit on another origin than the
  authority is blocked. No HSTS (belongs at the TLS-terminating proxy).
- Markdown links allow both http and https (library-fixed), behind its confirm
  dialog.
- The agent-inventory CSV export is built by the gateway and is not
  formula-neutralised.
- A dropped workflow stream, or a 2xx non-SSE answer, makes the web console fall
  back to the blocking run, so a workflow can run twice (the desktop client no
  longer does).
- Clearing chat buffers on 401 or expiry also drops the local draft. The
  delegate modal has no focus trap; Enter no longer confirms.

**office-worker and docs-mcp**
- Concurrent jobs share uid 1000 and the `/tmp` tmpfs: a script can read or
  alter another job's files and read the worker token from the server's
  `/proc/<pid>/environ`; a process with an empty environment or no dumpable flag
  escapes the end-of-job sweep (F79, see Partial).
- Memory and disk limits are per job: one script can fill the shared 512 MiB
  `/tmp`, and concurrency × `RLIMIT_AS` can exceed the 2g `mem_limit` (the cgroup
  is the backstop); soffice has no rlimit (F217).
- Python `re` has no timeout: ReDoS is bounded only by the 120 s job timeout.
- The F88 `WEBSERVICE`/`file://` exploit was never reproduced; the soffice-gated
  regression test has not run (no soffice on the test host), and the soffice
  profile is checked against the schema only.
- Raw stderr from qpdf, pandoc and scripts (truncated to 16 KiB) is echoed in 500
  bodies.
- docs-mcp has no authentication and listens on `0.0.0.0` in the mesh: every
  mesh peer can read the whole `./docs` corpus, including this register. The
  Host/Origin allowlist only stops DNS rebinding and browsers.

**desktop**
- `AIKONOS_DESKTOP_ALLOW_HTTP=1` sends bearers in clear text (red warning).
- Split-host IdPs (Google, some Keycloak proxy setups) are refused; ports are
  not compared.
- ID-token signature, `iss`, `aud` and `exp` are not checked (nonce only; the
  profile is display-only and the gateway checks every bearer).
- reqwest follows redirects on token, end-session and revocation POSTs.
- Caps: 32 MiB per body, 16 MiB per SSE event; `download_text` decodes UTF-8
  lossily. With a gateway older than `stream=1`, a workflow run ends with an
  error instead of running twice.
- The schedule edit form resends the stored pre-approvals and cannot untick
  them (F211; the web console can).
- Mark-of-the-Web is best effort and was not compiled or tested on Windows.
- A sign-out during an in-flight refresh leaves the new refresh token
  unrevoked; credentials already saved in `desktop.json` stay until the next
  prefs write or sign-in.

**Deploy, compose, CI**
- OpenFGA has no authentication; isolation is network-only (backend, which
  includes Grafana and the one-shots) (F100).
- NATS has two users with subject permissions; the gateway's password is not
  checked for placeholders, a same-uid gateway child can use it to read every
  tenant's audit stream, the URLs carry the passwords (visible in
  `docker inspect`), and the monitoring port 8222 is unauthenticated and
  published in local dev (F114).
- Dev CA: leaf keys 0644, 10-year certs; azure/onprem reuse it unless the
  operator supplies certs (F105).
- Node base images are not digest-pinned; `apt-get upgrade` keeps builds
  non-reproducible (F89, F107, F210). `scripts/pin-image-digests.sh` refreshes
  only the overlay digest for `dev-ca-mint`; the base pin is copied by hand.
- The broker's Vault token can still read the signing keys, so a stolen AppRole
  `secret_id` can mint tokens. An existing Vault keeps `update` on the signing
  keys until `scripts/compose-vault-seed.sh` is re-run (upgrade step), and that
  script passes the root token on the `docker compose` argv while it runs. The
  403 create-race mapping was tested only against a fake Vault; a policy without
  `create` is logged with its path but still ends in "gave up resolving Vault
  key" (F115).
- On-prem push-to-deploy builds from signed source on the host, not from the
  cosign-verified release images (F106); the post-receive check runs after the
  ref moved, so `receive.denyNonFastForwards`/`denyDeletes` are the primary
  control (F228).
- Backups are not encrypted; a restored local-dev Vault needs `init.env` kept
  outside the backup (F109).
- Grafana reads names across tenants by design (F49); FGA object ids are global,
  one tenant per store is documented, not enforced (F127).
- Traefik keeps `docker.sock:ro` and `trustForwardHeader` (F119, F229).
- No cargo-audit; the shared target cache is restored on PRs (F225).

## Process notes

- Sources: `findings.json` (241 findings with votes), `ledger.json` (fix status,
  the source of truth for every status here; `w3_verdict` holds the wave-3
  reviewer's verdict per package), `pkg/deferred.json` (deferral reasons), the
  16 implementer reports, `wave3.json` (wave-3 reviews and fixes) and
  `prior-status.tsv` (docs/17 mapping).
- Package commits are the implementer commits merged into the integration
  branch; w2-p9's report names `b17aa66`, which landed as `a7649ed`. Wave-3
  fix commits are taken from git (`--grep='review fixes'`): several differ
  from `wave3.json` because trailers were amended (p2a `ea71a6a` →
  `f5a200e`, p2c `c00b443` → `d028499`, p3b `f264ed6` → `23ac6f1`, p5's
  second commit `70572bb` → `4581d34`); p5 and p8 have two commits each.
- `prior-status.tsv` mapped M-06 to F5/G46 as `deferred` and L-27 to F71 only;
  it now carries the final statuses, L-27 → F71 and F76, and the missing L-22
  (F66) and I-12 (F98).
- Doc drift noted by w2-p10/w2-p11 is fixed: the onprem README "Verify" ports
  and the azure README and `.env` Vault wording by w2-p10 `b0a5e5f` (wave 3);
  the `vault-broker-policy.hcl` 403 comment in the w2-p11/p7b merge
  (`df764f8`); `AIKONOS_DESKTOP_ALLOW_HTTP` was in docs/16 since `3c2f621`; the
  remaining "Vault is dev-mode or in-memory" lines in the `compose.azure.yaml`
  and `compose.onprem.yaml` comments, docs/00, docs/10, docs/ONBOARDING and
  `scripts/compose-verify-hardening.sh` by the seam fixes (`1afc810`).
- No `.env` file was read for this report; no secret values are reproduced.

## Wave 3 review fixes

**Method.** One adversarial reviewer (Claude Opus) per package re-verified every
finding of that package on the merged tree at `86c86a9`, with the finding, the
package decision and the implementer's report, and looked for bypasses and
regressions the fix introduced; one more reviewer checked the merge seams
(interceptor chains, env and compose wiring, startup guards against the shipped
examples, the gateway image, migrations on a fresh Postgres, CI jobs). The
package implementers then applied the review fixes in their worktrees, each
with a regression test that fails on `86c86a9` where a test applies, and the
branches were merged (`376a115`). Totals: 209 verdicts (167 fixed, 38 partial, 3 not claimed, 1 not fixed) and 104 issues (22 medium, 82 low) over the 16 packages; the seam review made 7 checks (5 fixed, 2 partial) and raised 9 issues (2 medium, 7 low). Verdicts are per
package, so an id reviewed by two packages counts twice; w2-p10's `docs`
verdict is not a finding.

Status changes from wave 3 (15, recorded in `ledger.json` with the reviewer's verdict in `w3_verdict`): fixed → partial: F30, F33, F54, F57, F120, F217, G13, F66, F114, F133, F139, F200; partial → fixed: F52, F85, F84.

| pkg | verdicts | issues | fix commit | applied | skipped or left, and why |
|---|---|---|---|---|---|
| p1 | 24 fixed, 4 partial | 1 medium, 6 low | `79dada9` | All 7 issues: startup-only legacy workspace migration with quarantine instead of a first-come claim (G32, F175); `group-` refused on user assertions; SCIM acts on both GUID forms (F133); revocation re-checked on every StreamTaskEvents event, subscribe audited; generic connector errors; audited group-claim failure; DB-backed F2 and interceptor-order tests. | Nothing skipped. Left: a SCIM block written before the upgrade on an upper-case GUID until the next SCIM write (F133; a data migration was out of scope); F140 upstream revoke and Vault purge (accepted). |
| p2a | 10 fixed | 6 low | `f5a200e` | Per-version share check on Get and Pin (F17), claims only for handed-out runs and a 1 h claim hold against overlapping fires, create-only scheduled session files (F172), publish re-pins the owner's own schedules (F18), member-access tests. | "Changed from vN to vM" on re-confirm needs `workflow_version` on ScheduledRun (proto frozen) and webui work. Left: a one-off run skipped in the claim loop stays ACTIVE with no next fire (needs a fail-or-retry decision); the claim hold is a fixed 1 h. |
| p2b | 16 fixed, 3 partial | 4 low | `2ef76f9` | A stored provider key is used only when the row has `has_key`, and provider store read errors fail closed (M-14, G1); A7 allowlist regression tests at Complete and at token use. | argsHash on NaN/Inf: cannot happen (structpb stringifies them), pinned by a test. docs/17 wording: docs package (done here). Accepted partials: G24, G6 web-search half (w2-p11 has it), F120 (DLP flag deferred). |
| p2c | 7 fixed, 1 partial | 3 low | `d028499` | Graph path segments refuse `:` and `\` through onedrivefs.ValidateSegment (G28); http MCP URLs to public hosts are refused even with the private opt-in (F189). | A stale comment in workspacefs/router.go and the "authored by the user" wording in the gateway system prompt: other packages' files, no runtime effect. |
| p3a | 6 fixed, 2 partial | 2 medium, 5 low | `4f24d20` | Dropped events are logged and published again (F28); Verify stops 2 s before the deadline with 16 workers and reports "incomplete" instead of an error (F30); a put that landed despite a timeout does not fork the chain; memory audit after the bundle lock; no Vault detail in chain breaks; tests for the fail-closed key path and the wait split. | F30's persisted, signed checkpoint: a new stored artifact plus UI and gateway paging (design change). Accepted by the decision: callers ignore Emit errors. |
| p3b | 15 fixed, 6 partial, 1 not claimed | 5 medium, 5 low | `23ac6f1` | Binary keys exempt from the `<` placeholder check; per-envelope cap of 50 delegated tasks (F139); staging charged to the sender's quota with an hourly sweep (F33); 1000 streams and 64 connections per IP (F200); ListDir up to the entry quota; verified session Move; startup re-sign of legacy sessions (F40); generic fileError sentinels; no leading-dot skill names; tests. | Reviewer-accepted partials F250, G17 (closed by w2-p9), F227 and the F101 skip. Left: grpc-go decodes before auth (F200); the legacy no-user tool-proxy path is not quotaed (F33); the F139 count races; compose does not pass the new gRPC limits. |
| p4a | 6 fixed, 2 partial | 2 medium, 7 low | `ae973c2` | Pinned agents never fall back to the env provider and a refused re-resolve evicts the child (F203); per-user share of the pool (F58); server-minted runId (F62); "owner grant revoked" counts as revoked (F129); trimmed schedule-approval reason (F66); branch pre-approval re-reads the approval mode (F51); tests. | The scheduleWorkflow version TOCTOU against CreateScheduledRun needs `CreateScheduledRunRequest.workflow_version` (proto frozen) and a broker check. |
| p4b | 14 fixed, 5 partial, 1 not claimed, 1 not fixed | 3 medium, 5 low | `73fe67e` | Bounded keyword matching, about 20 ms worst case (F202); abort on an early disconnect (F208); `TSX_DISABLE_CACHE=1` for children and the M-24 entry in SECURITY.md (F55); auth before the admin upload bodies (F57); gate reason trimming (F66); catalog name clip 300 (G13); `exp` required on bearers (F63). | Nothing skipped. Left, design: a child can report zero tokens (F54); no rate limit or connection cap on :8080 (F57); the transfer preview hides the description (G13, proto); the child shares the gateway uid (F55). |
| p5 | 10 fixed, 2 partial | 1 medium, 5 low | `2eb0b34`, `4581d34` | No desktop pre-approval baseline, an untick checklist and a runbook query for legacy schedules (F211); full hidden-character reveal including the approval reason (F213); no markdown table CSV download (F73); owner-bound chat storage (F74); a warning when the runtime OIDC settings are unset. `4581d34` only rewrites the regex with ASCII escapes. | No broker migration of legacy pre-approvals (it would also remove consent a user meant to give); the desktop edit form has no untick checklist. |
| p6 | 10 fixed, 3 partial | 4 medium, 5 low | `4a1525a` | `--nproc` dropped (it counted the host uid); zip reads bounded by bytes read and expansion checked first (F82); query echo caps; document parsers under memory limits; sequential /proc sweep; 512 MiB upload budget and 80 MiB output cap; corrected token comment; Host-denial log in docs-mcp; tests. | Nothing skipped. Left, design: per-job PID and mount namespace (F79); one job can fill the shared /tmp tmpfs and aggregate memory can pass mem_limit (F217). The soffice F88 test never ran (no soffice on the host). |
| p7a | 10 fixed, 4 partial, 1 not claimed | 5 low | `f236c51` | Two NATS users with subject permissions instead of one token (F114, in part); NATS parse errors redacted; the broker refuses placeholder office-worker and NATS credentials; idempotent dev-CA copy with a recovery message; onprem README "Verify" rewritten. | Read access from a same-uid gateway child (F114; needs the M-24 uid split); no placeholder check on the gateway's NATS password (gateway file); F218 backend-only bundle bind (fatal bind, IPAM collision on-prem); F105, F210, F110 residuals per the package decision. |
| p7b | 9 fixed, 3 partial | 8 low | `f57e9ab` | Deploy-hook baseline falls back to the bare HEAD (F228); runbook steps for the hook and the Vault policy re-seed (F115); non-object plan steps denied (F123); PGPASSWORD off the docker argv; no chmod of a caller's OUTPUT_DIR; one CI check per line; aikonosctl `fullmatch`; backup and aikonosctl tests in CI. | Nothing skipped. Left: the policy reaches an existing Vault only by the operator step; compose-vault-seed.sh still puts the root token on the docker argv while it runs (follow-up). |
| p8 | 12 fixed | 2 medium, 4 low | `ce48372`, `c3af867` | No read timeout on streams and no blocking re-run after a dropped stream or a 2xx JSON answer; hidden characters escaped in approval arguments (F213); bidi-safe save names (F220); plain-http IdP warning (F91); the tests share the production tree walk (F219). | docs/16 and docs/17 rows: docs package (done here). Left: the web console's workflow stream fallback can still run a workflow twice (p5 scope). |
| w2-p9 | 7 fixed, 2 partial | 2 medium, 4 low | `49d7744` | Agent memory recall needs `can_use` (F171); desktop Verify sends `content_sha256` (G19); FileSize callers fail closed on a truncated listing (G17); MintSessionGrant TTL capped at the bearer's lifetime. | ResolveRunAgent owner grant: needs `ResolveRunAgentRequest.owner_grant` (proto frozen), deferred to the next proto wave. docs/17 M-06 residual note: docs package (done here). |
| w2-p10 | 6 fixed, 1 partial | 5 low | `b0a5e5f` | Tenant-switch upgrade note (seeding, orphan sweep one hour after start); corrected `:9092` reach; onprem trust-proxy pinning documented (F56); OIDC runtime wording in five places; posture regression test, local-entra included; stale Vault and onprem docs fixed. | Backend-only bundle listener: a hostname bind is fatal on a DNS failure (design change). The onprem trust-proxy default stays `1`; check-env-drift.sh does not cover local-entra. |
| w2-p11 | 5 fixed | 5 low | `57d8a9f` | Stored web-search max_results clamped (G6); generic file and org-settings backend errors (F41); Vault policy comment corrected and a 403 on key create logged (F115); capability-key recovery runbook fixed (hard delete, correct path). | Nothing skipped. Left: the bootstrap still ends with a generic "gave up resolving Vault key" (the 403 log diagnoses it); webui code keyed on the old error texts was not audited. |

### Decisions

- **F139: a cap per envelope instead of ACCEPTED.** Wave 1 stamped
  `delegated_by` only for an ACCEPTED envelope. The p3b reviewer showed that
  this broke the webui flow (Inbox → *Send to agent* dismisses the envelope,
  then starts the chat) and still let a recipient accept once and stamp any
  number of chats during the envelope's 72 h. p3b replaced it with a cap: each
  envelope stamps at most 50 tasks as `delegated_by` its sender, counted by
  `CountEnvelopeTasks`; past the cap, or when the count fails, the stamp is
  dropped and audited as `delegation_ignored` (`envelope_task_cap`,
  `envelope_count_failed`). The finding stays `partial`: the recipient can
  still attribute up to 50 chats to the sender, and concurrent creates can pass
  50 slightly (count-then-create).
- **p3b: a connection cap per IP, not a global one.** The reviewer asked for a
  connection limit next to the stream cap (F200). A global
  `netutil.LimitListener(256)` was tried first; because 9090/9091 are
  published on the host in local dev, anyone could hold every slot and keep the
  gateway from reconnecting. p3b caps connections per remote IP instead
  (`broker.grpc_max_conns_per_ip`, default 64, excess closed at once and not
  logged) and raises the stream cap to 1000 (`broker.grpc_max_streams`) so the
  gateway's single shared channel is not starved. Compose passes neither.
- **w2-p9 test adjusted for p3b's larger list cap.**
  `TestPersonalSkillSouth_TruncatedListingNotRead` writes 5000 filler entries
  to truncate the size listing. p3b raised the ListDir limit to max(5000, entry
  quota), so the listing no longer truncated; the merge `c09c0a1` sets the
  entry quota to 0 in that test. The fail-closed path it pins is unchanged.
- **Desktop app-crate tests do not link on the dev host.** The
  `aikonos-desktop` test binary needs the libxcb and libxkbcommon development
  libraries, which the WSL dev host lacks. p5 and w2-p9 ran the client-crate
  tests (66/66) plus `cargo check` and `clippy` of the workspace; p8 linked
  the app tests against stub libraries (67 client + 20 app passed). The desktop
  CI workflow (`.github/workflows/desktop.yml`, Windows) runs
  `cargo test --workspace`.
- **Statuses.** A finding whose wave-3 reviewer said `partial` or `not_fixed`
  is `fixed` only where a wave-3 fix closed the reviewer's gap; where the gap
  stays open (an accepted residual or a skipped item), it is `partial`
  (Confidentiality > Integrity > Availability; `fixed` is never claimed for an
  open exploit). F115 and F211 count as `fixed` because their remaining step is
  a documented upgrade action in the runbook.

### Merge-seam review

One reviewer read the merged tree at `86c86a9` against `0ef97ce` and ran the
broker build, vet and tests, the gateway typecheck and touched tests, the
env-drift and posture checks, `docker compose config` for every overlay, a
gateway image build and smoke run, `migrate.sh` twice on a fresh
`postgres:16-alpine` (65 migrations, then none), and the new CI gates. Five of
seven checks passed; the interceptor chains and the production guards against
the shipped examples are `partial`. It raised 9 issues (2 medium, 7 low). Three
were already closed by package review fixes; the rest were fixed after the
wave-3 merges in `1afc810` (branch `security/w3-seams`, merged as `6728508`).

| # | sev | where | issue | outcome |
|---|---|---|---|---|
| 1 | medium | `broker/cmd/broker/main.go:1017-1021` | The north stream chain is only OIDC, tenant, revocation: no audit interceptor (so no panic recovery and no audit of stream denials), no provisioner, no size gate. | Fixed in `1afc810`: `auth.AuditStreamInterceptor` (recover, then audit; a panic becomes Internal plus a DENY event) and `Provisioner.StreamInterceptor`; the stream chain is OIDC, tenant, revocation, provisioner, audit. A client-closed stream is audited ALLOW (decision below). |
| 2 | medium | `deploy/compose/.env.azure.example:146-148` | The azure and onprem examples mark `AIKONOS_CAPABILITY_ROOT_KEY` required and suggest `openssl rand -hex 32`, which NewMinter rejects (fatal start); empty means Vault read-or-create. | Fixed in `1afc810`: both examples ship it empty (optional; to pin, a base64 64-byte ed25519 private key) and it left the onprem README's `openssl rand -hex 32` list. |
| 3 | low | `broker/cmd/broker/main.go:684-690` | secretsGuard does not cover `AIKONOS_OFFICE_WORKER_TOKEN`, so a shipped placeholder starts silently. | Already fixed by p7a `f236c51` (transportSecrets adds the office token and the NATS credential). |
| 4 | low | `broker/cmd/broker/main.go:1017` | StreamTaskEvents checks revocation only when the stream opens. | Already fixed by p1 `79dada9` (block and revocation checked before every delivered event). An idle stream stays open until the next event or a disconnect but delivers nothing. |
| 5 | low | `.github/workflows/ci.yml:157` | CI runs neither the office-worker suite nor `scripts/test_aikonosctl.py`. | Fixed: `1afc810` adds an office-worker-test job (pandoc, poppler, qpdf, tesseract, `uv sync --locked`, `npm test`; soffice tests skip) and `office-worker:test` in Taskfile `test:all`; p7b `f57e9ab` already ran the aikonosctl tests. Not yet run on a real runner. |
| 6 | low | `.github/workflows/ci.yml:1-12` | ci.yml has no `permissions` block and uses tag-pinned actions. | Fixed in `1afc810`: top-level `permissions: contents: read`, read-only scopes for the scorecard job, checkout, upload-artifact, setup-go and setup-node pinned by SHA, `persist-credentials: false`. |
| 7 | low | `broker/cmd/broker/main.go:1001-1022` | The north interceptor order is built inline and untested. | Fixed: p1 `79dada9` (northInterceptors, TestNorthInterceptors) and `1afc810` (TestNorthInterceptorOrder pins both chains slot by slot). |
| 8 | low | `broker/internal/db/alerts.go:96` | A comment says alerts_prune comes from migration 066; it is 065. | Fixed in `1afc810`. |
| 9 | low | `broker/internal/db/migrations/064_scim_token_expiry.sql:13` | 064 is the only recent migration without its own `schema_migrations` INSERT (migrate.sh records it anyway). | Fixed in `1afc810`. |

**Seam status: all 9 issues fixed.** Since `1afc810`, StreamTaskEvents passes
the same gates as unary calls except the request-size gate: JIT provisioning
also fires on stream calls, and the broker records an `aikonos.broker.rpc` audit
event when a stream ends: ALLOW for a normal end or a client-closed stream,
DENY for a handler error, DENY plus `Internal` for a recovered panic. Recording a
client-closed stream (`Canceled` with a done context) as ALLOW is a decision:
a DENY for every closed browser tab would flood the `high_deny_rate` alert and
bury real denials (integrity of the alert signal). Left: an idle stream of a
user blocked mid-stream stays open until its next event or a disconnect, but
delivers nothing; the new office-worker CI job has not yet run on a hosted
runner (the first CI run on `main` will show it). The `1afc810` commit also
fixes the remaining "Vault is dev-mode or in-memory" wording (see
[Process notes](#process-notes)) and the personalskill FileSize comment.
