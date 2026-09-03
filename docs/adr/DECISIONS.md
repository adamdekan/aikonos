# Architecture Decision Records

Format: context → decision → consequences.
Every significant architectural call goes here before the code changes.

---

## ADR-001 — Talos Linux as host OS

**Date**: 2026-04
**Status**: Accepted

**Context**: Need an immutable, attestable OS for the cluster node(s). Options were
Fedora CoreOS (Butane/Ignition), Flatcar, Bottlerocket, and Talos Linux.

**Decision**: Talos Linux.

**Consequences**:
- No SSH, no shell on nodes — all access via `talosctl` API. Reduces attack surface dramatically.
- Machine config is pure YAML (talhelper), fully version-controlled, fully reproducible.
- TPM-backed disk encryption and measured boot out of the box.
- Talos handles k3s/k8s bootstrapping — we don't manage kubeadm or k3s install scripts.
- Operational tradeoff: steeper learning curve for operators unfamiliar with Talos.
  Mitigated by `talosconfig` in the repo and runbook in OPS-RUNBOOK.md.
- Pod networking with Cilium requires disabling Talos's default flannel
  (handled in `patches/cluster-wide.yaml`).

**Superseded (Docker-only pivot):** Aikonos now deploys via **Docker Compose only** —
the Talos/k8s/Cilium substrate is gone. Services run as Compose containers on a bridge
network; there is no host-OS attestation layer. See `compose.yaml` +
`deploy/compose/README.md`. The immutable-OS / measured-boot goals are deferred to a
future production substrate.

---

## ADR-002 — Go for Broker, Python for Planner Validator

**Date**: 2026-04
**Status**: Accepted

**Context**: Need to pick implementation languages for the two core services.

**Decision**: Go for Broker; Python for Planner Validator.

**Consequences**:
- Broker is the security-critical chokepoint — Go's static typing, explicit error
  handling, and single-binary deployment reduce the attack surface vs. Python.
- Planner Validator lives in LLM-adjacent territory — pydantic schema validation,
  rapid prompt engineering iteration, and the grpcio ecosystem are better in Python.
- Two build systems in the repo. Mitigated by Taskfile unifying the build targets.
- gRPC generated code in both languages from shared proto definitions.

**Superseded (planner removed):** the standalone Python Planner Validator is gone —
commit `06abd9e` deleted the whole `planner/` directory. `SubmitPlan`'s OPA +
OpenFGA gates are now the sole plan-time policy chokepoint. See ADR-011.

---

## ADR-003 — Biscuit tokens over Macaroons

**Date**: 2026-04
**Status**: Accepted

**Context**: Need capability tokens that support attenuation (narrow, never expand),
are short-lived, single-use, and verifiable without calling back to the issuer.

**Decision**: Biscuit tokens (biscuit-auth/biscuit-go, biscuit-auth/biscuit-python).

**Consequences**:
- Datalog-based checks are expressive but have a learning curve.
- Attenuation-only guarantee is formally verified by the biscuit spec — not hand-rolled.
- Third-party blocks support delegation chains natively.
- Go, Python, and Rust libraries available — covers all our services.
- Alternative considered: Macaroons — rejected because tooling is less maintained
  and third-party block semantics are more complex to reason about.

---

## ADR-004 — NATS JetStream over Kafka for inter-agent bus

**Date**: 2026-04
**Status**: Accepted

**Context**: Need durable message bus for Task Envelopes. Options: NATS JetStream,
Kafka/Redpanda, RabbitMQ.

**Decision**: NATS JetStream.

**Consequences**:
- Lighter operational footprint than Kafka — single process, no ZooKeeper.
- Native subject-based ACLs map directly to RBAC (tenant accounts, user subjects).
- Request/reply built in — envelope status channels work without extra plumbing.
- mTLS + JWT account isolation covers multi-tenancy without custom middleware.
- If we ever need full Kafka semantics (log compaction, consumer groups at Kafka scale),
  switching to Redpanda is a config change (Kafka-compatible API). The envelope schema
  and routing logic don't change.

