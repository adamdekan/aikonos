# Security policy

## Reporting a vulnerability

Report privately through GitHub's private vulnerability reporting on this
repository: **Security → Report a vulnerability**. That channel is preferred
over email because it keeps the report, the fix and the advisory in one place.

Please do not open a public issue for a suspected vulnerability.

What helps a report move fast:

- the version or commit you tested
- which deployment variant (local, Azure, on-prem)
- whether enforcement was seeded and active, since an unseeded stack runs in
  dev allow-all mode and behaves differently
- the smallest reproduction you have

You should expect an acknowledgement within three working days and an
assessment within ten. If a report is accepted, we agree a disclosure date
with you and credit you in the advisory unless you ask otherwise.

## Supported versions

Pre-1.0. Only the latest tagged release receives fixes. There are no backports.

## Verifying a release

The images of every tagged release are signed with cosign by this repository's
release workflow, without a long-lived key, and each carries a CycloneDX SBOM
attestation and SLSA build provenance. Verify a release before you deploy it:

```bash
scripts/verify-release.sh v1.2.3 --provenance
```

Accept exactly one signer:
`https://github.com/adamdekan/aikonos/.github/workflows/release.yml@refs/tags/<version>`,
issued by `https://token.actions.githubusercontent.com`. What a valid signature
proves, and what it does not, is set out in
[docs/14-signed-releases.md](docs/14-signed-releases.md).

## Scope

In scope: anything that lets a principal act outside its granted authority.
That includes bypassing the plan-time gate, forging or widening a capability
token, escaping the tool sandbox, reading another tenant's data, tampering
with the audit chain, or extracting a provider credential from the agent
process.

Out of scope: findings that require an already-compromised tenant
administrator, and the accepted-risk items below.

## Known limitations

These are open findings from the internal zero-trust audit. Each is documented
with the reasoning behind not scheduling it, so an operator can judge it
against their own threat model rather than discover it later. The first three
share one boundary: registering an MCP server is an administrator action, and
the administrator is treated as the trust boundary.

**MCP server authentication is not enforced.** The broker does not require an
MCP server to authenticate before routing tool calls to it. The agent side is
gated on every call, but server-side authentication is convention rather than
enforcement. An administrator may deliberately attach a local, unauthenticated
MCP server, and that is an intentional vetted choice rather than exposure to
an untrusted principal.

**Agents share one Vault credential.** All agents run under the same broker
Vault AppRole; there is no distinct credential per agent type. The broker is
the only Vault client in the architecture. No agent-side component, neither
the sandboxed agent process nor the gateway parent, holds a Vault credential
or reaches Vault directly. Secrets arrive through mediated paths only:
capability tokens, proxy-injected provider keys, and connector tokens. One
agent's compromise therefore does not open a path to another agent's secrets,
because no agent-to-agent Vault path exists.

**No mutual TLS between broker and MCP servers.** MCP endpoints are reached
over plain HTTP through an SSRF-hardened dialer that re-validates on every
connect and every redirect. The administrator who attaches the server is the
trust boundary here, not the transport.

If your threat model does not accept an administrator as a trust boundary,
these three become relevant to you and you should say so in an issue. That is
useful signal for prioritising them.

**The sandboxed agent process shares the gateway's user (M-24).** Each agent
process is forked from the gateway under the same uid in the same container.
Its environment is reduced to an allowlist and it does not use the on-disk
module cache, but code running inside it can still read anything that uid
can: the gateway's SVID key under `/tls`, the gateway's environment through
`/proc/<ppid>/environ`, and the shared `/tmp`. This needs code execution in
the agent process first (its tools run no shell, so a dependency or parser bug
is required), and the broker still requires the owner grant for calls made
on behalf of a user. The fix is a separate uid or container for the agent
process; a distinct uid needs `CAP_SETUID`, which the container drops, and
Node's permission model does not work with the TypeScript loader the process
runs under.

## Hardening the deployment you run

