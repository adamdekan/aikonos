# Aikonos — Developer Onboarding Guide

> Generated from the project knowledge graph. Keep in sync with `README.md`.

---

## Project Overview

**Aikonos** is a security-first agentic orchestration platform. It provides the infrastructure for LLM-powered agents to run tasks in isolated sandboxes, subject to a layered policy engine that enforces authorization, resource limits, and human-approval gates before any consequential action is taken.

| Attribute | Value |
|---|---|
| Primary languages | Go (broker), Python (office-worker, aikonosctl), TypeScript (agent-gateway), Vue (webui) |
| Config/infra languages | YAML, Protobuf, Rego, SQL, Dockerfile, Compose |
| Key frameworks | gRPC, Docker Compose, Vault, OPA, OpenFGA, NATS, Postgres, MinIO, OpenTelemetry |
| Deployment | Docker Compose only (`compose.yaml`, profiles `core`/`full`/`obs`/`dev`/`docs-mcp`) |

The platform is built around three ideas:

1. **Zero-trust identity** — broker↔gateway traffic is mTLS over a static dev-CA (`scripts/compose-dev-ca.sh`); each service loads its leaf cert from files. No long-lived shared secrets.
2. **Effect-class gating** — every action is tagged with an effect class (`read_only` → `infrastructure`); riskier classes require human approval.
3. **Immutable audit** — all task events are hash-chained and written to MinIO with object lock; nothing can be deleted silently.

---

## Architecture Layers

### 1. Architecture Documentation
Design docs, threat model, and architecture decision records. Start here before touching code.

| File | What it covers |
|---|---|
| `docs/00-aikonos-architecture.md` | Full system design: isolation layers, policy stack, workload identity, audit chain; Docker-Compose as-built, with Kubernetes described but not yet deployed — **the primary reference** |
| `docs/01-broker-design.md` | Detailed broker responsibilities: task intake, authorization, plan validation, capability minting, sandbox lifecycle |
| `docs/04-threat-model.md` | Security threats: compromised sandboxes, LLM prompt injection, data exfiltration, inter-agent loops, broker compromise |
| `docs/adr/DECISIONS.md` | Key decisions: Go for broker, Protobuf contracts, biscuit tokens (each carries a Docker-only-pivot supersession note where it was infra-specific) |

### 2. Protobuf Contracts
The API constitution. Read these before any service code — they define all data shapes and RPCs.

| File | What it covers |
|---|---|
| `proto/broker.proto` | Core gRPC APIs: `BrokerService` (task/approval/envelope/admin RPCs, north-bound) and `SandboxService` (south-bound only: plan submission, tool invocation, scheduler twins, LLM/rate-limit helpers) |
| `proto/plan.proto` | `Plan` message, `EffectClass` enum (8 levels from `read_only` to `infrastructure`), `PlanStep` |
| `proto/audit.proto` | `AuditEvent` with hash-chaining fields and signature support |
| `proto/skill.proto` | `SkillMetadata` + `SkillSpec`: effect class, runtime, scopes, SBOM reference |
| `proto/workspace.proto` | `WorkspaceManager` service (15 RPCs): encrypted per-user volumes, artifact storage, RAG |

### 3. Broker Service (Go)
Core orchestration engine. All agent requests pass through here.

| File | What it covers |
|---|---|
| `broker/cmd/broker/main.go` | Entry point: wires SPIFFE, OpenTelemetry, dual gRPC servers (north-bound + south-bound), pgxpool |
| `broker/internal/broker/service.go` | gRPC handlers: `CreateTask`, `GetTaskState`, `SubmitPlan`, `ApproveTask`, `CancelTask`, `EmitStatus`, `InvokeTool` |
| `broker/internal/db/tasks.go` | Task repository: all Postgres operations, `Transition()` state machine enforcement, RLS via `withTenant()` |
| `broker/internal/db/migrations/001_initial_schema.sql` | DB schema: `tasks`, `plan_steps`, `capability_tokens`, `envelopes`, `kill_switches`, `approval_requests` |
| `broker/internal/policy/engine.go` | Policy engine stub: wraps OpenFGA (ReBAC) + OPA (ABAC); effect-class decisions |
| `broker/internal/audit/emitter.go` | Audit emitter: writes `AuditEvent` to MinIO with hash chaining; Phase 0 logs to stdout |

