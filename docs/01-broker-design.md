# Aikonos Broker — Design Document

**Component**: Aikonos Broker (core orchestration service)
**Version**: v0.1
**Status**: Design

## 1. Purpose

The Broker is the single policy enforcement and orchestration point for all agentic activity in Aikonos. No agent spawns, no tool call, no inter-agent message, no skill execution happens without passing through it. It is deliberately a chokepoint — the security model depends on it.

## 2. Responsibilities

1. **Session & task intake** from the frontend API gateway
2. **Authorization** — resolve user → groups → permitted skills/tools/MCPs via the policy engine
3. **Plan validation** — receive agent-generated execution plans, validate each step against policy
4. **Capability minting** — issue attenuated biscuit tokens for each planned action
5. **Sandbox lifecycle** — request agent sandboxes from the orchestration layer (K8s), inject capabilities, tear down
6. **Tool call brokering** — proxy every tool invocation through itself for real-time policy checks
7. **Inter-agent routing** — validate, sign, and route Task Envelopes via NATS
8. **Audit emission** — every decision produces an audit event
9. **Quota & rate enforcement** — per-user, per-group, per-tenant cost and request budgets
10. **Kill-switch execution** — freeze/terminate agents on command

## 3. Non-responsibilities

The Broker does **not**:
- Run LLM inference (delegated to an inference gateway)
- Execute skills (delegated to sandboxes)
- Store conversation content (delegated to workspace storage)
- Manage identities (delegated to IdP / workload CA — SPIRE in the original k8s design; a static dev CA under Compose)
- Make policy decisions in isolation — it asks the policy engine

Keeping the Broker small is a security property.

## 4. Internal Architecture

```
                    ┌────────────────────────────┐
                    │     API Gateway (mTLS)     │
                    └─────────────┬──────────────┘
                                  │ gRPC
                    ┌─────────────▼──────────────┐
                    │   Session Service          │  ← validates OIDC token, loads user context
                    └─────────────┬──────────────┘
                                  │
                    ┌─────────────▼──────────────┐
                    │   Task Controller          │  ← state machine per task
                    └──┬──────┬──────┬───────────┘
                       │      │      │
              ┌────────▼──┐ ┌─▼────┐ ┌▼──────────┐
              │ Planner   │ │ PEP  │ │ Cap Mint  │
              │ Validator │ │      │ │ (biscuit) │
              └────────┬──┘ └──┬───┘ └────┬──────┘
                       │       │          │
                       └───────┼──────────┘
                               │
          ┌────────────┬───────┴───────┬─────────────┐
          │            │               │             │
    ┌─────▼─────┐ ┌────▼────┐ ┌────────▼────┐ ┌──────▼──────┐
    │ Sandbox   │ │ Tool    │ │ Envelope    │ │ Audit       │
    │ Manager   │ │ Proxy   │ │ Router      │ │ Emitter     │
    │ (K8s)     │ │         │ │ (NATS)      │ │             │
    └───────────┘ └─────────┘ └─────────────┘ └─────────────┘
```

Each box is a logical module. In implementation they can be one process (Go or Python/asyncio) with internal interfaces, or separate microservices communicating over localhost mTLS. Start as a modular monolith; extract later only if a component has different scaling or security requirements.

## 5. Task State Machine

Every task in Aikonos runs through this state machine. The Broker owns it.

```
   CREATED
      │  (user submits task)
      ▼
   PLANNING ────────────┐
      │                 │ plan rejected
      │  (LLM proposes  ▼
      │   tool calls)   REJECTED
      ▼
   VALIDATING
      │
      ├─ policy deny ──► DENIED
      ├─ needs_approval ──► AWAITING_APPROVAL
      │                          │  (human ack)
      │                          ▼
      │                    APPROVED
      │                          │
      ▼                          │
   EXECUTING ◄───────────────────┘
      │
      ├─ step success ──► (loop: back to PLANNING if multi-step)
      ├─ tool error ──► ERROR ──► (retry or FAILED)
      ├─ policy deny mid-exec ──► TERMINATED
      ├─ deadline exceeded ──► TIMEOUT
      ▼
   COMPLETED
```

Transitions are logged. State lives in Postgres (for durability across Broker restarts) with an in-memory cache keyed by task ID.

## 6. Plan Validation — The Core Security Loop

The agent (LLM) never directly invokes tools. Flow:

