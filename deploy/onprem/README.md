# Aikonos on-prem test deployment (on-prem)

Deploys the Aikonos stack on a server that already runs Traefik with a company-issued
TLS certificate. Only company Entra ID users can sign in.

```
LAN → Traefik (traefik_net, company cert)
                 │
                 ├── aikonos.<host>            → webui:4200      ──/api,/agui,/audit──► agent-gateway:8080
                 ├── aikonos.<host>/docs        → docs-site:8080  (static, PathPrefix)
                 └── aikonos-api.<host>         → agent-gateway:8090 (API-key)
```

Auth: the SPA performs Entra OIDC auth-code + PKCE itself. Every API call carries an
Entra bearer token validated by the broker. No edge auth proxy — the static SPA files
are public by design; all sensitive surfaces require a valid bearer.

### Docs site routing

The end-user/administrator documentation site (`docs-site/`) is served at
`https://aikonos.example.com/docs` by its own `docs-site` compose service — same
host and TLS cert as the webui, no new FQDN or SAN needed. It's a separate Traefik router
(`aikonos-docs`, `deploy/compose/compose.onprem.yaml`) matching `Host(...) && PathPrefix(/docs)`;
that longer rule outranks the webui's bare `Host` rule, so `/docs` requests route to the docs
site instead of falling through to the SPA. The image is built with `DOCS_BASE=/docs` so the
site's own links resolve correctly under the subpath — no strip-prefix middleware needed, since
the built files already live at `html/docs/` inside the image.

---

## Prerequisites

| # | What | Check |
|---|------|-------|
| 1 | Docker Engine 24+, Compose **v2.24.0+** (the floor the `compose-config` CI job enforces for the `!reset` / `!override` merge tags `compose.onprem.yaml` uses on `ports:` to clear or replace the base file's host-port publishes; support is verified in CI against v2.29.7 and locally against v5.x) | `docker compose version` |
| 2 | External Traefik running, joined to `traefik_net` | `docker network ls \| grep traefik_net` |
| 3 | TLS cert covers `aikonos.example.com` AND `aikonos-api.example.com` | `openssl x509 -in /path/to/server.crt -noout -ext subjectAltName` |
| 4 | One Entra app registration (see below) | Entra portal |
| 5 | `openssl` and `fga` CLI on the host | `openssl version`, `fga version` |

### Entra app registration

You need **one** app registration in your tenant:

**Aikonos SPA:**
- Platform: Single-page application
- Redirect URI: `https://aikonos.example.com/auth/callback`
- Expose an API: add scope `access_as_user` (App ID URI: `api://<client-id>`)
- API permissions: `openid`, `profile`, `User.Read` (delegated)
- For tenant-wide OneDrive (optional, see below): also add delegated Graph
  `Files.ReadWrite` + `offline_access`, create a **client secret** (Certificates
  & secrets), then **Grant admin consent** for the tenant on the API permissions
  page — this app IS the OBO app, no separate registration.
- Note the **Application (client) ID** → used for `AIKONOS_WEBUI_OIDC_CLIENT` and `AIKONOS_OIDC_AUDIENCE`

> Tenant ID goes into `ENTRA_TENANT_ID`. The SPA performs PKCE login directly with
> Entra; no separate web-platform "edge gate" registration is needed.

### Tenant-wide OneDrive (optional)

Once the app registration above carries the Graph delegated permissions +
client secret + admin consent, connect the tenant once instead of asking every
user to link OneDrive individually:

1. Sign in as a tenant admin → **Admin → Settings → Microsoft 365**.
2. Enter the Entra tenant ID, the Application (client) ID, and the client
   secret from the app registration above.
3. **Test connection** — runs a real on-behalf-of (OBO) exchange with your own
   sign-in and reports exactly what's missing (consent not granted, bad
   secret, or a mismatched app registration).
4. **Enable**.

Every user then gets a default `Apps/Aikonos` OneDrive folder and a working-folder
control under the chat composer (local vs. OneDrive) — no per-user connect step.
The **audience constraint**: this only works when the M365 connection reuses the
same app registration users sign in with; a different `client_id` fails the
test with an audience-mismatch error. Dev/Keycloak stacks have no Entra app
registration, so they show no OneDrive option.

