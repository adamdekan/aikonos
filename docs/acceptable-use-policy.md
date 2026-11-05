# Aikonos Acceptable Use Policy

**Version:** 1.0  
**Status:** Active  
**Owner:** Security Team  
**Effective:** 2026-06-19

---

## 1. Permitted Principals

Access to the Aikonos platform is restricted to:

- Employees and contractors provisioned into the Aikonos tenant via Keycloak (OIDC auth-code + PKCE S256) or a federated enterprise IdP (Entra ID).
- Service accounts provisioned via `AIKONOS_PROVISIONING_SEED_FILE` at broker startup; each must be scoped to a named tenant.

**Not permitted:**

- Anonymous or unauthenticated access at any layer. The broker's OIDC interceptor rejects all requests without a valid bearer token. `svc-` prefixed subjects are blocked at `Validator.Validate` — they cannot act as north-side principals.
- Shared login credentials or team accounts. Each principal must have a distinct Keycloak/IdP identity.

---

## 2. Data Classification

| Class | Definition | Aikonos handling |
|-------|------------|-----------------|
| **PUBLIC** | No confidentiality requirement | Permitted in agent plans and tool calls without additional approval |
| **INTERNAL** | Internal business data, not regulated | Permitted without additional approval; workspace isolation (`Root/<tenant>/<user>/`) enforced by `workspacefs` |
| **CONFIDENTIAL** | Sensitive business data (financial, strategic, contractual) | Requires explicit `security-team` FGA grant before any plan step reads or writes the data; HITL approval required for any external write step in the same plan |
| **RESTRICTED** | PII, PHI, or data subject to legal/regulatory obligation (GDPR, HIPAA, SOC 2 scope) | Requires written legal sign-off before processing; must not leave the `workspace-data` volume (no `write_external` steps); `plan_validation.rego` enforces the `reads_sensitive + write_external` violation at `SubmitPlan` — plans combining these are denied unless `has_dlp_attestation` is present |

Operators classifying data at RESTRICTED must add the label to the relevant workspace directory and inform the security team. Classification is sticky — a plan that reads RESTRICTED data inherits RESTRICTED handling for all subsequent steps in that execution.

---

## 3. Mandatory Human-in-the-Loop (HITL) Actions

The following actions **must** trigger a HITL approval step (`NEEDS_HUMAN`) before the broker issues a Biscuit capability token for execution:

| Action | Rationale |
|--------|-----------|
| File or directory deletion (`doc.delete`, or any tool with `effect_class=destructive`) | Irreversible; recovery requires backup restore |
| Outbound email via `email.draft` with `send=true` | Externally visible; cannot be recalled |
| External OAuth writes (Google Drive, OneDrive writes, any connector with `write` scope) | Modifies data in external systems under the user's identity |
| Any plan step with `effect_class=destructive` as declared in the skill manifest | Policy-as-code enforcement: `tool_invocation.rego` raises `NEEDS_HUMAN` on this class |

HITL gates are wired in the gateway's `selectRunApprover`. Automated runs (scheduler, agent API) that encounter a HITL step are suspended until a human approves via the Inbox view.

---

## 4. Prohibited Uses

The following are prohibited for all principals regardless of granted permissions:

- **Credential exfiltration.** Using any tool or agent to read, copy, or transmit Vault secrets, OIDC tokens, Biscuit capability tokens, API keys, or mTLS private keys outside their intended runtime scope. The `EgressProxy` structurally prevents the Pi child from accessing the LLM provider key; this policy extends the intent to all credential classes.

- **Training external models on Aikonos output.** Submitting conversations, agent outputs, or workspace artifacts to external model training pipelines (including fine-tuning endpoints) without explicit security team approval and a data handling contract with the provider.

- **Rate limit bypass.** Deliberately triggering the `CheckRateLimit` fail-open condition (e.g., by disrupting broker gRPC on the mesh network) to exceed RPM/TPM quotas. Rate limits are a cost-control and DoS mitigation — circumventing them is a policy violation regardless of technical feasibility.

- **Unauthenticated MCP servers in production.** Registering or routing to an MCP server that does not require OAuth, bearer token, or mTLS authentication in any environment other than `dev`/`test`. The `mcp-echo` server is `dev`/`test` only.

- **API key sharing across agents.** Each agent must have a dedicated API key (minted via Admin → Agents → Keys). Sharing keys across agents removes the audit attribution that ties actions to a specific agent identity.

- **Bypassing the approval flow.** Attempting to invoke a tool without a valid, scope-matched Biscuit token. The broker's `InvokeTool` fails closed on missing or invalid tokens; attempting to forge or replay tokens is a security incident.

---

## 5. Enforcement

**Technical controls:**

- `policies/opa/plan_validation.rego` — evaluated at `SubmitPlan`; denies plans that violate data classification rules or include prohibited step combinations.
- `policies/opa/tool_invocation.rego` — per-step evaluation; raises `NEEDS_HUMAN` for HITL-required actions, `DENIED` for prohibited tools.
- OpenFGA ReBAC — deny-by-default for personal tasks; `CheckFGA(user, can_invoke, skill:<tool>)` per step; no relation = no access.
- Biscuit capability tokens — one per approved step, scope narrows only; `InvokeTool` verifies scope before routing.
- WORM audit trail — hash-chained + HMAC-SHA256 signed events to MinIO object-lock (365-day retention); non-repudiable.

**Policy:**

This document is the policy layer above the technical controls. Technical controls enforce what they can; this AUP covers intent and scope. Violations of this policy — including technical bypasses and violations not catchable by code — must be reported to the security team.

**Violations:**

Report to the security team via the designated incident channel. Confirmed violations may result in access revocation, account suspension, and escalation per the organization's disciplinary process.

---

## Change log

### 2026-06-19 — Initial version

First publication. Addresses Domain 10 (AI Governance) Foundation-blocking gap identified in `docs/13-zero-trust-audit.md`.
