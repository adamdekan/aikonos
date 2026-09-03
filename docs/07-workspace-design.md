# Aikonos User Workspace — Design Document

**Component**: Persistent User Workspace
**Version**: v0.1
**Status**: Design
**Relates to**: 00-aikonos-architecture.md §8, 01-broker-design.md §5

---

## 1. Purpose & Scope

Every Aikonos user owns a persistent, encrypted, isolated workspace. It is their private environment inside the platform — the place their agent "lives," their data is stored, their preferences are applied, and their history is retained.

The workspace is **not** ephemeral. Unlike sandboxes (which are destroyed after each task), the workspace persists across sessions, tasks, reboots, and platform upgrades. It is the continuity layer between interactions.

The workspace is **not** a shared resource. No other user, no other agent, no platform service can read a user's workspace without an explicit capability token scoped to that workspace. Even the Broker has no standing read access.

### What the workspace stores

| Category | Contents |
|---|---|
| **Conversation history** | Full message logs, per-conversation, with role, timestamp, token count, task linkage |
| **Agent personality** | System prompt templates, persona definitions, behavioral preferences |
| **User preferences** | UI settings, notification preferences, default skills, language/locale, approval thresholds |
| **Personal documents** | User-uploaded files (PDF, DOCX, text, code, images, etc.) |
| **Artifacts** | Task outputs — reports, summaries, drafts, code produced by agent workflows |
| **Private RAG index** | Embeddings of the user's documents and conversations for semantic retrieval |
| **Credentials** | OAuth tokens, API keys, and connection secrets for personal MCP connectors — stored in Vault, referenced here |
| **Task history** | Metadata of all tasks run by the user: state, plan, audit reference, cost, timestamps |
| **Sharing state** | What the user has explicitly shared with whom (files, conversations, artifacts) |

### What the workspace does NOT store

- Capability tokens (issued per task, never persisted)
- Policy definitions (those live in Git / OpenFGA — workspace can store user preferences that *inform* policy requests, but not policy itself)
- Other users' data, even if delegated tasks touched it
- Sandbox scratch data (wiped on sandbox teardown)
- Tenant-wide shared resources (those have their own storage)

---

## 2. Physical Storage Architecture

Each workspace is isolated at the storage layer, not just the application layer.

### 2.1 Volume layout

```
/workspaces/
  <user_id>/                        ← LUKS-encrypted volume, per-user key
    workspace.key.ref               ← pointer to Vault path (not the key itself)
    data/
      conversations/                ← conversation store
        <conv_id>/
          messages.jsonl            ← append-only newline-delimited JSON
          metadata.json
      tasks/
        <task_id>/
          plan.json                 ← validated plan (from Broker)
          result.json               ← task result
          audit_ref.txt             ← pointer to audit trail in MinIO
      documents/
        <doc_id>/
          raw/                      ← original uploaded file
          metadata.json             ← name, mime, upload time, tags, sharing state
      artifacts/
        <artifact_id>/
          content                   ← file or structured output
          metadata.json
      rag/
        index.db                    ← SQLite for chunk metadata (not the vectors)
        chunks/                     ← raw text chunks for re-embedding
      credentials/
        <connector_id>.ref          ← pointer to Vault secret path; never the secret itself
    config/
      personality.json              ← agent persona and system prompt config
      preferences.json              ← all user preferences
      skills.json                   ← user's skill overrides / pinned versions
      mcp_connections.json          ← registered MCP connectors (refs, not tokens)
    scratch/                        ← per-task temp space, managed by Broker
      <task_id>/                    ← created at task start, wiped at task end
```

### 2.2 Encryption

Each user's volume is LUKS2-encrypted with a unique data encryption key (DEK):

```
DEK → stored in HashiCorp Vault at path: secret/workspaces/<tenant_id>/<user_id>/dek
    → Vault unseals with its own key (Shamir shares or cloud KMS)
    → DEK rotated on: password reset, admin request, right-to-erasure workflow
```