---

## One-time setup on the on-prem host

All steps run **on the server** unless noted.

### 1. Clone the repo into the apps directory

```bash
ssh <user>@on-prem
mkdir -p ~/apps
git clone ~/repos/aikonos.git ~/apps/aikonos
cd ~/apps/aikonos
```

### 2. Prepare .env

```bash
cp deploy/compose/.env.onprem.example .env
```

Edit `.env` — fill every `<...>` placeholder. Generate secrets:

```bash
# For POSTGRES_PASSWORD, MINIO_ROOT_PASSWORD, VAULT_DEV_ROOT_TOKEN_ID,
# AIKONOS_AUDIT_SIGNING_KEY, AIKONOS_CAPABILITY_ROOT_KEY, AIKONOS_API_KEY_PEPPER:
openssl rand -hex 32
```

Key values to fill from the Entra registration:

| .env variable | Source |
|---|---|
| `ENTRA_TENANT_ID` | Entra portal → Directory (tenant) ID |
| `AIKONOS_TENANT_ID` | Same as `ENTRA_TENANT_ID` |
| `AIKONOS_OIDC_AUDIENCE` | SPA app client ID |
| `AIKONOS_WEBUI_OIDC_CLIENT` | SPA app client ID |

Replace every `<ENTRA_TENANT_ID>` literal in the OIDC URL fields.

### 3. Mint inter-service mTLS certs

```bash
bash scripts/compose-dev-ca.sh
```

Idempotent — safe to re-run. Produces `deploy/compose/tls/` certs used for
broker ↔ agent-gateway mTLS.

### 4. Build and start the core stack

```bash
# Build webui first — OIDC vars are baked into the SPA at build time.
docker compose build webui

# Bring up the full core stack (postgres, vault, nats, openfga, broker, gateway, webui).
# Migrations run automatically before the broker starts.
docker compose up -d
```

### 5. Seed Vault, OpenFGA, and skill bundles

```bash
# Provision the broker's Vault AppRole (writes AIKONOS_VAULT_ROLE_ID + SECRET_ID into .env).
bash scripts/compose-vault-seed.sh

# Seed the OpenFGA store + model + dev tuples (writes AIKONOS_POLICY_OPENFGA_STORE_ID into .env).
bash scripts/compose-seed-openfga.sh

# Seed sample skill bundle (optional).
bash scripts/compose-seed-skill-bundles.sh

# Recreate broker and gateway to pick up the new .env values.
docker compose up -d --force-recreate broker agent-gateway webui
```

Or equivalently: `task compose:seed` (if `task` is installed).

### 6. Seed yourself as tenant admin

Sign in once at `https://aikonos.example.com`, then decode your access
token to get your Entra `oid`:

```bash
# Paste the access token between the dots and run:
echo '<header>.<payload>.<sig>' | cut -d. -f2 | base64 -d 2>/dev/null | python3 -m json.tool | grep '"oid"'
```

Add the `admin` relation for your `oid` in `scripts/compose-seed-openfga.sh`:

```
user:<your-oid> admin tenant:<AIKONOS_BROKER_TENANT_ID>
```

Re-run `bash scripts/compose-seed-openfga.sh && docker compose up -d --force-recreate broker`.

---

## Install the git deploy hook

Run **on the on-prem host** after step 1:

```bash
cp ~/apps/aikonos/scripts/git-deploy-hook.sh ~/repos/aikonos.git/hooks/post-receive
chmod +x ~/repos/aikonos.git/hooks/post-receive
```

After the hook is installed, every push to the bare repo automatically:
1. Verifies the pushed ref's signature (see **Signing setup** below)
2. Checks out the new code to `~/apps/aikonos`
3. Rebuilds every service image (`docker compose build` — webui always rebuilds since OIDC
   vars bake in at build time; other services rebuild cheaply via the Docker layer cache when
   unchanged)
4. Runs `docker compose up -d` (recreates changed services, leaves others alone)

**Add the remote on your dev machine (run locally):**

```bash
git remote add on-prem ssh://<user>@on-prem/~/repos/aikonos.git
```

**Deploy:**

```bash
git push on-prem main
```

---

## Signing setup — signed git deploys