The stack ships with enforcement available but not active until seeded. A
stack that has not been seeded logs `OpenFGA is disabled — dev allow-all
mode` and authorizes everything. Never run that configuration anywhere real.
See the enforcement guide in `docs/` for the seeding steps and how to verify
the result.

External agent API keys (`tk_…`) always expire. Lower the tenant setting
`api_keys.max_ttl_days` (default and ceiling 90) to force faster rotation, and
watch the key list for keys about to expire or unused for a long time.

Prefer key pairs to `tk_` keys for external agents (AG-05). Register the
agent's public key (P-256 or Ed25519) under Admin → Agents → Public keys; the
agent then sends a signed JWT client assertion (RFC 7523, `iss` = `sub` =
agent id, `aud` = `AIKONOS_EXTERNAL_AUDIENCE`, `exp` at most five minutes
ahead, single-use `jti`) instead of a bearer secret. The private key never
leaves the agent. Public keys expire under the same `api_keys.max_ttl_days`
cap; rotate by registering the new key before retiring the old one. Key-pair
auth is off until the gateway's `AIKONOS_EXTERNAL_AUDIENCE` is set. The
`jti` replay cache is held in the broker process: run one broker replica, or
accept that a restart reopens a replay window of at most five minutes.

To cut a person or an agent off before their tokens expire, block the user or
revoke all sessions of the user or agent in the admin UI (Access Control →
Members, Agents). Both take effect within 30 seconds on every broker replica;
see `docs/user-lifecycle.md`.

Give every agent more than one owner, or a group owner, so it keeps an
accountable owner when a person leaves. Admin → Agents marks an agent
*orphaned* when it has no owner or all its owners are blocked.

To stop an agent, suspend it (Admin → Agents → Suspend, with a reason; AG-03).
Its running tasks stop at their next tool call, and new runs, API key calls and
scheduled fires are refused until it is activated again. New agents start
staged and need an owner who is not blocked to activate. An agent orphaned for
longer than `identity.orphan_grace_days` (default 7) is suspended
automatically.

Every task runs as a registered agent (AG-01). A chat or schedule that picks
none runs as the user's default agent (Settings → Chat → *Default agent*) or,
without one, the tenant default agent, which acts with the user's own skills.
MCP connectors attached to the tenant default agent are available to every user
who may use it, so attach a connector there only when everyone should have it.

An agent never does more than the person it acts for (AG-02): a skill or MCP
connector needs the person's own grant next to the agent's. Tenants start in
audit mode (`identity.enforce_user_bound=audit`), which records would-deny
events (Admin → Settings → Identity); switch to enforce once grants are fixed.
External `svc-` runs and agents marked as service agents are exempt and flagged
in their audit events.

Let the identity provider drive users and groups with SCIM 2.0 (AG-16, setup in
`docs/18-scim-provisioning.md`): a user deactivated in Entra is blocked in
aikonOS, and group membership follows Entra. Set `provisioning.mode=scim`; it also
turns off the email-rule grants, which trust an unverified email claim. SCIM
tokens are shown once and stored as a peppered hash; rotate or revoke them under
Admin → Access Control → Provisioning.

A yes on a step-up approval (OPA `require_step_up`) needs the approver's
recent multi-factor sign-in (AG-13): the token's `amr` names a second factor or
its `acr`/`acrs` matches `approvals.step_up_acr`, and `auth_time` is at most
`approvals.step_up_max_age_minutes` old. Otherwise the broker refuses with
`step_up_required` and records `aikonos.broker.task.step_up_required`. The web
UI then asks for a fresh sign-in and retries. Entra setup:
`docs/12-entra-login.md` → *Step-up for risky approvals*.

Attaching an MCP connector to an agent grants none of its tools (AG-08): pick
them under Admin → Agents → *Tools on <connector>*, or use the *Read-only
preset*. The broker refuses any other tool of that connector at plan and at
call time. An MCP tool runs as `destructive` (approval plus step-up on every
call) until a tenant admin confirms its effect class under Admin → MCP →
*Tools*; the server's `readOnlyHint` and the tool's name are never trusted.
The model is shown only the tools its agent may call; the others are not
listed in its session.
