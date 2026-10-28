# Another OIDC identity provider

> How to point aikonOS at an OpenID Connect provider other than Microsoft Entra
> ID or the bundled Keycloak. The broker and the gateway validate tokens with
> generic OIDC and JWKS code, so a new IdP is configuration, not code. Entra has
> its own guide: [12-entra-login.md](12-entra-login.md).

A deployment trusts **one** issuer at a time. To move users from one IdP to
another, follow "Switch IdPs" below; aikonOS does not accept two issuers side by
side.

## What the IdP must issue

The web UI sends one bearer token (access or ID token) to the gateway, and the
gateway forwards it to the broker. Both check it. The token needs:

| Requirement | Claim | Why |
|---|---|---|
| Issuer equal to `AIKONOS_OIDC_ISSUER`, character for character | `iss` | Exact match. A trailing slash counts. Multi-tenant issuer templates are not supported. |
| Audience that names aikonOS | `aud` contains `AIKONOS_OIDC_AUDIENCE` | Required whenever an issuer is set; the broker refuses to start without it, because no audience check accepts tokens minted for any other client of the IdP. |
| Signature with RS256, RS384, RS512 or ES256 | header `alg`, `kid` | Other algorithms are rejected. The `kid` must be in the JWKS. |
| A stable, never-reused user id | `AIKONOS_OIDC_SUBJECT_CLAIM` (default `sub`) | This is the FGA principal, the task owner and the per-user storage key. Use an immutable id, not an email that can be reassigned. It must not start with `svc-` (reserved for agent service accounts). |
| An email or user name | `email`, else `preferred_username` | The gateway refuses a token with neither. It is the human-readable actor in the audit log. |
| The aikonOS tenant, as a UUID | `AIKONOS_OIDC_TENANT_CLAIM` (default `tenant_id`) | Row-level security keys on it. A token without the claim is refused (`auth.tenant.missing`); a value that is not a UUID fails every call with `invalid tenant_id`. |
| For step-up approvals (AG-13) | `auth_time`, and `amr` or `acr` | See "Step-up" below. |

Most IdPs issue `iss`, `sub`, `email` and a signature out of the box. The two
things you normally have to add are the **audience** and the **tenant claim**.

### Audience

The token must carry an audience aikonOS can check. Two ways:

- **An API / resource for aikonOS** (preferred). Register a resource server
  (called API, resource, or authorization server audience depending on the
  IdP), for example `aikonos-broker`, and have the web UI client request a
  token for it. Set `AIKONOS_OIDC_AUDIENCE` to that identifier and send the
  access token (`AIKONOS_WEBUI_OIDC_TOKEN=access`).
- **The client id**. If the IdP cannot issue an access token for a custom
  audience, send the ID token (`AIKONOS_WEBUI_OIDC_TOKEN=id`); its `aud` is the
  web UI client id by construction. Set `AIKONOS_OIDC_AUDIENCE` to the client
  id. The ID token cannot be exchanged on behalf of the user, so this is the
  login-only model.

Some IdPs issue opaque (non-JWT) access tokens unless an API audience is
requested. aikonOS needs a JWT; check with step 4 of the setup.

### Tenant claim

aikonOS needs the tenant as a UUID in a claim. Pick one UUID for the
deployment (or reuse `AIKONOS_TENANT_ID`) and make the IdP emit it:

- **Constant claim** (one aikonOS tenant per IdP app): a static or hardcoded
  claim mapper on the client, for example `tenant_id` =
  `2b0e5f1c-…`. Keycloak: mapper type *Hardcoded claim*. Other IdPs call it a
  static claim, a claims transformation or a token hook that adds a fixed value.
- **Per-user attribute** (several tenants share an IdP): store the UUID as a
  user or organisation attribute and map it into the token. The bundled
  Keycloak realm does this with a *User Attribute* mapper on `tenant_id`
  (`deploy/compose/keycloak-realm.json`).

If the IdP already issues a UUID that identifies the organisation (Entra's
`tid` is one), set `AIKONOS_OIDC_TENANT_CLAIM` to that claim instead of adding
one. Do not use a claim the user can edit themselves.

## Setup

1. **Register the web UI client** in the IdP: public client (no secret),
   authorization code with PKCE, redirect URI
   `https://<host>/auth/callback` (`http://localhost:4200/auth/callback`
   locally). Allow the `openid profile email` scopes.
2. **Add the audience** (API identifier, or the client id for the login-only
   model) and the **tenant claim** as described above, both on the token the
   web UI will send.