The deploy hook (`scripts/git-deploy-hook.sh`) verifies the pushed ref's tip commit (or
tag, if a tag was pushed) against an **allowed-signers** file before checking out and
deploying. This is an **adoption path**: until the allowed-signers file exists on the
server, the hook prints a loud warning and deploys unverified — nothing breaks until an
operator opts in by creating the file.

### 1. Generate or choose an SSH signing key (each deployer, on their dev machine)

```bash
ssh-keygen -t ed25519 -C "you@example.com" -f ~/.ssh/id_ed25519_signing
```

An existing SSH key works too — a dedicated signing key just keeps deploy signing
separate from host auth.

### 2. Configure your git client to sign commits with it

```bash
git config gpg.format ssh
git config user.signingkey ~/.ssh/id_ed25519_signing.pub
git config commit.gpgsign true     # sign every commit; or pass -S per commit
```

Sign a specific commit or tag without the global flag:

```bash
git commit -S -m "..."
git tag -s v1.2.3 -m "..."
```

### 3. Build the allowed-signers file (on the on-prem host, one time)

One line per trusted signer: `<principal> <key-type> <base64-key>` — the principal is
whatever identity string the operator wants to record (an email, a username); it is not
independently checked, but keep it recognizable for audit purposes.

```bash
# On on-prem, as the deploy user:
mkdir -p /etc/aikonos
cat >> /etc/aikonos/allowed_signers <<'EOF'
alice@example.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI...
bob@example.com   ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI...
EOF
```

Collect each deployer's public key (`~/.ssh/id_ed25519_signing.pub` from step 1) and
append it. The file itself is **not tracked in this repo** — it lives only on the deploy
host, keyed to who is actually trusted to push there.

### 4. Hook environment configuration

| Variable | Default | Effect |
|---|---|---|
| `AIKONOS_DEPLOY_ALLOWED_SIGNERS` | `/etc/aikonos/allowed_signers` | Path to the allowed-signers file. |
| `AIKONOS_DEPLOY_REQUIRE_SIGNED` | unset | `false` skips verification entirely (logged). `true` fails closed — refuses to deploy if the allowed-signers file is missing (misconfiguration), instead of falling back to the warn-and-proceed adoption path. Unset (default) behaves adoption-style: verify if the file exists, warn-and-proceed if it doesn't. |

Set these in the hook's process environment on the on-prem host (e.g. exported in the git user's
shell profile, or prefixed on the `post-receive` script itself) if you need non-default
values.

### 5. Adoption path

1. **Before** any allowed-signers file exists: every push deploys with a loud
   `WARNING: ... signature verification is NOT enforced` — nobody is blocked.
2. Create `/etc/aikonos/allowed_signers` with your team's keys (step 3) whenever ready —
   from that point every push is verified; an unsigned or unrecognized-signer push is
   refused with instructions to sign.
3. Optionally set `AIKONOS_DEPLOY_REQUIRE_SIGNED=true` to make a missing allowed-signers
   file a hard failure instead of a silent-warn fallback (belt-and-suspenders once
   signing is fully rolled out).

An unsigned or unverifiable push is refused with:

```
ERROR: [aikonos-deploy] Signature verification FAILED for <sha> (refs/heads/main) — deploy refused.
...
  To sign your commits with an SSH key:
    git config gpg.format ssh
    git config user.signingkey ~/.ssh/id_ed25519.pub
    git config commit.gpgsign true      # or pass -S per commit
    git commit -S -m '...'              # or: git tag -s ...
```

---

## Verify

```bash
# Functional smoke test (auto-detects Entra from the issuer).
# Note: in the onprem overlay, webui and gateway are Traefik-only (no host ports).
# The verify script checks localhost:4200 and localhost:8080 which are internal-only
# by design — expect those 2 checks to fail. The important checks (broker OpenFGA
# enforcement, Vault capability key, audit sink) run via broker's host port 9090-9091
# and will pass if the stack is healthy.
bash scripts/compose-verify.sh

# Check all services are healthy.
docker compose ps

# Confirm Traefik is routing to the webui.
curl -I https://aikonos.example.com    # → 200 (static SPA; OIDC handled in-browser)

# Confirm the docs site routes separately from the webui.
curl -I https://aikonos.example.com/docs/   # → 200 (static docs site)

# Confirm API surface (API-key auth, no browser gate).
curl -I https://aikonos-api.example.com/v1/agents/  # → 401 (no key)
```

