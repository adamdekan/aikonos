# Aikonos Operations Runbook

**Audience**: Solo developer / operator during MVP phase.
**Update**: After every Phase completion and after every incident.

> Aikonos deploys via **Docker Compose only**. See `deploy/compose/README.md` for the
> operator guide and `compose.yaml` for the stack. Profiles: `core` / `full` / `obs` / `dev` / `docs-mcp`.

---

## Stack access

```bash
cd ~/repos/aikonos

# All services are plain Docker containers on the `aikonos` bridge network.
docker compose ps
docker compose logs broker        # or any service name
```

Published local URLs:

| Surface | URL |
|---|---|
| Unified webui | http://localhost:4200 |
| agent-gateway | http://localhost:8080 |
| Keycloak | http://localhost:18080 |
| Grafana (obs profile) | http://localhost:3030 |
| MinIO console | http://localhost:9001 |

---

## Daily dev startup

The stack is brought up and torn down with `task compose:*` (thin wrappers over
`docker compose --profile …`). Durable state lives in named Docker volumes, so a
host reboot or `docker compose down` (without `-v`) preserves Postgres, MinIO, and
the workspace.

```bash
cd ~/repos/aikonos

# 1. (First run only / after a .proto change) generate stubs the image builds COPY in.
(cd agent-gateway && npm ci)   # provides ts-proto for the gateway TS stubs
task proto:gen

# 2. Start the core stack (infra + broker + agent-gateway + webui)
task compose:up

# 3. Seed dev-CA + OpenFGA, then recreate the broker so it enforces ReBAC.
#    Without this the broker runs in dev allow-all stub mode.
task compose:seed

# 4. Smoke-test enforcement, audit, and the UI
task compose:verify

# 5. Open the UI at http://localhost:4200
```

`task compose:down` stops the stack (it does **not** pass `-v`, so volumes survive).

---

## Vault operations

### After a restart (local dev — durable + auto-unseal)
Local dev runs Vault on **durable `file` storage** (`deploy/compose/vault/vault.hcl`,
named volume `vault-data`) — same backend as the prod overlays. The `vault-init`
sidecar **auto-inits and auto-unseals** on every `up` (gated on
`AIKONOS_VAULT_AUTO_UNSEAL=true` in `.env`), so KV **survives restart**:

- **LLM provider API keys**, **connector OAuth tokens**, the **capability root key**,
  the **gateway-grant HMAC key**, and the **audit signing key** all persist. No
  re-entering keys in Admin, no re-linking connectors after a restart.
- **AppRole seeding** (`task compose:seed`) is **first-boot-only** now — the auth
  config persists too. Safe to re-run any time (idempotent).

Nothing to run after a restart; `vault-init` unseals automatically (logs
`unsealed`/`already unsealed`). **Reset** dev Vault to a clean slate with
`docker compose down -v` (drops the `vault-data` volume). Init/unseal material is
persisted to `/vault/file/init.env` on that volume — **dev only**; the flag is
`false` for azure/onprem, where the sidecar no-ops and unseal is manual (below).

### azure/onprem overlays — durable Vault, first-boot init + unseal

`deploy/compose/compose.azure.yaml` and `compose.onprem.yaml` override `vault` with a
`file` storage backend (`deploy/compose/vault/vault.hcl`, named volume `vault-data`) —
**no dev-mode, no auto-unseal.** A freshly created stack's Vault starts **uninitialized
and sealed**; `scripts/compose-vault-seed.sh` detects this and refuses with a pointer
back to this section instead of failing on an opaque auth error.

**First boot (once per durable Vault, i.e. once per fresh `vault-data` volume):**

