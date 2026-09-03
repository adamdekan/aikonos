# Compose deployment — operator guide

Brings up the full Aikonos platform under Docker Compose. Profiles:
`core` (infra + broker + gateway + webui), `full` (same service set as core),
`obs` (OTel Collector + Grafana LGTM stack),
`dev` (development-only extras), `docs-mcp` (streamable-HTTP docs MCP server).

> **Azure VM deployment?** See `../azure/README.md` — adds a Traefik + oauth2-proxy +
> Entra ID edge (the `compose.azure.yaml` overlay + `.env.azure.example`) so only your
> tenant's users can sign in. Local dev keeps using `.env.local.example` → `.env`.

> **In-cluster-equivalent gap.** `docker compose config` validation and per-piece checks are the
> acceptance gate for this environment. Full live bring-up (all images built, healthy, end-to-end
> governed flow) is validated in-cluster per the project's standing posture.

---

## Prerequisites

| # | Step | Why |
|---|------|-----|
| 1 | Docker Engine 24+ with Compose v2 | `docker compose` (v2 plugin), not `docker-compose` (v1). |
| 2 | `openssl` on the host | Used by `scripts/compose-dev-ca.sh` to mint the dev CA + leaf certs. |

> **No host proto-gen needed (H6).** Each service Dockerfile generates its own proto stubs in-image
> (broker: `protoc` + `protoc-gen-go`/`-go-grpc`; gateway: `protoc` +
> the ts-proto plugin), so `docker compose build` works from a clean checkout with no host toolchain.
> The gateway build context is the **repo root** (dockerfile `agent-gateway/Dockerfile`) so it can reach
> `proto/`; host `gen/` is `.dockerignore`'d so the in-image stubs win. `task proto:gen` is still used
> for **local** (non-Docker) builds and tests that read `gen/` from disk.

---

## Bring-up steps

```bash
# 1. Prepare env (images self-generate proto stubs at build time — no host proto:gen prep).
cp deploy/compose/.env.local.example .env
# Edit .env — at minimum review POSTGRES_PASSWORD, AIKONOS_DB_APP_PASSWORD
# (broker's non-superuser DB role; see OPS-RUNBOOK "Database roles"), and AIKONOS_SIGNING_KEY.
# To use external agent endpoints (F3), set AIKONOS_API_KEY_PEPPER to a random
# secret (e.g. `openssl rand -hex 32`) on BOTH broker and gateway — it ships
# blank in .env.local.example, and without it minting an agent API key fails closed
# (FAILED_PRECONDITION: api key pepper not configured). The pepper is the HMAC
# key for API-key hashing; it is never stored in the DB. Rotating it invalidates
# every existing agent API key.

# 2. Mint dev TLS certs (idempotent; skips if certs already present)
bash scripts/compose-dev-ca.sh

# 3. Start the core stack (infra + broker + agent-gateway + webui)
docker compose --profile core up --build

# 4. Provision the broker's Vault AppRole (least-privilege), seed OpenFGA, then
#    recreate the broker so it authenticates to Vault and enforces ReBAC.
#    - vault-seed: enables approle, writes a policy scoped to the broker's KV
#      paths (vault-broker-policy.hcl), and injects role_id/secret_id into .env.
#      Without it the broker can't reach Vault → ephemeral capability key,
#      connectors + MCP-auth fail. (Re-run after any Vault restart — dev Vault is
#      in-memory.) The root token is used ONLY by this script, never the broker.
#    - openfga-seed: find-or-creates the store, writes model + dev tuples, sets
#      AIKONOS_POLICY_OPENFGA_STORE_ID in .env. Without it the broker runs the dev
#      allow-all stub (webui Roles: "OpenFGA is disabled"). Requires `fga` CLI + jq.
#    `task compose:seed` runs all three steps for you.
bash scripts/compose-vault-seed.sh
bash scripts/compose-seed-openfga.sh
docker compose up -d broker

# 5. (Optional) Observability stack — OTel Collector + Grafana LGTM
docker compose --profile obs up --build
```

