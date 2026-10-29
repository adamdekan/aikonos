# SCIM 2.0 provisioning with Entra ID (AG-16)

With SCIM the identity provider drives two things in aikonOS:

- **Deactivation.** A user disabled, unassigned or deleted in Entra is blocked in
  aikonOS (the AG-06 user block). The block stops all use at once, and owned
  agents fall into the orphan sweep (AG-04).
- **Group membership.** Entra groups assigned to the app become aikonOS groups.
  Membership is written as FGA tuples `user:<objectId> member group:<name>`.

Sign-in itself does not change: users still sign in through OIDC (see
[12-entra-login.md](12-entra-login.md)).

A tenant runs one of two modes, set in `provisioning.mode`:

| Mode | Who grants groups | SCIM endpoint |
|---|---|---|
| `jit` (default) | Email rules at first sign-in (Admin → Access Control → Provisioning) | Refuses every call (403) |
| `scim` | Entra, through SCIM | Serves |

Under `scim`, email rules grant nothing and new rules are refused. This removes
finding M-02 (groups granted from an unverified email claim) for the tenant.

## What aikonOS stores

| SCIM attribute | Use |
|---|---|
| `externalId` | The user's OIDC subject. **Required.** Map it to the Entra `objectId`. Cannot change after creation. |
| `userName` | Lookup key (`filter=userName eq "…"`), unique per tenant, case-insensitive |
| `emails`, `displayName` | Shown in SCIM responses |
| `active` | `false` sets the user block; `true` lifts a block that SCIM set |
| Group `displayName` | Gives the aikonOS group name: lowercase, other characters become `-` (`Finance Team` → `finance-team`). The name is fixed when the group is created, so a rename in Entra keeps the tuples. |
| Group `members` | One FGA `member` tuple per member |

Other attributes (`name.*`, `title`, the enterprise extension) are accepted and
ignored.

`DELETE /Users/{id}` deactivates the user (block) and removes the user's group
tuples. A reactivation lifts only a block that SCIM set; a block an admin set by
hand stays until an admin lifts it.

Supported operations: `GET /Users` and `/Groups` with `filter=<attr> eq "<value>"`
(`userName`, `externalId`, `id`; `displayName` for groups), `startIndex` and
`count`; `GET`, `POST`, `PUT`, `PATCH` (Entra-style `Add`/`Replace`/`Remove`,
including `members[value eq "…"]` and string booleans), and `DELETE` on single
resources. Not supported: `/ServiceProviderConfig`, `/Schemas`, `/ResourceTypes`,
`/Bulk`, sorting, and filters other than `eq`.

## 1. Prepare aikonOS

1. Set `AIKONOS_EXTERNAL_BASE_URL` on the agent-gateway to the public URL of
   the external surface (port 8090 by default), for example
   `https://aikonos.example.com:8090`. Entra must reach it over HTTPS from the
   internet. Without it, the admin page guesses `https://<host>:8090/scim/v2`.
2. Entra sends a burst of requests during a sync. If the external surface
   answers 429, raise `AIKONOS_EXTERNAL_RATE_LIMIT` (requests per minute per IP,
   default 60).
3. In aikonOS, open **Admin → Access Control → Provisioning**.
4. Under **Directory sync (SCIM)**, select **Create token**. Copy the token. It is
   shown once; aikonOS keeps only a hash.
5. Copy the **SCIM tenant URL** (`…/scim/v2`).

Do not switch the mode yet. Under `jit` the endpoint answers 403, so Entra cannot
make changes before you are ready.

## 2. Configure the Entra enterprise app

1. Entra admin center → **Enterprise applications** → your aikonOS app (the one
   used for sign-in) → **Provisioning** → **Get started**.
2. **Provisioning Mode:** Automatic.
3. **Admin Credentials:** Tenant URL = the SCIM tenant URL; Secret Token = the
   token from step 1.4.
