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
| Kubernetes deployment (Helm chart, operator) | Compose-only deployment ends most enterprise evaluations before they start |
| Independent security audit | A security product asserting its own posture is not evidence |
| Signed releases, SBOM, reproducible images | Supply-chain expectations for anything in an authorization path |
| Agent inventory and lifecycle | Discovering, onboarding and retiring agent identities is currently manual |

## Under consideration

Not committed, listed so the direction is visible.

- **Delegation interop.** Cross App Access and OIDC-A are converging on how
  one agent calls another system on a user's behalf. Aikonos mints its own
  grants today; speaking a standard would remove an integration cliff.
- **Content inspection.** Tool results are scanned for injection patterns and
  annotated, never rewritten. A policy-driven redaction stage would let an
  operator choose to block rather than only flag.
- **Adversarial testing in CI.** The policy layer has conformance vectors.
  The prompt-facing surface has none.

## Deferred by design

Three findings from the zero-trust audit are open with an accepted-risk
rationale rather than a schedule. They are documented as known limitations in
[SECURITY.md](SECURITY.md) so an operator can judge them against their own
threat model instead of discovering them later.
