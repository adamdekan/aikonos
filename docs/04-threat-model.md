# Aikonos Threat Model

**Scope**: Full Aikonos platform
**Methodology**: STRIDE + LLM-specific additions, asset-centric
**Version**: v0.1
**Status**: Draft — requires review by security team and red team before production

## 1. Purpose & Scope

This document enumerates threats to the Aikonos platform, the assets they target, and the mitigations. It is a living document — revised on every architecture change, incident, and red team engagement.

**In scope**: Aikonos platform, agents, skills, MCP connectors, inter-agent comms, data flows between users and agents, control plane, special security accounts.

**Out of scope**: physical security of the data center, internal Anthropic/LLM-provider security, enterprise IdP security (assumed trustworthy per federation agreement).

**Trust boundaries**: visualized in §3.

## 2. Assets

| Asset | Sensitivity | Notes |
|---|---|---|
| User workspace data (conversations, artifacts, RAG indices) | High | Per-user, per-tenant; may contain regulated data |
| Capability tokens (biscuits) | High | Short-lived but sufficient for abuse if stolen mid-task |
| SPIFFE SVIDs | High | Workload identities; short-lived |
| Policy definitions | High | Compromise = silent privilege escalation |
| Audit log | Critical | Tampering destroys forensic capability |
| LLM prompts/outputs | High (content) | May carry sensitive data downstream |
| MCP connector credentials (OAuth tokens, API keys) | Critical | Access to external systems (Gmail, GitHub, SIEM, etc.) |
| MITM CA private key (SSL inspection) | Critical | Compromise = cluster-wide MITM |
| Tenant encryption keys (KMS) | Critical | Compromise = offline decryption of tenant data |
| Broker signing key | Critical | Compromise = mint arbitrary capabilities |
| Skill/MCP signing keys (Sigstore) | Critical | Compromise = deploy malicious code as trusted |
| Control-plane service identities | Critical | `aikonos-netsec`, `aikonos-iam`, etc. |

## 3. Trust Boundaries

> **Topology note.** The diagram below describes the original Kubernetes deployment topology
> (control plane, sandbox pool, tenant MCS separation). Aikonos currently deploys via Docker Compose;
> the logical trust boundaries (identity verification, policy evaluation, audit at each crossing)
> remain the same, but the sandbox pool tier is not yet wired under Compose. See README Status.

```
                 ┌───────────────────────────────────────────┐
                 │              PUBLIC INTERNET              │
                 └──────────────────┬────────────────────────┘
                                    │  mTLS, WAF
                 ═══════════════════╪═══════════════════════ ← boundary: external / DMZ
                                    ▼
                 ┌───────────────────────────────────────────┐
                 │      API Gateway, Web Frontend            │
                 └──────────────────┬────────────────────────┘
                 ═══════════════════╪═══════════════════════ ← boundary: DMZ / control plane
                                    ▼
                 ┌───────────────────────────────────────────┐
                 │      Control Plane (Broker, PEP, IdP,     │
                 │      Vault, KMS, Audit)                   │
                 └──────────────────┬────────────────────────┘
                 ═══════════════════╪═══════════════════════ ← boundary: control / data plane
                                    ▼
                 ┌───────────────────────────────────────────┐
                 │          Sandbox Pool (data plane)        │
                 │  ┌─────────┐ ┌─────────┐ ┌─────────┐      │
                 │  │ Sandbox │ │ Sandbox │ │ Sandbox │      │
                 │  │  (t1)   │ │  (t1)   │ │  (t2)   │      │
                 │  └─────────┘ └─────────┘ └─────────┘      │
                 └──────────────────┬────────────────────────┘
                 ═══════════════════╪═══════════════════════ ← boundary: tenant / tenant (MCS, SPIFFE, NATS accounts)
                                    ▼
                 ┌───────────────────────────────────────────┐
                 │   External Systems (MCPs, LLM providers,  │
                 │   SaaS connectors)                        │
                 └───────────────────────────────────────────┘
                 ═══════════════════ ← boundary: Aikonos / external (egress proxy, DLP, SSL inspection)
```

Every boundary is a point where:
- Identity is verified (mTLS, OIDC, SPIFFE)
- Policy is evaluated
- Audit is emitted
- Data is classified/sanitized

## 4. Threats (STRIDE + LLM-specific)

### 4.1 Spoofing

