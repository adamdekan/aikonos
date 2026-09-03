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
against their own threat model rather than discover it later. All three share
one boundary: registering an MCP server is an administrator action, and the
administrator is treated as the trust boundary.

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

## Hardening the deployment you run

The stack ships with enforcement available but not active until seeded. A
stack that has not been seeded logs `OpenFGA is disabled — dev allow-all
mode` and authorizes everything. Never run that configuration anywhere real.
See the enforcement guide in `docs/` for the seeding steps and how to verify
the result.
