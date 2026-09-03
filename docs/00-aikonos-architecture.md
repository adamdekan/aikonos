# Aikonos: Security-First Agentic Orchestration Platform

**Architecture Proposal — v0.1**

> **As-built note (Docker-only pivot).** This document is the original v0.1 design
> proposal and is preserved as the conceptual target. The platform now **deploys via
> Docker Compose only** (`compose.yaml`, profiles `core`/`full`/`obs`/`dev`/`docs-mcp`; see
> `deploy/compose/README.md`). The Kubernetes / Talos / ArgoCD / Cilium / SPIRE
> orchestration described in Layers 0–2 is **not** the deployed substrate:
> - **Layer 0 (immutable host OS)** and **Layer 2 (k8s control plane, Cilium, SPIRE,
>   admission control)** are not built — services run as Compose containers on a
>   bridge network.
> - **Workload identity (SPIFFE/SPIRE)** is replaced by a **static dev-CA**
>   (`scripts/compose-dev-ca.sh`) issuing broker/gateway leaf certs; the broker loads
>   its cert from `AIKONOS_BROKER_TLS_*` files.
> - **Layer 1 sandbox isolation (gVisor/Kata k8s pods)** is **not built** — there is no
>   broker-side sandbox or per-task pod. The governed substrate — broker, policy stack
>   (OPA + OpenFGA), capability tokens, the gateway-driven `InvokeTool` loop, NATS, MinIO
>   WORM audit — runs in full; task execution is gateway-managed (the agent-gateway
>   isolates work via its parent/child process split, not a k8s pod sandbox).
>
> Read the layers below as *intended* design; read `deploy/compose/README.md` for what
> actually runs.

## 1. Core Architecture Layers

### Layer 0 — Immutable Host OS
Use Fedora CoreOS, Flatcar, or Bottlerocket as the base. Rationale: transactional updates (rpm-ostree/OSTree), read-only `/usr`, atomic rollback, minimal attack surface. No package manager on running nodes — updates ship as signed OS images. Combine with Secure Boot + measured boot + TPM 2.0 attestation so nodes that drift from the golden image fail cluster join.

### Layer 1 — Workload Isolation
Don't rely on SELinux alone — use it as one layer in defense-in-depth:
- **gVisor or Kata Containers** as the primary sandbox for agent execution (user-space kernel / microVM). This is the hard boundary.
- **SELinux in enforcing mode** with custom policy modules per agent role (MCS categories per user/tenant).
- **seccomp-bpf** profiles scoped per skill type (network skill vs. filesystem skill vs. compute skill).
- **Landlock** LSM for per-process filesystem restriction within the sandbox.

Each agentic task runs in an ephemeral sandbox, torn down after execution. Home folders mount in via bind mounts with `nodev,nosuid,noexec` where possible.

### Layer 2 — Orchestration Control Plane
Kubernetes is the obvious substrate, hardened:
- KubeVirt or Kata runtime class for agent pods
- Cilium (eBPF) for network policy, identity-aware L7 filtering, and transparent encryption (WireGuard mesh)
- SPIFFE/SPIRE for workload identity — every agent, skill, and service gets an SVID, not a long-lived token
- OPA/Gatekeeper or Kyverno for admission control
- External Secrets Operator → HashiCorp Vault for all secrets

### Layer 3 — Agent Runtime
A custom orchestrator (the "Aikonos Broker") that:
- Receives tasks from the web frontend
- Resolves the user's RBAC → allowed skills/tools/MCPs
- Builds an execution plan with explicit capability grants
- Spins up sandboxed agent workers with JIT credentials
- Streams results back through an audited channel

### Layer 4 — Frontend & API Gateway
Web app (your interaction surface), behind mTLS-terminating reverse proxy. All API calls flow through a policy enforcement point that validates the request against the user's session, group memberships, and current risk score.

## 2. Identity, Access & Zero Trust

**IdP**: Keycloak or external OIDC (Entra ID, Okta). SAML/OIDC federation for enterprise customers.

**Authentication**: WebAuthn/FIDO2 mandatory for humans. Short-lived OIDC tokens (5–15 min). Device posture checks (managed device, disk encryption, patch level) gate session issuance.

**Authorization model** — push past pure RBAC toward **ReBAC + ABAC hybrid**:
- Groups → role bindings (the RBAC layer)
- Attribute policies: time of day, source network, data sensitivity label, risk score
- Relationship-based: "user X owns workspace Y, can delegate skill Z to agent spawned from workspace Y"

Use **OpenFGA** or **Cedar** (AWS) as the policy engine — both handle this natively and are battle-tested. RBAC alone will not scale for agent delegation chains.