---

## ADR-005 — OpenFGA for ReBAC, OPA for ABAC

**Date**: 2026-04
**Status**: Accepted

**Context**: Need a hybrid authorization model: relationships (delegation, ownership)
+ contextual attributes (time, risk score, data sensitivity).

**Decision**: OpenFGA handles relationship checks; OPA handles contextual policy.
Broker orchestrates both — FGA result is fed as an input attribute to OPA.

**Consequences**:
- Two policy engines to operate. Mitigated by both having good Helm charts and
  being deployed as in-cluster services.
- Division of concerns is clear: "can Alice invoke skill X?" → FGA.
  "is this invocation allowed given current context?" → OPA.
- Policy-as-code: FGA model in `policies/fga/model.fga`, OPA in `policies/opa/*.rego`.
  Both in Git, both tested in CI.
- Alternative considered: Cedar (AWS) — rejected because OpenFGA has better
  Go/Python SDK support and is CNCF-associated.

**Superseded (Docker-only pivot):** the decision stands; only the deployment changed.
OpenFGA and OPA now run as **Docker Compose services** (not Helm charts). OpenFGA uses
the Postgres datastore (`postgres-data` volume) via the `openfga-migrate` one-shot, and
is seeded by `scripts/compose-seed-openfga.sh` (`task compose:seed`). With the store id
unset the broker falls back to a dev allow-all stub. See `deploy/compose/README.md`.

---

## ADR-006 — Single-use nonces for capability tokens

**Date**: 2026-04
**Status**: Accepted

**Context**: Capability tokens must be replay-proof. If a sandbox is compromised and
its token is stolen, the attacker should not be able to reuse it.

**Decision**: Every token carries a UUID nonce. The Tool Proxy marks the nonce as used
in Redis on first use. Subsequent use of the same nonce returns 401.

**Consequences**:
- Redis becomes a dependency of the Tool Proxy hot path. Redis failure = Tool Proxy
  cannot validate tokens = fail-closed (deny all). Acceptable per our fail-closed policy.
- Nonce TTL in Redis matches token TTL (5 minutes). Stale nonces are cleaned by janitor.
- Replay window is zero — even within the TTL, a token can only be used once.
- For streaming tools (multi-response): token is re-minted per step, not per stream chunk.

**Superseded (Docker-only pivot):** the Redis nonce store was never built — there is no
Redis service in `compose.yaml`. The Tool Proxy is **in-process** in the broker and
`InvokeTool` authorizes the Biscuit scope directly (`broker/internal/capability` →
`Authorize`); replay-hardening via single-use nonces is deferred. Capability tokens are
short-lived and minted per step, which bounds the replay window in practice.

---

## ADR-007 — gVisor for MVP sandbox, Kata deferred

**Date**: 2026-04
**Status**: Accepted

**Context**: Need strong sandbox isolation. Architecture doc specifies Kata Containers
for high-risk tasks (WRITE_EXTERNAL+) and gVisor for lower-risk tasks.

**Decision**: gVisor only for MVP on single-node Talos. Kata deferred to post-MVP.

**Consequences**:
- Kata on a single-node Talos setup requires nested virtualization or bare-metal KVM.
  On the dev machine this adds significant complexity for zero security benefit
  (the security comes from the whole stack, not just the sandbox runtime).
- The runtime class selection logic exists in `sandbox/manager.go:SelectRuntime()`.
  Adding Kata later is: (1) install Kata on the node, (2) flip the return value.
  No other code changes.
- For the MVP, ALL tasks use gVisor regardless of effect class. This is documented
  in the threat model as a known gap.

**Superseded (Docker-only pivot):** under Docker Compose there was no k8s API, so the
sandbox manager (`broker/internal/sandbox/manager.go`) was **disabled**
(`AIKONOS_SPIFFE_SANDBOX_PATH_PREFIX=""`) and neither gVisor nor Kata ran. Isolated
per-task agent-sandbox spawning was **not yet wired for Docker** — the governed substrate
(policy stack, capability tokens, gateway-driven `InvokeTool` loop, audit) ran without
it. This was the known limitation called out in `docs/00-aikonos-architecture.md` and
`docs/ONBOARDING.md`.