Browser → `https://aikonos.example.com` → SPA loads → SPA redirects to Entra sign-in
→ company user logs in → SPA receives auth code → exchanges for bearer → lands in Aikonos.

---

## Observability

The action/audit trail is **always-on** and independent of the obs profile — view it in
the webui (admin → Audit, live SSE) or verify chain integrity with `python3 scripts/aikonosctl
audit verify`. The obs profile below adds the second half: broker metrics, traces, and
container logs surfaced in Grafana.

### Bring up the obs stack

The obs services (OTel collector + Grafana LGTM: Loki, Tempo, Prometheus) live in the base
`compose.yaml` under `profiles: [obs]`, so they start on the on-prem overlay with no extra
config.

```bash
# 1. Point the broker at the collector. Host:port, NO scheme — a URL breaks the exporter.
#    Empty/unset → broker exports nothing (silent no-op, by design).
echo 'AIKONOS_OTEL_ENDPOINT=otel-collector:4317' >> .env

# 2. Start the obs stack.
docker compose --profile obs up -d

# 3. Recreate the broker so it re-reads AIKONOS_OTEL_ENDPOINT. Env bakes in at container
#    create — without this the obs stack runs but receives no broker telemetry.
docker compose up -d --force-recreate broker
```

Verify the broker→collector→prometheus path is flowing:

```bash
# Broker initialized its exporter (expect "OpenTelemetry metrics initialized").
docker compose logs --since=2m broker | grep -i 'otel\|metrics'

# Metrics are landing in the collector's Prometheus endpoint.
docker compose exec otel-collector wget -qO- localhost:8889/metrics | grep -i aikonos | head
```

### Reaching Grafana — SSH tunnel only

Grafana is **not exposed** on the on-prem host: no Traefik route, no published host port reachable from
the network. This is deliberate — the obs console is operator-only and stays off the company
network. Reach it by forwarding the container's host-bound port over SSH from your laptop:

```bash
# From your workstation. 3030 is Grafana's host port inside on-prem (compose.yaml).
ssh -L 3030:localhost:3030 -N <user>@example.com
# → open http://localhost:3030  (dashboards: Tempo=traces, Loki=logs, Prometheus=metrics)
```

Do not add a Traefik route or `oauth2-proxy` rule for Grafana to "make it easier" — tunnel-only
access is the security posture, not an oversight.

### Reaching OpenFGA — loopback only

OpenFGA's host port is published on `127.0.0.1:8082`, not on every interface. Its HTTP API is
**unauthenticated** (no `OPENFGA_AUTHN_METHOD` is set), and its write endpoint is what grants group
membership and skill access — so anyone who can reach that port can rewrite the tuples `SubmitPlan`
and `InvokeTool` gate on, bypassing the ReBAC check without touching the broker, OIDC, or Biscuit.
Loopback keeps the seed script and admin `curl`s working from the deploy host while removing the
remote path.

Every documented workflow already targets loopback (`scripts/compose-seed-openfga.sh` defaults to
`FGA_API_URL=http://127.0.0.1:8082`), so this changes nothing you run over SSH on the host. From a
workstation, tunnel it the same way as Grafana:

```bash
ssh -L 8082:localhost:8082 -N <user>@example.com
```

NATS is not published at all on-prem. It runs unauthenticated and carries the audit event stream, so
a host publish would let any reachable host subscribe to `aikonos.audit.>` and read every audit
event, or publish forged events into the subject the gateway's audit consumer trusts. Broker→NATS
and gateway→NATS both ride the `mesh` network and need no host port.

Both `ports:` blocks live in `deploy/compose/compose.onprem.yaml`. A plain `ports:` list there
**merges** with the base file's rather than replacing it, which silently leaves the wide binding in
place — hence `!override` / `!reset`. Verify against the rendered config, never the source:

```bash
docker compose -f compose.yaml -f deploy/compose/compose.onprem.yaml config --format json \
  | jq '.services.openfga.ports, .services.nats.ports'
```

### Shipping to a SIEM

