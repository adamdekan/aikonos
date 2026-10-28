# User lifecycle: sign-in, access, and disablement

> Review note (2026-06-12, updated 2026-10-28 for AG-06). Identity lifecycle belongs to
> the IdP; aikonOS governs authorization after sign-in. Since AG-06 aikonOS can also
> block a user and revoke all sessions of a user or an agent, so access ends at once
> instead of at token expiry. There is still no user delete.

## How a user gets the ability to sign in

aikonOS stores no passwords and no user accounts. Authentication is fully delegated to
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

## What aikonOS controls: authorization, not authentication

A signed-in user with zero group memberships hits deny-by-default:

- Every tool invocation in a personal session is denied at `SubmitPlan`
  (`userSkillDecision` in `broker/internal/broker/service_plan.go:539`, fail-closed on FGA
  error).
- No tenant-admin pages (the `admin` relation on `tenant:<name>` gates them).
- Effectively, revoking all of a user's groups in Access Control is a soft-disable for
  anything useful: they can open the UI and chat, but the agent cannot act.

## Blocking a user and revoking sessions

Disabling a user in the IdP stops new tokens, but issued tokens, owner grants and
capability tokens stay valid until they expire (up to an hour). AG-06 closes that
window with two admin actions, both in **Admin → Access Control → Members** (one row
per user; each needs a reason):

- **Block** stops everything the user does: their next north RPC is refused
  (`PermissionDenied`, audit `auth.subject.blocked`), the gateway's owner grant for
  them is refused (`aikonos.broker.task.denied`, reason `subject_blocked`), their
  running tasks stop at the next tool call (`aikonos.broker.tool.denied`, reason
  `subject_blocked`), and their schedules are parked at the next claim
  (`last_status = subject_blocked`, audit `aikonos.scheduler.run.skipped.subject_blocked`).
  **Unblock** lifts it; the user resumes parked schedules themselves. The Members
  list shows each user as *active* or *blocked* (hover for who blocked them, when and
  why). An admin cannot block themself.
- **Revoke sessions** moves the user's revocation epoch ("not before") to now:
  every IdP token (`iat`), owner grant and capability token issued before it is void,
  so the user must sign in again (`Unauthenticated`, audit `auth.token.revoked`). The
  user is not blocked; a new sign-in works at once.

**Admin → Agents → Revoke sessions** does the same for an agent: its outstanding
capability tokens (refused as `capability_revoked`) and owner grants (`grant_revoked`)
die at their next use, its API keys (`tk_`) minted before the revoke stop resolving
(mint new ones), and its schedules created before the revoke are parked at the next
claim (`actor_revoked`); recreate them to resume.

Every action is audited with the acting admin and the reason:
`aikonos.broker.user.blocked`, `aikonos.broker.user.unblocked`,
`aikonos.broker.user.sessions_revoked`, `aikonos.broker.agent.sessions_revoked`.

How it works:

- A user block is an active row in `kill_switches` (scope `user`, subject = the
  user's principal id). A row with scope `tenant` blocks the whole tenant; it has no
  UI because it would lock out the admins too: set and lift it with `aikonosctl` /
  SQL (see `OPS-RUNBOOK.md`, Kill switch procedures).
- The epoch lives on `user_directory.revoked_before` and `agents.revoked_before`
  (migration 054). Capability tokens carry an `issued_at` fact; a task's token is
  checked against the later epoch of its subject and its actor. A token, grant or IdP
  token issued in the same second as a revoke is refused too (second precision; this
  errs on the side of refusal).
- The broker caches block and epoch reads for 30 s. A change is visible at once on
  the broker replica that made it and within 30 s on any other. A failed read refuses
  the call (`Unavailable`) instead of letting it through.

## Agent owners and orphaned agents

Every agent has one or more owners: users (`owner_user`) or groups (`owner_group`,
every member of the group is an owner). Owners may use the agent and edit its SOUL;
only user owners may delegate. Admins add and remove owners in **Admin → Access
Control → Agents** (select the agent, then *Owners*). The broker refuses to remove
the last owner (`FailedPrecondition`): add the new owner first. Each change is
audited as `aikonos.broker.agent.owner.added` or `aikonos.broker.agent.owner.removed`
(a refused removal as `owner.removed` with decision `DENY`).

The creator becomes the first owner. Agents created before that rule may have no
owner tuple: at startup the broker grants `owner_user` to the creator
(`agents.created_by`) of every agent without one, audited as `owner.added` with actor
`system:owner-backfill`. The sweep is idempotent.

**Admin → Agents** marks an agent *orphaned* when it has no owner or every owner is a
blocked user. A group owner counts as present without checking its members. Fix it by
adding an owner who is not blocked. Orphaned agents still run today; activation
and automatic suspension of orphaned agents come with AG-03.

## Default agents

Every task runs as a registered agent (AG-01). When a chat or a schedule names no
agent, the broker binds the user's default agent:

- **The user's setting.** Each user may pick a default agent in **Settings → Chat →
  Default agent**. Only agents the user can use (`can_use`) are offered, and the
  broker re-checks `can_use` on save (audited as `aikonos.broker.agent.default.set`)
  and on every run. If the user loses access, runs fall back to the tenant default
  agent; the setting is removed when its agent is deleted.
- **The tenant default agent.** One per tenant, created by the broker at startup and
  on first use, marked *default* in **Admin → Agents** and not deletable. It acts with
  the user's own skills (the personal skill check applies, not the agent's skill
  list) and sees no agent memory, so personal chat keeps working with no agent
  picked. Every user may use it through the FGA tuple `user:* usable_by`, written
  when the agent is created (audited as `aikonos.broker.agent.usable_by.added`,
  actor `system:default-agent`). To narrow it, grant `usable_by` to groups and then
  remove `user:*`; the broker restores `user:*` at startup only when the agent has no
  `usable_by` tuple at all. A user without `can_use` on it is refused (`403` in chat).

An agent acts within its user's rights (AG-02). A skill or MCP connector the
user may not use is recorded as a would-deny in audit mode and refused once the
tenant sets `identity.enforce_user_bound=enforce`. Removing a user from a group
therefore also removes that group's connectors from every agent run for the user.
Grant a single person a connector in **Admin → MCP → Access**.

A schedule created without an actor fires as its owner's default agent, resolved at
each fire. With `identity.obo=enforce` such a schedule stays parked, as before.

## Known gaps (accepted for now)

| Gap | Today | Possible future feature |
|---|---|---|
| Remove a stale user from the Users list | `user_directory` rows persist forever | delete-from-directory action, paired with a bulk revoke of the user's tuples |
| One-click "revoke everything" | revoke group-by-group in the Users tab; Block stops all use without touching grants | "Revoke all access" button = delete every tuple whose subject is the user |
| Directory sync from the IdP | block by hand in Members | SCIM 2.0 provisioning (AG-16) maps IdP deactivation to a block |

Decision: Entra stays the front door and aikonOS the permission system behind it; the
block and revoke-all are aikonOS-side kill switches for when IdP revocation lags.