The workspace is **decrypted at mount time** by the Workspace Manager service (a privileged component) and mounted into the Pod that needs access, not into every Pod that runs. Decryption is authorized by a scoped Vault policy, not by a stored credential in the cluster.

Right-to-erasure: delete the DEK from Vault → the volume becomes permanently inaccessible without the key. No need to zero the physical disk.

### 2.3 Kubernetes PVC layout

```yaml
# One PVC per user, bound to a single PV
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: workspace-<user_id>
  namespace: aikonos-workspaces
  labels:
    aikonos.com/user-id: <user_id>
    aikonos.com/tenant-id: <tenant_id>
    aikonos.com/component: workspace
spec:
  accessModes: [ReadWriteOnce]          # Only one pod mounts at a time
  storageClassName: aikonos-workspace    # Custom SC: local-path or Longhorn
  resources:
    requests:
      storage: 20Gi                     # Per-user quota, configurable by admin
```

The Broker requests a mount via the Workspace Manager; the Workspace Manager mounts the right subpath (not the whole workspace) into the sandbox pod.

### 2.4 Subpath mounting

A task gets exactly the subpath it needs — not the whole workspace:

```
Task: "Summarize Q3 incidents"
Sandbox mounts:
  /workspace/data    → ro: data/documents/<relevant docs>
  /workspace/scratch → rw: scratch/<task_id>/   (wiped on teardown)
  /workspace/out     → append-only: artifacts/<new artifact id>/
```

Broker decides the subpath. The agent declares what it needs in the plan (`required_workspace_paths`), Broker validates against policy, mounts only what's approved.

---

## 3. Workspace Manager Service

A dedicated service that owns all workspace lifecycle operations. The Broker never touches workspace volumes directly — it requests operations from the Workspace Manager.

### 3.1 Responsibilities

- Create, destroy, and resize user workspaces
- Mount / unmount workspace subpaths into sandbox pods
- Encrypt / decrypt using Vault-managed DEKs
- Enforce per-user storage quotas
- Run garbage collection (expired scratch, orphaned task artifacts)
- Execute right-to-erasure workflows
- Expose a gRPC API to the Broker (not to sandboxes or frontend directly)

### 3.2 API surface

```proto
service WorkspaceManager {
  // Lifecycle
  rpc CreateWorkspace(CreateWorkspaceRequest) returns (WorkspaceHandle);
  rpc DestroyWorkspace(DestroyWorkspaceRequest) returns (google.protobuf.Empty);
  rpc GetWorkspaceInfo(GetWorkspaceInfoRequest) returns (WorkspaceInfo);
  rpc SetQuota(SetQuotaRequest) returns (google.protobuf.Empty);

  // Mount management (called by Broker for sandbox lifecycle)
  rpc RequestMount(MountRequest) returns (MountHandle);
  rpc ReleaseMount(ReleaseMountRequest) returns (google.protobuf.Empty);

  // Document management (called by Broker on behalf of frontend)
  rpc IngestDocument(IngestDocumentRequest) returns (DocumentHandle);
  rpc DeleteDocument(DeleteDocumentRequest) returns (google.protobuf.Empty);
  rpc ListDocuments(ListDocumentsRequest) returns (ListDocumentsResponse);

  // Artifact management
  rpc RegisterArtifact(RegisterArtifactRequest) returns (ArtifactHandle);
  rpc ListArtifacts(ListArtifactsRequest) returns (ListArtifactsResponse);

  // Config (personality, preferences)
  rpc GetConfig(GetConfigRequest) returns (WorkspaceConfig);
  rpc UpdateConfig(UpdateConfigRequest) returns (WorkspaceConfig);

  // Erasure
  rpc EraseWorkspace(EraseWorkspaceRequest) returns (EraseReceipt);
}
```

Every call carries `tenant_id` and `user_id` in metadata, validated by SPIFFE identity of the caller (only the Broker SVID is authorized).

### 3.3 Workspace Manager security posture