The `migrate` one-shot runs automatically on every `up` before the broker starts.
It applies every `broker/internal/db/migrations/*.sql` not yet in `schema_migrations` and exits 0.
Re-running is safe (already-applied versions are skipped).

---

## Profiles

| Profile | Services started |
|---------|-----------------|
| `core` | postgres, openfga, minio, vault, nats, keycloak, opa, migrate, dev-ca-mint, broker, agent-gateway, webui |
| `full` | alias of `core` (the standalone admin/observability/frontend consoles were folded into `webui` and removed) |
| `obs` | otel-collector, grafana, loki, tempo, prometheus (OTel/LGTM stack) |
| `dev` | development-only extras |
| `docs-mcp` | aikonos-docs-mcp — streamable-HTTP MCP server serving the repo's Markdown docs on port `8060` |

Start `obs` alongside `core` or `full`:
```bash
docker compose --profile core --profile obs up --build
```

Start the docs MCP server (opt-in; joins the mesh, port `8060`):
```bash
docker compose --profile docs-mcp up -d --build
```

Register it in the webui (Admin → MCP connections): URL `http://aikonos-docs-mcp:8060/mcp`,
transport `streamable_http`, auth `none`. See `docs-mcp/README.md` for corpus mount details
and on-prem attach instructions.

To enable broker tracing, set `AIKONOS_OTEL_ENDPOINT=otel-collector:4317` in `.env` (value is
`host:port` with **no scheme** — `otlptracegrpc` dials bare gRPC; a `http://` prefix causes the
exporter to misparse the host and silently fail), then recreate the broker:
```bash
# In .env:
AIKONOS_OTEL_ENDPOINT=otel-collector:4317

docker compose up -d broker
# Confirm: docker compose logs broker | grep "Initializing OpenTelemetry"
```
Leave `AIKONOS_OTEL_ENDPOINT` empty (the default) when the obs profile is not running — the broker
skips trace export and starts cleanly with no exporter errors.

---

## LLM providers + cost analytics (F4)

LLM providers are **per-tenant** and admin-managed (Admin → LLM Providers): name, endpoint, api,
models with per-token **price-in / price-out**, and a write-only api-key. The key is stored in **Vault**
(`secret/data/providers/<tenant>/<id>`), never in Postgres. One provider is the tenant **default** —
the always-available fallback when an agent's preferred provider is unset.

**Seeding.** On boot the broker reads `deploy/compose/llm-providers.yaml` (mounted at
`/seed/llm-providers.yaml`) and inserts any provider id not already present for the dev tenant
(`AIKONOS_TENANT_ID`). It is idempotent and upsert-if-absent: after first boot the DB/admin UI is
authoritative, and editing the YAML only seeds brand-new ids. `apiKey: ${OPENROUTER_API_KEY}` resolves
in the broker container and is written to Vault; a blank key seeds the provider with `has_key=false`
(set it later in the admin UI). Set `AIKONOS_LLM_PROVIDERS_SEED_FILE=` empty to disable seeding.