1. Agent emits a **structured plan**: list of `{tool, args, justification, expected_effect_class}`
2. Broker parses and schema-validates the plan
3. For each step:
   - Resolve the tool → its registered manifest (effect class, required scopes, schemas)
   - Query PEP (policy engine) with: `(user, group, task_context, tool, args, effect_class)`
   - If `allow`: mint capability token with exact scope, single-use nonce, TTL
   - If `require_approval`: push to AWAITING_APPROVAL, notify frontend
   - If `deny`: abort task, audit, return reason to agent (which may replan)
4. Only approved steps execute
5. After each step, broker re-evaluates: the tool output may change risk posture

**Critical**: the agent sees only the *outcome* of validation, never the raw policy rules. Otherwise LLMs learn to game policies.

## 7. Capability Tokens (Biscuit)

Biscuit is chosen over macaroons for better tooling and Rust/Python/Go support.

**Why biscuit**:
- Attenuation only (holder can narrow, never expand)
- Offline verification (sandboxes can verify without calling back, though we call back anyway)
- Datalog-based checks — expressive without being Turing-complete
- Third-party blocks for delegation chains

**Token structure**:

```
Authority block (Broker-signed):
  user("alice@corp");
  agent("spiffe://aikonos.com/alice/agent-42");
  task("task-uuid");
  right("siem.query", "read");
  resource_scope("incidents.q3");
  check if time($t), $t < 2026-04-20T17:00:00Z;
  check if operation($op), ["read"].contains($op);
  nonce("uuid-single-use");
```

**Delegation block** (added by agent when delegating to sub-agent):

```
  attenuate right("siem.query", "read", filter="severity>=high");
  check if delegated_depth($d), $d < 3;
```

**Verification**: every tool call presents its token to the Tool Proxy, which verifies signature + runs checks against current context (time, resource, nonce consumed). Nonces tracked in Redis with TTL.

## 8. Tool Proxy

All tool and MCP calls go through the Tool Proxy, not directly from sandbox to target.

Why:
- Validates capability token on every call (defense-in-depth)
- Schema-validates arguments (prevents arg smuggling)
- Strips/redacts sensitive fields per policy (DLP hook point)
- Rate limits per `(user, tool, resource)`
- Logs request/response for audit
- Rewrites external egress through the identity-aware proxy

Sandbox networking is configured so the only reachable endpoint is the Tool Proxy. No direct egress, no direct MCP, no direct database access.

## 9. Inter-Agent Envelope Routing

When an agent sends a Task Envelope:

1. Sandbox publishes to `outbound.<tenant>.<user>` NATS subject (only subject it can write to)
2. Envelope Router (inside Broker) consumes
3. Validates signature, checks sender policy for `send_task` permission to recipient
4. Attenuates capability token further (sender's scope ∩ policy-allowed delegation)
5. Re-signs envelope with Broker's signature
6. Writes to audit log
7. Publishes to `inbox.<tenant>.<recipient>` NATS subject
8. Notifies recipient frontend

Recipient acceptance flow is handled by Task Controller, not directly by sandboxes.

## 10. Data Model (Key Tables)

```sql
-- Simplified. Actual schema has more fields, RLS, encryption.

tasks (
  task_id uuid PRIMARY KEY,
  parent_task_id uuid NULL,
  tenant_id uuid NOT NULL,
  owner_user_id text NOT NULL,
  state task_state NOT NULL,
  created_at timestamptz,
  deadline timestamptz,
  cost_budget int,
  cost_consumed int,
  trace_id text
);

plan_steps (
  step_id uuid PRIMARY KEY,
  task_id uuid REFERENCES tasks,
  seq int,
  tool_id text,
  args_hash text,     -- full args in audit log, not here
  effect_class text,
  policy_decision text,
  capability_token_id uuid,
  state step_state,
  started_at timestamptz,
  ended_at timestamptz
);

capability_tokens (
  token_id uuid PRIMARY KEY,
  task_id uuid,
  issued_to text,     -- SPIFFE ID
  scope jsonb,
  ttl_seconds int,
  nonce text UNIQUE,
  revoked bool,
  issued_at timestamptz
);

envelopes (
  envelope_id uuid PRIMARY KEY,
  parent_task_id uuid,
  from_user text,
  to_target jsonb,
  state envelope_state,
  delivered_at timestamptz,
  accepted_at timestamptz
);
```

Audit events go to append-only object storage, not this DB.

## 11. APIs

**North-bound (frontend → Broker)** — gRPC with mTLS + OIDC:

- `CreateTask(TaskSpec) → TaskHandle`
- `GetTaskState(task_id) → TaskState`
- `StreamTaskEvents(task_id) → stream TaskEvent`
- `ApproveTask(task_id, decision)`
- `CancelTask(task_id, reason)`
- `SendEnvelope(EnvelopeSpec) → EnvelopeHandle`

**South-bound (sandbox → Broker)** — gRPC with SPIFFE SVID:

- `SubmitPlan(task_id, plan) → PlanValidation`
- `InvokeTool(task_id, step_id, tool_call, capability_token) → ToolResult` (via Tool Proxy)
- `RequestSubagent(task_id, subagent_spec) → SubagentHandle`
- `EmitStatus(task_id, status_update)`

**East-west (Broker ↔ services)** — internal mTLS:

- Policy engine: `Decide(PolicyQuery) → Decision`
- NATS: pub/sub on well-defined subjects
- Audit emitter: fire-and-forget gRPC (async, but backed by local spool)

## 12. Concurrency & Scaling

- Broker is stateless wrt requests — state lives in Postgres + Redis
- Horizontal scaling: N instances behind service mesh, partitioned by tenant
- Long-running task control uses leader election per task (Redis lock) to avoid split-brain
- Target: 10k concurrent tasks, 1k plans/sec validation, p99 < 100ms for policy decisions (policy engine cached)

## 13. Failure Modes & Resilience

| Failure | Impact | Mitigation |
|---|---|---|
| Policy engine down | No new decisions | Cached decisions for read-only policies; fail-closed for writes |
| Postgres down | No new tasks | Read-only mode; in-flight tasks continue from Redis cache |
| NATS down | No inter-agent comms, no new envelopes | Local queue spool with backpressure; alert |
| Vault down | No new secrets/tokens | Cached short-TTL tokens continue; no new sandboxes |
| Broker crash mid-task | Task state persisted | Task resumes from last checkpoint; capability tokens revoked if mid-step |

**Fail-closed principle**: when policy cannot be evaluated, the default is deny. Availability loss is preferable to unauthorized action.

## 14. Observability

Every request carries an OpenTelemetry trace context. Spans:

- `broker.task.create`
- `broker.plan.validate` (child spans per step)
- `broker.policy.decide`
- `broker.capability.mint`
- `broker.tool.invoke`
- `broker.envelope.route`

Metrics (Prometheus):
- `broker_tasks_total{state, tenant}`
- `broker_policy_decisions_total{decision, tool}`
- `broker_plan_validation_duration_seconds`
- `broker_capability_tokens_minted_total`
- `broker_envelopes_total{direction, outcome}`
- `broker_kill_switch_activations_total`

Logs: structured JSON, correlation via trace ID.

## 15. Threat-Driven Design Choices

| Threat | Design response |
|---|---|
| Compromised sandbox escalates privileges | Capability tokens bind to SPIFFE ID; single-use nonces; Tool Proxy re-validates |
| LLM generates malicious plan | Plan validator + PEP; effect-class policies; approval gates |
| Agent exfiltrates via tool args | Tool Proxy schema-validates + DLP redacts |
| Inter-agent loop (Alice→Bob→Alice→…) | parent_task_id chain depth limit + cycle detection |
| Broker compromise = full compromise | Minimize Broker surface; all actions audited externally; split signing keys (HSM) |
| Stolen capability token replayed | Single-use nonce + short TTL + revocation list |
| Tenant A sees Tenant B data | Tenant partitioning at every layer (DB RLS, NATS accounts, SPIFFE trust domains) |

## 16. Open Design Questions

1. **Language**: Go for performance/deployment footprint, or Python for ecosystem fit and faster iteration? Leaning Go for the Broker core, Python for Planner Validator (LLM-adjacent logic).
2. **Plan format**: JSON Schema vs Protobuf vs custom DSL. Protobuf probably wins for strictness.
3. **Approval UX**: synchronous (task blocks) vs asynchronous (task parked, resumes on approval). Async required for practical use.
4. **Sub-agent spawning by agents**: allow by default with hard depth limit, or require per-user opt-in? Leaning opt-in with group-level policy.
5. **Multi-region**: does the Broker span regions, or is each region its own Broker? Inter-region envelopes cross federation boundary explicitly.

## 17. MVP Scope

For a first working system:

- Single Broker process, Go
- OpenFGA as PEP
- Postgres + Redis
- NATS JetStream single cluster
- Biscuit tokens
- Hardcoded set of 3 skills and 2 MCPs for validation
- Kata Containers as sandbox runtime
- Manual approval for every non-read-only action
- Basic frontend with task list, plan viewer, approve button

Everything else — anomaly detection, auto-approval policies, federation, multi-region — comes later.
