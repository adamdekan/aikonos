# Aikonos on Azure (single-VM dev deployment)

Runs the existing Docker Compose stack on one Azure VM, fronted by **Traefik**
(TLS via Let's Encrypt) and gated by **oauth2-proxy** against **Microsoft Entra ID**,
so only your tenant's users can reach it. Identity for the app itself swaps from
Keycloak to Entra by env alone (see `../../docs/12-entra-login.md`).

This is a **dev** target. Datastores stay containerized; Vault is dev-mode in-memory.
Production hardening (persistent Vault / Azure Key Vault, managed Postgres, Blob
audit) is out of scope — see the migration plan and `PROJECT-REVIEW-2026-06-10.md`.

## What it builds

| Layer | Choice |
|---|---|
| Host | one Ubuntu VM in `rg-tiakon-dev-weu-001` (West Europe) |
| Ingress | Traefik v3 (docker provider, ACME HTTP-01) on :80/:443 |
| Tenant gate | oauth2-proxy (generic OIDC) → single-tenant Entra app |
| App identity | Entra ID (single-tenant), app-native OIDC — broker validates the bearer |
| FQDN/TLS | Public IP DNS label `<label>.westeurope.cloudapp.azure.com` + Let's Encrypt |
| Datastores | containerized (Postgres/MinIO/Vault) on a Premium data disk |

```
Internet ─443─► Traefik ─forwardAuth─► oauth2-proxy ─OIDC─► Entra (single tenant)
                  │  (authed) │
                  ├──────────►│ webui:4200 ──/api,/agui,/audit──► agent-gateway:8080
                  └─ api.<fqdn> ─────────────► agent-gateway:8090 (API-key, no gate)
```

## Steps

### 1. Provision infra (run locally; needs `az login`)
```bash
cd deploy/azure
az login
./provision.sh                      # prints the FQDN + ssh command when done
```
Overridable via env: `VM_SIZE`, `DNS_LABEL`, `ADMIN_IP` (SSH allow-list), `DATA_DISK_GB`, …
Default size `Standard_D8s_v5` (32 GB); minimum `Standard_D4s_v5` (16 GB).

### 2. Entra app registrations (run locally; needs app-registration rights)
```bash
./entra-setup.sh <FQDN>             # the FQDN printed by provision.sh
```
Copy the printed env values. If your tenant enforces admin consent, grant it once
(the script prints the command).

### 3. On the VM: repo + env
```bash
ssh azureuser@<FQDN>
git clone <repo-url> aikonos && cd aikonos
cp deploy/compose/.env.azure.example .env
# edit .env: paste the entra-setup.sh values + rotate every *-CHANGE-ME secret
#   (openssl rand -hex 32 for signing/pepper/capability keys; rand -base64 32 done for cookie)
```
The `.env` already carries `COMPOSE_FILE=compose.yaml:deploy/compose/compose.azure.yaml`
and `COMPOSE_PROFILES=core`, so every plain `docker compose` (and the seed scripts)
apply the azure overlay automatically.

### 4. Bring up
```bash
bash scripts/compose-dev-ca.sh                # inter-service mTLS certs
docker compose build webui                    # bakes Entra VITE_* into the SPA
docker compose up -d                          # full stack + Traefik + oauth2-proxy
bash scripts/compose-vault-seed.sh            # Vault AppRole
bash scripts/compose-seed-openfga.sh          # OpenFGA store + model + tuples
docker compose up -d --force-recreate broker agent-gateway webui
```
Traefik obtains the Let's Encrypt cert on first HTTPS hit (port 80 must be reachable —
the NSG allows it).

### 5. Seed yourself as tenant-admin (by Entra `oid`)
Sign in once at `https://<FQDN>`, decode the access token, copy your `oid`, add
`user:<oid> admin tenant:<AIKONOS_BROKER_TENANT_ID>` to `scripts/compose-seed-openfga.sh`,
re-run it, then `docker compose up -d --force-recreate broker`. (See
`../../docs/12-entra-login.md:64`.)

## Verify
- `curl -I https://<FQDN>` → valid Let's Encrypt cert; `http://<FQDN>` → 308 to https.
- Browser → `https://<FQDN>` → redirected to Entra; a tenant user logs in and lands in
  the SPA; a non-tenant account is refused by Entra.
- Chat/files load (the SPA's PKCE bearer is accepted by broker/gateway).
- `task compose:verify` (auto-detects Entra from the issuer; provide
  `AIKONOS_ENTRA_SMOKE_TOKEN` for the real-bearer check).
- `https://api.<FQDN>/v1/agents/<id>/invoke` with a valid agent API key works WITHOUT a
  cookie (proves the ext router skips the edge gate).

## Notes / gotchas
- **webui OIDC is build-time.** Changing any `AIKONOS_WEBUI_OIDC_*` requires
  `docker compose build webui` (Vite bakes them into the SPA).
- **Keycloak still runs but is unused** on azure (kept only to satisfy the base
  `depends_on`; it is not routed and is not the IdP). Idle cost ~1-2 GB.
- **Vault is in-memory.** After any Vault restart, re-run
  `bash scripts/compose-vault-seed.sh && docker compose up -d broker`.
- **Traefik reads the docker socket (read-only).** Known attack surface; acceptable for
  dev, revisit (socket-proxy) before prod.
- **oauth2-proxy email claim.** Entra v2 tokens may omit `email`; the overlay keys the
  session on `preferred_username`. Adjust `OAUTH2_PROXY_OIDC_EMAIL_CLAIM` if your tenant
  differs.