3. **Set the configuration** for broker, gateway and web UI:

   | Setting | Value |
   |---|---|
   | `AIKONOS_OIDC_ISSUER` | the IdP's `issuer` from `/.well-known/openid-configuration` |
   | `AIKONOS_OIDC_JWKS_URL` | the `jwks_uri` from the same document (required: the gateway does not discover it) |
   | `AIKONOS_OIDC_AUDIENCE` | the API identifier, or the web UI client id |
   | `AIKONOS_OIDC_SUBJECT_CLAIM` | the immutable user id claim (`sub` unless the IdP has a better one) |
   | `AIKONOS_OIDC_TENANT_CLAIM` | the claim from step 2 |
   | `AIKONOS_TENANT_ID` | the same UUID, so seeded data lands in that tenant |
   | `AIKONOS_BROKER_TENANT_ID` | unchanged: the admin gate keys on this name |
   | `AIKONOS_WEBUI_OIDC_AUTHORITY` | the issuer URL |
   | `AIKONOS_WEBUI_OIDC_CLIENT` | the web UI client id |
   | `AIKONOS_WEBUI_OIDC_SCOPE` | `openid profile email`, plus the API scope if the IdP needs one to set the audience |
   | `AIKONOS_WEBUI_OIDC_TOKEN` | `access` (API audience) or `id` (client id audience) |

   Then recreate the containers:
   `docker compose up -d --force-recreate broker agent-gateway webui`.
4. **Check one token.** Sign in at the web UI, copy the bearer from the
   browser's network tab (a request to `/api/...`) and decode its payload
   (`cut -d. -f2 | base64 -d`, padding aside). Confirm: `iss` matches exactly,
   `aud` contains the audience, the subject claim and the tenant claim are
   present and the tenant is a UUID.
5. **Make yourself admin.** Write
   `user:<subject> admin tenant:<AIKONOS_BROKER_TENANT_ID>` (as in
   `scripts/compose-seed-openfga.sh`) and recreate the broker. Reload the web
   UI: the Admin section appears.
6. **Users and groups.** Assign roles in Admin → Access Control, or let SCIM
   provision them if the IdP speaks SCIM 2.0
   ([18-scim-provisioning.md](18-scim-provisioning.md)).

## Verify

- A user who is not an admin gets 403 on `/admin/*`.
- The broker log shows no `claims invalid: iss` or `aud` rejections. A bad token
  is audited as `auth.token.rejected`; a token without the tenant claim as
  `auth.tenant.missing`.
- `scripts/compose-verify.sh` has a real-bearer check for Keycloak and Entra
  only; for another IdP, the token check of step 4 and the two checks above
  replace it.

Errors and their cause:

| Symptom | Cause |
|---|---|
| 401 at every call, broker logs `claims invalid: iss` | issuer differs (scheme, host, trailing slash) |
| 401, `claims invalid: aud` | the token's audience is not `AIKONOS_OIDC_AUDIENCE`; for an access token the API audience was not requested |
| 401, `missing tenant_id claim` (gateway) | the tenant claim mapper is not on the token the web UI sends (access vs ID token) |
| 400 `invalid tenant_id` | the tenant claim is not a UUID |
| 401, `missing email or preferred_username` | add the `email` scope or a user name claim |
| 403 on Admin after sign-in | the admin tuple names a different subject than the configured subject claim |

## Step-up

Step-up approvals (AG-13) count a yes only when the approver's token shows a
recent multi-factor sign-in: `auth_time` within
`approvals.step_up_max_age_minutes`, and `amr` naming a second factor (`mfa`,
`otp`, `hwk`, ...) or `acr` equal to `approvals.step_up_acr`. The web UI asks for
it with `max_age=0` and `acr_values=<approvals.step_up_acr>`. Configure the IdP
to honour `acr_values` (an MFA policy keyed on that value) and to put
`auth_time` and `amr` or `acr` into the token the web UI sends. Without
`auth_time` every step-up approval is refused (fail closed); denials still
work.

## Switch IdPs

Subjects differ between IdPs, so every FGA tuple that names a user (admin role,
group membership, agent owner, skill access) must be written again for the new
subjects. Keep `AIKONOS_BROKER_TENANT_ID` and, if the data should stay, the
tenant UUID: emit the old UUID as the tenant claim of the new IdP. Before the
switch, export the agent inventory (Admin → Agents → *Export inventory*) to
see which owners must be mapped to new subjects; agents whose owners are not
mapped show as orphaned.

## Agent inventory export

Identity-governance tools can import aikonOS agents from the inventory export
(AG-14): Admin → Agents → *Export inventory (JSON)* or *(CSV)*, or
`GET /api/admin/agents/inventory?format=json|csv` with a tenant-admin bearer.
Each agent carries its id, name, status, owners (`user:` and `group:`
subjects), whether it is a service agent, MCP connectors, allowed MCP tools,
skills, the number of live API keys and public keys, the nearest key expiry
and the last time any of its keys was used. The system prompt is not exported.
Every export is audited as `aikonos.broker.agent.inventory.exported`; it fails
rather than list an agent without owners when OpenFGA cannot be read. In the
CSV, lists are joined with `;` and a cell starting with `=`, `+`, `-` or `@` is
prefixed with `'` so a spreadsheet does not run it as a formula.
