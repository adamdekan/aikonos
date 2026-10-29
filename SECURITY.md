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

These are the residual risks of the current release, found by the internal
audits and left open on purpose or for a later change. The ids in parentheses
are findings in
[docs/20-security-audit-2026-10-29.md](docs/20-security-audit-2026-10-29.md),
which has the evidence and also lists the deferred findings. They are written
down so an operator can judge them against their own threat model rather than
discover them later.

**MCP servers (the administrator is the trust boundary).** Registering an MCP
server is an administrator action, and the next three items rest on that.

- **MCP server authentication is not enforced.** The broker does not require an
  MCP server to authenticate before it routes tool calls to it; the agent side
  is gated on every call. An administrator may deliberately attach a local,
  unauthenticated MCP server.
- **Agents share one Vault credential.** All agents run under the broker's one
  Vault AppRole; the broker is the only Vault client, and no agent process or
  gateway holds a Vault credential. Secrets reach agents only through mediated
  paths (capability tokens, proxy-injected provider keys, connector tokens), so
  there is no agent-to-agent Vault path.
- **No mutual TLS between broker and MCP servers.** MCP servers are reached over
  https, or over plain http to loopback and, with
  `AIKONOS_TOOLPROXY_MCP_ALLOW_PRIVATE`, to private addresses and single-label
  mesh names, through an SSRF-hardened dialer that re-validates every connect
  and redirect (F189). MCP results are not scanned for prompt injection (F26).

If your threat model does not accept an administrator as a trust boundary,
these three become relevant to you and you should say so in an issue. That is
useful signal for prioritising them.

**Broker: identity and grants**

- A compromised gateway, or code running in its agent process (M-24 below),
  can act for any user whose live bearer it sees, through a session grant that
  lives at most as long as that bearer, and for the scheduled and external runs
  it holds grants for. Owner grants are stateless, multi-use for their TTL and
  not bound to a task id (F5, F171, F183).
- `GetGatewayApproval` mints step tokens for an approved gateway-managed task on
  the gateway's identity alone, without an owner grant; it covers only steps
  that were already voted (F5).
- `ResolveRunAgent` takes the user id from the gateway's request (gateway peer
  and block check only) and does not check the revocation epoch; the owner-grant
  field it needs is deferred to the next protocol change (F171).
- The tenant settings for unattended runs and pre-approval are enforced only in
  the gateway; the broker does not check where an approval came from (H-06).
- Run one broker replica: the `jti` replay cache, the connector token lock and
  the skill-overlay cache invalidation are per process (F14, G22, F25).
- SCIM: a deactivation sent before the 0.7.5 upgrade for an upper-case GUID
  `externalId` does not block the user's token subject until SCIM writes that
  user again, so restart provisioning in the IdP after upgrading (F133). The
  check that refuses adopting a group in use can race an admin grant (F4).
- Vault over plain http stays allowed to loopback and single-label hosts,
  because the compose Vault listener is plaintext (F108).
- MCP OAuth tokens are not revoked upstream and not purged from Vault when a
  connection is deleted; OneDrive tokens cannot be revoked upstream (up to 90
  days of refresh) (F140, G23).
- Legacy workspace directories that two identities shared before 0.7.5 are
  quarantined, not handed to either; a directory whose owner is unknown to the
  broker stays unreachable until an operator moves it (G32, F175).

**Broker: plans, tools and workflows**

- The exfiltration (DLP) policy rule cannot fire until a broker-derived
  "sensitive data was read" flag and a broker-verified DLP stage exist (F120,
  F121).
- Content the model reads is not scanned for prompt injection: MCP results,
  south personal-skill reads, and skills written before the `Skills/` write
  guard (F23, F26).
- Managed OneDrive (the sign-in bootstrap and the OneDrive workspace backend)
  ignores the tenant's connector allowlist, while the `onedrive.*` tools honour
  it (G24).
- A scheduled run's claim is a timestamp, not a per-claim token, so a
  compromised gateway can report a result for any claimed run (F172). A
  workflow version published between the gateway's check and the schedule's
  creation is not caught (F52).
- With OpenFGA disabled (dev mode) the scheduler and skill-transfer gates allow
  everything (F191, F192).
- The spend cap reads a 30-second cache, so spend can overshoot it (F27).

**Broker: audit, limits and delegation**

- Audit favours availability: mutations succeed while the audit store is down,
  callers ignore emit errors, and there is no local spool, so queued events are
  lost on a crash (M-18, threat model D-07).
- Chain verification is time-boxed and has no stored checkpoint, so a large
  trail is verified only for its oldest part and reported as "incomplete"
  (F30). A chain that was never signed shows only in the
  `audit_chain_unsigned_total` metric.
- The workspace quota is per user only (no tenant total), and the legacy
  tool-proxy path without a user is not counted (F33). Symlink containment is
  check-then-use (F44).
- grpc-go decodes a message of up to 16 MiB before any interceptor runs.
  Connections are capped at 64 per IP and streams at 1000 per connection, and
  there is no per-user rate limit on the north API (F200, G48).
- A delegation recipient can mark up to 50 tasks per envelope as delegated by
  the sender during the envelope's 72 hours (F139). There is no per-sender
  envelope quota, so a sender can push older envelopes out of an inbox (F250).