**Cost.** The configured prices ARE the per-token cost block Pi uses to price every model turn. The
gateway reads `usage` on each turn and reports it to the broker (`EmitLlmUsage`), which records a signed
`llm.usage` audit event **and** OTLP counters (`llm_tokens_total`, `llm_cost_total`,
`llm_requests_total`, labelled by provider/model/tenant/agent/direction — never user). With the `obs`
profile up these land in Prometheus → the Grafana **"LLM Analytics"** dashboard (folder *Aikonos*,
http://localhost:3030): tokens + cost over time, cost by agent, top spenders.

### LLM Spend dashboard

Prometheus retention is short and user/session/run ids are cardinality-unsafe as labels, so durable
spend analytics come from Postgres instead. Every billable call also lands one row in
`llm_usage_events`, and the Grafana **"LLM Spend"** dashboard (same *Aikonos* folder,
http://localhost:3030) reads it directly: day/week/month totals and a month-burn forecast, spend by
user / group / agent / model / provider / source, cache hit rate, top sessions and runs, and cap
utilization against `spend_caps`.

For that read path, `grafana` joins the otherwise datastore-only `backend` network — a deliberate
deviation from the network-isolation table below. It connects as `aikonos_grafana`, a role `migrate.sh`
provisions with `SELECT` on five analytics tables and nothing else: no DML, no `BYPASSRLS`, no other
table. Set **`AIKONOS_GRAFANA_DB_PASSWORD`** in `.env` (it reaches `migrate` as the role's password and
`grafana` as `GRAFANA_PG_RO_PASSWORD`, which `datasources.yaml` reads). Rotating it means re-running
`docker compose up migrate` and recreating `grafana`.

---

## User provisioning (email → groups)

New users start with **zero** skill grants (personal sessions are deny-by-default). Tenant admins
manage **provisioning rules** (Admin → Provisioning): an email matcher → list of FGA groups. Two
matcher kinds: exact (`alice@contoso.com`) and domain wildcard (`*@contoso.com`). On a user's
**first authenticated call** the broker matches the verified token email against the rules and
writes `user:<oid> member group:<g>` tuples once (`user_directory.provisioned_at` records the
claim). Exact rules additionally apply **immediately** to users who have already signed in;
wildcard rules never retro-apply to already-provisioned users. Deleting a rule does not revoke
granted tuples — revoke via Admin → Roles. Email is only the provisioning selector; the FGA
subject is always the verified OIDC `oid`/`sub`.

**Seeding (optional).** `AIKONOS_PROVISIONING_SEED_FILE` points at a YAML
(`rules: [{matcher, groups: []}]`, see `deploy/compose/provisioning.yaml.example`). Idempotent,
insert-if-matcher-absent for `AIKONOS_TENANT_ID`; the admin UI is authoritative after first boot.
Unset (the default) disables seeding.

---

## Network topology

Three least-privilege Docker networks:

| Network | Type | Members |
|---------|------|---------|
| `mesh` | bridge (internet egress) | broker, agent-gateway, webui, nats, keycloak, dev-ca-mint |
| `backend` | `internal: true` | postgres, vault, minio, opa, openfga, openfga-migrate, migrate, grafana |
| `obs` | `internal: true` | otel-collector, grafana, loki, tempo, prometheus |

The **broker** joins `[backend, mesh]` — the only app on `backend`, the trusted intermediary between
the mesh and raw datastores. All other app services (agent-gateway, webui) join `[mesh]` only.
**grafana** joins `[obs, backend]` for the LLM Spend dashboard's read-only Postgres datasource
(see above); it holds a `SELECT`-only role and can reach no other datastore capability.

**Assume-breach property**: a breached `agent-gateway` cannot resolve or connect to `postgres`,
`vault`, or `minio` — Docker DNS returns `ENOTFOUND` for names not on a shared network.

**Host ports on `internal` networks:** Docker does **not** publish a service's host `ports:` when the
service is attached only to internal networks. The `backend` datastores (`postgres`, `vault`, `minio`,
`openfga`, `opa`) are therefore intentionally **not** on localhost — inspect them with
`docker compose exec <svc> ...` (e.g. `docker compose exec minio mc ls local/aikonos-audit/`,
`docker compose exec -e VAULT_ADDR=http://127.0.0.1:8200 -e VAULT_TOKEN=root-token-local-dev vault vault kv get -mount=secret broker/capability`
— dev Vault is HTTP, so `VAULT_ADDR` must be set or the CLI defaults to HTTPS and errors). Services with a `mesh` leg
(`broker` 9090/9091, `agent-gateway` 8080, `webui` 4200, `keycloak` 18080, `nats` 4222,
`otel-collector` 4317) and the `obs` dashboards (`grafana` 3030, `prometheus` 9095, `loki` 3100,
`tempo` 3200 — `obs` is a non-internal bridge) keep their host ports.

**Adding a new service**: assign to `[mesh]` by default; add `backend` only if it genuinely dials
postgres/vault/minio/opa/openfga directly. Never assign both `backend` and `mesh` without review —
that role belongs to the broker alone.

**Verification**: `scripts/compose-verify-netseg.sh` proves the segmentation from the live stack.

---

## Portable observability archive

The `obs` profile's OTel Collector supports four archive sinks, selected by `AIKONOS_OBS_ARCHIVE`:

| Value | Sink | Credentials required |
|-------|------|----------------------|
| `file` (default) | `fileexporter` → `/archive` (`obs-archive` volume) | None |
| `s3` | `awss3exporter` — AWS S3 or any S3-compatible endpoint (set `S3_ENDPOINT` for MinIO/R2/B2) | `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `S3_BUCKET`, `S3_REGION` |
| `azure` | `azureblobexporter` — Azure Blob (connection string **or** managed identity / service principal) | `AIKONOS_OBS_AZURE_AUTH_TYPE` + the creds for that type (see below) |
| `siem` | `otlphttp` — generic OTLP-HTTP forward, see [Shipping to a SIEM](#shipping-to-a-siem) | `AIKONOS_SIEM_OTLP_ENDPOINT`, `AIKONOS_SIEM_OTLP_AUTH_HEADER` |

The default (`file`) requires no cloud credentials and writes to the `obs-archive` named volume.
Set `AIKONOS_OBS_ARCHIVE=s3`, `=azure`, or `=siem` in `.env` before `docker compose up`.

#### Azure auth types

`AIKONOS_OBS_AZURE_AUTH_TYPE` selects how the collector authenticates to Blob storage.
The azureblobexporter in collector `0.130.0` implements exactly four types — there is **no**
federated `workload_identity` type; for AKS/host workload identity use `system_managed_identity`
(the pod/host's assigned identity). `AIKONOS_OBS_AZURE_ACCOUNT_URL`
(`https://<account>.blob.core.windows.net/`) is required for every type except `connection_string`.

| `AIKONOS_OBS_AZURE_AUTH_TYPE` | Required env | Use |
|---|---|---|
| `connection_string` (default) | `AIKONOS_OBS_AZURE_CONNECTION_STRING` | dev / shared key; carries its own endpoint |
| `system_managed_identity` | `AIKONOS_OBS_AZURE_ACCOUNT_URL` | prod on Azure with a system-assigned identity (no secret) |
| `user_managed_identity` | `…_ACCOUNT_URL` + `AIKONOS_OBS_AZURE_CLIENT_ID` | prod with a user-assigned identity |
| `service_principal` | `…_ACCOUNT_URL` + `…_TENANT_ID` + `…_CLIENT_ID` + `…_CLIENT_SECRET` | app registration |

### Shipping to a SIEM

Splunk, Datadog, and Elastic all ingest OTLP natively, so `AIKONOS_OBS_ARCHIVE=siem` uses one
generic `otlphttp` exporter instead of a vendor-specific one. It runs **alongside** the local
Tempo/Loki/Prometheus dashboards, not instead of them — set it and both keep working.

```bash
AIKONOS_OBS_ARCHIVE=siem
AIKONOS_SIEM_OTLP_ENDPOINT=https://<your-siem>:4318
AIKONOS_SIEM_OTLP_AUTH_HEADER=Bearer <token>   # full header value; format is receiver-specific
```

`AIKONOS_SIEM_OTLP_AUTH_HEADER` is optional — leave it unset for an endpoint that needs no auth;
the collector then sends an empty `Authorization` header, which unauthenticated OTLP receivers
ignore.

### Log attribution

The filelog receiver tails `/var/lib/docker/containers/*/*-json.log` (bind-mounted read-only) and
ships all container logs to Loki. Each log record carries a `container_short` label (12-char hex
prefix of the 64-char container id, parsed from the log file path via a stanza `regex_parser`
operator), making logs filterable per container in Grafana: `{container_short="f0a82eb76523"}`.

The full 64-hex `container_id` attribute is also extracted but is redacted to `****` by the
`redaction` processor (its `[a-zA-Z0-9+/]{40,}` blocked-values rule matches 64-char hex strings).
Use `container_short` for per-container filtering.

`/var/run/docker.sock` is **not** mounted into the collector — mounting it would give the collector
(and any attacker who compromises it) full Docker daemon control, violating the assume-breach
hardening baseline (H1/H2). Friendly `service.name` log attribution (mapping container ids to
service names) is deferred to per-service OTLP log export, a later F4 slice.

---

## Durable volumes

| Volume | Contents | Durable across |
|--------|----------|----------------|
| `postgres-data` | Postgres data dir (`aikonos` + `openfga` databases) | Container restart, `compose down` (without `-v`) |
| `minio-data` | MinIO object store — WORM audit trail | Container restart, `compose down` (without `-v`) |
| `workspace-data` | Per-user agent workspace (`Root/<tenant>/<user>/`) | Container restart, `compose down` (without `-v`) |
| `obs-archive` | OTel Collector file-sink archive | Container restart, `compose down` (without `-v`) |
| `vault-data` | Vault `file` storage backend (`/vault/file` in-container) | Container restart, `compose down` (without `-v`) — **all variants** now (local dev auto-unseals via the `vault-init` sidecar; azure/onprem unseal manually). `down -v` wipes it |

`docker compose down -v` removes all volumes — use only when a full reset is intended.

---

## Backup and restore

Use `scripts/compose-backup.sh` (single script, two subcommands).

### Backup

```bash
bash scripts/compose-backup.sh backup [OUTPUT_DIR]
# OUTPUT_DIR defaults to ./backups/<timestamp>/
```

Captures:
- `postgres-all.sql` — `pg_dumpall` of the entire Postgres instance (aikonos + openfga databases).
- `minio-<bucket>/` — `mc mirror` of the MinIO audit bucket.
- `workspace.tgz` — tar of the `workspace-data` volume.
- `obs-archive.tgz` — tar of the `obs-archive` volume.
- `vault-data.tgz` — tar of the `vault-data` volume, **all variants** now that local dev is
  durable too (skipped with a log line only if the volume doesn't exist). Vault must be **sealed
  or stopped** when this runs — its own file-storage backend is not crash-consistent snapshot-safe
  otherwise; stop the `vault` service first (`docker compose stop vault`), back up, then start it
  and unseal per `docs/OPS-RUNBOOK.md` "Vault operations" (local dev re-unseals automatically on
  `docker compose start vault` via the `vault-init` sidecar).

### Restore

```bash
bash scripts/compose-backup.sh restore BACKUP_DIR [--yes]
```

**Restore ordering (load-bearing):**

1. **Postgres first** — broker and OpenFGA read from it on startup.
2. **MinIO bucket second** — the audit chain head is stored in object metadata; restoring it before the broker starts ensures the chain resumes correctly.
3. **Workspace volume third** — agents read it at runtime, not at startup.
4. **obs-archive volume** — no load-bearing order; only the optional `obs` profile reads it.
5. **vault-data volume (all variants)** — restore before starting `vault`; the restored
   store comes back **sealed**. azure/onprem need a manual unseal (`docs/OPS-RUNBOOK.md`); local
   dev re-unseals automatically via `vault-init`. Either way, unseal before the broker's Vault
   client authenticates.
6. **Reconcile `AIKONOS_POLICY_OPENFGA_STORE_ID` in `.env`** from the backed-up value before starting
   the broker. This id is seed-written by `compose:seed` (`compose-seed-openfga.sh`'s
   find-or-create), not re-derivable from the Postgres dump — a mismatch points the broker at the
   wrong (or a nonexistent) FGA store even though the OpenFGA data itself restored correctly.
7. **Start broker + other apps only after all steps complete.**

Without `--yes` the script prompts for confirmation before overwriting data.

### Restore drill (periodic)

`compose.yaml` binds fixed host ports (webui `4200`, agent-gateway `8080`, keycloak `18080`,
minio console `9001`, grafana `3030` — see `docker compose port` or grep `compose.yaml` for
`ports:`), with no project-name parameterization. A scratch-project drill stack therefore
**cannot run alongside the main stack** — both would try to bind the same host ports, and
`docker compose up` for the second one fails or silently reuses the first's containers. The
drill instead runs the main stack **down** for its duration:

```bash
# 0. Stop the main stack (volumes are preserved — no -v).
docker compose down

# 1. Bring up a scratch-project stack with a different COMPOSE_PROJECT_NAME
#    (fresh, empty volumes — distinct names, does not touch the main stack's data).
COMPOSE_PROJECT_NAME=aikonos-restore-drill docker compose --profile core up -d postgres minio

# 2. Restore the backup into the scratch project's volumes.
bash scripts/compose-backup.sh restore BACKUP_DIR --yes
# reconcile AIKONOS_POLICY_OPENFGA_STORE_ID in .env from the backup, then:
COMPOSE_PROJECT_NAME=aikonos-restore-drill docker compose --profile core up -d --build

# 3. Verify. This validates the drill stack ONLY because it is now the sole
#    stack bound to those host ports (the main stack is down) — task compose:verify
#    always talks to whatever is listening on 4200/8080/etc, not to a project by name.
task compose:verify

# 4. Tear the drill down (throwaway, not a durable environment) and bring the main stack back.
COMPOSE_PROJECT_NAME=aikonos-restore-drill docker compose --profile full --profile obs down -v
docker compose --profile core up -d
```

A clean `compose:verify` pass in step 3 is the drill's success signal.

### Volume snapshot (coarse fallback)

When a logical `pg_dump` is not feasible (e.g. Postgres is not running):

```bash
OUT=./backups/snapshot-$(date +%Y%m%dT%H%M%S)
mkdir -p "$OUT"
PREFIX="${COMPOSE_PROJECT_NAME:-aikonos}"
for vol in postgres-data minio-data workspace-data obs-archive; do
  docker run --rm -v "${PREFIX}_${vol}:/data:ro" -v "${OUT}:/out" alpine \
    tar czf "/out/${vol}.tgz" -C /data .
done
```

Restore by reversing: mount the volume writable, extract the tarball. This is not a consistent logical
backup (no transaction boundary) — use `pg_dumpall` for Postgres whenever possible.

---

## Dev CA + TLS

`scripts/compose-dev-ca.sh` mints:
- `deploy/compose/tls/ca.crt` / `ca.key` — dev root CA (marked DEV ONLY, long-lived).
- `deploy/compose/tls/broker.crt` / `broker.key` — leaf cert, URI-SAN `spiffe://aikonos.com/broker` + DNS-SAN `broker`.
- `deploy/compose/tls/agent-gateway.crt` / `agent-gateway.key` — leaf cert, URI-SAN `spiffe://aikonos.com/agent-gateway` + DNS-SAN `agent-gateway`.

Re-running with certs already present is a no-op unless `--force` is passed.
The DNS-SAN matches the compose service name (load-bearing for mTLS hostname verification).

---

## Supply chain — pinned image digests (prod)

`deploy/compose/compose.digests.yaml` is a checked-in overlay pinning every **pulled** base
image (postgres, vault, nats, keycloak, minio, opa, openfga, otel-collector, grafana, loki,
tempo, prometheus, traefik, oauth2-proxy, ...) to a `@sha256:...` digest instead of a mutable
tag. Locally built services (broker, agent-gateway, webui, aikonos-docs-mcp, mcp-echo)
have no upstream pulled image and are not covered — they're pinned by the git commit that builds
them.

**Prod deploys** append it with an extra `-f` (or add it to `.env`'s `COMPOSE_FILE` colon-list,
same mechanism the azure/onprem overlays already use):

```bash
docker compose -f compose.yaml -f deploy/compose/compose.azure.yaml \
  -f deploy/compose/compose.digests.yaml up -d
```

**Local dev never includes it** — mutable tags are fine for dev iteration and pinning would just
add friction to `docker compose pull`.

**Refresh cadence:** every dependency bump (any base image version change in `compose.yaml` or
an overlay). Regenerate with:

```bash
bash scripts/pin-image-digests.sh
```

The script needs Docker + network access to `docker pull` each image; it never edits
`compose.yaml` or the overlays, and fails loud (naming the image) if any pull or digest
resolution fails — it never writes a partial file. CI parses the digests overlay on top of both
prod variants but does not run the pin script (it needs live pulls).

---

## Container hardening

All services inherit the `x-hardening` anchor (`<<: *hardening`).

### Universal (every service via the anchor)

| Key | Value | Purpose |
|-----|-------|---------|
| `security_opt` | `no-new-privileges:true` | Blocks privilege escalation via setuid/setgid bits |
| `cap_drop` | `ALL` | Zero Linux capabilities; add back only what is proven necessary |
| `pids_limit` | `512` | Fork-bomb containment |
| `mem_limit` | `1g` | Memory-exhaustion ceiling (counts tmpfs on cgroup v2) |
| `cpus` | `2` | CPU burst ceiling |

Per-service overrides where the default is too tight:

| Service | Override | Reason |
|---------|----------|--------|
| agent-gateway | `mem_limit: 2g` | Node + Pi harness + per-user sessions |
| keycloak | `mem_limit: 2g` | JVM startup requires ~1.5 g (MaxRAMPercentage=70) |

### Read-only rootfs (app services only)

| Service | `read_only` | `tmpfs` | Reason for omission |
|---------|------------|---------|---------------------|
| broker | yes | `/tmp:size=64m` | — |
| agent-gateway | yes | `/tmp:size=128m`, `/home/node/.npm:size=32m` | tsx module cache + npm notifier |
| webui | yes | `/tmp:size=32m` | — |
| postgres | **no** | — | Writes DB data to rootfs; backend-isolated (H1) |
| minio | **no** | — | Writes runtime state alongside data volume; backend-isolated |
| vault | **no** | — | Dev-mode inmem + rootfs touches; backend-isolated |
| nats | **no** | — | JetStream store on rootfs; mesh-isolated |
| keycloak | **no** | — | H2 DB + Quarkus runtime state; mesh-isolated |
| opa | **no** | — | Decision-log buffer on rootfs; backend-isolated |
| openfga | **no** | — | Runtime state on rootfs; backend-isolated |
| otel-collector | **no** | — | Archive volume writes + rootfs scratch; runs as root (DEV); obs-isolated |
| grafana | **no** | — | Session/plugin state on rootfs; obs-isolated |
| loki | **no** | — | Index + chunk data on rootfs; obs-isolated |
| tempo | **no** | — | Trace data on rootfs; obs-isolated |
| prometheus | **no** | — | TSDB on rootfs; obs-isolated |

### Convention for new services

- Always add `<<: *hardening`.
- Default `read_only: true` + `tmpfs: [/tmp:size=64m]`; remove only if the service writes rootfs paths that can't be tmpfs'd, and add a `# read_only omitted:` comment stating why.
- Override `mem_limit` only after measuring (`docker stats`); override `pids_limit` only with a documented reason.
- `cap_add` only capabilities the image provably needs; document each one.

### Verification

```bash
bash scripts/compose-verify-hardening.sh   # pids+mem on all services; read_only on app services; 3a re-affirm
bash scripts/compose-verify-netseg.sh      # H1 network segmentation (5/5)
bash scripts/compose-verify.sh             # functional smoke (10/10)
```