| ID | Threat | Target | Impact | Likelihood | Mitigations |
|---|---|---|---|---|---|
| S-01 | Attacker impersonates user via stolen OIDC token | User session | Account takeover | Medium | Short TTL tokens, device binding, WebAuthn, risk-based re-auth |
| S-02 | Rogue workload presents forged SPIFFE SVID | Inter-service auth | Lateral movement | Low | SVID issued only to attested workloads; trust domain isolation |
| S-03 | Spoofed Task Envelope appears to come from manager | Inter-agent delegation | Unauthorized task execution on recipient | Medium | JWS signature by Broker (not by sender sandbox); sender attestation |
| S-04 | Malicious skill masquerades as trusted skill | Skill registry | Execution of malicious code | Medium | Cosign signatures, SBOM, provenance attestation, signing key in HSM |
| S-05 | Phishing attack replicates Aikonos frontend | User credentials | Credential theft | Medium | WebAuthn resists phishing; domain pinning; user training |
| S-06 | MCP server spoofed / traffic hijacked | MCP connector | Data exfil or malicious tool results | Low–Medium | mTLS to MCP, certificate pinning, egress proxy enforcement |

### 4.2 Tampering

| ID | Threat | Target | Impact | Likelihood | Mitigations |
|---|---|---|---|---|---|
| T-01 | Policy modification to grant excess privileges | Policy repo | Silent privilege escalation | Medium | GitOps + signed commits + CODEOWNERS + four-eyes review; detect via policy diff audit |
| T-02 | Audit log tampering to hide malicious activity | Audit store | Loss of forensic capability | Medium | Write-once object storage with object lock; dual-destination logging (SIEM + archive); cryptographic chaining |
| T-03 | Skill package altered after signing | Skill registry | Trojan execution | Low | Sigstore verification on pull; content-addressed storage |
| T-04 | Capability token modified to widen scope | Biscuit token | Privilege escalation | Low | Biscuit attenuation-only guarantee; signature verification at Tool Proxy; nonce replay protection |
| T-05 | Configuration drift from approved state | Compose stack / K8s cluster state | Unauthorized config | Medium | Under Compose: `compose-verify.sh` + `compose-verify-hardening.sh` CI validation; `compose.yaml` in Git with PR review. Under k8s (production target): ArgoCD drift detection, admission controllers (Kyverno/Gatekeeper). |
| T-06 | LLM response tampered in transit | Inference gateway | Manipulated agent behavior | Low | mTLS on all inference channels; internal inference over private network |
| T-07 | Modified skill at runtime (on disk) | Sandbox | Altered behavior | Low | Read-only rootfs; integrity measured at start; process integrity monitoring |

### 4.3 Repudiation

| ID | Threat | Target | Impact | Likelihood | Mitigations |
|---|---|---|---|---|---|
| R-01 | User denies initiating a destructive task | Audit trail | Dispute, compliance failure | Medium | Non-repudiable auth (WebAuthn), full session trace with trace IDs, approval signatures |
| R-02 | Agent action can't be attributed to initiating user | Inter-agent delegation chain | Forensic blind spot | Medium | Envelope delegation chain captured; both delegator and executor in audit |
| R-03 | Broker action not attributable to specific request | Broker audit | Investigation gap | Low | All Broker actions tagged with trace ID, request ID, user context |

### 4.4 Information Disclosure

| ID | Threat | Target | Impact | Likelihood | Mitigations |
|---|---|---|---|---|---|
| I-01 | Cross-tenant data leakage via shared sandbox node | Workspace data | Tenant breach | Low–Medium | SPIFFE trust domains per tenant; NATS accounts per tenant; MCS categories; dedicated node pools for high-sensitivity |
| I-02 | Prompt injection in retrieved content triggers data exfil | Any in-context data | Exfil via tool call | **High** | Plan validation; DLP on write_external; content/instruction separation; egress allowlists |
| I-03 | LLM outputs contain training data or other users' content | User-visible output | Privacy/confidentiality breach | Medium | Local inference for sensitive tiers; per-tenant model isolation for fine-tuned models |
| I-04 | Side-channel leakage (cache, timing) between co-located sandboxes | Memory contents | Cross-tenant disclosure | Low | SMT disabled on high-sensitivity nodes; dedicated nodes; kernel mitigations current |
| I-05 | Secrets logged accidentally | Audit log | Secret exposure | Medium | Redaction pipeline on audit ingest; secret detection scans; developer lint rules |
| I-06 | MCP connector leaks other-tenant data (multi-tenant MCPs) | External system | Data leak | Medium | Per-tenant MCP instances where possible; MCP sandbox isolation; connector-level tenancy checks |
| I-07 | Stolen capability token used to exfil via Tool Proxy | Workspace data | Data exfil | Low | Single-use nonces; short TTL; anomaly detection on token use; DLP |
| I-08 | Compromised sandbox reads mounted workspace subpath | Partial workspace | Limited data leak | Medium | Minimal subpath mount per task; read-only where possible; audit on read |
| I-09 | Vector DB query exposes embeddings reconstructible to source text | RAG content | Partial disclosure | Low | Per-tenant isolation; no cross-tenant retrieval; consider encrypted embeddings for highest tier |