The obs profile's archive sink also supports forwarding to an enterprise SIEM instead of (or
alongside) the local Grafana dashboards: set `AIKONOS_OBS_ARCHIVE=siem` in `.env`.
Splunk/Datadog/Elastic all ingest OTLP natively, so this uses one generic `otlphttp` exporter —
set `AIKONOS_SIEM_OTLP_ENDPOINT` and, if the endpoint requires auth,
`AIKONOS_SIEM_OTLP_AUTH_HEADER` (full header value, e.g. `Bearer <token>`). See
[deploy/compose/README.md#shipping-to-a-siem](../compose/README.md#shipping-to-a-siem) for the
full archive-sink reference (`file`/`s3`/`azure`/`siem`).

---

## Rolling deploys

After the hook is in place, deploy by pushing:

```bash
git push on-prem main
```

The hook rebuilds every service image (`docker compose build`, since commit `316fc7f`) — not
just webui — so a code push to broker/gateway/docs-site never ships stale binaries. `up -d`
still only recreates a container whose image or config actually changed, so unaffected
services restart cheaply.

**After changing `.env` variables on the server**, recreate the affected service:

```bash
cd ~/apps/aikonos
docker compose up -d --force-recreate <service>
```

**After any `AIKONOS_WEBUI_OIDC_*` change**, rebuild the webui:

```bash
cd ~/apps/aikonos
docker compose build webui && docker compose up -d --force-recreate webui
```

---

## Pinned image digests (supply chain)

`deploy/compose/compose.digests.yaml` (checked in, see `deploy/compose/README.md` "Supply
chain") pins every pulled base image (postgres, vault, nats, keycloak, minio, opa, openfga,
otel-collector, grafana/loki/tempo/prometheus, ...) to a `@sha256:...` digest. On on-prem, add it
to `.env`'s `COMPOSE_FILE` alongside the onprem overlay:

```bash
# .env
COMPOSE_FILE=compose.yaml:deploy/compose/compose.onprem.yaml:deploy/compose/compose.digests.yaml
```

Refresh cadence: every dependency bump. Regenerate from a machine with Docker + network access
(not necessarily on-prem itself) via `bash scripts/pin-image-digests.sh`, commit the result, then
redeploy. Locally built images (broker, agent-gateway, webui) are pinned by the deployed commit,
not by this file.

---

## Maintenance

**Vault restart (wiped — in-memory dev mode):**

```bash
cd ~/apps/aikonos
bash scripts/compose-vault-seed.sh
docker compose up -d --force-recreate broker
```

**Re-apply migrations (after a migration drift):**

```bash
cd ~/apps/aikonos
docker compose up migrate
```

**Backup durable volumes:**

```bash
cd ~/apps/aikonos
bash scripts/compose-backup.sh backup
```

**Attaching a self-hosted MCP server (or any external container):**

The broker reaches an MCP server by dialing it directly over Docker networking, so
the MCP container must (1) share a network with the broker and (2) be reachable past
the broker's SSRF guard. Three things are required — all three, or the agent silently
sees no MCP tools:

1. **Join the shared network.** Aikonos's mesh network is named `aikonos_mesh` (stable,
   set in `compose.yaml`). In the MCP project's compose file, declare it external and
   attach the service to it alongside its own default network:

   ```yaml
   networks:
     aikonos_mesh:
       external: true
   services:
     mcp:
       networks: [default, aikonos_mesh]   # default → reaches its own DB; aikonos_mesh → reachable by broker
   ```

   Then `docker compose up -d` in that project. Verify:
   `docker network inspect aikonos_mesh --format '{{range .Containers}}{{.Name}} {{println}}{{end}}'`
   should list the MCP container.

2. **Allow private targets on the broker.** The mesh subnet is RFC1918 (172.x), which
   the broker's SSRF guard rejects by default. Set in `~/apps/aikonos/.env`:

   ```
   AIKONOS_TOOLPROXY_MCP_ALLOW_PRIVATE=true
   ```

   then `docker compose up -d --force-recreate broker`. (This disables the SSRF guard
   for **all** broker MCP dials — acceptable on-prem where MCP servers live only on
   internal Docker networks; do not enable on an internet-exposed tenant.)