- The broker's policy bundle listener on `:9092` is unauthenticated plaintext
  and binds every broker interface, so every backend, mesh and office peer
  (including attached MCP containers on `aikonos_mesh` and model-authored
  scripts in office-worker) can fetch it. It holds only the Rego policy (F37,
  F218).
- Denial alerts are rate-limited by a cooldown, and compose wires no alert sink
  by default (F34).

**agent-gateway**

- **The sandboxed agent process shares the gateway's user (M-24).** Each agent
  process is forked under the gateway's uid in the same container, with an
  allowlisted environment and no on-disk module cache, but code running in it
  can read the gateway's SVID key under `/tls`, the gateway's environment
  (including its NATS password) through `/proc/<ppid>/environ`, and the shared
  `/tmp`. It needs code execution in the agent process first, and the broker
  still requires the owner grant for calls on behalf of a user; the fix is a
  separate uid or container, which needs `CAP_SETUID` (dropped) or a loader that
  works with Node's permission model (F55).
- Spend accounting trusts the agent process: a compromised one can report zero
  tokens for real calls and so evade spend caps (F54).
- The internal API on `:8080` has no rate limit or connection cap (F57). On the
  external API (`:8090`), `AIKONOS_EXTERNAL_TRUST_PROXY=1` trusts one forwarded
  hop from any peer that reaches the gateway directly; on-prem, pin Traefik's
  address (`deploy/onprem/README.md`) (F56).
- LLM provider endpoints may be plain http (F66, G2).
- A personal skill's one-line description (up to 300 characters) reaches the
  system prompt of every recipient, and the transfer preview does not show it
  to them (G13).
- Two users at their per-user share can fill the agent process pool (F58).
- External invocation records live only in a 1000-entry in-memory ring (F205),
  and the gateway does not check that a NATS audit event's tenant matches its
  subject (F204).

**webui and desktop**

- The web console keeps tokens in `sessionStorage` and revokes them only on
  explicit sign-out with an IdP that has a revocation endpoint (not Entra)
  (M-27). It sends no HSTS (set it at the TLS-terminating proxy).
- After a dropped stream the web console falls back to a blocking run, so a
  workflow can run twice. The agent-inventory CSV export is not
  formula-neutralised.
- `AIKONOS_DESKTOP_ALLOW_HTTP=1` makes the Windows app send bearers in clear
  text. The app does not verify the ID token's signature (it is display-only;
  the gateway checks every bearer), and it follows redirects on token and
  revocation requests.
- The Windows app's schedule edit form resends a schedule's stored
  pre-approvals and cannot untick them (the web console can) (F211).

**office-worker and docs-mcp**

- Jobs are not isolated from each other: concurrent jobs share uid 1000 and the
  `/tmp` tmpfs, so a model-authored script can read or alter another job's
  files and read the worker token from the server's `/proc/<pid>/environ`, and a
  process that clears its environment escapes the end-of-job sweep. The fix is a
  per-job PID and mount namespace (F79).
- Resource limits are per job: one script can fill the shared 512 MiB `/tmp`,
  concurrent jobs together can exceed the container memory limit (the cgroup is
  the backstop), and Python regexes have no timeout beyond the 120 s job limit
  (F217).
- The LibreOffice profile that blocks external links and `WEBSERVICE` is
  checked against its schema only; the exploit test needs soffice and has not
  run (F88).
- docs-mcp has no authentication: every mesh peer can read the whole `./docs`
  corpus, including this register. Its Host/Origin allowlist only stops DNS
  rebinding and browsers (F85).

**Deployment and supply chain**

- OpenFGA has no authentication; its isolation is network-only (F100).
- NATS has one user each for the broker and the gateway, with subject
  permissions, but the gateway's password is not checked for placeholders, a
  same-uid agent process can read it and then every tenant's audit stream, and
  the monitoring port 8222 is unauthenticated (published in local dev) (F114).
- The broker's Vault token can read the signing keys. An existing Vault keeps
  the old policy, which allows overwriting them, until
  `scripts/compose-vault-seed.sh` is run again (an upgrade step), and that
  script exposes the Vault root token on the `docker compose` command line while
  it runs (F115).
- The dev CA writes leaf keys mode 0644 with 10-year certificates, and the azure
  and onprem overlays reuse it unless the operator supplies certificates (F105).
- On-prem push-to-deploy builds signed source on the host rather than the
  cosign-verified release images, and its check runs after the ref moved, so
  set `receive.denyNonFastForwards` and `receive.denyDeletes` (F106, F228).
- Backups are not encrypted (F109). Compose does not refuse the shipped
  placeholder passwords for Postgres, Keycloak, MinIO and Grafana (F227).
- Base images are not pinned by digest and builds run `apt-get upgrade`, so
  they are not reproducible; there is no `cargo audit` (F89, F107, F210, F225).
- Traefik keeps the Docker socket (read-only) and trusts forwarded headers
  (F119, F229).
- Grafana can read names across tenants by design, and one tenant per OpenFGA
  store is documented but not enforced (F49, F127).
- The OpenTelemetry collector runs as root, its OTLP and Loki receivers are
  unauthenticated inside the network, and an http SIEM endpoint is not refused
  (F110).

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