### 4.5 Denial of Service

| ID | Threat | Target | Impact | Likelihood | Mitigations |
|---|---|---|---|---|---|
| D-01 | Runaway agent loop exhausts LLM budget | Cost / inference capacity | Service degradation, financial | **High** | Per-task cost budget; per-user rate limit; plan depth limit; loop detection on parent_task_id chain |
| D-02 | Inter-agent envelope storm (fan-out attack) | NATS bus, recipient queues | Platform unavailable | Medium | Per-sender rate limits; queue depth limits; circuit breakers on routing |
| D-03 | Sandbox resource exhaustion (CPU, RAM) affects node | Node availability | Noisy neighbor, eviction | Medium | cgroup limits, K8s resource quotas, PID limits, CPU throttling |
| D-04 | Policy engine flooded with decisions | PEP | Auth failures cluster-wide | Low | Decision caching with invalidation; rate limits; PEP horizontal scaling; fail-closed default |
| D-05 | Broker overwhelmed by task submissions | Broker | Service down | Low | Per-tenant quotas; admission control; horizontal scaling; queueing with shed |
| D-06 | Malicious skill spins up infinite sub-sandboxes | Cluster capacity | Capacity exhaustion | Medium | Sub-agent depth + count caps enforced at Broker; per-user concurrent sandbox limit |
| D-07 | Audit log write pipeline backpressure | Audit integrity | Lost events or stalled actions | Medium | Local spool with bounded buffer; alert on backpressure; degraded mode: fail-closed on audit failure for high-risk actions |

### 4.6 Elevation of Privilege

| ID | Threat | Target | Impact | Likelihood | Mitigations |
|---|---|---|---|---|---|
| E-01 | Sandbox escape via kernel exploit | Host node | Full node compromise | Low | Kata microVM for high-risk classes; rapid patching via immutable OS; defense-in-depth |
| E-02 | Sandbox escape via gVisor Sentry bug | Host node | Full node compromise | Low | Use Kata where stakes are high; gVisor for lower-risk tasks only |
| E-03 | Privilege escalation within sandbox (container breakout) | Sandbox | Higher capabilities within sandbox | Low | Rootless, dropped caps, no_new_privs, read-only rootfs, Landlock |
| E-04 | Agent chains legitimate tool calls to achieve unauthorized outcome | Authorization model | Effective privilege escalation | **Medium** | Plan-level validation, not just per-step; cost/complexity heuristics; anomaly detection |
| E-05 | Capability token attenuation bypass | Biscuit implementation | Unauthorized actions | Low | Use battle-tested biscuit library; regression tests; formal properties verified |
| E-06 | Delegation chain extends beyond intended scope | Inter-agent delegation | Unauthorized execution | Medium | Attenuation enforced by biscuit; depth limit; policy validation at each hop |
| E-07 | Compromised meta-agent abuses its platform-wide read | Cross-tenant disclosure | Serious breach | Low | Meta-agents have narrow scopes; anomaly agent can't act; janitor can't read workspace content |
| E-08 | Compromised Broker mints arbitrary capabilities | Everything | Catastrophic | Very Low | HSM-backed signing key; Broker hardening; split components with separate identities; monitoring for unusual minting patterns |
| E-09 | Stolen admin/break-glass account | Full platform | Catastrophic | Low | MFA + physical token; session recording; alerting on use; time-limited; regular rotation |
| E-10 | Supply-chain: malicious dependency in skill | Sandbox then possibly escape | Code execution in sandbox | Medium | SBOM, dependency scanning, vendored builds, signed releases, no runtime package install |

### 4.7 LLM-Specific Threats

These sit somewhat outside STRIDE but are central to Aikonos's risk profile.

| ID | Threat | Target | Impact | Likelihood | Mitigations |
|---|---|---|---|---|---|
| L-01 | Direct prompt injection (user input) | Agent behavior | Unauthorized actions | **High** | Plan validation; policy enforcement regardless of LLM reasoning; approval gates |
| L-02 | Indirect prompt injection (retrieved content, tool results) | Agent behavior | Unauthorized actions, exfil | **High** | Instruction/data separation; provenance tags on retrieved content; DLP; untrusted-content processing isolation |
| L-03 | Jailbreak via creative framing | Model guardrails | Unsafe outputs | Medium | Platform policy is independent of model alignment; we do not trust the model to refuse |
| L-04 | Sensitive data leaking to external LLM provider | Tenant data | Provider-side exposure | Medium (if using external LLMs) | Local inference for sensitive tiers; DLP pre-send; provider-specific data handling contracts |
| L-05 | Model inversion / training data leakage in fine-tuned models | Prior tenant data | Cross-tenant disclosure | Low | Per-tenant fine-tunes only; no shared fine-tunes across tenants; document in trust boundary |
| L-06 | Adversarial tool-call hallucination (phantom tool args) | Schema validation | Attempted unauthorized action | Medium | Strict schema validation at Tool Proxy; unknown-tool rejection; args-within-scope check |
| L-07 | Chain-of-thought exfiltration via reply channels | Leaked reasoning | Disclosure | Low | Agent CoT not persisted to audit in full; redaction on reply channels |
| L-08 | LLM output used to construct malicious plan the agent then executes | Self-loop exploit | Unauthorized actions | Medium | Plan validation is not bypassed by how the plan was generated |

