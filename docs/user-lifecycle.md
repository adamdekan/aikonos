# User lifecycle: sign-in, access, and disablement

> Review note (2026-06-12). Records the current — intentional — design: Aikonos has no
> user disable/delete. Identity lifecycle belongs to the IdP; Aikonos governs only
> authorization after sign-in.

## How a user gets the ability to sign in

Aikonos stores no passwords and no user accounts. Authentication is fully delegated to
the OIDC identity provider (Entra ID in the live deployment; Keycloak in dev).

A user can sign in if and only if the IdP issues them a valid token for the configured
issuer, audience, and tenant. The gateway (`agent-gateway/src/auth/verify.ts`) and the
broker (`broker/internal/auth/oidc.go`) only *validate* bearers — neither mints or
stores credentials.

- **Grant sign-in:** assign the user to the Entra enterprise app (or allow any tenant
  member to authenticate, depending on the app's "assignment required" setting).
- **Revoke sign-in:** disable or delete the user in Entra, or remove their app
  assignment. The next token request fails; already-issued tokens remain valid until
  expiry (~1 hour).

## How users appear in the Access Control → Users tab

Users materialize just-in-time. The first authenticated north RPC writes the verified
subject to `user_directory` and fires the provisioning claim
(`broker/internal/provisioning/`, migration 017). There is no "create user" action and,
deliberately, no "delete user" action — the directory is an observed record of who has
authenticated, not an account store.

When the tenant's Microsoft 365 connection is configured (Admin → Settings → Microsoft 365), a
user's OneDrive connects itself the same way: the first authenticated request after an
Entra sign-in mints a delegated Graph token on the user's behalf — no connect click. If
the token later dies (long inactivity, revocation), the connector shows
`reconnect_needed` and heals automatically at the user's next sign-in.

**Provisioning matcher timing.** Two matcher forms behave differently. Wildcard rules
(`*@domain.com`) are JIT-only: they fire at first sight, while `provisioned_at` is
still NULL, and do nothing for users who have already authenticated (whose
`provisioned_at` is set). Exact-email rules
(`alice@domain.com`) are retroactive: when `AddProvisioningRule` is called, it
immediately calls `SubjectsByEmail` and writes FGA tuples for any already-seen users
matching that address (`broker/internal/broker/provisioning_admin.go:118`). If an
existing user is not getting the groups you expect from a wildcard rule, the reason is
this timing difference — add an exact-email rule, or use the Access Control tab to grant
groups directly. The Users tab lists every `user_directory` entry even before it holds
any FGA tuple (`principalsFrom` in `broker/internal/broker/roles.go`), so a user stuck by
the wildcard-timing gap is still selectable there to grant directly.

**Startup seed.** At broker boot, `provisioningseed.Seed()` reads the file at
`AIKONOS_PROVISIONING_SEED_FILE` (YAML) and idempotently inserts any rules whose matchers
are not already present (`broker/internal/provisioningseed/seed.go:40`). Invalid entries
are logged and skipped; existing matchers are never overwritten, so admin edits via the
Provisioning tab always win. The file is not auto-mounted — you must add a bind-mount and
set the env var explicitly. See `deploy/compose/provisioning.yaml.example` for the
format.

## What Aikonos controls: authorization, not authentication

A signed-in user with zero group memberships hits deny-by-default:

- Every tool invocation in a personal session is denied at `SubmitPlan`
  (`userSkillDecision` in `broker/internal/broker/service_plan.go:539`, fail-closed on FGA
  error).
- No tenant-admin pages (the `admin` relation on `tenant:<name>` gates them).
- Effectively, revoking all of a user's groups in Access Control is a soft-disable for
  anything useful: they can open the UI and chat, but the agent cannot act.

## Known gaps (accepted for now)

| Gap | Today | Possible future feature |
|---|---|---|
| Platform-level block (defense-in-depth if IdP revocation lags) | none — any valid token passes | `blocked` flag in `user_directory`, checked in the OIDC interceptor → all RPCs 403 instantly, no waiting for token expiry |
| Remove a stale user from the Users list | `user_directory` rows persist forever | delete-from-directory action, paired with a bulk revoke of the user's tuples |
| One-click "revoke everything" | revoke group-by-group in the Users tab | "Revoke all access" button = delete every tuple whose subject is the user |

Decision: the current design stands. Entra is the front door; Aikonos is the permission
system behind it. Revisit this note if a Aikonos-side kill switch or directory cleanup
becomes necessary.
