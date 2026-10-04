# Roadmap

Open work only. What already works is described in the README.

## Invariants

Any change must keep these intact. They are the security thesis of the
architecture, not preferences.

- **Four-gate, fail-closed authorization.** Identity, then access, then
  routing, then capability. Each gate is independent, and a gate that cannot
  reach its decision denies.
- **Effect class is routing only, never authorization.** This is the
  load-bearing safety property. A tool's declared effect decides where the
  call goes, never whether it is allowed.
- **Per-call access re-check.** `InvokeTool` re-evaluates OpenFGA on every
  call. An approval granted at plan time is not cached into execution.
- **Identity binding.** The caller's identity is derived from the validated
  token, never read from the request body. A forged `user_id` is rejected.
- **Enforce at the unbypassable boundary.** A control belongs where no code
  path can route around it.
- **Audit answers who, what, when and why.** Every authorization decision is
  an event, and the trail is queryable and independently verifiable.
- **Configuration can only restrict, never widen.** Runtime config tunes
  routing and convenience. It cannot grant access.

## Before 1.0

| Item | Why it blocks |
|---|---|
| Audit store hardening | Default retention is off unless `audit.retention_days` is set, and governance mode lets an account with bypass rights delete events. An event whose write fails after retries is dropped while the chain head moves on, so the trail shows a break rather than an explained gap. Verification reads at most 5,000 events, and it orders them by id while the chain is in emit order, so concurrent writes can show as false breaks |
| Connectors against real tenants | Microsoft 365, OneDrive and Google Drive are unit-tested only. The documents that regulated organisations want agents to work on live in exactly these systems |
| OpenBao in place of Vault | Vault is licensed under BUSL-1.1, which is not open source. OpenBao is its MPL-2.0 fork with the same API |
| Security advisories and VEX | Release images are scanned and refused when they carry a fixable critical vulnerability. There are no published advisories yet, and no VEX statements for findings the code cannot reach |
| Independent security audit | A security product asserting its own posture is not evidence |
| Kubernetes deployment (Helm chart) | Docker Compose suits evaluations and single-host installations. Larger installations standardise on Kubernetes |

## Research

Open questions rather than features. Each can fail, and each result will be
published under the same licence as the rest, including an unfavourable one.

- **A machine-checkable enforcement model.** A formal model of the gates and a
  checker that runs against a deployment's own policy and returns a proof or a
  counterexample: authority never grows along a delegation chain, and nothing
  outside the granted set executes, whatever the model outputs.
- **Measured containment.** An adversarial corpus of prompt-injection vectors
  and a harness that measures how often one reaches an action outside the
  granted set, published per release with the failing cases. The same
  measurement decides whether an information-flow layer at the tool call, a
  risk pre-screen that can only escalate, or a stage that blocks content
  instead of flagging it actually narrows the gap.
- **Per-action isolation.** Each tool call in its own sandbox, bound to the
  capability that authorised it, within a latency budget interactive use
  tolerates. Capability tokens become single-use as part of the same work.
- **Sovereign operation.** A deployment profile with a locally hosted model
  and no external network egress, with a pass or fail tool-use evaluation per
  candidate model.

## Later

Usability for the people who approve, audit and get refused:

- Accessibility tested against WCAG 2.2 AA, starting with the approval and
  administration screens.
- German and English as a per-user setting.
- Denials in plain language: which rule refused, and what would be needed.
- Required human approval wherever automation touches a decision about a
  person.

## Under consideration

Not committed, listed so the direction is visible.

- **Delegation interop.** Cross App Access and OIDC-A are converging on how
  one agent calls another system on a user's behalf. Aikonos mints its own
  grants today; speaking a standard would remove an integration cliff.
- **Reproducible image builds.** Release images are signed and carry SBOMs
  and provenance, which proves who built them. Only a bit-for-bit rebuild
  would let a third party confirm they follow from the source alone.
- **Agent inventory and lifecycle.** Discovering, onboarding and retiring
  agent identities is manual today.
- **Access-decision replay.** Policy decisions replay against the archived
  policy that made them. Access checks record the OpenFGA model and their
  result, but not the relationship tuples behind it; archiving tuple history
  would let them replay too.

## Deferred by design

Three findings from the zero-trust audit are open with an accepted-risk
rationale rather than a schedule. They are documented as known limitations in
[SECURITY.md](SECURITY.md) so an operator can judge them against their own
threat model instead of discovering them later.