### 4. Policy Engine
Hybrid ReBAC + ABAC authorization. OPA handles attribute checks; OpenFGA handles relationship checks.

| File | What it covers |
|---|---|
| `policies/opa/plan_validation.rego` | Validates plans: budget, justification, max steps, DLP patterns, infra-class gating |
| `policies/opa/tool_invocation.rego` | Tool-level authorization: `read_only`/`write_local` auto-allow; `write_external`+ requires approval |
| `policies/opa/envelope_send.rego` | Inter-agent message rules: attenuation checks, cost budgets, delegation depth limits |
| `policies/fga/model.fga` | OpenFGA model: 10 entity types (user, group, tenant, workspace, skill, task, agent, etc.) |
| `policies/fga/tuples/dev-seed.yaml` | Dev seed: tenant membership tuples for admin, alice, bob |

### 5. Skill System
First-party manifests show the pattern every agent-callable skill follows. Skills execute
in-process inside the broker — a manifest declares the contract (effect class, scopes,
tool schema); it doesn't ship an implementation to run.

| File | What it covers |
|---|---|
| `skills/web.fetch/skill.yaml` | Manifest: effect class `read_only`, Python 3.12 runtime, `web:read` scope |
| `skills/doc.write/skill.yaml` | Manifest: effect class `write_local`; writes to user workspace |
| `skills/email.draft/skill.yaml` | Manifest: effect class `write_external`; always requires human approval |
| `broker/internal/toolproxy/web_fetch.go` | Implementation: the broker's native Go handler for `web.fetch` — fetches the URL, extracts text, returns a structured result |

### 6. Infrastructure (Docker Compose)
Every service is a container on the `aikonos` bridge network, declared in `compose.yaml`
at the repo root. No Kubernetes, no GitOps controller.

| File | What it covers |
|---|---|
| `compose.yaml` | The whole stack. Profiles: `core` (infra + broker + gateway + webui + one-shots), `full` (alias of core), `obs` (OTel Collector + Grafana LGTM), `dev` (dev extras), `docs-mcp` (docs MCP server). Infra services (postgres, openfga, minio, vault, nats, keycloak, opa) are always-on. |
| `deploy/compose/README.md` | Operator guide: prerequisites, bring-up, profiles, durable volumes, backup/restore, dev-CA + TLS. |
| `deploy/compose/.env.local.example` | Env template — copy to `.env`, fill `OPENROUTER_API_KEY` at minimum. |
| `deploy/compose/keycloak-realm.json` | Imported realm `aikonos` (users alice/bob, admin@aikonos.com). |
| `deploy/compose/migrate.sh` | Run by the `migrate` one-shot: applies `broker/internal/db/migrations/*.sql` + creates the `openfga` DB. |
| `deploy/compose/tls/` | dev-CA + leaf certs (minted by `scripts/compose-dev-ca.sh`); the cert DNS-SAN equals the compose service name (load-bearing for mTLS). |
| `deploy/compose/obs/` | OTel Collector / Grafana / Loki / Tempo / Prometheus configs for the `obs` profile. |

Durable state is named Docker volumes: `postgres-data` (aikonos + openfga DBs),
`minio-data` (WORM audit), `workspace-data` (`Root/<tenant>/<user>/`), `obs-archive`.
They survive `docker compose down` (without `-v`).

### 7. DevOps Scripts & CLI
| File | What it covers |
|---|---|
| `Taskfile.yml` | Build/test + compose wrappers: `task proto:gen`, `task broker:build`, `task compose:{up,full,obs,seed,verify,backup,down}`. |
| `scripts/aikonosctl` | Developer CLI: task CRUD, plan submit/validate, policy check, audit tail/verify. |
| `scripts/compose-dev-ca.sh` | Mints the dev-CA + broker/gateway leaf certs (idempotent; `--force` to regenerate). |
| `scripts/compose-seed-openfga.sh` | Find-or-create the OpenFGA store, write model + dev tuples, set `AIKONOS_POLICY_OPENFGA_STORE_ID` in `.env`. |
| `scripts/compose-verify.sh` | Enforcement/audit/UI smoke test against the running stack (`task compose:verify`). |
| `scripts/compose-backup.sh` | Backup/restore the durable volumes (`pg_dumpall`, `mc mirror`, workspace tar). |