**Superseded (removed, not just disabled):** commit `06abd9e` deleted
`broker/internal/sandbox/` outright — the runtime-class selection logic this ADR
describes no longer exists in the tree. See ADR-011.

---

## ADR-008 — Append-only audit log in MinIO with object lock

**Date**: 2026-04
**Status**: Accepted

**Context**: Audit trail must be tamper-evident for NIS2/DORA/ISO27001 compliance.
Attackers who compromise the platform must not be able to erase their tracks.

**Decision**: MinIO with object lock (COMPLIANCE mode). Events are written once and
cannot be overwritten or deleted until the retention period expires.

**Consequences**:
- Object lock must be enabled at bucket creation — cannot be added to an existing bucket.
  The bucket is created with object locking by the audit emitter on first write
  (default `GOVERNANCE` retention = `audit.retention_days`).
- The audit emitter hash-chains events (each event includes the hash of the prior event).
  This detects gaps even if object lock is somehow bypassed.
- MinIO is not a SIEM. Events are also streamed to an external SIEM
  (Elastic/Splunk/Sentinel) in Phase 5. MinIO is the immutable archive; SIEM is
  for real-time alerting.
- Retention period: 7 years default (configurable per tenant per regulation).

**Superseded (Docker-only pivot):** the decision stands; only the deployment changed.
MinIO now runs as a **Docker Compose service** backed by the `minio-data` named volume
(durable across `compose down` without `-v`), not a Helm-deployed in-cluster instance.
The audit trail is opt-in via `AIKONOS_AUDIT_*` in `.env`. See `deploy/compose/README.md`.

---

## ADR-009 — No direct agent-to-agent networking

**Date**: 2026-04
**Status**: Accepted

**Context**: Inter-agent communication model. Direct sockets vs. brokered messaging.

**Decision**: All inter-agent communication via NATS JetStream through the Broker.
Sandboxes cannot initiate direct connections to other sandboxes.

**Consequences**:
- Policy is enforced at one chokepoint (Broker) — not at N×N sandbox connections.
- Audit is complete — every envelope is logged before delivery.
- Delegation attenuation is cryptographically enforced by the Broker before delivery.
- Tradeoff: higher latency for inter-agent calls vs. direct sockets. Acceptable
  because inter-agent calls are task-level (seconds), not request-level (milliseconds).
- Sandboxes that need intra-task coordination (e.g., planner + worker within one task)
  use in-process asyncio, not the enterprise bus. See architecture doc §7.

---

## ADR-010 — CancelTask valid during EXECUTING

**Date**: 2026-07
**Status**: Accepted

**Context**: `CancelTask` was documented as first-class but `isValidTransition`
never allowed `EXECUTING → CANCELLED`, so a user-initiated cancel on a running
task always failed with `FailedPrecondition`. The executor also had no way to
interrupt an in-flight step loop.

**Decision**: Add `TaskStateCancelled` to the `TaskStateExecuting` transition
row. `CancelTask` now succeeds on an EXECUTING task and calls a new executor
`Cancel(taskID)` registry entry (a `context.CancelFunc` registered around the
cancelable context `Launch` hands `Execute`, wrapping the existing 5-minute
execution cap) to stop the step loop before its next iteration.

**Race rule — cancelled, not failed**: `Execute`'s step loop checks `ctx.Err()`
at the top of each iteration. When the context was cancelled (not deadline-
exceeded), `Execute` must not force the task to `FAILED` — the task is already
(or about to be) `CANCELLED` via `CancelTask`'s own transition. `failTask`
treats a rejected transition on an already-terminal task as a quiet no-op
(Debug/Info log, not Warn/Error) rather than surfacing it as an execution
failure, since losing this race is expected, not a bug.

**Consequences**:
- Cancelling a running task now works; the sandbox and cancel registry entry
  are torn down as a result of the successful transition, not as a substitute
  for it.