```bash
cd ~/apps/aikonos    # or wherever the overlay stack is deployed

# 1. Bring the vault service up (part of the normal `docker compose up`, or standalone):
docker compose up -d vault

# 2. Initialize. Pick key-shares/threshold for your operational reality — a single
#    operator can use 1/1; multi-operator setups should use Shamir splitting
#    (e.g. -key-shares=5 -key-threshold=3) so no one person holds a working key alone.
docker compose exec vault vault operator init \
  -address=http://127.0.0.1:8200 \
  -key-shares=1 -key-threshold=1

# This prints unseal key(s) and a root token ONCE. Vault does not store them and
# cannot regenerate them — capture them immediately.

# 3. Unseal (repeat with a different key share until threshold is met):
docker compose exec vault vault operator unseal -address=http://127.0.0.1:8200 <unseal_key>

# 4. Seed the broker's scoped AppRole (uses the root token from step 2 this one time).
#    This also enables the KV-v2 engine at secret/ — durable Vault does NOT
#    auto-mount it (unlike dev-mode), and the broker's whole Vault surface lives
#    there, so this step is mandatory, not just for the AppRole.
VAULT_TOKEN=<root_token> bash scripts/compose-vault-seed.sh
docker compose up -d --force-recreate broker
```

**Where the unseal keys and root token must NOT be stored:** not in this repo, not in
`.env` (which is excluded from version control but is still a plaintext file on the
host and is never read/written by tooling that handles secrets), not in any committed
file. Use an operator's own secrets manager, a physical safe/split-custody scheme for
Shamir shares, or your organization's break-glass procedure. The root token should be
revoked (`vault token revoke <token>`) after the AppRole is seeded — the broker never
needs it again; only the AppRole credentials `scripts/compose-vault-seed.sh` writes to
`.env` are used at runtime.

**After every restart** (container restart, host reboot, `docker compose down` without
`-v`): the durable Vault comes back **sealed** — its KV data survives (that's the point
of `vault-data`), but the process must be unsealed again before the broker's Vault
client can authenticate:

```bash
docker compose exec vault vault operator unseal -address=http://127.0.0.1:8200 <unseal_key>
# repeat per key share until threshold is met
```

There is no auto-unseal configured; no cloud KMS or HSM integration is in
scope for this phase, so every restart requires the manual step above. `vault-data` is a durable volume — see `deploy/compose/README.md`
"Durable volumes" and "Backup and restore".

---

## Kill switch procedures

### Freeze a user immediately
```bash
python3 scripts/aikonosctl kill-switch user alice@corp
```

### Check active kill switches
```bash
python3 scripts/aikonosctl kill-switch status
```

### Deactivate a kill switch
```bash
docker compose exec postgres \
  psql -U aikonos -d aikonos -c \
  "UPDATE kill_switches SET active=false, deactivated_at=NOW(), deactivated_by='operator'
   WHERE scope='user' AND subject='alice@corp' AND active=true;"
```

**IMPORTANT**: Kill switch drills must be run monthly. Schedule: first Monday of each month.
After each drill, write the result to `docs/kill-switch-drills.md`.

---

## Service lifecycle

### Restart a single service
```bash
docker compose up -d --force-recreate broker     # picks up new env / re-reads config
```

### Rebuild after a code change
```bash
task proto:gen                                    # if a .proto changed
docker compose --profile core build broker
docker compose up -d --force-recreate broker
```