The Workspace Manager is a **privileged component** — it holds the ability to mount and decrypt any user's volume. Treat it accordingly:

- Runs under its own SPIFFE identity (`spiffe://aikonos.com/workspacemanager`)
- Its Vault policy allows only `read` on `secret/workspaces/<tenant_id>/<user_id>/dek` — not `list`, not cross-tenant
- Receives mount requests only from the Broker (SVID-validated)
- All operations audit-logged
- Source reviewed on every change; no external dependencies in the hot path

---

## 4. Conversation Store

Conversations are the primary interaction record. They must be append-only, queryable, and privacy-preserving.

### 4.1 Schema

Each conversation is a directory of an append-only `messages.jsonl` file:

```json
// One line per message in messages.jsonl
{
  "msg_id": "uuid",
  "conv_id": "uuid",
  "task_id": "uuid | null",
  "role": "user | assistant | system | tool_result",
  "content": "string | [content_block]",
  "timestamp": "ISO8601",
  "tokens": { "input": 120, "output": 340 },
  "model": "claude-sonnet-4-5 | ollama/llama3.2 | ...",
  "tool_calls": [...],
  "metadata": {
    "redacted": false,
    "sensitivity_label": "internal | confidential | public",
    "linked_artifacts": ["artifact_id"],
    "linked_documents": ["doc_id"]
  }
}
```

`metadata.json` per conversation:

```json
{
  "conv_id": "uuid",
  "title": "Q3 Incident Summary",
  "created_at": "ISO8601",
  "updated_at": "ISO8601",
  "pinned": false,
  "archived": false,
  "shared_with": [],
  "personality_snapshot_id": "uuid",
  "tags": ["security", "incident"]
}
```

### 4.2 Retention and right-to-erasure

- Retention period: configurable per tenant (e.g., 90 days for all conversations, or indefinite for pinned ones)
- Right-to-erasure: when triggered, the conversation directory is deleted, the RAG index chunks derived from it are removed, and the audit log records the erasure event (the audit event itself is NOT deleted — that would destroy the forensic trail; instead it records "data for user X erased at T")
- PII redaction pipeline: an optional background process scans `messages.jsonl` for PII patterns and marks lines with `"redacted": true`, replacing sensitive content with `[REDACTED]`. This is the tenant's decision whether to run.

---

## 5. Agent Personality and Preferences

### 5.1 Personality config (`personality.json`)

This is what makes "your agent" yours. It customizes the system prompt, persona, and behavioral defaults.

```json
{
  "version": "1",
  "persona": {
    "name": "Alex",
    "description": "My security research assistant. Precise, technical, no filler.",
    "language": "en",
    "tone": "technical",
    "verbosity": "concise"
  },
  "system_prompt": {
    "base": "You are Alex, a security research assistant...",
    "extensions": [
      {
        "id": "ext-siem-context",
        "label": "SIEM context",
        "content": "Our SIEM is Elastic. Indices follow the pattern logs-*. Alert IDs are prefixed ELK-.",
        "enabled": true
      }
    ],
    "pinned_context": [
      {
        "id": "ctx-org-kb",
        "label": "Org knowledge base",
        "doc_id": "uuid-of-uploaded-doc",
        "include_mode": "summary | full | rag"
      }
    ]
  },
  "model_preferences": {
    "default_model": "auto",
    "fallback_to_local": true,
    "max_tokens_per_turn": 4096,
    "temperature": 0.3
  },
  "created_at": "ISO8601",
  "updated_at": "ISO8601"
}
```

**Architectural note**: the system prompt is assembled by the Planner Validator, not the frontend, just before each LLM call. The Planner Validator reads `personality.json` from the workspace (via Broker → Workspace Manager → read-only mount) and merges it with the platform system prompt. The platform system prompt cannot be overridden by the user — it contains the security framing and tool call format requirements. User personality appends to it.

**Security note**: user-controlled system prompt extensions are treated as untrusted input, the same as any tool result. They inform the agent's behavior but cannot override platform policy or grant new capabilities.