3. **Register by DNS name, not IP.** In Aikonos (Admin → MCP / connector), set the
   server URL to the container DNS name — `http://eu-regs-mcp:3000` — never a raw
   `172.x` IP. Container IPs change on every restart; the DNS name is stable on the
   shared network.

#### Worked example: attach the bundled Aikonos docs MCP server

The repo ships a ready-made docs MCP server (`docs-mcp/`) that lets agents answer
questions about Aikonos's own documentation. To enable it:

1. **Start the service** (run in `~/apps/aikonos`):

   ```bash
   docker compose --profile docs-mcp up -d --build
   ```

   This starts `aikonos-docs-mcp` on port `8060`, already joined to `aikonos_mesh`.
   No separate network declaration is needed — it is part of the base `compose.yaml`.

2. **Allow private targets** (already required per step 2 above — confirm `.env` has):

   ```
   AIKONOS_TOOLPROXY_MCP_ALLOW_PRIVATE=true
   ```

   If you just added it: `docker compose up -d --force-recreate broker`.

3. **Register in the webui** (Admin → MCP connections → Add):

   | Field | Value |
   |-------|-------|
   | Name | `aikonos-docs` |
   | URL | `http://aikonos-docs-mcp:8060/mcp` |
   | Transport | `streamable_http` |
   | Auth | none |

   The broker dials the container by DNS name over the mesh — no raw IP, no extra
   network join required. Assign the server to an agent to make its tools available.

#### Attaching the Grafana MCP server

Gives agents read-only access to the company Grafana at
`https://dashboards.example.com`. Upstream `grafana/mcp-grafana`, wired as
the `mcp-grafana` compose profile in the base `compose.yaml` — already on `aikonos_mesh`,
so no external-network declaration is needed.

1. **Build the CA chain.** Grafana's leaf is typically issued by an internal
   issuing CA that sends no intermediate, so the container needs both that CA
   and the root it chains to. One cert alone is not enough — verification still
   fails. Substitute your own PKI's distribution point and cert names below.

   ```bash
   cd ~/apps/aikonos
   base=http://crl.example.com
   curl -s "$base/Example%20Issuing%20CA.crt" | openssl x509 -inform DER  >  deploy/compose/tls/grafana-ca.crt
   curl -s "$base/Example%20Root%20CA.crt"    | openssl x509 -inform DER  >> deploy/compose/tls/grafana-ca.crt

   # Prove the chain verifies before starting anything.
   curl -s --cacert deploy/compose/tls/grafana-ca.crt \
     https://dashboards.example.com/api/health    # → {"database":"ok",...}
   ```

   `deploy/compose/tls/` is gitignored, so the file stays on the host and survives
   deploys. If the company PKI is rotated, re-run this step.