- The step loop can still complete a step that was already mid-flight when
  cancel fired — cancellation only prevents the *next* step from starting,
  per the non-goal of not interrupting a tool call mid-flight.
- Every future edit to `isValidTransition` (`broker/internal/db/tasks.go`)
  must add a corresponding entry here, per the map's own comment.

---

## ADR-011 — Sandbox, executor, and standalone planner removed

**Date**: 2026-07
**Status**: Accepted

**Context**: ADR-007's Docker-only pivot left the gVisor sandbox manager disabled
but present in the tree; the broker executor and the standalone Python planner
(ADR-002) had similarly become dead weight the governed substrate (policy stack,
capability tokens, gateway-driven `InvokeTool` loop, audit) already ran without.
Carrying unused code, plus an ADR ledger that still described it as live or merely
disabled, cost more than it protected.

**Decision**: Physically remove `broker/internal/sandbox/`, `broker/internal/executor/`,
and the whole `planner/` directory (commit `06abd9e`, "chore: remove dead sandbox,
executor, planner").

**Consequences**:
- ADR-002 (Go for Broker, Python for Planner Validator) and ADR-007 (gVisor for MVP
  sandbox) are both superseded by this decision — see their own Superseded notes.
- Isolated per-task agent-sandbox spawning remains unwired for Docker, the same known
  limitation ADR-007 already called out; this ADR records that the code is gone, not
  merely disabled.
- No replan-time Python validation layer exists; `SubmitPlan`'s OPA + OpenFGA gates are
  the sole plan-time policy chokepoint.
- `Taskfile.yml` has zero `planner` targets — `task planner:test` and
  `task compose:planner` no longer exist; any doc still referencing them is stale.

---

## ADR-012 — InvokeTool writes EXECUTING; APPROVED may fail directly

**Date**: 2026-07
**Status**: Accepted

**Context**: No code in the broker ever transitioned a task to EXECUTING.
ADR-011 removed `broker/internal/executor/`, which had been the only writer, and
nothing took over — `TaskStateExecuting` survived solely in the two proto↔db
mapping switches in `service.go`. Since `APPROVED` led only to `{EXECUTING,
CANCELLED}` and nothing produced EXECUTING, every task dead-ended at APPROVED
and the gateway's terminal `EmitStatus` was always rejected. Found on the on-prem host:
343 tasks in APPROVED, zero COMPLETED, zero FAILED, zero EXECUTING, with
`EmitStatus: transition rejected` in the broker log on every tool call. ADR-010's
`EXECUTING → CANCELLED` edge was consequently unreachable too.

**Decision**: Two parts.

1. `SandboxService.InvokeTool` performs `APPROVED → EXECUTING` immediately before
   dispatching to the Tool Proxy, conditional on the task actually being APPROVED
   so multi-call tasks transition once. InvokeTool is now the broker's only
   execution path, which makes it the only correct producer.
2. Add `TaskStateFailed` to the `TaskStateApproved` row. A tool call refused
   *before* dispatch (capability gate, cost-budget pre-gate, rate limiter, org
   effect-class switch) never becomes EXECUTING, so its caller reports FAILED
   from APPROVED.

`APPROVED → COMPLETED` is deliberately **not** added: a task cannot complete work
it never started. If that transition is ever attempted, the bug is a missing
EXECUTING write, not a missing edge.

**Consequences**:
- Tasks reach terminal states again; the `tasks` table stops growing without
  bound, and task-transition/plan-outcome metrics record real outcomes.
- ADR-010's `EXECUTING → CANCELLED` edge becomes reachable in practice for the
  first time.
- A failed `APPROVED → EXECUTING` write logs a warning and the tool call still
  proceeds — bookkeeping must not fail an already-authorized call. The terminal
  EmitStatus is then rejected as before, which is the pre-existing behavior and
  strictly no worse.
- Per ADR-010's closing note and the map's own comment, this entry is the record
  for the `isValidTransition` edit.
