# 13 — Zero Trust Audit (AI Agent Deployment)

> Audit date: 2026-06-19. Latest pass: 2026-08-07 (see the Executive Summary's 2026-08-07 update).
> Framework: *Zero Trust for AI Agents* — Anthropic, May 2026 (NIST SP 800-207 / NSA ZIGs 2026 / OWASP Top 10 for Agentic Applications 2026).
> Scope: Aikonos as a Claude-powered agentic platform. The AI harness is **Pi** — a custom TypeScript/Python agent loop running in a forked child of `agent-gateway`. Claude Code is a **developer tool only** and its `.claude/settings.json` permission model has no bearing on what the deployed platform enforces. All findings are against the Pi runtime, broker, toolproxy, and compose stack.
> Tier scoring: F = Foundation, E = Enterprise, A = Advanced.

---

## Executive Summary

| Tier | Result |
|------|--------|
| **Foundation** | ✅ CERTIFIED (under the admin-only MCP access model) — see Domain 6 |
| **Enterprise** | ✅ CERTIFIED (2026-07-04) — see census below; residual WARNs are documented non-blocking caveats |
| **Advanced** | ❌ Not certified — advanced-tier gaps remain (HSM/TPM keys, ML anomaly detection, AI-BOM automation beyond the script) |

**Foundation status — 2026-06-27 update.** Domain 6 (the sole Foundation blocker) is cleared:
1. **OpenSSF Scorecard in CI** — now runs as an informational job in `.github/workflows/ci.yml` (checksum-pinned Scorecard CLI v5.5.0, non-blocking).
2. **MCP server auth-at-registration** — documented **accepted risk**: MCP connection is admin-only, so the trust boundary is the admin who vets each local server. Reopens if MCP attach ever becomes non-admin. See `ROADMAP.md` → ZT6.

**Enterprise status — 2026-07-04 update (CP-B4, certified).** The three gaps named when the ladder was originally scoped were resolved or
risk-accepted across Phases 0-4 (SIEM export → Domain 3 PASS; KMS-managed audit signing key →
Domain 3/7 PASS; image signing → Domain 6 RISK-ACCEPTED), leaving exactly one unaccepted
Enterprise-tier ❌ per the prior audit-doc pass (CP4.4): Domain 4's "Automated behavioral baseline
learning." This build (`broker/internal/baseline/` + migration 032, see Domain 4 above) closes
that gap, so the census below is re-run in full — not rubber-stamped — before flipping the
verdict.

**Full Enterprise-tier (E) census, all ten domains, as of this pass:**

| Domain | Row | Status |
|---|---|---|
| 1 | Per-agent distinct identity | RISK-ACCEPTED |
| 1 | mTLS broker↔MCP | RISK-ACCEPTED |
| 2 | `InvokeTool` per-call FGA check | PASS |
| 2 | OPA `envelope_send` scope constraint | PASS |
| 2 | n-of-m approvals + SoD | PASS |
| 2 | HITL for NEEDS_HUMAN steps | PASS |
| 3 | SIEM / external log export | PASS |
| 3 | Audit HMAC signing key (KMS) | PASS |
| 4 | Threshold alerting on rate-limit breach | PASS |
| 4 | Automated behavioral baseline learning | **PASS (this build)** |
| 5 | Output content filtering (PII/creds) | PASS |
| 5 | Spotlighting (untrusted content demarcation) | WARN |
| 6 | AI-BOM (OWASP) | PASS |
| 6 | Signed compose / service images | RISK-ACCEPTED |
| 7 | Audit HMAC signing key (KMS) | PASS |
| 7 | Vault durable storage (was: inmem in dev) | **PASS (2026-08-07)** |
| 7 | Per-agent-type distinct Vault credential | RISK-ACCEPTED |
| 8 | Session file integrity verification | PASS |
| 9 | Signed compose / configs | WARN |
| 10 | *(no Enterprise-tier row — Domain 10's only non-Foundation row is Advanced)* | — |

Zero unaccepted Enterprise-tier ❌ **FAIL** remain. Applying the spec's certification rule
literally — flip to CERTIFIED only if no Enterprise ❌ remains unaccepted; RISK-ACCEPTED rows are
not blockers. **Enterprise is now ✅ CERTIFIED.**

Two E-tier rows are WARN, not FAIL — they do not block certification per the rule above, but are
recorded here as explicit non-blocking caveats rather than silently dropped (a third, Vault inmem
in dev, cleared to PASS on 2026-08-07 — see the update section below):

- **Spotlighting (Domain 5)** — model-compliance, not structurally enforced. The instruction is
  present and pinned by a red-first test, but a sufficiently capable prompt-injection could still
  get the model to disregard it; there is no structural (non-LLM) enforcement layer analogous to
  the SSRF guard or Biscuit scoping.
- **Signed compose/configs (Domain 9)** — adoption-gated and on-prem-only. Verification exists
  (`git-deploy-hook.sh`) but defaults to warn-and-proceed until `AIKONOS_DEPLOY_ALLOWED_SIGNERS`
  is configured, and only covers the on-prem git-push deploy path (azure/local have no
  equivalent).

No other Enterprise-tier row was found unaccepted on this census — the automated baseline
learner closed the one real remaining gap; there was no second gap hiding behind it.

**2026-08-07 update — audit pass over six subsystems shipped since 2026-07-04.** Full findings,
evidence, and remediation contract are documented separately. Not duplicated here.

Six agent-facing subsystems had shipped since the last pass and appeared nowhere in this document:
subagent fan-out (`spawn_subagents`), the bridge-direct Biscuit relaxation (`b7dab3d`), OKF agent
memory + semantic recall, personal-skill authoring and transfer, the office document tools, and the
Grafana MCP server (`f12b9f1`). Each was audited against the Enterprise census.

| # | Finding | Severity | Disposition |
|---|---|---|---|
| 1 | The workspace Files API bypassed the `.agent/` write guard entirely — any authenticated user could `POST /files` a concept into `.agent/Memory/` with forged `verified` frontmatter, with no `skill:memory.write` grant, and auto-recall then surfaced it labeled human-reviewed | HIGH | **Closed** (CP1, `8f2b408`). New rows in Domains 5 and 8 |
| 2 | The depth-1 subagent cap was enforced only by omitting `spawn_subagents` from the schema handed to the branch child; the parent never refused the IPC call, and FGA cannot distinguish depth 0 from depth 1 | MEDIUM | **Closed** (CP2, `7647812`). New row in Domain 2 |
| 3 | Untrusted-content scanning was annotate-only across three new subsystems, and `memory.read`'s `frontmatter`/`index` modes were not scanned at all | MEDIUM | **Narrowed** (CP3, `6e12bc7`). New rows in Domain 5. Domain 5's spotlighting row **stays WARN** — see below |
| 4 | office-worker took multipart ingress with no busboy `limits`; a decompression bomb driving the container to its 2 GiB `mem_limit` killed every in-flight office job for every tenant | MEDIUM | **Closed** (CP4, `63a0cdb`). New row in Domain 5 |
| 5 | The four `office.*.create` tools execute model-authored Node/Python with no `vm` sandbox, module allowlist, or tool-specific seccomp profile, classified `WRITE_LOCAL` with the reasoning recorded nowhere | LOW | **Recorded, no code change** (CP5) — effect class of the four create tools is documented with an explicit reopen trigger. The container boundary is the control and was re-verified |
| 6 | Three already-tracked residuals in `.claude/project/followups/` | LOW | Unchanged, still tracked; this pass did not touch them |

Checked and found sound, recorded so a later pass does not re-audit: `b7dab3d` is not a regression
(the `decisionOnly` branch covers two tools that map to bare capability skills with no toolregistry
scope, so no Biscuit was ever mintable; the full `SubmitPlan` → OPA → `CheckFGA` decision still
runs); subagent identity, isolation, eviction, and fan-out pre-gating; the parent/child credential
split after `reason`/`vision`/`embed` were added; personal-skill zip import; office XML handling and
the per-job LibreOffice profile; and memory's group pre-gate and preamble flattening.

**The Enterprise certification is not re-derived by this pass and does not change.** No finding
above was an unaccepted Enterprise-tier ❌ FAIL — Finding 1 was a Foundation-tier integrity control
that was broken and is now fixed, not a census row that had been passing on a false premise. The
census table above is amended in exactly one place (Domain 7's Vault row, WARN → PASS, on evidence
that predates this pass), and the verdict stands as recorded on 2026-07-04.

**Significant strengths** (well above typical agentic deployments):
- **Child process isolation**: Pi loop child structurally cannot hold the provider API key — the `EgressProxy` parent binds loopback-only, injects the key at forward time, and gives the child only a per-session opaque token. Prompt injection cannot exfiltrate the key.
- **Four-layer fail-closed authorization**: OIDC/SPIFFE → OpenFGA ReBAC (deny-by-default for personal tasks) → OPA ABAC (whole-plan + per-step) → Biscuit capability token (one per approved step, scope narrows only). Any layer denying blocks execution.
- **Plan-level exfil guard**: `plan_validation.rego` raises a violation if any step reads sensitive data (`reads_sensitive=true`) AND any step writes externally, without `has_dlp_attestation` — a structural defense against prompt-injection read-then-exfil attacks at plan submission.
- **SSRF at TCP `Dialer.Control`**: checked at every `connect()` including redirects — DNS rebind and TOCTOU attacks structurally blocked.
- **WORM audit trail**: hash-chained + HMAC-SHA256 signed events to MinIO object-lock; chain head persists across restarts via S3 object metadata.

---

## Domain 1 — Identity & Authentication

**Verdict: ⚠️ WARN**

| Control | Tier | Status | Evidence |
|---------|------|--------|----------|
| JWKS-backed JWT verification | F | ✅ PASS | `agent-gateway/src/auth/verify.ts:44` — `jwtVerify` with cached `createRemoteJWKSet`; issuer + audience validated; JWKS auto-rotates on unknown kid |
| Fail-closed on missing claims | F | ✅ PASS | `verify.ts:51-65` — throws on missing sub/email/tenant; no partial principal accepted |
| SPIFFE mTLS (south gRPC) | F | ✅ PASS | Static dev-CA mints URI-SAN `spiffe://aikonos.com/<svc>` + DNS-SAN; broker reads `AIKONOS_BROKER_TLS_*`; gateway uses same CA for its broker gRPC clients |
| Token expiry | F | ✅ PASS | OIDC JWTs expire; JWKS auto-rotation on kid miss; no long-lived bearer stored |
| Split-horizon issuer decoupling | F | ✅ PASS | `AIKONOS_OIDC_ISSUER` (browser-facing) ≠ `AIKONOS_OIDC_JWKS_URL` (container-facing) — prevents "claims invalid: iss" when container URL differs |
| Pi child identity binding | F | ✅ PASS | Child identity bound from spawn record at fork time; never from IPC message body — a rogue message cannot impersonate another user |
| Scheduler HMAC owner grant | F | ✅ PASS | Unattended scheduler uses broker-minted HMAC owner grant (`gateway_tasks.go`); task owner derived from the grant, not from the RPC body — `req.OwnerUserId` disagreement rejected; approver derived from stored task row |
| OAuth 2.0 / auth for MCP servers | F | ⚠️ WARN | Connector MCP servers (Drive/OneDrive) use Vault-backed OAuth. `mcp-echo` dev server is unauthenticated — now gated to `[dev]` compose profile, excluded from `core`/`full` production stacks. `aikonos-mcp-grafana` (`compose.yaml:758`, opt-in `mcp-grafana` profile at `:761`) authenticates **outward** to Grafana with a service-account token but requires no caller auth **inward** — see Domain 6 |
| Per-agent distinct identity | E | ⚠️ RISK-ACCEPTED | All agents share the same broker AppRole; no distinct Vault credential per agent type. Accepted: the broker is the sole Vault client — agents never dial Vault directly (`agent-gateway`'s Pi child holds no Vault credential at all; the parent's egress proxy mediates all secret use). Reopens if any agent-side component gains its own Vault path. See `ROADMAP.md` → ZT7 |
| mTLS broker↔MCP | E | ⚠️ RISK-ACCEPTED | MCP servers reached over HTTP with SSRF guard; no mutual TLS between broker and MCP endpoints. Accepted: MCP attach is admin-only and every connection is SSRF-guarded at the dialer (`broker/internal/toolproxy/mcp.go`) — the trust boundary is the vetting admin, not the transport. The static dev-CA (`scripts/compose-dev-ca.sh`) already mints SPIFFE-URI-SAN certs for internal services and can mint MCP client certs once mTLS is needed. Reopens if MCP attach becomes non-admin, or any MCP server moves off localhost/trusted infra. See `ROADMAP.md` → ZT8 |
| Hardware-backed identity | A | ❌ FAIL | No HSM/TPM. Vault's unseal/root key material is software-held on a `file` storage backend (`compose.yaml:216-256`); durable, but not hardware-sealed — the Advanced-tier ask is unchanged |

**Remediation:**
- Bind the Vault AppRole to a machine identity (the inmem→durable-backend half of this bullet is done — see the 2026-08-07 update below).

**2026-07-03 update (CP1.3).** Per-agent distinct Vault credential (row above) is now recorded as an explicit risk acceptance rather than an open WARN, mirroring ZT6's MCP-auth acceptance: the broker is the sole Vault client in this architecture, so a shared AppRole does not create agent-to-agent Vault-path exposure today.

**2026-07-04 update (CP4.4).** mTLS broker↔MCP (row above) moves WARN→RISK-ACCEPTED, mirroring
ZT6's MCP-auth acceptance and ZT7's per-agent-Vault acceptance: admin-only attach + the existing
SSRF guard together substitute for transport-level mutual auth today. Reopen trigger recorded in
`ROADMAP.md` → ZT8.

**2026-08-07 update.** Two corrections. (1) The Advanced-tier hardware-backed-identity row cited
"Vault is in-memory dev mode" — false since durable Vault shipped; corrected in place. Vault's key
material is software-held on a `file` backend, which is what keeps this row ❌ FAIL at the Advanced
tier, but the inmem premise is gone. (2) A second MCP server, `aikonos-mcp-grafana`, now exists —
see Domain 6 for the risk-acceptance analysis; it is opt-in and off by default, so it changes no
row's status here.

---

## Domain 2 — Access Control & Privilege

**Verdict: ✅ PASS (Foundation)**

The Pi runtime's access control is implemented entirely at the application layer — not via a runtime-injected sidecar or host policy. It is deny-by-default and fail-closed at every enforcement point.

| Control | Tier | Status | Evidence |
|---------|------|--------|----------|
| Deny-by-default (personal tasks) | F | ✅ PASS | `broker/internal/broker/service_plan.go:468` — `userSkillDecision` calls `CheckFGA(user, can_invoke, skill:<tool>)` per step; deny unless a relation exists; fail-closed on FGA error |
| Whole-plan OPA validation | F | ✅ PASS | `policies/opa/plan_validation.rego` — evaluated at `SubmitPlan`; any deny on any step → DENIED |
| Per-step OPA evaluation | F | ✅ PASS | `policies/opa/tool_invocation.rego` — evaluated for each step; step-up and human-approval paths explicit |
| Biscuit capability scoping | F | ✅ PASS | `broker/internal/capability/capability.go:63` — one Biscuit per approved step; empty scope list rejected; `Attenuate` is keyless (can only narrow) |
| Agent.Skills boundary | F | ✅ PASS | Agent-bound tasks: `agent.Skills` is authoritative at `SubmitPlan`; tools not in the agent's bundle are denied regardless of user grants |
| Approval aggregate: deny beats approve | F | ✅ PASS | Any deny → DENIED; step-up → NEEDS_STEP_UP; human approval → NEEDS_HUMAN; only unanimous non-deny → APPROVED |
| `InvokeTool` per-call FGA check | E | ✅ PASS | `mcp:` tool calls: `CheckFGA` re-evaluated at `InvokeTool` time (not only at plan submission) |
| OPA `envelope_send` scope constraint | E | ✅ PASS | `policies/opa/envelope_send.rego` — constrains what scopes a delegated agent may forward; mirrors Biscuit attenuation in policy |
| n-of-m approvals + SoD | E | ✅ PASS | `approval_required_n` config; separation-of-duty enforced at approval layer |
| HITL for NEEDS_HUMAN steps | E | ✅ PASS | Gateway HITL approver wired; unresolved approval blocks execution |
| IPC tool-membership backstop | F | ✅ PASS | `agent-gateway/src/ipc/bridge-server.ts:389` — `dispatch` refuses any op naming a tool outside the session plan's `allowedToolNames`, captured per child at fork time (`ipc/supervisor.ts:775`, held at `bridge-server.ts:171`). Covers `gate` (`:182`) and every bridge-direct op that *is* a Pi tool, each naming its tool with a literal: `delegate` `:197`, `workflow_save` `:205`, `workflow_run` `:213`, `workflow_list` `:221`, `workflow_publish` `:229`, `workflow_propose` `:237`, `analyze_image` `:250`, `workflow_schedule` `:258`, `spawn_subagents` `:291`. Fail-open on an empty set (`:345-347`), commented as deliberate — defence in depth against a compromised child, not the primary gate |
| JIT / session-scoped grants | A | ⚠️ WARN | Grants are persistent (FGA tuples); no automatic expiry on session end; revocation is manual |

**2026-07-03 update (F24).** Delegation scope resolution (the set of scopes a sender may forward via `SendEnvelope`, checked against the `OPA envelope_send scope constraint` row above) is now FGA-derived: `senderDelegableScopes` (`broker/internal/broker/delegation_scopes.go`) resolves `Policy.ListObjects` → `toolregistry.RequiredScope`, fails closed on an FGA error or nil `ToolRegistry`. The hardcoded dev-stub scope list is used only when FGA is disabled (dev allow-all mode); envelope-send audit events now carry a `delegation_scopes: "fga-derived"|"dev-stub"` marker so the source is visible per event, and the startup warning naming the stub fires only when FGA is disabled.

**2026-07-03 update (F17).** Tenant scoping hardened at the DB layer: `withTenant` now sets `app.current_tenant` via a bound `set_config($1,...)` parameter (`broker/internal/db/tasks.go`) instead of string-interpolated `SET`, closing a GUC-injection surface; `db.NewPool` (`broker/internal/db/pool.go`) installs a fail-closed `AfterRelease` hook that resets the GUC on every connection release and destroys the connection if the reset fails, preventing tenant-context leakage across pooled connections.

**2026-08-07 update (CP2, `7647812`).** New row above: an IPC tool-membership backstop at the
gateway's parent/child boundary. It was added because the depth-1 subagent cap was documented as
"enforced structurally, not by prompt" in `pi/session-plan.ts` and
was not: omitting `spawn_subagents` from the tool schema constrains a well-behaved child only, and
nothing downstream catches a child that sends the IPC message anyway — a branch runs under the
caller's own real identity, so `CheckFGA(user, can_invoke, skill:subagents)` passes at depth 1
exactly as at depth 0. The parent now refuses any op naming a tool outside the session's own set,
which closes recursive fan-out and, more generally, every case where a compromised child invokes a
tool it was never granted. The deny-by-default rows above are unaffected — this is a backstop in
front of them, not a replacement.

**2026-08-07 correction.** The deny-by-default evidence cited
`broker/internal/broker/service.go`; the per-step `CheckFGA` now lives in
`service_plan.go:468` (`userSkillDecision`). Citation corrected in place.

---

## Domain 3 — Observability & Auditing

**Verdict: ⚠️ WARN**

| Control | Tier | Status | Evidence |
|---------|------|--------|----------|
| Hash-chained WORM audit | F | ✅ PASS | `broker/internal/audit/emitter.go` — SHA-256 chain + HMAC-SHA256 sig → MinIO object-lock `GOVERNANCE`/365 days; async fire-and-forget (no latency added to RPCs) |
| Chain survives restarts | F | ✅ PASS | `chainHashMeta` S3 user-metadata key; `loadChainHeads` seeds per-tenant `lastHash` on startup |
| NATS→SSE live audit stream | F | ✅ PASS | Gateway `/api/audit/stream` SSE; webui consumes it for real-time visibility |
| State-transition audit on every RPC | F | ✅ PASS | `audit.Emitter.Emit` called on every task state transition in broker |
| OTel collector in stack | F | ✅ PASS | `otel-collector` in `obs` profile (Loki + Tempo + Prometheus + Grafana) |
| OTel tracing enabled by default | F | ⚠️ WARN | `compose.yaml:447` — `AIKONOS_OTEL_ENDPOINT` defaults to empty; operator must opt in |
| SIEM / external log export | E | ✅ PASS | `deploy/compose/obs/otel-collector-config-siem.yaml` — `otlphttp/siem` exporter wired into the traces/logs/metrics pipelines alongside the local Tempo/Loki/Prometheus sinks, selected via `AIKONOS_OBS_ARCHIVE=siem` (`compose.yaml`); endpoint/auth configured via `AIKONOS_SIEM_OTLP_ENDPOINT`/`AIKONOS_SIEM_OTLP_AUTH_HEADER`; documented in `deploy/compose/README.md` ("Shipping to a SIEM") and `deploy/onprem/README.md` |
| Audit HMAC signing key (KMS) | E | ✅ PASS | `broker/internal/secrets/vault.go` — read-or-create Vault KV-v2 versioned key `broker/audit-signing-key` (`cas=0`, mirrors the capability-key pattern); `broker/cmd/broker/main.go:resolveAuditSigningKey` sources it, each signed event records `signing_key_version` (`broker/internal/audit/emitter.go`); `VerifyChainVersioned` (`broker/internal/audit/verify.go:61`) resolves the key per recorded version and rejects a `signing_key_version` downgrade (`cp1_2_test.go:TestVerifyChainVersioned_SignatureDowngradeRejected`); env `AIKONOS_AUDIT_SIGNING_KEY` remains a fallback (version 0) when Vault is unavailable, with a loud grep-stable warning (`"audit signing key: Vault unavailable — falling back to static env key"`) |
| Full Pi reasoning provenance | A | ⚠️ WARN | Audit captures tool invocation + outcome; Pi's intermediate reasoning steps are not included in the audit record |

**Remediation:**
- Default `AIKONOS_OTEL_ENDPOINT=otel-collector:4317` in `.env.local.example` so tracing is on out of the box with the obs profile.

**2026-07-03 update (partial D3 progress).** Archive export gained S3 and Azure Blob exporters, selected via the existing `AIKONOS_OBS_ARCHIVE=file|s3|azure` switch (`deploy/compose/obs/otel-collector-config-s3.yaml`, `deploy/compose/obs/otel-collector-config-azure.yaml`, wired at `compose.yaml`). This is archive storage, not a SIEM feed — no `otlphttp` export to Splunk/Datadog/Elastic exists yet, so the SIEM / external log export row above remains ❌ FAIL.

**2026-07-03 update (CP1.2).** Audit HMAC signing key row moves WARN→PASS: the key now lives in Vault KV-v2 as a versioned secret with a documented rotation procedure (`docs/OPS-RUNBOOK.md`), replacing the static dev string this row previously flagged. The remediation line calling for this ("Wire `SigningKey` from Vault transit") is removed as resolved (KV-v2, not the Transit engine, per the CP1.2 design decision — functionally equivalent for this row's concern: rotation without re-keying the chain).

**2026-07-03 update (CP2.2).** SIEM / external log export row moves FAIL→PASS, closing the gap the 2026-07-03 partial-progress note above left open: `deploy/compose/obs/otel-collector-config-siem.yaml` adds an `otlphttp/siem` exporter (traces, logs, and metrics) selected via `AIKONOS_OBS_ARCHIVE=siem`, running alongside — not replacing — the local Tempo/Loki/Prometheus sinks; endpoint and auth are configured via `AIKONOS_SIEM_OTLP_ENDPOINT`/`AIKONOS_SIEM_OTLP_AUTH_HEADER`, documented in `deploy/compose/README.md` ("Shipping to a SIEM") and `deploy/onprem/README.md`. This is now a real feed to Splunk/Datadog/Elastic (any OTLP-HTTP-native receiver), not just archive storage.

---

## Domain 4 — Behavioral Monitoring

**Verdict: ✅ PASS (Foundation + Enterprise) — Advanced-tier drift/ML anomaly detection remains a gap**

| Control | Tier | Status | Evidence |
|---------|------|--------|----------|
| In-process RPM/TPM rate limiter | F | ✅ PASS | `broker/internal/ratelimit/` — sliding-window limiter; hot-reload on policy mutation (migration 021); wired to both `BrokerService` and `SandboxService` |
| Egress rate check before LLM request | F | ✅ PASS | `EgressProxy.setRateLimitChecker` calls south `CheckRateLimit` RPC before forwarding each completion request |
| Fail-open on rate-limit transport error | F | ✅ PASS | `agent-gateway/src/llm/rate-limit-breaker.ts` — `createRateLimitBreaker` wraps `CheckRateLimit`: steady-state fail-open (a broker restart must not black out LLM egress — recorded design intent), but after `AIKONOS_GATEWAY_RATELIMIT_BREAKER_THRESHOLD` (default 5) consecutive transport failures the breaker opens and fast-fails **without** issuing the RPC (true load-shed), probing every 10th open-state call; any resolved RPC resets it. A denied result never trips the breaker. Bounds the "network attacker forces fail-open → unlimited egress" window instead of leaving it unbounded (`agent-gateway/test/rate-limit-breaker.test.ts`) |
| Documented per-agent baselines | F | ✅ PASS | `docs/agents/baselines.md` (Phase 4, CP4.3) — per-agent-type expected tool set (from skill bundles), RPM/TPM ceiling (`rate_limit_policies` shape), typical data volume, and the PromQL to check each against `rate_limit_hits_total` + the F23 lifecycle instruments |
| Threshold alerting on rate-limit breach | E | ✅ PASS | `deploy/compose/obs/grafana/provisioning/alerting/rate-limit-alert.yaml` — Grafana unified alert on `increase(rate_limit_hits_total{result="denied", exported_job="aikonos-broker"}[5m]) > 0`; fires after 1m, severity=warning |
| Automated behavioral baseline learning | E | ✅ PASS | `broker/internal/baseline/` (Recorder/Learner/Detector) + migration 032 (`agent_behavior_windows`, `agent_baselines`) — the broker observes every agent-bound `InvokeTool` call in-process (no OTel/Prometheus round-trip), persists rolling windows, and periodically computes a learned per-agent envelope (`tool_set`, `rpm_p95`, `cost_p95`). Deviation past a maturity gate (`sample_windows ≥ AIKONOS_BASELINE_MIN_SAMPLE_WINDOWS`, default 30) emits a `aikonos.agent.baseline_drift` audit event + `aikonos.broker.agent.baseline_drift` OTel counter (`aikonos_broker_agent_baseline_drift_total`), alerted by `deploy/compose/obs/grafana/provisioning/alerting/baseline-drift-alert.yaml`. Monitoring only — never blocks `InvokeTool` |
| Drift / ML anomaly detection | A | ❌ FAIL | Not implemented |

**2026-07-04 update (CP4.1).** Fail-open-on-transport-error row moves WARN→PASS. The prior remediation ("evaluate fail-closed… circuit-breaker pattern is a middle ground") is implemented as exactly that middle ground: `createRateLimitBreaker` (`agent-gateway/src/llm/rate-limit-breaker.ts`) keeps fail-open as the steady-state posture but opens after `AIKONOS_GATEWAY_RATELIMIT_BREAKER_THRESHOLD` (default 5) consecutive transport failures, thereafter fast-failing without an RPC (load-shed) and probing every 10th open-state call; a resolved RPC resets it and a denied result never trips it. The bare unbounded fail-open the row previously flagged no longer exists.

**2026-07-03 update.** The alert row previously cited `aikonos_rate_limit_exceeded_total`, a series no broker instrument ever emitted — the alert was silently dead. Corrected (commit `3b10f34`) to `increase(rate_limit_hits_total{result="denied", exported_job="aikonos-broker"}[5m]) > 0`, matching the counter the broker actually registers (`broker/internal/ratelimit/limiter.go` `recordHit`, wired `broker/cmd/broker/main.go`) and the working Broker Actions dashboard PromQL.

**2026-07-04 update.** Automated behavioral baseline learning (Enterprise-tier row above) moves
FAIL→PASS: `broker/internal/baseline/` derives a learned per-agent envelope from observed
`InvokeTool` activity (migration 032), and drift past the maturity gate surfaces as an audit
event + OTel counter + Grafana alert — see the row's evidence above and
`docs/agents/baselines.md`'s "Automated baseline learning" section for the full model. This was
the sole unaccepted Enterprise blocker recorded in the executive summary; see that section for
the recomputed verdict.

**2026-07-04 update (CP4.3, closed out by CP4.4).** Documented per-agent baselines (Foundation-tier
row above) moves FAIL→PASS: `docs/agents/baselines.md` now documents expected tool set, RPM/TPM
ceiling, and typical data volume per agent type, with the PromQL to verify each. The now-resolved
"Document baseline RPM/TPM ceiling…" remediation bullet is removed above. This is distinct from the
Enterprise-tier "Automated behavioral baseline learning" row below, which remains ❌ FAIL — a
hand-authored reference doc is not automatic derivation from OTel traces.

---

## Domain 5 — Input Validation & Output Controls

**Verdict: ⚠️ WARN**

| Control | Tier | Status | Evidence |
|---------|------|--------|----------|
| SSRF guard at TCP `Dialer.Control` | F | ✅ PASS | `broker/internal/toolproxy/web_fetch.go:241` `hardenedClient`, `dialer.Control` assigned at `:244` — blocks loopback/RFC1918/link-local/cloud-metadata at every `connect()` including after redirects |
| DNS rebind protection | F | ✅ PASS | Guard re-evaluated post-DNS at every TCP connect; redirect to private host blocked even after an initially public DNS response |
| MCP guarded dialer | F | ✅ PASS | `broker/internal/toolproxy/mcp.go:7-8` — same SSRF-hardened dialer as `web.fetch` |
| `AllowPrivateHosts` is test-only | F | ✅ PASS | Flag documented `MUST be false in production`; production codepath never sets it — `toolproxy_test.go:23` |
| SSRF unit tests | F | ✅ PASS | `TestWebFetch_SSRFGuardBlocksPrivate` covers loopback, RFC1918, IPv6 loopback, metadata IP, and redirect bypass — `toolproxy_test.go:60-108` |
| Biscuit scope narrows only | F | ✅ PASS | `capability.go` — `Attenuate` is keyless; a Pi child cannot expand the scope it was minted with |
| OPA `envelope_send` scope fence | F | ✅ PASS | Delegation scope checked at policy layer before any outbound envelope |
| OPA plan-level exfil guard | F | ✅ PASS | `plan_validation.rego` — violation raised if plan reads sensitive data + writes externally without `has_dlp_attestation`; blocks read-then-exfil prompt-injection pattern at `SubmitPlan` |
| Workspace path isolation | F | ✅ PASS | `workspacefs` enforces `Root/<tenant>/<user>/` prefix; cross-tenant writes structurally rejected |
| `.agent/` write guard at the user-facing file RPCs | F | ✅ PASS | `broker/internal/broker/files.go:83` `agentDirGuard`, delegating to `workspacefs.UnderAgentDir` (`workspacefs/store.go:136`); called by `UploadWorkspaceFile` (`files.go:224`), `DeleteWorkspaceFile` (`:251`), `MoveWorkspaceFile` on **both** `FromPath` and `ToPath` (`:276`, `:279`), `CreateWorkspaceDir` (`:303`) — `PermissionDenied`, not a storage error. `.agent/Sessions/` is carved out by the case-sensitive `UnderSessionsDir` (`workspacefs/store.go:152`); the block side casefolds (`workspacefs/store.go:141`). Both predicates normalize through `CleanRel` (`guardClean`, `workspacefs/store.go:118`) so guard and resolver cannot disagree on one string. The tool seam's own `agentDirGuard` (`toolproxy/workspace_seam.go:93`) now delegates to the same predicate and takes no Sessions carve-out |
| Injection scan on every `memory.read` mode | F | ✅ PASS | Annotate-only, matching the `*.extract` posture: `toolproxy/memory.go:130` (`mode=index`), `:186` (`mode=frontmatter` and `mode=concept`, via `memoryReadConcepts`) |
| Flagged concepts excluded from memory auto-recall | F | ✅ PASS | `broker/internal/broker/memory_south.go:123` — `ListMemoryConceptsSouth` omits any concept whose `recallVisibleText` (`:181`; covers `id`, `title`, `description`, `tags` — the union of what `buildMemoryPreamble` and `memory-semantic.ts` put in front of a model) matches `toolproxy.ScanForInjection`. Structural omission, not annotation, because auto-recall has neither a human nor a model decision in the loop; each omission emits `aikonos.memory.recall_omitted` (`:35`, emitted at `:157`) so a false positive is visible rather than silent |
| office-worker ingress bounds | F | ✅ PASS | `office-worker/src/multipart.js:54` `parseMultipart` passes busboy a `limits` object (`:63-76`) capping file size (`DEFAULT_MAX_UPLOAD_BYTES` = 11 MiB, `:31` — deliberately at or above the broker's 10 MiB `workspacefs.MaxFileBytes` so no broker-accepted request is rejected here), file count (`:39`), and field size/count (`:44-45`). Every cap rejects with HTTP 413 (`:23`) rather than busboy's default silent truncation. Per-job Node heap ceiling via `--max-old-space-size` (`handlers/run-script.js:103`) so one model-authored script cannot OOM the shared container |
| Output content filtering (PII/creds) | E | ✅ PASS | `agent-gateway/src/routes/agui.ts:redactCredentials` — per-delta scan for AKIA keys, GitHub PATs/tokens, generic api_key/bearer/secret patterns; redacts in place before SSE flush; 7 unit tests |
| Spotlighting (untrusted content demarcation) | E | ⚠️ WARN | `agent-gateway/src/pi/system-prompt.ts` `buildSystemPrompt` — unconditional section instructing the model that tool results and fetched external content are data, not instructions, and to ignore directives embedded in quoted material; pinned by `agent-gateway/test/system-prompt-spotlighting.test.ts`; model-compliance only, not structurally enforced |

**2026-07-03 update.** The prior evidence cited `session.ts:31`, a file deleted in the F29 prompt-collapse refactor (`buildSystemPrompt` was consolidated into `pi/system-prompt.ts`) — the spotlighting guidance was silently dropped along with it and the audit row went stale. Restored (commit `f2cfe69`) as an unconditional untrusted-content section in `buildSystemPrompt`, now pinned by a red-first test so a future prompt refactor can't drop it unnoticed.

**2026-08-07 update (CP1 `8f2b408`, CP3 `6e12bc7`, CP4 `63a0cdb`).** Four rows added above.

- **`.agent/` write guard at the user-facing file RPCs (CP1).** This was a real bypass, not a
  hardening nicety: the guard existed but lived one layer too high. `agentDirGuard` sat in the
  toolproxy seam with exactly one caller, so `doc.write`, office outputs, and cloudfile `save_to`
  were refused while `UploadWorkspaceFile`/`MoveWorkspaceFile`/`CreateWorkspaceDir`/
  `DeleteWorkspaceFile` — which call the backend directly — were not. The storage layer did not
  catch it either (`workspacefs.Store.resolve` reserved only `config`; `reservedFirstSegment`'s
  `.agent` entry governs backend routing, and `Router.Write` forwarded a reserved path to
  `Local.Write` unchanged). Any authenticated user could therefore write a memory concept with
  forged `verified` frontmatter, defeating `skill:memory.write`, `ComposeConcept`'s provenance
  stamping, `ValidateID`, the `MaxConcepts` cap, and the `VerifyMemoryConcept`-is-sole-writer
  invariant in one request. Self-scoped only (`callerIdentity` binds tenant and user to the OIDC
  principal), and not reachable by a prompt-injected Pi child, which has no route to the Files API.
  Now closed at the boundary every user-facing writer crosses.
- **Memory read/recall scanning (CP3).** `memory.read`'s `frontmatter` and `index` modes were not
  scanned at all; auto-recall — the one path where stored content reaches a model with neither a
  user action nor a model decision behind it — consumed concepts unfiltered. Both are addressed
  above, with recall omitting rather than annotating because there is nobody in that loop to act on
  an annotation.
- **office-worker ingress bounds (CP4).** The broker's 10 MiB cap bounded the *compressed* input
  only, and all concurrent office jobs share one process and one container.

**The spotlighting row above stays ⚠️ WARN.** CP3 narrows *what untrusted content reaches the model
via auto-recall*; it does not make spotlighting structural. Spotlighting remains an instruction in
`buildSystemPrompt` that a sufficiently capable prompt injection can talk the model out of, with no
non-LLM enforcement layer analogous to the SSRF guard or Biscuit scoping. The injection-pattern
regex set is likewise evadable by rephrasing — CP3 removes the cheapest automated path into
auto-recall, and closes no class.

---

## Domain 6 — Supply Chain Integrity

**Verdict: ✅ PASS (Foundation, 2026-06-27) — Scorecard added; MCP-auth risk-accepted**

| Control | Tier | Status | Evidence |
|---------|------|--------|----------|
| `go.sum` + `package-lock.json` present | F | ✅ PASS | Dependency graph reproducible; no floating version resolution |
| MCP server authentication required | F | ⚠️ RISK-ACCEPTED | Not enforced at registration, but MCP connection is admin-only — admins vet each local server (trust boundary = admin, already higher-privilege). `mcp-echo` gated to `dev` profile. Reopens if MCP attach becomes non-admin. `ROADMAP.md` → ZT6. **Extends to `aikonos-mcp-grafana`** (`compose.yaml:758-827`) on the same terms — see the 2026-08-07 note below for why, and for the one property that differs |
| CI / automated build and test gate | F | ✅ PASS | `.github/workflows/ci.yml` — 7 blocking jobs (`broker-test` `:14`, `policy-test` `:43`, `gateway-test` `:62`, `webui-test` `:92`, `docs-mcp-test` `:122`, `migrations` `:144`, `compose-config` `:210`) plus an informational, checksum-pinned `scorecard` job (`:265`, `continue-on-error: true` at `:274`); all official GitHub-maintained actions |
| Docker image versioned tags | F | ✅ PASS | Azure/onprem prod overlays now pin every pulled image by digest: `scripts/pin-image-digests.sh` resolves `RepoDigests` for every `image:` across the base + overlays and writes the checked-in `deploy/compose/compose.digests.yaml`; documented prod invocation appends `-f deploy/compose/compose.digests.yaml` (`deploy/compose/README.md` "Supply chain", `deploy/onprem/README.md`); `compose-config` CI parses base+azure+digests and base+onprem+digests. Local dev deliberately keeps mutable tags (fast iteration) — that scope is unchanged and not a gap |
| OpenSSF Scorecard in CI | F | ✅ PASS | `scorecard` job in `.github/workflows/ci.yml` — checksum-pinned Scorecard CLI v5.5.0, informational (non-blocking), uploads results artifact |
| AI-BOM (OWASP) | E | ✅ PASS | `scripts/generate-ai-bom.sh` generates the committed `docs/AI-BOM.md` — LLM providers (name/dialect/model id/vision capability, never key material; committed form documents dev/test rows are deliberately excluded and regenerates against a live deployment with `--live-db`), skill bundles + `sbom_ref`, Pi harness + broker versions |
| Signed compose / service images | E | ⚠️ RISK-ACCEPTED | `scripts/git-deploy-hook.sh` verifies the pushed ref's tip commit/tag SSH/GPG signature against an allowed-signers file before checkout+build on the on-prem git-push deploy path — this signs the **source tree** the images are built from, not the built images themselves. Registry-based cosign image signing is deferred — it would reopen if images are ever pushed to a registry. The source-signing mechanism itself is adoption-gated: defaults to warn-and-proceed if `AIKONOS_DEPLOY_ALLOWED_SIGNERS` is absent; `AIKONOS_DEPLOY_REQUIRE_SIGNED=true` fails closed once the file exists (`deploy/onprem/README.md` "Signing setup") |
| Immutable MCP server hosting | A | ❌ FAIL | No immutable registry; local builds via `--build` |

**Remediation:**
- MCP server auth-at-registration: **risk-accepted** — MCP connection is admin-only (admins vet local servers). Reopen and enforce (require `auth_required` / bearer / mTLS, refuse unauthenticated in production profiles) if MCP attach becomes non-admin. See `ROADMAP.md` → ZT6.

**2026-07-03 update.** The CI row previously undercounted the gate: it now runs 8 blocking jobs — `broker-test`, `policy-test`, `planner-test`, `gateway-test`, `webui-test`, `docs-mcp-test`, `migrations`, `compose-config` — plus the informational `scorecard` job (unchanged, checksum-pinned CLI, non-blocking). Evidence corrected to the full job list.

**2026-08-07 correction.** The count above is superseded: the standalone Python planner and its `planner-test` job were removed at `06abd9e` (no `planner/` directory exists), leaving **7** blocking jobs plus the informational `scorecard`. The row's evidence is corrected in place; this note records that the change was a deletion, not a regression in coverage.

**2026-08-07 update — `aikonos-mcp-grafana` (`f12b9f1`).** A second MCP server now exists and the
MCP-auth risk acceptance above is extended to it, explicitly rather than by silence.

What it is: the upstream `grafana/mcp-grafana:1.0.0` image on the `mesh` network with no published
port (`compose.yaml:758-827`), behind an opt-in `mcp-grafana` compose profile (`:761`) that is off
in `core`/`full`. The broker dials it at `http://aikonos-mcp-grafana:8000/mcp`, which requires
`AIKONOS_TOOLPROXY_MCP_ALLOW_PRIVATE=true` on the broker (`compose.yaml:477`, default `false`) because
`mesh` is RFC1918 — that is the SSRF guard being opted out of deliberately for one target, not
bypassed. It carries no inward caller authentication: any `mesh` member that can reach port 8000
gets its tool surface.

Why the acceptance extends: the trust boundary is identical to the existing one. Enabling the
profile, minting the Grafana service-account token, and flipping the private-IP opt-in are all
admin acts, and the server holds no Aikonos credential — only an outward Grafana service-account
token. The property that *differs* from `mcp-echo` and is worth stating: `mcp-echo` is gated to the
`dev` profile and unauthenticated-and-harmless, whereas this server is production-intended and its
authority ceiling is the Grafana service account's role. It deliberately runs **without**
`--disable-write` (the reasoning is in the compose comment at `:801-819`): the flag would swap in a
GET-only API implementation and make panel data unreachable, so the boundary that stops write tools
is the service account's Viewer role, enforced server-side by Grafana. Mint that account above
Viewer and this becomes a genuine write path — reopen this row if that happens, or if MCP attach
ever becomes non-admin.

**2026-07-04 update (CP3.1/CP3.2/CP3.3).** Three rows move: (1) Docker image versioned tags WARN→PASS — prod overlays now pin by digest via the new `pin-image-digests.sh`/`compose.digests.yaml` pair, resolving the digest-pin remediation bullet (removed above); (2) AI-BOM FAIL→PASS — `generate-ai-bom.sh` + committed `docs/AI-BOM.md`; (3) Signed compose / service images FAIL→RISK-ACCEPTED — `git-deploy-hook.sh`'s new source-tree signature verification is a deliberate substitute for registry image signing (cosign stays a documented non-goal), not the row's literal ask; the substitute is itself adoption-gated per the evidence above.

---

## Domain 7 — Credential & Secret Management

**Verdict: ✅ PASS (Foundation), enterprise gaps remain**

| Control | Tier | Status | Evidence |
|---------|------|--------|----------|
| Provider API key never in Pi child | F | ✅ PASS | `egress-proxy.ts:5-19` — key lives only in parent in-memory `Map`; child receives a per-child opaque token; unknown token → 403 |
| EgressProxy loopback-only bind | F | ✅ PASS | `127.0.0.1` only; not reachable from mesh network; per-child token prevents cross-child impersonation |
| Header smuggling guard | F | ✅ PASS | `cookie`, `x-api-key`, `x-forwarded-for`, `x-real-ip` stripped from child requests before upstream forward |
| HTTPS upstream only | F | ✅ PASS | `egress-proxy.ts` selects `node:https` for `https:` upstreams; no cleartext to production LLM provider |
| Vault KV-v2 for capability root key | F | ✅ PASS | `broker/cmd/broker/main.go` — read-or-create with `cas=0`; concurrent replicas converge on first writer; key survives broker restart while Vault is up |
| Vault AppRole least-privilege scoping | F | ✅ PASS | AppRole policy scoped to `broker/capability`, `workspaces/*`, `mcp/*` only |
| Connector OAuth tokens in Vault | F | ✅ PASS | JIT token refresh at `InvokeTool` time; tokens not persisted in Postgres |
| `.env` gitignored | F | ✅ PASS | Secrets never enter the git tree |
| Audit HMAC signing key (KMS) | E | ✅ PASS | Vault KV-v2 versioned key `broker/audit-signing-key` (read-or-create, `cas=0`); per-event `signing_key_version`; `VerifyChainVersioned` (`broker/internal/audit/verify.go`) rejects a version-downgrade; env fallback = alarmed degraded mode (see Domain 3 for full evidence) |
| Vault durable storage (was: inmem in dev) | E | ✅ PASS | Durable `file` storage in **every** compose variant. Base `compose.yaml:216-256` mounts `deploy/compose/vault/vault.hcl` (`storage "file" { path = "/vault/file" }`) and the `vault-data` volume (`compose.yaml:111`, `:238`); azure/onprem overlays mount the same config + volume (`compose.azure.yaml:29-51`, `compose.onprem.yaml:81-103`). Local dev is auto-unsealed first-boot-only by the `vault-init` one-shot (`compose.yaml:264-283`), gated on `AIKONOS_VAULT_AUTO_UNSEAL` so azure/onprem keep their manual-unseal posture; first-boot `vault operator init`/`unseal` procedure in `docs/OPS-RUNBOOK.md`; `vault-data` covered by `scripts/compose-backup.sh` |
| Per-agent-type distinct Vault credential | E | ⚠️ RISK-ACCEPTED | All agents share the same broker AppRole. Accepted: the broker is the sole Vault client — no agent-side component (Pi child, `agent-gateway` parent) holds a Vault credential or dials Vault directly, so a shared AppRole is not agent-to-agent exposure under the current architecture. Reopens if any agent-side component gains its own Vault path. See `ROADMAP.md` → ZT7 |
| Hardware-bound root key | A | ❌ FAIL | Not implemented; Vault Transit is the next step before HSM |

**2026-07-03 update (CP1.1/CP1.2/CP1.3).** Three rows updated: (1) audit signing key moves WARN→PASS — see Domain 3's CP1.2 note for full evidence; (2) Vault inmem-in-dev evidence corrected — azure/onprem overlays now run a durable `file`-backend Vault with a documented init/unseal runbook, narrowing the remaining WARN to local dev (an intentional, unchanged posture); (3) per-agent-distinct-Vault-credential moves WARN→RISK-ACCEPTED, mirroring how ZT6 accepts the MCP-auth gap — reopen trigger recorded in `ROADMAP.md`.

**2026-08-07 update.** The Vault row moves WARN→PASS, and is retitled because its name no longer
described anything. Local dev stopped running `-dev` in-memory mode when durable Vault shipped: the base `compose.yaml` now mounts the same `vault.hcl` `file`
backend and the same `vault-data` volume the azure/onprem overlays do, with a flag-gated
`vault-init` one-shot auto-unsealing local dev first-boot-only. The row's premise — "local dev is
deliberately in-memory" — is simply gone, so the WARN had nothing left to warn about. This is a
correction of stale documentation, not a control landed by this audit pass; the executive summary's
census is amended accordingly. The remaining Vault gap is Advanced-tier only (hardware-bound root
key, Domain 1 / this domain's last row).

---

## Domain 8 — Memory & Context Safety

**Verdict: ✅ PASS (Foundation)**

Pi's memory safety model is process-based, not application-level — the child process structurally cannot retain state across users or sessions without a restart.

| Control | Tier | Status | Evidence |
|---------|------|--------|----------|
| Child process holds no long-lived secrets | F | ✅ PASS | Provider key, bearer tokens, owner grants all held in parent process only; child receives only its per-child egress token at spawn |
| Cross-child impersonation blocked | F | ✅ PASS | Per-child token in `EgressProxy`; token mismatch → 403; child cannot forge another child's identity |
| Workspace file isolation | F | ✅ PASS | `workspacefs` enforces `Root/<tenant>/<user>/` prefix at `Open`/`Write` — cross-tenant path traversal rejected at the fs layer |
| Session context scoped to child | F | ✅ PASS | Pi loop context window lives in the child process; process replacement at pool eviction clears all in-memory context |
| Session files in workspace volume | F | ✅ PASS | `workspace-data` volume stores `.agent/Sessions/` per user; tenant isolation via path prefix; no shared session store |
| Owner identity from spawn record | F | ✅ PASS | `ownerGrant` bound at spawn from broker-minted HMAC grant; IPC message body cannot override identity |
| Session file integrity verification | E | ✅ PASS | `broker/internal/workspacefs/store.go` — writes to `.agent/Sessions/` also write a `<name>.sig` = HMAC-SHA256(content) keyed from Vault KV-v2 `broker/workspace-session-key` (read-or-create, CP1.2 pattern; `AIKONOS_WORKSPACE_SESSION_SIGNING_KEY` degraded fallback); `ReadWorkspaceFile` (the sole execution-seeding read path) verifies when a `.sig` exists — mismatch → gRPC `FailedPrecondition` + audit event; absent `.sig` = legacy file, allow+log; `.sig` excluded from listings |
| Versioned session rollback | A | ❌ FAIL | No mechanism to restore a session to a known-good state after suspected context poisoning |

**2026-07-04 update (CP4.2, closed out by CP4.4).** Session file integrity verification moves
FAIL→PASS: every `.agent/Sessions/` write now gets an HMAC-SHA256 sidecar keyed from Vault, and the
one execution-seeding read path refuses a tampered record loudly instead of silently trusting it.
This row was implemented under Phase 4's CP4.2 but its audit-doc update was folded into this CP4.4
pass rather than shipped as its own checkpoint.

**2026-08-07 update (CP1, `8f2b408`).** Two things, one a fix and one a wording correction.

The fix: until this commit the `.agent/` tree — this domain's server-maintained context store, which
holds both `Sessions/` and the memory bundles under `Memory/` — was writable through the user-facing
file RPCs with no guard at all. The guard existed only in the toolproxy seam. A user could upload a
memory concept carrying forged `verified` provenance and have auto-recall present it as
human-reviewed in their own later turns. It was a self-scoped integrity failure, not a cross-user or
cross-tenant one, and it was genuinely broken for the whole period between the memory feature
shipping (`a276cd2`) and this fix. Closed at the RPC boundary — see Domain 5's new `.agent/` write
guard row for the control and its evidence.

The wording correction: the session-file-integrity row above reads as though the `.sig` sidecar
proves a session record was not authored by its user. It does not, and never did. `Store.Write`
signs anything landing under `SessionsDirPrefix`, including a record uploaded through
`UploadWorkspaceFile` — which the webui legitimately does, and which the CP1 guard deliberately
carves out. The control proves "written through the broker", i.e. it detects out-of-band filesystem
tampering, which is its actual design intent. The row keeps its ✅ PASS on that reading.

---

## Domain 9 — Integrity & Recovery

**Verdict: ✅ PASS (Foundation)**

| Control | Tier | Status | Evidence |
|---------|------|--------|----------|
| Container hardening on all services | F | ✅ PASS | `compose.yaml:38-45` — `<<: *hardening`: `no-new-privileges:true`, `cap_drop: ALL`, `pids_limit: 512`, `mem_limit: 1g`, `cpus: 2` on every service |
| Non-root + read-only rootfs | F | ✅ PASS | App services: `user: node`/`nobody`, `read_only: true`; datastore exceptions documented with rationale in compose |
| Three-network segmentation | F | ✅ PASS | `backend` internal (postgres/vault/minio/opa/openfga), `mesh` bridge (app services), `obs` internal (telemetry) — datastores unreachable from mesh |
| Durable named volumes | F | ✅ PASS | Five volumes survive restarts — `postgres-data`, `minio-data`, `workspace-data`, `obs-archive`, `vault-data` (`compose.yaml:106-111`); ephemeral only: NATS, Keycloak H2 |
| Compose + policy files in version control | F | ✅ PASS | `compose.yaml`, `policies/opa/*.rego`, `deploy/compose/` all tracked in git |
| No CI gate on config changes | F | ✅ PASS | `.github/workflows/ci.yml` runs all seven blocking jobs on every push and PR; `compose-config` (`:210`) specifically config-parses every compose variant and runs `scripts/check-env-drift.sh` |
| Signed compose / configs | E | ⚠️ WARN | On-prem git-push deploys now verify the pushed ref's tip commit/tag signature before checkout (`scripts/git-deploy-hook.sh`) — the signed tree includes `compose.yaml`, `deploy/compose/compose.onprem.yaml`, `.env.*.example`, and `policies/opa/*.rego`, so a verified deploy structurally covers this row's configs. Narrowed, not closed: (1) adoption-gated — defaults to warn-and-proceed until `AIKONOS_DEPLOY_ALLOWED_SIGNERS` is configured, `AIKONOS_DEPLOY_REQUIRE_SIGNED=true` fails closed once the file exists (`deploy/onprem/README.md` "Signing setup"); (2) scoped to the on-prem git-push path only — azure/local dev deploys have no equivalent signature verification |
| Immutable deploy images | A | ⚠️ WARN | Images built locally via `docker compose --build`; no signed immutable registry |

**2026-08-07 corrections.** Three evidence citations in this domain were stale; all are
documentation drift, no control changed. (1) The hardening anchor is `compose.yaml:38-45`, not
`:42-44`. (2) Durable named volumes are now **five**, not four — `vault-data` joined them when
Vault moved to `file` storage in every variant, and Vault is no longer in the ephemeral list (see
Domain 7). (3) The config-change CI row named a `planner` test suite that no longer exists; it now
cites the seven blocking jobs and `compose-config` specifically, which is the job that actually
gates config changes.

**2026-07-04 update (CP3.2).** Signed compose / configs row moves FAIL→WARN: `scripts/git-deploy-hook.sh` now verifies a signed commit/tag before building on the on-prem deploy path, which structurally signs the compose files and OPA policies living in that tree (see Domain 6's "Signed compose / service images" row for the same mechanism applied to the separate image-signing gap, which it does not close). Remains WARN rather than PASS because the verification is adoption-gated (warn-and-proceed default) and on-prem-only.

---

## Domain 10 — AI Governance & Policy

**Verdict: ✅ PASS (Foundation)**

All Foundation-tier governance controls now in place. One Advanced gap remains.

| Control | Tier | Status | Evidence |
|---------|------|--------|----------|
| OPA policy-as-code | F | ✅ PASS | `policies/opa/` — `plan_validation.rego`, `tool_invocation.rego`, `envelope_send.rego`; tested via `task policy:test` |
| Architecture + threat documentation | F | ✅ PASS | `README.md`, `docs/02-policy-model.md`, `docs/04-threat-model.md` |
| Acceptable use policy | F | ✅ PASS | `docs/acceptable-use-policy.md` — permitted principals, data classification (PUBLIC/INTERNAL/CONFIDENTIAL/RESTRICTED), mandatory HITL actions, prohibited uses, enforcement mapping to OPA + FGA |
| Incident response runbook | F | ✅ PASS | `docs/incident-response-runbook.md` — S1–S4 tiers; 5 scenarios (prompt injection, capability key compromise, audit chain break, rate-limit bypass, MCP compromise) with detect/isolate/recover steps |
| Agent / MCP deployment approval process | F | ✅ PASS | `docs/agent-deployment-checklist.md` — new agent + new MCP server checklists (skill bundle, effect class audit, FGA grants, rate-limit policy, dedicated API key, smoke test) |
| Policy compliance metrics | A | ❌ FAIL | No coverage or dwell-time measurement against the OPA/FGA rule set |

---

## Prioritized Remediation Plan

### Phase 1 — Foundation blockers

| Priority | Domain | Action | Status |
|----------|--------|--------|--------|
| P1 | 10 | Write AUP + incident response runbook + deployment checklist | ✅ done |
| P1 | 6 | Re-introduce CI (`broker:test`, `policy:test`, `planner:test`, `gateway-test`) | ✅ done |
| P1 | 6 | Require MCP server auth at broker registration (block unauthenticated in production) | ⬜ deferred — accepted risk (MCP connection is admin-only; admins vet local servers). Reopens if MCP attach becomes non-admin. See `ROADMAP.md` → ZT6 |
| P1 | 6 | Add OpenSSF Scorecard to CI | ✅ done — checksum-pinned CLI job in `.github/workflows/ci.yml` |

### Phase 2 — Observability & monitoring

| Priority | Domain | Action | Status |
|----------|--------|--------|--------|
| P2 | 3 | Default `AIKONOS_OTEL_ENDPOINT` to `otel-collector:4317` in `obs` profile | ⬜ deferred (causes dial errors on core-only stack) |
| P2 | 3 | Wire `SigningKey` from Vault transit | ⬜ deferred (requires Vault transit engine) |
| P2 | 4 | Add Grafana alert on rate-limit breach metric | ✅ done |
| P2 | 4 | Evaluate fail-closed on `CheckRateLimit` transport error | ✅ done — circuit breaker (`rate-limit-breaker.ts`, CP4.1): fail-open steady-state, opens + load-sheds after N consecutive transport failures |

### Phase 3 — Depth controls

| Priority | Domain | Action | Status |
|----------|--------|--------|--------|
| P3 | 5 | Add spotlighting delimiters to Pi system prompt template | ✅ done |
| P3 | 5 | Add output credential-pattern scan before SSE flush | ✅ done |
| P3 | 6 | Pin Docker image tags to digests in `compose.yaml` | ✅ done — `scripts/pin-image-digests.sh` + `deploy/compose/compose.digests.yaml`, prod overlays |
| P3 | 1 | Gate `mcp-echo` to dev/test compose profiles only | ✅ done |

---

## Design Test: Impossible vs. Tedious

| Control | Class | Reasoning |
|---------|-------|-----------|
| Provider API key never in Pi child (`EgressProxy`) | **Impossible** | No IPC path carries the key to the child; even a fully compromised Pi loop cannot read it |
| SSRF guard at TCP `Dialer.Control` | **Impossible** | Checked at connect, not at DNS preflight; redirect and DNS rebind attacks both fail at the TCP layer |
| Biscuit `Attenuate` keyless narrowing | **Impossible** | A child cannot expand its minted scope — the math forbids it without the root key |
| `read_only: true` container rootfs | **Impossible** | Rootfs writes fail at the kernel level; no escape via ephemeral file creation |
| FGA deny-by-default (personal tasks) | **Impossible** | No relation = no access; FGA error = no access; there is no "default allow" path |
| OPA plan-level exfil guard | **Impossible** | `plan_validation.rego` raises a violation at `SubmitPlan` before any step executes; attacker cannot defer the check to runtime |
| Rate limiting (RPM/TPM) | **Tedious only** | A patient attacker is not deterred; rate limits bound damage, not determined attackers |
| Fail-open `CheckRateLimit` (bounded by breaker) | **Tedious bypass, bounded** | A network attacker forcing gRPC failures now trips the breaker (CP4.1) after N consecutive failures → fast-fail load-shed, not unlimited egress; steady-state stays fail-open by design so a broker restart never blacks out egress |
| Workspace path prefix (`Root/<t>/<u>/`) | **Impossible** | Enforced at `workspacefs.Open`; no string manipulation bypasses the prefix check at fs layer |

---

*Generated by `/zero-trust-audit` — framework: Zero Trust for AI Agents (Anthropic May 2026).*