2. **Mint a Grafana service account token.** In Grafana → Administration → Users and
   access → Service accounts → Add service account, role **Viewer** → Add service
   account token. Put it in `~/apps/aikonos/.env`:

   ```
   AIKONOS_GRAFANA_MCP_URL=https://dashboards.example.com
   AIKONOS_GRAFANA_MCP_TOKEN=<the token>
   # Only if you put the chain somewhere other than the default path above:
   # AIKONOS_GRAFANA_MCP_CA_FILE=./deploy/compose/tls/grafana-ca.crt
   ```

   **Viewer is load-bearing here, not a default.** The server runs *without*
   `--disable-write` (see the next subsection for why), so mcp-grafana does expose
   write tools — `update_dashboard`, `patch_dashboard`, annotation writes,
   `create_incident`, `alerting_manage_rules`, snapshot create/delete — and the
   generic `grafana_api_request` tool will happily send a POST or DELETE anywhere in
   the API. Every one of those is rejected by Grafana itself because the token is
   Viewer. That role *is* the boundary. Mint this account at Editor or Admin and an
   agent gains a real write path to production dashboards and alert rules with no
   second gate behind it.

   Nor is there necessarily a human in the loop. Aikonos classes an MCP tool with no
   read-only annotation as `WRITE_EXTERNAL` and routes it to approval, but an agent
   whose `approval_mode` is `auto` pre-authorizes every tool of every attached MCP
   server regardless of effect class (`resolveAutoApproveAllowlist`). On a stack
   where the Grafana agents run `auto`, Viewer is the only thing standing between
   the model and a mutation.

   **Reading a dashboard's data, not its definition.** A Grafana dashboard is a JSON
   definition; the rows live in the datasource behind each panel, so `get_dashboard_*`
   answers with the definition by design. `run_panel_query` executes a panel's own
   query and returns rows — upstream leaves its `runpanelquery` category out of the
   default tool set, so `compose.yaml` passes an explicit `--enabled-tools` list to
   add it. That flag replaces the default list rather than extending it, so re-sync
   it against `cmd/mcp-grafana/main.go` when bumping the image tag.

   `run_panel_query` covers Prometheus, Loki, ClickHouse, CloudWatch, InfluxDB and
   BigQuery panels only; anything else returns "not supported by run_panel_query".
   Elasticsearch/OpenSearch, Quickwit, Graphite, Athena and Snowflake have their own
   query tools, each in a category that must likewise be appended to
   `--enabled-tools`. Panels on a SQL datasource (Postgres/MySQL/MSSQL) or Infinity
   have no tool at all.

   **Why `--disable-write` is not set.** The dashboards on this deployment are
   postgres-backed, which is the case with no tool — so the only route to their rows
   is Grafana's own `POST /api/ds/query` through `grafana_api_request`.
   `--disable-write` does not narrow that tool's endpoints; it substitutes a GET-only
   implementation (`tools/api.go` `AddAPITools`), which cannot issue the POST. With
   the flag set, no agent can ever read a number off these dashboards — only the SQL
   that would produce it. The flag was dropped as a deliberate trade against the
   Viewer role above. Reinstate it and panel data goes away again; the fix is not to
   raise the token's role.

3. **Allow private targets** (same requirement as any MCP server — see step 2 of the
   section above). Confirm `.env` has `AIKONOS_TOOLPROXY_MCP_ALLOW_PRIVATE=true`, then
   `docker compose up -d --force-recreate broker`.

4. **Start the profile:**

   ```bash
   docker compose --profile mcp-grafana up -d
   docker compose logs aikonos-mcp-grafana   # → "Starting Grafana MCP server using StreamableHTTP transport"
   ```

5. **Register in the webui** (Admin → MCP connections → Add):

   | Field | Value |
   |-------|-------|
   | Name | `grafana` |
   | URL | `http://aikonos-mcp-grafana:8000/mcp` |
   | Transport | `streamable_http` |
   | Auth | none |

   Auth is `none` on purpose: the Grafana credential lives in the MCP container's
   environment, not in the broker→MCP hop, which stays on the internal mesh.

   The URL must be exactly this DNS name and port. `mcp-grafana` validates the `Host`
   header against an allowlist (`--allowed-hosts=aikonos-mcp-grafana:8000` in
   `compose.yaml`); registering a raw `172.x` IP is answered `403 forbidden: host not
   allowed`, on top of the container-IP instability reason that applies to every MCP
   server.

   Then assign the server to an agent to expose its tools.

---

## Gotchas

