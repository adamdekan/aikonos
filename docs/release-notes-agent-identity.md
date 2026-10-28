# Release notes: agent identity (AG-01 to AG-17)

This release makes every task name a human subject and a registered agent
(on-behalf-of, AG-17) and tightens keys, approvals and schedules around that.
GitHub release notes are generated from commits; this page is the upgrade
guide that goes with them. Read it before you upgrade a running deployment.

## Upgrade order

1. **Upgrade every audit-chain verifier first.** Audit events now carry a new
   top-level field, `actor_agent_id`, which is part of the canonical bytes that
   the chain hash and the HMAC signature cover. A broker or tool built before
   this release decodes such an event without the field, so it computes a
   different hash. It reports a chain break and a signature failure for every
   new event, although the trail is intact. Old events verify unchanged on both
   versions.
   - Upgrade all broker replicas together. Do not run a mixed fleet in which an
     old replica serves `VerifyAuditChain` while a new one writes events.
   - Rebuild any offline or SIEM-side verifier that embeds
     `broker/internal/audit` before the first new broker writes.
   - Do not roll a broker back past this release once it has written events.
     The older version reports the new events as tampered.
2. **Apply the database migrations** (`048` onwards) with the normal migrate
   job, then start the new broker, then the agent-gateway, webui and desktop
   client. The gateway, webui and desktop client must all be from this release
   (see the approvals section below).
3. **Keep `identity.obo = audit`** (the default) until the audit trail shows
   no tool denials with reason `obo_context_mismatch` and no
   `aikonos.scheduler.run.skipped.actor_missing` events. Then switch the tenant
   to `enforce` in Admin, Settings, Identity. Broker config
   `gateway_grant.accept_v1` stays `true` for this release so older gateways'
   v1 grants still work in audit mode. Enforce refuses them.

## Agent API keys now expire (AG-15)

Every `tk_` key has a mandatory expiry (tenant cap `api_keys.max_ttl_days`,
default and ceiling 90). Migration `052` backfills existing keys to
`created_at + 90 days`, but never sooner than 14 days after the migration ran.
That gives every key at least a 14-day grace to rotate. An expired key gets the
same response as a revoked one (REST 401). Before the grace ends, check the
admin key list (it warns at 7 days left) and mint replacement keys for
external integrations.

## Approvals require the argument digest (AG-07)

`SubmitPlan` returns `args_digest`. `ApproveTask`, `ApproveGatewayTask` and the
gateway's `POST /approve/:id` must echo the digest. A missing or different
digest is refused (`FailedPrecondition`, HTTP 409) and the approval keeps
waiting. A denial needs no digest. As a result:

- A gateway, webui or desktop client from before this release cannot approve
  anything. Upgrade them in the same window as the broker.
- An approval modal that was open across the upgrade has no digest. Reload it,
  or approve from Inbox, which reads the digest from the broker.
- External clients that call the approve RPC directly must send the digest
  they received from the approval request.

## Scheduled runs

Each schedule is a standing delegation from its owner to an agent. The
delegation expires 90 days after creation; migration `049` gave existing rows
90 days from the time it ran. When it expires, the schedule is parked and the
owner sees "Delegation expired" in Schedules. The owner can extend it by
another 90 days (the extend button; audited as
`aikonos.scheduler.run.delegation_extended`) and then resume it. Admins can
revoke a delegation but cannot extend one in another user's name.

Under `identity.obo = enforce`, a schedule with no actor agent is parked at its
next claim, and resuming it is refused with `FailedPrecondition`. The owner sets
an agent ("Acts as") in the edit form first, then resumes.