---

## Key Concepts

### Effect Classes
Every task, plan step, and skill is tagged with one of 8 effect classes in ascending risk order:

```
read_only → write_local → write_external → credential_access
  → destructive → network_egress → human_interaction → infrastructure
```

The broker enforces a ceiling: a plan's overall effect class is the maximum of its steps. Classes above `write_local` require explicit approval; `write_external` and above always prompt a human gate.

### Task State Machine
Tasks flow through states enforced exclusively in `db.TaskRepo.Transition()`:

```
PENDING → AWAITING_APPROVAL → APPROVED → RUNNING → COMPLETED
                            ↘ REJECTED
PENDING/RUNNING → CANCELLED
```

Never bypass `Transition()`. The RLS guard `withTenant()` must be called at the start of every DB query.

### Dual gRPC Servers
The broker runs two gRPC servers:
- **North-bound (`:9090`)** — faces frontends and the gateway; OIDC bearer auth, tenant from the JWT claim
- **South-bound (`:9091`)** — faces the gateway; SPIFFE-style mTLS (dev-CA leaf certs), tenant carried on the request

### Identity (dev-CA mTLS)
Under compose, broker↔gateway mTLS uses a **static dev-CA** (`scripts/compose-dev-ca.sh`) in place of SPIRE. The CA mints `broker` and `agent-gateway` leaf certs carrying both a URI-SAN (`spiffe://aikonos.com/<svc>`) and a DNS-SAN equal to the compose service name; the broker loads its server cert from `AIKONOS_BROKER_TLS_*` files. The DNS-SAN matching the service name is load-bearing for hostname verification — don't rename `broker`/`agent-gateway` without regenerating certs (`scripts/compose-dev-ca.sh --force`).

### Audit Hash Chain
Every `AuditEvent` includes `prior_event_hash`. This creates a tamper-evident chain: any deletion or modification breaks all downstream hashes. Events are written to MinIO with object lock so they cannot be overwritten.

---

## Guided Tour

Work through these seven steps to build a mental model of the full system.

### Step 1 — Project Overview
Read `README.md` and `docs/00-aikonos-architecture.md`. The architecture doc is long but essential — it explains the isolation stack, policy model, and audit chain before you see any code.

### Step 2 — Protobuf Contracts
Read all five `.proto` files. These are the single source of truth for every API surface. Pay particular attention to `plan.proto` (effect classes) and `broker.proto` (the north-bound `BrokerService` RPCs and the south-bound `SandboxService` RPCs). All Go and Python code is generated from these.

```bash
task proto:gen   # regenerate gen/go/ after any .proto change
```

### Step 3 — Broker Core
Read `broker/cmd/broker/main.go` then `broker/internal/broker/service.go`. `main.go` wires all dependencies; `service.go` is where requests are processed. Trace the path of a `CreateTask` RPC from receipt through policy check to DB insert.

### Step 4 — Database Schema & State Machine
Read `broker/internal/db/migrations/001_initial_schema.sql` then `broker/internal/db/tasks.go`. The schema has RLS policies on every table — understand `withTenant()` before writing any query. The state machine in `Transition()` is the single enforcement point; all other code calls it.