## 5. Top Risks (Prioritized)

Based on likelihood × impact:

1. **L-02 Indirect prompt injection** → exfil or unauthorized action. Single biggest ongoing risk; continuous research needed.
2. **D-01 Runaway agent loops** → cost DoS. Operationally likely; quota infrastructure is mandatory from MVP.
3. **E-04 Tool chain privilege escalation** → legitimate-looking plan achieves unauthorized outcome. Requires plan-level (not just step-level) analysis.
4. **I-02 Data exfil via prompt injection** (special case of L-02) — warrants its own monitoring.
5. **T-01 Policy modification** → silent privilege escalation. GitOps + signing + review essential.
6. **T-02 Audit tampering** → blinds the defender. Object-lock storage non-negotiable.
7. **I-01 Cross-tenant leakage** → existential for multi-tenant deployments.
8. **E-08 Broker compromise** → low likelihood, catastrophic impact. Justifies HSM + minimal surface.
9. **L-04 External LLM data exposure** → contractual + technical controls; local inference for sensitive tiers.
10. **S-04 / E-10 Supply chain** (malicious skill/dependency) → rigorous publish pipeline.

## 6. Residual Risk

Even with all mitigations applied:

- **Prompt injection is not fully solvable**. Treat it as "contain the blast radius" not "prevent."
- **Insider threat by authorized users** is covered by audit/anomaly detection but not prevention.
- **Zero-day kernel exploits** against host can compromise isolation layers despite defense-in-depth.
- **LLM provider-side incidents** (if using external providers) are outside our control.
- **Social engineering of break-glass holders** is mitigated by procedure but not eliminated.

These residual risks are acknowledged, monitored, and included in incident response playbooks.

## 7. Key Assumptions

This threat model rests on:

1. Enterprise IdP is trustworthy and correctly federated.
2. Hardware root of trust (TPM) is intact on all nodes.
3. Sigstore / transparency logs for signing keys are trustworthy.
4. Underlying cloud provider (if used) meets baseline security guarantees.
5. Operators run patching within defined SLAs (critical patches within 72h).
6. Red team engagements happen at least annually.
7. Incident response playbooks exist and are rehearsed.

If any of these assumptions break, the model must be re-evaluated.

## 8. Review & Revision Process

This threat model is:

- **Reviewed** on every major architecture change (Broker, policy, sandbox, network model)
- **Updated** on every significant incident (lessons learned → new threat or mitigation)
- **Tested** via red team engagement annually
- **Cross-checked** against external frameworks (MITRE ATT&CK for Enterprise + Containers, OWASP LLM Top 10, CSA guidance)
- **Published internally** with change log; diffs reviewed by security team
- **Versioned** alongside the platform; v0.1 correlates with Aikonos architecture v0.1

## 9. Action Items From This Draft

Before production launch:

- [ ] Red team engagement covering L-01, L-02, E-04 specifically
- [ ] Build automated test harness for sandbox escape scenarios (§15 of sandbox doc)
- [ ] Implement audit log chaining + object lock before any production data
- [ ] HSM integration for Broker signing key; documented key ceremony
- [ ] Document and rehearse incident response for top 10 risks
- [ ] External security review of policy model (OpenFGA + OPA interaction)
- [ ] Formal verification or at minimum property-based tests for biscuit attenuation
- [ ] Supply-chain pipeline review (SBOM, provenance, signing, admission)
- [ ] Cross-tenant isolation test suite run continuously in CI

## 10. Open Questions

1. **Is a single threat model sufficient, or do we need per-deployment (per-enterprise) variants?** Probably a shared base + enterprise-specific addenda for their integrations.
2. **Do we publish the threat model to customers?** Sanitized version yes — it is a trust-building artifact for security-conscious buyers.
3. **How do we threat-model enterprise-contributed skills and MCPs?** Each submission needs a lightweight threat review; automation where possible.
4. **At what point do we formalize with tools like Microsoft TMT or IriusRisk?** When we're past MVP; early iteration stays in markdown for velocity.