| Symptom | Cause | Fix |
|---|---|---|
| Browser redirects to Entra but gets "AADSTS50011: reply URL mismatch" | Redirect URI in the SPA registration doesn't match | Register `https://aikonos.example.com/auth/callback` in the SPA app registration |
| Traefik returns 404 for `aikonos.example.com` | `traefik_net` network not joined / Traefik not seeing container labels | Confirm `docker network ls \| grep traefik_net`; check `docker compose logs` for network errors |
| TLS warning / cert mismatch | Cert SANs don't cover the subdomain | Run `openssl x509 -in /path/to/server.crt -noout -ext subjectAltName` |
| "OpenFGA is disabled — dev allow-all mode" | OpenFGA not seeded | `bash scripts/compose-seed-openfga.sh && docker compose up -d --force-recreate broker` |
| `/admin/roles` Users tab shows `alice@aikonos.com`, `bob@aikonos.com`, `admin@aikonos.com` | `compose-seed-openfga.sh` was run with `AIKONOS_SEED_DEMO_TUPLES=1` — it seeds Keycloak demo accounts into OpenFGA (demo/local-dev only) | Delete the stale tuples via the FGA API: `curl -X POST http://localhost:8082/stores/$STORE_ID/write -H 'Content-Type: application/json' -d '{"deletes":{"tuple_keys":[{"user":"user:alice@aikonos.com","relation":"member","object":"tenant:aikonos-dev"},{"user":"user:bob@aikonos.com","relation":"member","object":"tenant:aikonos-dev"},{"user":"user:admin@aikonos.com","relation":"admin","object":"tenant:aikonos-dev"}]}}'`. The script skips demo tuples by default (fail closed) — on on-prem/production, leave `AIKONOS_SEED_DEMO_TUPLES` unset. |
| `Vault key resolution failed — falling back to EPHEMERAL key` | Vault restarted (in-memory) | `bash scripts/compose-vault-seed.sh && docker compose up -d broker` |
| `claims invalid: iss` | AIKONOS_OIDC_ISSUER has literal `<ENTRA_TENANT_ID>` | Replace placeholder with actual tenant GUID in `.env` |
| **401 on every API call; gateway logs `no applicable key found in the JSON Web Key Set`** | **Entra signs ID tokens with an app-specific key only published when `?appid=<client_id>` is on the JWKS URL** | **Append `?appid=<WEBUI_APP_CLIENT_ID>` to `AIKONOS_OIDC_JWKS_URL`, then `docker compose up -d --force-recreate broker agent-gateway`** |
| Webui blank / "cannot connect to gateway" | Webui built with wrong OIDC authority | `docker compose build webui && docker compose up -d --force-recreate webui` |
| Assigned MCP server invisible to its agent; agent says it has no MCP tools | MCP container not on `aikonos_mesh`, registered by a raw `172.x` IP, and/or broker SSRF guard blocking the private dial (gateway log: `web.fetch: loopback/private targets are not permitted`) | See **Maintenance → Attaching a self-hosted MCP server**: join `aikonos_mesh`, set `AIKONOS_TOOLPROXY_MCP_ALLOW_PRIVATE=true`, register by container DNS name |
| Grafana MCP tools all fail with `x509: certificate signed by unknown authority` | `deploy/compose/tls/grafana-ca.crt` is missing, holds only the issuing CA without the root, or Docker created a *directory* there because the file did not exist at `up` time | Rebuild the chain (both certs, concatenated) per **Attaching the Grafana MCP server** step 1, `rm -rf` the stray directory if one was created, then `docker compose up -d --force-recreate aikonos-mcp-grafana` |
| Grafana MCP server registered but every call returns `403 forbidden: host not allowed` | Registered by raw IP or a hostname other than `aikonos-mcp-grafana:8000`, which `--allowed-hosts` rejects | Re-register the URL as exactly `http://aikonos-mcp-grafana:8000/mcp` |
| Grafana MCP tools return `401 Invalid API key` | Service account token wrong, expired, or revoked; `.env` edited without recreating the container | Re-mint the token, update `AIKONOS_GRAFANA_MCP_TOKEN`, `docker compose up -d --force-recreate aikonos-mcp-grafana` |
| `column "X" does not exist` | Migration drift | `docker compose up migrate` |

### Seeding the first admin user

After a fresh deploy the OpenFGA store has no admins, so every user lands with
deny-by-default access. To grant the first user full admin:

1. Have the user sign in once (this is enough — no API call needs to succeed).
2. Get their Entra object ID (`oid`). Either decode their ID token, or read it
   from the broker/gateway logs after their first request. From the browser
   DevTools console:
   ```js
   const key = Object.keys(sessionStorage).find(k => k.startsWith('oidc.user:'));
   const idToken = JSON.parse(sessionStorage.getItem(key)).id_token;
   const claims = JSON.parse(atob(idToken.split('.')[1].replace(/-/g,'+').replace(/_/g,'/')));
   console.log('oid:', claims.oid, 'email:', claims.email);
   ```
3. Write the admin tuple to OpenFGA (replace the OID and store ID):
   ```bash
   curl -s -X POST http://localhost:8082/stores/<STORE_ID>/write \
     -H 'Content-Type: application/json' \
     -d '{"writes":{"tuple_keys":[
       {"user":"user:<OID>","relation":"admin","object":"tenant:<AIKONOS_BROKER_TENANT_ID>"}
     ]}}'
   ```
   An empty `{}` response means success. The user refreshes the page and now has
   full admin access.