### Step 5 — Policy Engine
Read the three Rego files and `policies/fga/model.fga`. OPA handles attribute-based checks (is this plan's budget under limit? does it contain DLP patterns?). OpenFGA handles relationship checks (is this user a member of this tenant? does this agent have access to this workspace?). The broker calls both on every task.

### Step 6 — Infrastructure Stack
Read `compose.yaml` then `deploy/compose/README.md`. The compose file declares every service and its profiles (`core`/`full`/`obs`/`dev`/`docs-mcp`); the README is the operator guide (bring-up, durable volumes, backup/restore, dev-CA). Note the one-shots (`dev-ca-mint`, `migrate`, `openfga-migrate`) that run-then-exit before the apps start.

### Step 7 — Skills & DevOps
Read `skills/web.fetch/skill.yaml` — this is the template every skill manifest follows. Then read `broker/internal/toolproxy/web_fetch.go`, the authoritative implementation: skills execute in-process in the broker, and the manifest only declares the contract. Then skim `scripts/aikonosctl` for the developer CLI surface and `Taskfile.yml` for all available task targets.

---

## File Map (by layer)

```
aikonos/
├── proto/                         # API contracts (read first)
│   ├── broker.proto               # BrokerService + SandboxService RPCs
│   ├── plan.proto                 # Plan, PlanStep, EffectClass
│   ├── audit.proto                # AuditEvent + hash-chain fields
│   ├── skill.proto                # SkillMetadata + SkillSpec
│   └── workspace.proto            # WorkspaceManager RPCs
│
├── broker/                        # Core Go service
│   ├── cmd/broker/main.go         # Entry point (SPIFFE, OTel, gRPC wiring)
│   ├── internal/
│   │   ├── broker/service.go      # gRPC handlers
│   │   ├── db/tasks.go            # Task repo + state machine
│   │   ├── db/migrations/         # DB schema (Postgres + RLS)
│   │   ├── policy/engine.go       # OpenFGA + OPA wrapper
│   │   └── audit/emitter.go       # MinIO audit writer
│   └── Dockerfile
│
├── policies/
│   ├── opa/                       # Rego policies (plan, tool, envelope)
│   └── fga/                       # OpenFGA model + dev seed tuples
│
├── skills/                        # First-party skill examples
│   ├── web.fetch/                 # read_only — URL fetch
│   ├── doc.write/                 # write_local — workspace doc
│   └── email.draft/               # write_external — requires approval
│
├── compose.yaml                   # The whole stack (profiles: core/full/obs/dev/docs-mcp)
├── deploy/compose/                # Compose operator assets
│   ├── README.md                  # Operator guide (bring-up, volumes, backup)
│   ├── .env.local.example               # Env template → copy to .env
│   ├── keycloak-realm.json        # Imported realm (users, clients)
│   ├── migrate.sh                 # DB migration one-shot entrypoint
│   ├── tls/                       # dev-CA + leaf certs
│   └── obs/                       # OTel/Grafana/Loki/Tempo/Prometheus configs
│
├── scripts/                       # Dev tooling
│   ├── aikonosctl                  # CLI
│   ├── compose-dev-ca.sh          # Mint dev-CA + leaf certs
│   ├── compose-seed-openfga.sh    # Seed OpenFGA store/model/tuples
│   ├── compose-verify.sh          # Enforcement/audit/UI smoke test
│   └── compose-backup.sh          # Backup/restore durable volumes
│
├── docs/                          # Design docs
│   ├── 00-aikonos-architecture.md  # System architecture (start here)
│   ├── 01-broker-design.md        # Broker internals
│   ├── 02-policy-model.md         # ReBAC + ABAC policy model
│   ├── 04-threat-model.md         # Security threats and mitigations
│   ├── 07-workspace-design.md     # Workspace + RAG design
│   ├── 10-enable-enforcement.md   # Turn on OIDC/ReBAC/NATS + compose smoke test
│   ├── OPS-RUNBOOK.md             # Daily compose ops, Vault recovery
│   └── adr/DECISIONS.md           # Architecture decision records
│
├── Taskfile.yml                   # All build/test/deploy tasks
├── go.mod                         # Go module root (github.com/adamdekan/aikonos)
```

---

## Complexity Hotspots

These files have `moderate` or `complex` knowledge-graph complexity — approach carefully and read the associated design doc first.

| Complexity | File | Why it's tricky |
|---|---|---|
| **Complex** | `docs/00-aikonos-architecture.md` | 11-section system design; sets assumptions for all other code |
| **Complex** | `docs/01-broker-design.md` | Detailed broker contract; deviating from it breaks security invariants |
| **Complex** | `docs/02-policy-model.md` | RBAC+ReBAC+ABAC interaction; easy to create holes if misunderstood |
| Moderate | `broker/internal/broker/service.go` | Every RPC path; policy + DB + audit must all be called correctly |
| Moderate | `broker/internal/db/tasks.go` | RLS + state machine; bypassing either can corrupt task integrity |
| Moderate | `broker/internal/db/migrations/001_initial_schema.sql` | RLS policies and trigger functions; understand before schema changes |
| Moderate | `broker/cmd/broker/main.go` | Startup wiring; order of SPIFFE/OTel/gRPC init matters |
| Moderate | `proto/broker.proto` | Changing RPCs breaks all clients; treat as a contract |
| Moderate | `proto/plan.proto` | EffectClass ordering is load-bearing; don't reorder enum values |
| Moderate | `policies/opa/plan_validation.rego` | DLP + budget logic; subtle Rego bugs can silently allow or deny everything |
| Moderate | `policies/opa/envelope_send.rego` | Delegation depth and attenuation; bugs here enable privilege escalation |
| Moderate | `policies/fga/model.fga` | 10-entity authorization model; adding a relation has non-obvious transitive effects |
| Moderate | `docs/OPS-RUNBOOK.md` | Recovery procedures; read before touching Vault or rotating secrets |
| Moderate | `scripts/aikonosctl` | Multi-command CLI; test against the running compose stack before changing |

---

## Getting Started

### Prerequisites
- Docker Engine 24+ with Compose v2 (`docker compose`, not `docker-compose`).
- `openssl` (dev-CA), `task` (Taskfile runner), Go 1.23+, Node 22.

### Start a session
```bash
cd ~/repos/aikonos

# One-time / after a .proto change: generate the stubs the image builds COPY in.
(cd agent-gateway && npm ci)             # provides ts-proto for gateway TS stubs
task proto:gen

cp deploy/compose/.env.local.example .env      # fill OPENROUTER_API_KEY at minimum
task compose:up                          # core stack: infra + broker + gateway + webui
task compose:seed                        # dev-CA + Vault AppRole + seed OpenFGA + skill bundles → broker enforces ReBAC
task compose:verify                      # smoke-test enforcement / audit / UI
# Open http://localhost:4200
```

### Common gotchas

| Symptom | Fix |
|---|---|
| Gateway image build fails at `COPY gen` | Generate stubs first: `(cd agent-gateway && npm ci)` then `task proto:gen` (TS stubs are skipped without the toolchain) |
| Roles page shows "OpenFGA disabled — dev allow-all" | `task compose:seed` (seeds the store + recreates the broker) |
| broker mTLS handshake fails | `bash scripts/compose-dev-ca.sh` — the leaf cert DNS-SAN must equal the service name |
| Connector "not connected" after a Vault restart | Vault dev-mode is inmem; re-link via the Connections popover (tokens are lost on restart) |

### Build & test
```bash
task proto:gen          # regenerate Go/TS stubs from proto/
task broker:build       # go build → bin/broker
task broker:test        # go test ./broker/...
task policy:test        # opa test policies/opa/
docker compose --profile core build broker   # rebuild the broker image
```

### Task execution is gateway-managed
There is no broker-side sandbox or executor. The per-user Pi agent-gateway drives every approved plan step by calling the broker's `InvokeTool`, and each call passes the four gates before the Tool Proxy runs it. Per-session agent isolation (the parent/child credential split) is the isolation boundary. The earlier per-task Kubernetes sandbox (`sandbox/manager.go`, gVisor/Kata) was removed in the Docker-only pivot.

---

## Further Reading

- `docs/07-workspace-design.md` — per-user encrypted volumes, RAG integration
- `docs/10-enable-enforcement.md` — turn on enforcement + run the compose smoke test
- `docs/OPS-RUNBOOK.md` — compose ops, kill-switch activation, secret rotation, incident response
- `docs/adr/DECISIONS.md` — rationale behind technology choices (with Docker-only-pivot notes)
