# Microsoft Entra ID login (F2 Phase 2)

> How to point Aikonos's OIDC at Microsoft Entra ID for real end-user login, in a
> way that **migrates cleanly from a dev/personal tenant to an enterprise tenant**
> later. The auth code is generic OIDC/JWKS — the swap is **env + a re-seed of
> identity tuples**, no code change.

## Why it migrates cleanly

| Concern | Use | Why |
|---|---|---|
| **Authz principal** (FGA `user:<x>`, task owner, per-user storage key) | Entra **`oid`** (the verified `Subject`) | Immutable; stable **across app registrations within a tenant**, and **non-reassignable** (a deleted user's `oid` is never reused — an email is). `sub` is pairwise *per app* — re-registering churns every tuple. The broker binds authz to `Subject` (`PrincipalID()`). `oid` needs the `profile` scope (the webui requests it). |
| **Audit / display actor** (`actor_user_id`) | Entra **`preferred_username`** (email) | Decoupled from the authz principal on purpose: decisions key on the strong immutable id, logs stay human-readable. Set via `ActorID()` (email-first); the gRPC audit interceptor is unchanged. |
| **RLS tenant** (`tenant_id` column) | Entra **`tid`** (a GUID) | Set `AIKONOS_TENANT_ID=<tid>` so seeded data lands under it. A different enterprise tenant = a different `tid` = its own clean dataset (dev data ≠ prod data — correct). |
| **Admin-gate tenant** (FGA `tenant:<name>`) | a stable slug (`AIKONOS_BROKER_TENANT_ID`) | The admin gate keys on this **name**, decoupled from the token. Keep it constant → admin-role tuples survive the swap. |
| **App model** | **single-tenant** app (now and in enterprise) | Single-tenant issuer is fixed `…/<tid>/v2.0` (exact-match validation works). Multi-tenant issuers vary per home tenant and would need `{tenantid}` template handling (not implemented). |

Cross-tenant, `oid`/`tid` necessarily differ (different directory), so user/group/role
tuples re-seed on migration — unavoidable with any IdP. Everything else is env.

## The config knobs (already in the code)

| Knob | Keycloak default | Entra |
|---|---|---|
| `AIKONOS_OIDC_ISSUER` | `http://localhost:18080/realms/aikonos` | `https://login.microsoftonline.com/<TENANT_ID>/v2.0` |
| `AIKONOS_OIDC_JWKS_URL` | container Keycloak certs URL | `.../discovery/v2.0/keys` — append `?appid=<WEBUI_APP_CLIENT_ID>` **only** for the login-only (ID-token) model |
| `AIKONOS_OIDC_AUDIENCE` | `aikonos-broker` | `<WEBUI_APP_CLIENT_ID>` (v2 access-token `aud` = the API's client id) |
| `AIKONOS_OIDC_SUBJECT_CLAIM` | `preferred_username` (= email; **not** `sub` — Keycloak's `sub` is an opaque realm UUID the dev-seed isn't keyed on) | `oid` |
| `AIKONOS_OIDC_TENANT_CLAIM` | `tenant_id` | `tid` |
| `AIKONOS_TENANT_ID` | `1111…` | `<TENANT_ID>` (your Entra tid) |
| `AIKONOS_BROKER_TENANT_ID` | `aikonos-dev` | **keep stable** |
| webui `VITE_OIDC_AUTHORITY` | Keycloak realm URL | `https://login.microsoftonline.com/<TENANT_ID>/v2.0` |
| webui `VITE_OIDC_CLIENT_ID` | `aikonos-webui` | `<WEBUI_APP_CLIENT_ID>` |
| webui `VITE_OIDC_SCOPE` | `openid profile` (Keycloak stamps `aud` via a mapper) | login-only: `openid profile email` · OneDrive/OBO: `openid profile <WEBUI_APP_CLIENT_ID>/.default` (GUID form — **not** `api://…`, see AADSTS90009 below) |
| webui `VITE_OIDC_TOKEN` | `access` | login-only: `id` · OneDrive/OBO: `access` (see "Two Entra models") |

`SUBJECT_CLAIM`/`TENANT_CLAIM` apply to **both** the broker validator and the gateway
bearer check; the broker validates the token a second time and is the authority.

No split-horizon issuer (unlike Keycloak's `localhost:18080` vs `keycloak:8080`):
Entra is one public URL reachable identically by the browser and the containers.

## Two Entra models: login-only vs OneDrive/OBO

The webui sends **one** bearer to the broker; which token it sends is chosen at
build time by `AIKONOS_WEBUI_OIDC_TOKEN`. Pick the model by whether the tenant
wants OneDrive — a login-only customer needs **no** exposed API and **no**
`access_as_user`.

| | Login only | Login + tenant OneDrive (OBO) |
|---|---|---|
| Graph delegated perms | `User.Read` | + `Files.ReadWrite`, `offline_access` |
| Expose an API / App ID URI | not needed | not needed for login (GUID-form `.default` is a self-token); the OBO exchange keys on `aud == client-id`, not the App ID URI |
| Client secret | none | required for OBO (entered at runtime, Admin → Settings → Microsoft 365) |
| `AIKONOS_WEBUI_OIDC_TOKEN` | `id` | `access` |
| `VITE_OIDC_SCOPE` | `openid profile email` | `openid profile <client-id>/.default` (GUID form, **not** `api://<client-id>/.default`) |
| `AIKONOS_OIDC_JWKS_URL` | `…/keys?appid=<client-id>` | `…/keys` (plain) |
| `AIKONOS_OIDC_AUDIENCE` | `<client-id>` | `<client-id>` |

Why it splits: the broker/gateway validate `aud == <client-id>`. An Entra
**access token** carries that `aud` when the SPA requests the app's own
`.default` scope. Because the SPA is both the client and the resource (it
requests a token for **itself**), Entra requires the **GUID-form** identifier
`<client-id>/.default`; the `api://<client-id>/.default` form fails at sign-in
with **AADSTS90009** ("Application … is requesting a token for itself. This
scenario is supported only if resource is specified using the GUID based App
Identifier"). No "Expose an API" / Application ID URI and no named
(`access_as_user`) scope are needed. A login-only app can instead send the **ID
token** (whose `aud` is the client id by construction, scope `openid profile
email`), signed with the app-specific key the JWKS endpoint publishes only under
`?appid=` — but an ID token cannot be an OBO assertion, so OneDrive/OBO must use
the access-token (GUID `.default`) column.

## Entra portal setup

1. **App registration** (single-tenant): "Accounts in this organizational directory only".
   Add a **SPA** platform with redirect URI `http://localhost:4200/auth/callback`.
2. **API permissions**: Microsoft Graph → Delegated → `User.Read` (+`openid`,
   `profile`) → *Grant admin consent*. This alone is enough for the **login-only**
   model (`AIKONOS_WEBUI_OIDC_TOKEN=id`, scope `openid profile email`, JWKS with
   `?appid=`).
3. **For tenant OneDrive (OBO) only** — add Graph delegated `Files.ReadWrite` +
   `offline_access`, create a **client secret** (Certificates & secrets), and
   *Grant admin consent*. Keep `AIKONOS_WEBUI_OIDC_TOKEN=access`, scope
   `openid profile <client-id>/.default` (GUID form — **no** "Expose an API" /
   Application ID URI is needed; the self-token's `aud` is the client id, which is
   all the OBO exchange checks), and a plain JWKS URL. This same app is the OBO app
   (see "Tenant-wide OneDrive").
4. Record the **Application (client) ID** and the **Directory (tenant) ID**.

`oid`/`tid`/`preferred_username` are emitted with the `profile` scope (already requested);
no custom claim mapping required.

## Test (dev/personal tenant)

1. Put the Entra values from the table into `.env` (and the webui build env). For a
   local/personal-tenant test the commented block in `deploy/compose/.env.local.example`
   lists them; for the Azure VM deployment use `deploy/compose/.env.azure.example`
   (Entra is the active config there) — see `deploy/azure/README.md`.
2. Rebuild + recreate webui (build-time `VITE_*`) and recreate broker + gateway:
   `docker compose build webui && docker compose up -d --force-recreate broker agent-gateway webui`.
3. Sign in at http://localhost:4200 → decode the access token once → copy your `oid`.
4. Seed yourself as tenant-admin: write `user:<oid> admin tenant:<AIKONOS_BROKER_TENANT_ID>`
   (extend `scripts/compose-seed-openfga.sh`'s admin tuple to your `oid`), then recreate the broker.

## Verify

`scripts/compose-verify.sh` (`task compose:verify`) **auto-detects the provider** from
`AIKONOS_OIDC_ISSUER`: a `login.microsoftonline.com` issuer switches the real-bearer check to the
Entra path. App-only (client-credentials) tokens lack user claims and are rejected by the gateway, so
the check needs a **real user token** — provide one of:

- `AIKONOS_ENTRA_SMOKE_TOKEN` — an access token copied from the browser after sign-in (and optionally
  `AIKONOS_ENTRA_SMOKE_TOKEN_DENY` for a non-admin, to exercise the 403 path); or
- ROPC mint for a **cloud-only, no-MFA** smoke user: `AIKONOS_ENTRA_SMOKE_TENANT_ID`,
  `_CLIENT_ID`, `_USERNAME`, `_PASSWORD` (+ `_CLIENT_SECRET` for a confidential client, `_SCOPE` to
  override the default `api://<aud>/access_as_user`, and `_USERNAME_DENY`/`_PASSWORD_DENY` for the deny
  case). ROPC is discouraged in production — use a dedicated test account only.

When no creds are set the Entra bearer check **skips** (the run still passes). With a token the check
asserts:

- **200** → bearer accepted by OIDC validation *and* the `oid` is authorized as admin.
- **403** → bearer accepted (issuer/audience/JWKS/`oid` mapping all correct); the `oid` just isn't an
  admin yet — seed `user:<oid> admin tenant:<AIKONOS_BROKER_TENANT_ID>`.
- **401** → bearer **rejected at validation** — check `AIKONOS_OIDC_ISSUER`/`_AUDIENCE`/`_JWKS_URL` and
  `SUBJECT_CLAIM=oid` / `TENANT_CLAIM=tid`.

Manual checks:
- A non-admin `oid` is **denied** on `/admin/*` (403).
- Broker log shows no `claims invalid: iss` / `aud` rejections.
- (FU2) an invalid bearer now emits an `auth.token.rejected` audit event.

## Tenant-wide OneDrive (OBO)

The same login app registration doubles as the OBO app for tenant-wide OneDrive
access — extend it with delegated Graph `Files.ReadWrite` + `offline_access`,
add a client secret, and grant admin consent, then configure it once in Admin →
Settings → Microsoft 365 (Test connection validates the whole chain). Full steps:
`deploy/onprem/README.md`'s "Tenant-wide OneDrive" subsection.

**Audience constraint**: the M365 connection must reuse this same app
registration — a different `client_id` means the OBO exchange fails with
AADSTS500011. Dev/Keycloak stacks have no Entra app registration, so they show
no OneDrive option.

## Migrate to the enterprise tenant (later)

1. Register the app in the enterprise tenant (single-tenant), expose the API, grant consent.
2. Re-point `AIKONOS_OIDC_*`, `AIKONOS_TENANT_ID`, and the webui `VITE_OIDC_*` at the
   enterprise tenant/app. **Keep `AIKONOS_BROKER_TENANT_ID`.**
3. Re-seed FGA tuples (group membership, tenant-admin, skill access) for the real
   users' `oid`s.
4. No code change, no rebuild beyond the webui's build-time `VITE_*` values.