### Agent config changes (persona / soul, skill grants)
The gateway pools long-lived Pi-loop child processes and freezes each child's
system prompt (base instructions + the agent's persona/soul) and tool allowlist
at spawn. On the next **idle** reuse it re-resolves the plan and respawns the
child when the persona or the tool allowlist changed — so an edit made in
Admin → Agents takes effect within ~10s (`PLAN_RECHECK_MS`) of the next reuse,
not mid-conversation. To apply a change immediately, evict the cached children:
```bash
docker compose restart agent-gateway              # drops all pooled children
```

### Recreate the whole core stack
```bash
task compose:down
task compose:up && task compose:seed
```

---

## Database roles (RLS enforcement)

Two Postgres roles, by design:

| Role | Attributes | Used by | Why |
|------|-----------|---------|-----|
| `aikonos` (`POSTGRES_USER`) | superuser, owns tables | `migrate` one-shot (DDL) only | needs DDL; superuser **bypasses RLS**, so the broker must not use it |
| `aikonos_app` | `LOGIN NOSUPERUSER NOBYPASSRLS`, DML-only | the **broker** (`AIKONOS_DB_USER=aikonos_app`) | RLS actually enforces tenant isolation against a non-superuser role |

The broker connects as `aikonos_app` so the per-table RLS policies (`tenant_id =
current_setting('app.current_tenant')`) are live. A superuser/owner role bypasses RLS
unconditionally, which would silently disable tenant isolation.

**Configure it:**

```bash
# .env — set BOTH passwords (distinct secrets):
POSTGRES_PASSWORD=<owner-pw>            # role aikonos (migrations)
AIKONOS_DB_APP_PASSWORD=<app-role-pw>    # role aikonos_app (broker); openssl rand -hex 32
```

`migrate.sh` provisions `aikonos_app` (create + grants, idempotent) using
`AIKONOS_DB_APP_PASSWORD`. **Deploy ordering matters** — the role must exist before the
broker starts:

```bash
docker compose up migrate                        # creates role + grants + fns (033)
docker compose up -d --force-recreate broker      # broker connects as aikonos_app
```

If `AIKONOS_DB_APP_PASSWORD` is unset, `migrate` fails loud (it will not silently fall back).
The two cross-tenant baseline maintenance sweeps run through `SECURITY DEFINER` functions
(`baseline_distinct_agents`, `baseline_prune_windows`, migration 033) owned by `aikonos` —
the only sanctioned RLS bypass.

**Verify RLS is enforcing** (should print `0`, not the table size):

```bash
docker compose exec -e PGPASSWORD="$AIKONOS_DB_APP_PASSWORD" postgres \
  psql -U aikonos_app -d aikonos -tAc \
  "SET app.current_tenant='00000000-0000-0000-0000-000000000000'; SELECT count(*) FROM tasks;"
```

## Secret rotation

### Rotate the broker app-role password (aikonos_app)
Edit `AIKONOS_DB_APP_PASSWORD` in `.env`, then:
```bash
# 1. Sync the live role (migrate does this idempotently, or do it directly):
docker compose exec postgres \
  psql -U aikonos -c "ALTER ROLE aikonos_app WITH PASSWORD '<new-app-pw>';"

# 2. Recreate the broker so it reconnects with the new password:
docker compose up -d --force-recreate broker
```
(`POSTGRES_PASSWORD` and `AIKONOS_DB_APP_PASSWORD` are independent — rotate each separately.)

### Rotate Postgres password
Edit `POSTGRES_PASSWORD` in `.env`, then:
```bash
# 1. Change it in the running database
docker compose exec postgres \
  psql -U postgres -c "ALTER USER aikonos WITH PASSWORD '<new-pw>';"

# 2. Recreate the services that read it from .env
docker compose up -d --force-recreate postgres broker
```
(Keep `.env` and the live DB in sync — the broker reads the password from `.env`.)

### Capability root key
The broker reads-or-creates a shared ed25519 key in Vault. To pin a fixed key across
restarts, set `AIKONOS_CAPABILITY_ROOT_KEY` in `.env` (base64 64-byte ed25519 private
key) and recreate the broker. Otherwise the read-or-created key now **persists** in the
durable Vault across restarts (all variants); it is only regenerated on a `down -v`
(dev) or a fresh `vault-data` volume — harmless since tokens are per-request.

### Audit signing key (CP1.2)
The broker reads-or-creates a versioned 32-byte HMAC key at Vault KV-v2 path
`broker/audit-signing-key`. Every emitted audit event records the KV-v2
`signing_key_version` it was signed under, so `VerifyChain` can span a rotation —
older events keep verifying under their original version, new events sign under
the new one, no backfill or re-signing needed.

**Rotate:**
```bash
# 1. Write a new version directly to Vault (bypasses the broker's cas=0
#    first-boot guard — this is a deliberate operator action).
NEW_KEY=$(openssl rand -base64 32)
docker compose exec vault vault kv put secret/broker/audit-signing-key key="${NEW_KEY}"

# 2. Restart the broker so it re-reads the (now-latest) version at startup.
docker compose up -d --force-recreate broker
```
After the restart, new events sign under the new version; `python3 scripts/aikonosctl
audit verify` (below) verifies the full chain across both versions transparently.

**Degraded mode:** if Vault is unreachable at broker startup, the broker falls back to
the `AIKONOS_AUDIT_SIGNING_KEY` env value (`signing_key_version=0`) and logs `audit
signing key: Vault unavailable — falling back to static env key`. Fix Vault access and
recreate the broker to resume Vault-backed, versioned signing. This degraded mode is
permitted-but-alarmed (broker keeps serving, loudly logged) rather than startup-blocking
— unlike the capability root key, whose Vault-unavailable fallback is gated by
`devModeGuard` and refuses to serve outside `AIKONOS_DEV_MODE=true`.

### M365 / tenant OneDrive secret
The tenant-wide OneDrive connection (Admin → Settings → Microsoft 365 panel) stores its Entra
client secret at KV-v2 `secret/m365/<tenant>/app`; per-user OBO refresh tokens land at
the existing connector paths (`secret/connectors/<tenant>/<user>/onedrive`). Both are
managed entirely at runtime — configure via the panel, never via env. The broker policy
grant lives in `deploy/compose/vault-broker-policy.hcl` (`secret/data/m365/*`). After a
`down -v` (dev) or fresh `vault-data` volume the connection must be re-entered in the
panel; user tokens re-mint themselves at the next Entra sign-in (lazy OBO bootstrap,
audit event `aikonos.broker.connector.obo_bootstrap`). Dead refresh tokens surface as
connector status `reconnect_needed` and self-heal the same way. Setup walkthrough:
`deploy/onprem/README.md` → "Tenant-wide OneDrive".

---

## Audit log operations

### Verify audit chain integrity
```bash
python3 scripts/aikonosctl audit verify
```

### Query recent audit events (MinIO)
```bash
# MinIO console:
open http://localhost:9001         # bucket: aikonos-audit · path: <tenant>/YYYY/MM/DD/

# Or via the mc client inside the container:
docker compose exec minio mc ls local/aikonos-audit/ --recursive | tail -20
```

The audit trail is hash-chained per tenant and written to the `minio-data` volume
with object-lock WORM, so it survives `compose down` (without `-v`).

---

## AI-BOM

`docs/AI-BOM.md` is a committed artifact — regenerate it at release (and whenever skill
bundles or component versions change) with:

```bash
scripts/generate-ai-bom.sh
```

The committed default emits a documented placeholder for the LLM-providers section —
providers are per-deployment Postgres (`llm_providers`) runtime state, not something a
checked-in artifact should pin (dev/test fixture rows would otherwise leak in). To
populate that section against a live deployment's actual providers:

```bash
scripts/generate-ai-bom.sh --live-db
```

(requires a reachable compose stack's `postgres` service; never selects key material).

---

## Agent behavioral baseline learning

The broker learns each agent's normal tool-use envelope from observed `InvokeTool` activity —
monitoring only, never blocks a call. See `docs/agents/baselines.md` for the full model; this is
the operator quick-reference.

**Tables** (migration 032):
- `agent_behavior_windows(tenant_id, agent_id, window_start, tool_id, invocations, cost_units)` —
  raw rolling per-window observations, pruned past retention.
- `agent_baselines(tenant_id, agent_id, tool_set, rpm_p95, cost_p95, sample_windows, first_seen,
  computed_at)` — the materialized learned envelope, one row per agent.

**Tuning** (`AIKONOS_BASELINE_*`, see `.env.local.example`): `ENABLED=true`,
`WINDOW_SECONDS=60`, `LEARN_INTERVAL_SECONDS=3600`, `MIN_SAMPLE_WINDOWS=30`,
`DRIFT_MULTIPLIER=2.0`, `RETENTION_WINDOWS=10080`.

**Reading a drift audit event** (`aikonos.agent.baseline_drift`, Decision `DENY` in the audit
record — this reflects the *finding*, not a blocked call): context carries `kind`
(`unknown_tool`/`rate`), `observed`, `ceiling` (rate drift only), `tool`, and `resourceRef
aikonos:agent:<id>`. Cross-reference `agent_baselines` for that `(tenant_id, agent_id)` to see the
envelope it drifted from.

```bash
docker compose exec postgres psql -U aikonos -d aikonos -c \
  "SELECT * FROM agent_baselines WHERE tenant_id = '<t>' AND agent_id = '<a>';"
```

Metric: `aikonos_broker_agent_baseline_drift_total` (Prometheus; `exported_job="aikonos-broker"` filter, same
gotcha as every other broker series). Grafana alert:
`deploy/compose/obs/grafana/provisioning/alerting/baseline-drift-alert.yaml`.

**This is monitoring, not enforcement.** A drift finding never fails, delays, or blocks
`InvokeTool` — enforcement stays with the rate limiter and the four authz layers. No sample_windows
< `AIKONOS_BASELINE_MIN_SAMPLE_WINDOWS` (default 30) → no drift is emitted at all (learning phase).

---

## Observability (Grafana dashboards)

Dashboards are provisioned from `deploy/compose/obs/grafana/provisioning/dashboards/`
and auto-reload within ~30s of a file change (no Grafana restart):

| Dashboard | UID | Source |
|---|---|---|
| LLM Analytics | `aikonos-llm-analytics` | Prometheus (`llm_*` counters) |
| Broker Actions | `aikonos-broker-actions` | Prometheus (gRPC RPC metrics) + Loki (audit-log user activity) |

### Enable the pipeline (required — dashboards are empty without it)

```bash
# 1. Wire the broker to export telemetry. Empty endpoint = initMeter builds a
#    reader-less MeterProvider that exports NOTHING (the #1 "No data" cause).
grep AIKONOS_OTEL_ENDPOINT .env        # must be: AIKONOS_OTEL_ENDPOINT=otel-collector:4317
#    if empty, set it, then recreate the broker:
docker compose up -d --force-recreate broker

# 2. Bring up the obs stack (otel-collector, prometheus, loki, tempo, grafana).
docker compose --profile obs up -d
docker compose ps | grep -E 'grafana|prometheus|loki|otel'   # all should be healthy
```

Then open Grafana (default host port `3030`; if that port is taken, publish on
another, e.g. `-p 3031:3000`) and find the dashboards under the **Aikonos** folder.

### Why panels can still read "No data"

- **All panels** — broker not exporting (`AIKONOS_OTEL_ENDPOINT` empty) or the obs
  stack is down. Fix per the two steps above.
- **LLM Analytics** — `llm_*` counters are create-on-first-increment; a real
  chat turn through a configured provider must run before they exist.
- **PromQL must filter `exported_job="aikonos-broker"`, not `job`.** Prometheus
  scrapes the collector under `job="otel-collector"` and preserves the broker's
  own job as `exported_job`. Prometheus is on host port `9095` (not 9090).
- **Broker Actions → User activity row** reads broker AUDIT logs from Loki
  (metrics deliberately omit user identity for cardinality). The `$actor` regex
  variable filters it — `@` for humans, `spiffe` for service identities, `.+` for
  all. Empty if the obs `logs` pipeline (filelog → Loki) is not flowing.

---

## Incident response (top risks from threat model)

### L-02: Suspected prompt injection / exfil attempt
1. Kill switch the affected user: `python3 scripts/aikonosctl kill-switch user <user>`
2. Pull their recent tasks from the audit log
3. Check MinIO for any write_external events in the past hour
4. If exfil confirmed: escalate, preserve evidence, contact legal

### D-01: Runaway agent loop (cost DoS)
1. `python3 scripts/aikonosctl kill-switch user <user>` — stops new tasks
2. Check cost_consumed vs cost_budget in Postgres
3. Check NATS for queue depth on `<tenant>.outbound.<user>`
4. After remediation: review per-task cost budget defaults

### T-01: Unauthorized policy change
1. `git log --oneline policies/` — check recent commits
2. If unauthorized: revert commit, rebuild + recreate OPA: `docker compose up -d --force-recreate opa`
3. Check the Vault and broker logs for any policy-related calls
4. Review access to the Git repo signing keys

### E-08: Broker compromise suspected
1. Kill switch the affected tenant's users (`aikonosctl kill-switch user …` per user)
2. Re-mint the dev-CA so the broker's old leaf cert is no longer trusted:
   `bash scripts/compose-dev-ca.sh --force` then `docker compose up -d --force-recreate broker agent-gateway`
3. Pin a fresh `AIKONOS_CAPABILITY_ROOT_KEY` in `.env` and recreate the broker
4. Take the broker offline for forensics: `docker compose stop broker`
   (the container is preserved for inspection until `docker compose rm broker`)