### 5.2 Preferences (`preferences.json`)

```json
{
  "version": "1",
  "ui": {
    "theme": "dark",
    "language": "en",
    "timezone": "Europe/Prague",
    "density": "compact",
    "default_view": "tasks"
  },
  "notifications": {
    "approval_requested": { "email": true, "in_app": true },
    "task_completed": { "email": false, "in_app": true },
    "task_failed": { "email": true, "in_app": true },
    "incoming_delegation": { "email": true, "in_app": true }
  },
  "agent_behavior": {
    "auto_approve_read_only": true,
    "require_plan_review": false,
    "default_cost_budget": 50,
    "max_task_duration_minutes": 30
  },
  "skill_defaults": {
    "web.fetch": { "default_timeout_s": 15 },
    "doc.write": { "default_format": "markdown" }
  }
}
```

Preferences are user-managed via the frontend. The Broker reads `agent_behavior` preferences when evaluating auto-accept policies — but these can only tighten, not loosen, the tenant-wide policy floor.

---

## 6. Document Storage

Documents are files the user uploads to make available to their agent.

### 6.1 Ingest pipeline

```
User uploads file (frontend)
        │
        ▼
Broker calls WorkspaceManager.IngestDocument()
        │
        ▼
Workspace Manager:
  1. Validate file (size limit, mime type allowlist, malware scan via ClamAV)
  2. Assign doc_id (UUID)
  3. Write original to data/documents/<doc_id>/raw/<original_filename>
  4. Write metadata.json
  5. Enqueue for RAG indexing
        │
        ▼
RAG Indexer (async worker):
  1. Extract text (pdfminer, python-docx, unstructured.io, etc.)
  2. Chunk (fixed-size with overlap, or semantic chunking)
  3. Embed (local embedding model — nomic-embed-text, or OpenAI ada-002 per policy)
  4. Store vectors in per-user vector store
  5. Store chunk text in data/rag/chunks/<doc_id>/
  6. Update index.db metadata
  7. Emit audit event: document.indexed
```

### 6.2 Document metadata

```json
{
  "doc_id": "uuid",
  "user_id": "uuid",
  "tenant_id": "uuid",
  "filename": "Q3-Incident-Report.pdf",
  "mime_type": "application/pdf",
  "size_bytes": 204800,
  "upload_at": "ISO8601",
  "tags": ["incident", "q3-2026"],
  "sensitivity_label": "confidential",
  "sharing": {
    "mode": "private",
    "shared_with_users": [],
    "shared_with_groups": []
  },
  "rag_status": "indexed | pending | failed",
  "chunk_count": 48,
  "embedding_model": "nomic-embed-text-v1.5",
  "indexed_at": "ISO8601"
}
```

### 6.3 Quotas and limits

| Setting | Default | Override |
|---|---|---|
| Max document size | 50 MB | Admin per-tenant |
| Max documents per user | 500 | Admin per-tenant |
| Total workspace storage | 20 GB | Admin per-user |
| Allowed MIME types | pdf, docx, txt, md, csv, json, py, js, ts, go, jpg, png | Admin per-tenant |

---

## 7. Private RAG (Retrieval-Augmented Generation)

Each user has their own vector store. No shared embedding index exists. A user's uploaded documents never appear in another user's retrieval.

### 7.1 Architecture

```
┌──────────────────────────────┐
│     User Workspace           │
│  data/rag/                   │
│    index.db   (chunk meta)   │
│    chunks/    (raw text)     │
└──────────────┬───────────────┘
               │ read-only mount
               ▼
┌──────────────────────────────┐
│   RAG Service (per-user      │
│   instance, ephemeral)       │
│                              │
│   Qdrant collection          │
│   (loaded from workspace,    │
│    destroyed after session)  │
└──────────────────────────────┘
```

The vector store is **not persistent** as a running Qdrant instance. Instead:

- Embeddings are stored in a compact binary format in the workspace volume (`data/rag/vectors.bin` — custom HNSW or Qdrant snapshot)
- At task start, if RAG is needed, a per-user ephemeral Qdrant collection is loaded from the snapshot
- Queries run against it during the task
- The collection is torn down at task end
- On new document ingest, the snapshot is updated (workspace stays on disk, the in-memory collection is rebuilt next time it's needed)

This is slightly slower than a persistent Qdrant but eliminates the attack surface of long-running per-user database processes.

### 7.2 RAG retrieval in a task

When an agent task needs workspace context:

1. Broker mounts `data/rag/` read-only into the sandbox
2. Sandbox runtime calls Broker's `QueryWorkspaceRAG(query, top_k, filters)` endpoint
3. Broker calls Workspace Manager to spin up ephemeral collection for the user
4. Results returned to sandbox as structured context (doc title, chunk text, relevance score, doc_id)
5. Context injected into LLM prompt with clear provenance markers
6. Ephemeral collection torn down with the sandbox

### 7.3 Retrieval security

- RAG queries are audited (which documents were retrieved for which task)
- A task's `required_workspace_paths` must include `rag/*` for RAG access
- Cross-user retrieval is architecturally impossible — each user's vectors exist only in their workspace
- If a user shares a document with a group, that group has access to the document, not to the user's vector index. Shared documents get re-indexed into the recipient's index.

---

## 8. Credentials Store

> **Status: implemented in Phase 9** for Google Drive + OneDrive (OAuth2 auth-code+PKCE, Vault token
> storage, JIT fetch+refresh in the Tool Proxy). Deviations from the original design: the
> connector-auth state store is **in-memory single-use** (single-replica; a DB table is the HA
> follow-up), connector ids are **fixed per provider** (`gdrive`/`onedrive`, one connection each), and
> the Vault client is **hand-rolled** (k8s-auth + KV-v2, no `hashicorp/vault/api` dep). North RPCs:
> `BeginConnectorAuth`/`CompleteConnectorAuth`/`ListConnectors`/`RevokeConnector`.

Users need to authorize personal MCP connectors (Gmail, GitHub personal, Slack DMs, etc.). These require OAuth tokens or API keys scoped to the user's account.

**Design principle**: the workspace stores references to credentials, not credentials. All actual secrets live in Vault.

### 8.1 Connector registration flow

```
User authorizes connector (e.g., Gmail OAuth) in frontend
        │
        ▼
Frontend calls Broker: RegisterMCPCredential(connector_id, scopes)
        │
        ▼
Broker calls OAuth flow (redirect or device code)
        │
        ▼
Broker receives token → stores in Vault:
  Path: secret/workspaces/<tenant_id>/<user_id>/connectors/<connector_id>
  Value: { access_token, refresh_token, expires_at, scopes }
        │
        ▼
Broker writes reference to workspace:
  config/mcp_connections.json → { connector_id, vault_path, scopes, authorized_at }
        │
        ▼
When a sandbox task needs Gmail:
  Broker fetches token JIT from Vault (refreshes if expired)
  Injects as biscuit-attenuated credential into the Tool Proxy request
  Never written to sandbox filesystem
```

### 8.2 Credential security guarantees

- OAuth tokens are never in workspace files, never in environment variables, never logged
- Sandboxes never see raw tokens — Tool Proxy fetches and uses them on the sandbox's behalf
- Revocation: user disconnects a connector → Broker deletes Vault secret, removes reference from `mcp_connections.json`, revokes any in-flight tokens via the provider's revocation endpoint
- Token refresh is done by the Workspace Manager / Broker, not by sandboxes

### 8.3 Working-folder routing (OneDrive OBO)

> **Status: implemented**. A tenant admin
> connects the whole org to Azure M365 once (Admin → Settings → Microsoft 365 panel);
> every user then picks a **working folder** — their local workspace volume
> above, or a folder in their own OneDrive reached via on-behalf-of (OBO)
> delegated Graph access, no per-user connect step of the kind §8.1 describes.

The working folder governs the Files explorer, composer uploads, `#`-mention
discovery, and the agent's `doc.write`/`doc.read`/`workspace.read` tools —
all of them route through one `workspacefs.Backend` seam and follow whichever
backend the user has selected (`GetWorkspaceBackend`/`SetWorkspaceBackend`).

Reserved first path segments always route to the local volume regardless of
the selected backend, decided on the **cleaned** relative path before any
preference lookup:

- `.agent/Sessions/` (+ HMAC sidecars) — chat session history
- `.agent/Memory/` — OKF agent-memory bundles (concept files + `index.md` +
  `log.md`); group bundles live under a synthetic `group-<slug>` user segment
  and agent bundles under `svc-<agentId>`
- `config/` — connector metadata, MCP config
- `references/` — vision reference images
- `Skills/` — personal-skill folders (`Skills/<name>/SKILL.md` + `references/`/`assets/`);
  unlike the others this segment stays **visible and writable** in the Files explorer — it is
  pinned local only so session-build discovery never depends on Graph latency, not to hide it

This keeps internal state off Graph entirely — the scheduler's server-side
session write needs no OneDrive-awareness. A pre-existing OneDrive folder named
`Skills` (any case) becomes local-pinned by this rule; the Router warn-logs each
such dropped remote entry.

General-purpose write tools (`doc.write`) additionally **reject** any cleaned
path under `.agent/` at the workspace seam: server-maintained state there
(session HMACs, memory frontmatter provenance) must only change through the
dedicated tools that own it (`memory.write`, the session store), never through
a free-form file write.

A preference lookup or remote-backend failure fails loud (never a silent
fallback to local). A dead OBO refresh token surfaces as `reconnect_needed`
and self-heals on the next request carrying a fresh Entra login bearer — no
separate reconnect step, unlike the per-user OAuth connectors in §8.1.

---

## 9. Sharing Model

Users can share individual items — not their whole workspace.

### 9.1 Sharable items

| Item | Who can share it | Granularity |
|---|---|---|
| Document | Owner | Read-only, to named user or group |
| Artifact | Owner | Read-only, to named user or group |
| Conversation | Owner | Read-only, snapshot at share time (not live) |
| Personality template | Owner | Clone-to-share (recipient gets their own copy) |

### 9.2 Sharing mechanics

- Sharing never grants access to the workspace volume directly
- A shared document is copied into the recipient's workspace at share time (not a live mount)
- Groups with shared access to a document cause it to appear in each member's document list as "shared" (not in their private workspace volume but in a tenant-level shared store)
- The originating owner retains the original; revocation removes the copy from the recipient's workspace

### 9.3 Sharing and audit

Every share, un-share, and access of a shared item is audited. If Alice shares a document with Bob and Bob's agent uses it in a task, the audit trail shows: Alice shared → Bob's task used it.

---

## 10. Workspace in the Sandbox Mount (Integration with Sandbox Stack)

How §9 of the sandbox doc maps to the workspace structure:

```
Sandbox mount points:
  /workspace/data    → read-only bind of data/<task-specified-subpath>
  /workspace/scratch → read-write tmpfs or bind of scratch/<task_id>/ (wiped on teardown)
  /workspace/out     → append-only bind to artifacts/<new_artifact_id>/

Landlock rules inside sandbox:
  Read: /workspace/data/*
  Write: /workspace/scratch/*, /workspace/out/*
  No access: anything outside /workspace/
```

The Broker ensures:
- The subpath in `data/` is only what the approved plan declared
- `scratch/` is a new UUID dir per task, never reused
- `out/` is a fresh artifact dir; the sandbox can only append, not overwrite

After task completion:
- `scratch/<task_id>/` → deleted by Broker (janitor sweeps orphans)
- `out/<artifact_id>/` → promoted to `artifacts/<artifact_id>/` with `metadata.json` populated from task result

---

## 11. Storage Quotas and Lifecycle

### 11.1 Quota enforcement

```
Workspace Manager enforces per-user quotas:
  - Total volume size (PVC limit)
  - Per-directory limits: documents/, artifacts/, rag/ (soft limits, alerted)

Broker enforces per-task limits:
  - Max scratch space per task
  - Max artifact size per task
  - Max documents queried per task (cost control for RAG embedding)
```

### 11.2 Lifecycle events

| Event | Action |
|---|---|
| User created | `CreateWorkspace()` → PVC created, DEK generated in Vault, directories initialized |
| User suspended | Workspace read-only-locked, no new mounts permitted |
| User deleted | `EraseWorkspace()` → DEK deleted from Vault, PVC released (data inaccessible), erasure audit event written |
| Right-to-erasure request | Same as user deleted, but erasure audit event includes GDPR/DORA reference |
| Storage quota exceeded | Writes blocked, user notified, admin alerted |
| Document deleted by user | File removed from workspace, RAG index updated, sharing revoked |
| Artifact TTL expired | Artifact removed, linked conversation updated |

### 11.3 Backup and recovery

MVP: MinIO snapshot of the workspace PVC on a schedule (daily, 7-day retention).

Production: workspace snapshot exported to an encrypted secondary store. Recovery restores the PVC and re-registers the DEK in Vault. Point-in-time recovery is a post-MVP capability.

---

## 12. How this changes the MVP plan

The workspace is not a phase — it's a dependency that runs through every phase. Here's when to build what:

| MVP Phase | Workspace work |
|---|---|
| **Phase 0** (Foundations) | Create the PVC structure, define `workspace.proto`, stub Workspace Manager with `CreateWorkspace` and basic mount — no crypto yet |
| **Phase 1** (Broker skeleton) | Wire `GetConfig` (preferences, personality) so Planner Validator can read them; stub returns defaults |
| **Phase 2** (Sandboxes) | Real subpath mounts into sandboxes; scratch lifecycle; artifact registration after task completion |
| **Phase 3** (Policy + frontend) | Frontend reads/writes preferences and personality config; conversation history stored on task completion |
| **Phase 4** (Delegation) | Workspace audit of shared items; shared document mechanics |
| **Phase 5** (Hardening) | LUKS encryption wired for real; Vault DEK flow; right-to-erasure; quota enforcement |
| **Phase 6** (LLM + demo) | Document ingest pipeline; private RAG operational; credentials store for personal MCP connectors |

The seams (PVC structure, workspace.proto, Workspace Manager service boundary) must be in place from Phase 0. The crypto and RAG can be deferred to Phase 5–6. **Do not** defer the directory layout or the proto — those are the skeleton everything hangs on.

---

## 13. Open Questions

1. **Conversation history size limits**: full token-level storage grows fast. Options: compress old conversations, store only last N turns in hot storage, archive to MinIO. Recommendation: start unbounded, add quotas in Phase 5 when you have real data on growth rates.

2. **Embedding model locality**: embedding with a local model (nomic-embed-text) keeps data inside the perimeter but is slower and potentially lower quality than OpenAI ada-002. Recommendation: local model for MVP; make it configurable per tenant, gated by DLP policy.

3. **Semantic chunking vs fixed-size**: semantic chunking (by paragraph / section boundary) produces better retrieval but requires ML. Fixed-size with 20% overlap is fine for MVP. Upgrade the chunker later without changing the storage format.

4. **Multi-device workspace access**: a user's agent runs on one node, but their frontend might be on another. The `ReadWriteOnce` PVC access mode means only one pod mounts the workspace volume at a time. For MVP this is fine (only sandboxes mount it, and those are sequential). Multi-device concurrent write is a Phase 8 problem.

5. **Workspace export**: should a user be able to export their entire workspace (conversations, documents, artifacts) as a zip? Yes, but that's a post-MVP feature requiring careful DLP review of what goes in the export. Design the export endpoint now as a stub; implement later.