4. **Mappings → Provision Microsoft Entra ID Users:**
   - Change the `externalId` mapping: source attribute **`objectId`**. (The
     default maps `mailNickname`, which is not the OIDC subject; users created
     that way would never match a sign-in.)
   - Keep `userName` ← `userPrincipalName`, matching precedence 1.
   - Keep the `active` mapping (`Switch([IsSoftDeleted], …)`).
   - Remove mappings you do not need; unmapped attributes are ignored anyway.
5. **Mappings → Provision Microsoft Entra ID Groups:** enabled. Keep
   `displayName` ← `displayName` (matching) and `members` ← `members`.
6. **Settings → Scope:** "Sync only assigned users and groups". Assign the
   users and groups that should exist in aikonOS under **Users and groups**.
7. Save.

The aikonOS sign-in uses the `oid` claim as subject (`AIKONOS_OIDC_SUBJECT_CLAIM=oid`,
see 12-entra-login.md). `externalId` must hold the same value; that is why it
maps to `objectId`.

## 3. Switch the mode and start

1. In aikonOS, set **Provisioning mode** to **Identity provider via SCIM**. From
   now on email rules grant nothing. Groups already granted by email rules stay
   until you revoke them in the Roles tab.
2. In Entra, select **Test Connection**. It must succeed.
3. **Provision on demand** for one test user first. Check:
   - Admin → Audit: `aikonos.broker.scim.user.created` with actor
     `scim-token:<id>`.
   - After the group is provisioned: Access Control → Users shows the user in
     the group.
4. Set **Provisioning Status** to On. Entra syncs every 40 minutes.

## Group names and grants

SCIM manages the membership of the groups it creates. It does not grant those
groups anything: an admin grants a group roles, skills or connectors in Access
Control as usual (for example `group:finance-team#member`), after SCIM has
created the group. SCIM does not adopt a group already used in aikonOS: if an
Entra group gives the name of a group that has members or grants, the create is
refused (409, audited as `scim.group.claim_refused`), so a provisioning token
cannot add members to a group an admin manages by hand. Rename the Entra group,
or remove that group's members and grants in Access Control and let Entra retry.
Two Entra groups that give the same name are refused (409) for the second.

## Token rotation and revocation

- **Rotate:** Provisioning → token row → **Rotate**. The old token stops working
  at once; paste the new one into Entra (Admin Credentials) and **Test
  Connection**. Entra retries failed calls, so a short gap loses nothing.
- **Revoke:** Provisioning → token row → **Revoke**. Use this when a token may
  have leaked. Provisioning stops until a new token is created and entered.

Every token change is audited (`aikonos.broker.scim.token.created`,
`aikonos.broker.scim.token.revoked`), and a call with an unknown token or under
`jit` is audited as `aikonos.broker.scim.auth.denied` (DENY).

## Audit events

| Event | When |
|---|---|
| `aikonos.broker.scim.user.created` / `.updated` / `.deleted` | User changes |
| `aikonos.broker.scim.user.deactivated` / `.reactivated` | `active` changed (block set / lifted) |
| `aikonos.broker.scim.group.created` / `.updated` / `.deleted` | Group changes, with the members added and removed |
| `aikonos.broker.scim.token.created` / `.revoked` | Admin token changes |
| `aikonos.broker.scim.auth.denied` | Unknown or revoked token, or the tenant is on `jit` |

## Troubleshooting

| Symptom | Cause |
|---|---|
| Test Connection fails with 403 | `provisioning.mode` is still `jit` |
| 401 | Token wrong, rotated or revoked |
| 400 `externalId is required` | The `externalId` mapping is missing; map it to `objectId` |
| 400 `externalId cannot change` | The mapping was changed after users were created. Delete and re-provision those users. |
| A provisioned user signs in but has no groups | `externalId` is not the subject the OIDC token carries (check `AIKONOS_OIDC_SUBJECT_CLAIM`) |
| 409 on a group | Another group already gives the same aikonOS name |