**JIT privilege elevation**: implement as short-lived capability tokens signed by the Broker, scoped to (user, skill, resource, TTL, single-use nonce). Think macaroons or biscuit tokens — they support attenuation, so an agent can only narrow its own rights, never expand them.

## 3. Agentic Action Control

**Plan-then-execute with approval gates**: agent generates a structured plan (tool calls + arguments + expected side effects) → policy engine evaluates it → high-risk actions require human approval via the frontend. Don't let agents free-run tool calls.

**Tool call classification**: every skill declares its effect class (read-only, write-local, write-external, network-egress, credential-access, destructive). Policies bind effect classes to groups and require step-up auth for destructive classes.

**Egress control**: all agent network traffic through an identity-aware egress proxy (Squid with ICAP, or a purpose-built proxy). Domain allowlists per group. DNS via Unbound with RPZ for sinkholing. This is also where SSL decryption/inspection lives.

**Prompt injection defense**: treat all external content (web pages, documents, tool results) as untrusted input. Separate "instructions" and "data" channels in the agent loop. Log and flag suspicious instruction patterns in retrieved content. This is an open research problem — contain it, don't claim to solve it.

**Agent memory**: durable knowledge lives in Open Knowledge Format bundles scoped per user, per group, and per agent, reachable only through two gated skills (`memory.read`, `memory.write`) that ride the same plan → policy → capability chain as every other tool — enablement is therefore a skill grant (per group) or an agent-skill entry (per agent), nothing bespoke. Auto-recall surfaces matching memory *metadata* into a turn as flattened, length-capped data — never instructions — and recalled content is announced in the UI. Trust is human-anchored: only an explicit verify action (user for their own bundle, the group's owner for a group bundle, admin for an agent bundle) promotes a generated memory to human-reviewed.

## 4. Special Security Accounts

Model these as **dedicated control-plane service identities** in an isolated cluster namespace, not interactive user accounts:

- `aikonos-netsec`: manages firewall rules (Cilium NetworkPolicy, egress proxy config), sinkhole lists
- `aikonos-tlsinspect`: operates the SSL inspection pipeline (dedicated MITM CA, key ceremony required for rotation)
- `aikonos-iam`: group/role management, with four-eyes approval on all changes
- `aikonos-audit`: read-only across everything, writes to immutable log store

Non-interactive by default — changes happen via signed, reviewed config commits (GitOps via ArgoCD/Flux), not ad-hoc clicks. Break-glass procedures for true emergencies with full session recording.

## 5. Meta-Agents

- **Janitor agent**: ephemeral sandbox cleanup, orphaned resource reaping, stale token revocation. Scoped to resource lifecycle APIs only.
- **Posture agent**: continuous compliance checks (CIS benchmarks, policy drift detection). Read-only, writes only to findings queue.
- **Anomaly agent**: baseline user/agent behavior, flag deviations. Feeds SIEM, cannot act autonomously.
- **Supply-chain agent**: validates skill/MCP signatures, scans new connectors before activation.

Each meta-agent runs under its own SPIFFE identity, minimum scope, and its actions are themselves audited as if it were a user.

## 6. Skills & MCP Connector System

**Skill lifecycle**:
1. Developer submits skill → static analysis + SAST + dependency scanning
2. Sandbox execution tests in isolated namespace
3. Signed by enterprise signing key (Sigstore/cosign)
4. Published to internal registry with manifest declaring capabilities, effect class, required scopes
5. Admin assigns to groups

**MCP connectors**: same pipeline. Each MCP runs in its own sandbox, gets its own SPIFFE identity, and its tool definitions are validated against a schema the Broker enforces at call time. Do not trust MCP server-declared tool schemas at runtime without validation.

## 7. Inter-Agent Communication

### Why not raw sockets

Sockets give you a transport, not a communication model. Direct agent-to-agent sockets are wrong for this system because:

- **No identity by default** — you'd bolt on mTLS, at which point gRPC is strictly better
- **No durability** — agents are ephemeral sandboxes; if the receiver isn't running, the message is lost
- **No audit chokepoint** — every connection is a separate thing to log and correlate
- **Policy bypass risk** — direct channels skip the policy engine where authorization must live
- **Increased sandbox attack surface** — agents with listening sockets are far more exposed than agents that only egress to a broker

### The right model: brokered async messaging with Task Envelopes

Agents never talk directly. They exchange **Task Envelopes** through a central message bus. Every envelope passes through the policy engine. This mirrors how humans actually delegate — managers file tickets, they don't shell into reports' laptops.

**Transport**: NATS JetStream or Redpanda (Kafka-compatible). NATS is lighter and has native subject-based auth that maps cleanly to RBAC; Redpanda wins if you need full Kafka semantics. For most enterprise cases, **NATS JetStream** is the better pick — durable streams, per-subject ACLs, request/reply built in, and mTLS + JWT-based account isolation.

**Envelope schema** (signed JWS, validated at every hop):

```json
{
  "envelope_id": "uuid",
  "parent_task_id": "uuid | null",
  "from": {
    "user_id": "alice@corp",
    "agent_id": "spiffe://aikonos.com/alice/agent-42",
    "workspace_id": "uuid"
  },
  "to": {
    "user_id": "bob@corp",
    "group_id": "eng-team-7 | null",
    "routing": "user | group | role"
  },
  "task": {
    "intent": "summarize_q3_incidents",
    "payload_ref": "s3://aikonos-payloads/...",
    "required_skills": ["siem.query", "doc.write"],
    "deadline": "2026-04-20T17:00:00Z",
    "priority": "normal"
  },
  "delegation": {
    "capability_token": "biscuit:...",
    "attenuated_scopes": ["read:incidents.q3"],
    "max_cost_units": 500
  },
  "audit": {
    "trace_id": "...",
    "issued_at": "...",
    "issuer_signature": "JWS..."
  }
}
```

### Routing model

Three delivery modes, all brokered:

1. **Direct** — user Alice's agent sends a task to user Bob. Lands in Bob's inbox queue. Bob's frontend notifies him; Bob approves (or auto-approves based on policy) before his agent picks it up.
2. **Group** — "send to eng-team-7". Broker fans out to the group's shared queue; any member (or designated on-call) can claim it. Useful for manager → team delegation.
3. **Role** — "send to on-call-SRE". Resolved dynamically against a role directory (IdP or PagerDuty integration).

### The critical security constraints

- **No transitive trust** — a task Alice sends to Bob does NOT run with Alice's credentials. Bob's agent runs the task with Bob's credentials, attenuated by the delegation scopes Alice specified. Alice can narrow, never expand.
- **Capability attenuation** — biscuit tokens let Alice hand Bob a capability that is strictly a subset of her own. Bob can further attenuate before his agent acts.
- **Human-in-the-loop by default** — inter-user tasks require recipient acknowledgment through the web UI unless an explicit auto-accept policy exists (e.g., manager → direct report within defined scope).
- **Quota and rate limiting** — per-sender, per-recipient, per-group. Stops runaway agent loops and accidental DoS.
- **Loop detection** — envelopes carry a `parent_task_id` chain; the Broker rejects chains exceeding configured depth (e.g., 5) and detects cycles.
- **Cross-tenant isolation** — NATS accounts map 1:1 to tenants; cross-tenant messaging is disabled by default and requires explicit federation config.

### Reply and status channels

Each envelope has a dedicated reply subject. Status updates (accepted, in-progress, completed, failed, needs-input) flow back to the sender's inbox, so the originating agent/user can track progress without polling and without a direct connection to the recipient's sandbox.

### Audit

Every envelope — issuance, policy decision, delivery, acceptance, execution start, execution end, reply — is written to the immutable audit log with the full trace ID chain. This is non-negotiable for forensics and compliance.

### Implementation sketch

```
[Alice's Agent]
      │
      │ 1. Publish TaskEnvelope to nats://tasks.outbound.alice
      ▼
[Aikonos Broker]
      │ 2. Validate signature, check sender policy
      │ 3. Resolve recipient (user/group/role)
      │ 4. Attenuate capability token
      │ 5. Write to audit log
      ▼
[NATS JetStream: tasks.inbox.bob]
      │ 6. Durable queue, mTLS-gated
      ▼
[Bob's Frontend]
      │ 7. Notify Bob, await acceptance (or auto-accept)
      ▼
[Bob's Agent Sandbox]
      │ 8. Spawn sandbox with attenuated capabilities
      │ 9. Execute, stream status back via reply subject
      ▼
[Alice's Inbox: status updates]
```

### Why this scales

- **Durable by default** — recipient offline is a non-event; JetStream holds messages
- **Observable** — one bus, one audit stream, one place to attach SIEM
- **Policy-centralized** — one PEP, no policy sprawl across direct connections
- **Composable** — same primitive supports agent→agent, agent→human, human→agent, meta-agent broadcasts

### When you might add something else

- **High-frequency intra-workflow agent chatter** (e.g., a planner agent coordinating tightly with worker agents *within a single task*): keep that inside a single sandboxed process or use an in-process actor system (e.g., asyncio tasks, or a small embedded NATS with ephemeral subjects). Don't use the enterprise bus for microsecond-scale internal coordination.
- **Streaming large artifacts**: don't put them in envelopes. Envelopes carry references to object storage (S3/MinIO with per-object capability URLs). Keeps the bus fast and the audit log readable.

## 8. Data Architecture

- **User workspaces**: per-user encrypted volumes (LUKS + per-user key from KMS). Home directories hold conversation state, artifacts, RAG indices.
- **Workspace backend routing**: a per-user preference routes workspace file operations to either local disk or the user's OneDrive working folder. The broker's `workspacefs.Router` makes one routing decision per operation: reserved prefixes (`.agent/`, `config/`, `references/` — session state, HMAC sidecars, connector config) always stay on local disk, decided on the cleaned path before any preference lookup, so internal state can never land in a cloud drive. Everything else follows the preference, and agent file tools (`doc.*`, `workspace.read`, office document outputs) follow it too. Remote access uses delegated Graph tokens minted on-behalf-of each user from their own Entra sign-in (admin configures one M365 connection per tenant; no per-user consent clicks, no app-only blanket permission). Failures are loud — a broken preference store or dead token returns an error rather than silently falling back to local, so a user's files never split across backends.
- **Conversation store**: append-only, encrypted at rest with per-tenant keys.
- **Vector DB**: per-tenant logical isolation (Qdrant or Weaviate with RBAC); consider per-tenant collection + encryption.
- **Audit log**: write-once to object storage with object lock (immutability), streamed to SIEM. Every agent action, every tool call, every policy decision, every envelope.

## 9. Observability & Audit

- **OpenTelemetry** for traces across Broker → agent → skill → MCP → inter-agent envelopes
- **Falco** + **Tetragon** (eBPF) for runtime syscall/behavior monitoring
- **Every LLM call logged**: prompt, context, tool calls, outputs. Per-tenant retention policies, redaction pipeline for PII.
- SIEM integration (Elastic, Splunk, or Sentinel) — plan for the event volume; agents are chatty.

## 10. Known Gaps & Open Questions

**Deployment invariant: one broker and one gateway per deployment.** Both services keep
in-process, per-instance state that a second replica would silently duplicate rather than share:
the broker's rate limiter tracks tenant usage in-process, so a second broker doubles every
tenant's effective RPM/TPM limit; the gateway's child pool is per-instance, so a second gateway
splits sessions across two disjoint pools instead of sharing one. The broker enforces its half at
startup with a session-level Postgres advisory lock — a second instance fails fast on boot rather
than running silently degraded. The gateway has no equivalent runtime guard yet; its half of the
invariant is documentation-only.

1. **Multi-tenancy boundary** — single-tenant (one enterprise per deployment) vs. multi-tenant SaaS changes the threat model sharply. Recommendation for v1: single-tenant, self-hosted or dedicated-cloud.
2. **Supply chain** — skills, MCPs, and LLM models are all supply-chain risks. Signed artifacts + SBOM + attestation required.
3. **LLM provider trust** — external APIs mean sensitive data leaves the perimeter. Local inference (vLLM, TGI) for sensitive tiers; external for lower tiers with DLP pre/post.
4. **Prompt injection & data exfil via tool chains** — LLM-layer threats need as much attention as OS-layer threats.
5. **Regulatory** — NIS2, DORA, GDPR, EU AI Act (high-risk systems). Bake compliance controls in from day one.
6. **Incident response** — global agent freeze, per-tenant freeze, per-user freeze. Test regularly.

## 11. Suggested Tech Stack Summary

The **Proposed** column is the original v0.1 target. The **As-built (Docker-only)**
column is what the Compose deployment actually runs.

| Layer | Proposed | As-built (Docker-only) |
|---|---|---|
| Host OS | Fedora CoreOS / Bottlerocket | host Docker Engine 24+ (no immutable OS layer) |
| Orchestration | Kubernetes + Kata Containers + gVisor | Docker Compose (profiles core/full/obs/dev/docs-mcp); no broker sandbox, execution gateway-managed |
| Networking | Cilium + WireGuard + egress proxy | Compose `aikonos` bridge network; SSRF guard + netacl egress allowlist in the Tool Proxy |
| Identity | Keycloak + SPIFFE/SPIRE + WebAuthn | Keycloak (OIDC) + static dev-CA mTLS (replaces SPIRE) |
| Policy | OpenFGA or Cedar + OPA (admission) | OpenFGA (ReBAC) + OPA (ABAC), both as Compose services |
| Secrets | HashiCorp Vault + External Secrets | Vault (dev-mode, inmem) Compose service |
| Inter-agent bus | NATS JetStream (mTLS + JWT accounts) | NATS JetStream Compose service |
| Capabilities | Biscuit tokens | Biscuit tokens (root key read-or-created in Vault) |
| Audit | OpenTelemetry + Falco + Tetragon + immutable object store | OpenTelemetry (obs profile) + MinIO WORM object-lock |
| Artifact signing | Sigstore/cosign + in-toto attestations | deferred |
| Broker | Python or Go service | Go service |
