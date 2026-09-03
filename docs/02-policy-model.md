# Aikonos Policy Model

**Component**: Authorization & policy engine
**Version**: v0.1
**Status**: Design

## 1. Design Principles

1. **Deny by default** — nothing is allowed unless a policy explicitly grants it
2. **Attenuation only** — delegation can narrow scope, never widen
3. **Contextual** — decisions depend on user, group, resource, action, environment, risk
4. **Auditable** — every decision produces a structured, reviewable record
5. **Testable** — policies must be unit-testable offline, with test fixtures
6. **Layered** — multiple engines cooperate; no single policy expresses everything
7. **Human-reviewable** — policies stored as code, reviewed via PR, deployed via GitOps

## 2. Why Hybrid (ReBAC + ABAC + RBAC)

Pure RBAC fails because:
- Agent delegation chains create relationships RBAC can't express cleanly
- Need contextual controls (time, risk, data classification) that are attribute-based
- Resources have ownership and sharing semantics (workspaces, conversations) that are relational

The model combines:

- **RBAC**: coarse-grained group → permission bindings (e.g., "devs can use github skills")
- **ReBAC**: relationships (owner, member, delegate, approver) between principals and resources
- **ABAC**: attribute-based rules over user, resource, action, environment

## 3. Policy Engine Stack

Two engines, two roles:

### OpenFGA — Relationship-centric decisions

Used for: "can user X perform action Y on resource Z?"

Fine-grained authorization store. Models entities, relations, and computed permissions.

### OPA (Rego) — Contextual / environmental decisions

Used for: admission control (K8s), plan validation rules, step-level policy with rich context.

OPA integrates with the Broker as a sidecar or library; latency < 5ms per decision on cached policies.

### Division of labor

| Question | Engine |
|---|---|
| Can Alice read workspace X? | OpenFGA |
| Is Alice a member of group "managers"? | OpenFGA |
| Can Alice delegate skill Y to Bob? | OpenFGA + OPA (context: delegation depth) |
| Is this tool call allowed given time of day, sensitivity, risk score? | OPA |
| Does this plan step exceed cost budget? | OPA |
| Is this admission into K8s allowed? | OPA / Gatekeeper |

## 4. OpenFGA Model

Example model in OpenFGA DSL:

```
model
  schema 1.1

type user

type group
  relations
    define member: [user, group#member]
    define manager: [user]

type tenant
  relations
    define admin: [user]
    define member: [user, group#member]

type workspace
  relations
    define tenant: [tenant]
    define owner: [user]
    define editor: [user, group#member]
    define viewer: [user, group#member] or editor
    define can_read: viewer
    define can_write: editor or owner
    define can_delete: owner

type skill
  relations
    define tenant: [tenant]
    define permitted_group: [group#member]
    define can_invoke: permitted_group

type mcp_connector
  relations
    define tenant: [tenant]
    define permitted_group: [group#member]
    define can_invoke: permitted_group

type task
  relations
    define owner: [user]
    define delegate: [user]
    define approver: [user, group#member]
    define can_approve: approver
    define can_cancel: owner or approver

type agent
  relations
    define spawned_by: [user, agent]
    define owner_user: [user]
    define can_delegate_to_user: owner_user
```

**Example checks**:
- `check(user:alice, can_invoke, skill:siem_query)` → yes/no
- `check(user:alice, can_approve, task:uuid-123)` → yes/no
- `check(user:alice, can_delegate_to_user, user:bob)` → yes/no (governs envelope sending)

## 5. OPA Policy Examples

### 5.1 Tool invocation policy

```rego
package aikonos.tool_invocation

import future.keywords.if
import future.keywords.in

default allow := false

# Base allow: OpenFGA says user can invoke, and effect class is below threshold
allow if {
    input.fga_decision == "allow"
    input.tool.effect_class in ["read_only", "write_local"]
    not high_risk_context
}

# Require approval for destructive / external actions
require_approval if {
    input.fga_decision == "allow"
    input.tool.effect_class in ["write_external", "destructive", "credential_access"]
}

# Step-up auth for destructive actions on production data
require_step_up if {
    input.tool.effect_class == "destructive"
    input.resource.sensitivity == "production"
}

# Deny outside business hours for high-risk tools unless on-call
deny_reason := "outside business hours for high-risk tool" if {
    input.tool.effect_class in ["destructive", "credential_access"]
    not business_hours
    not input.user.roles[_] == "on_call"
}

# Deny if risk score elevated
deny_reason := "user risk score elevated" if {
    input.user.risk_score > 70
    input.tool.effect_class != "read_only"
}

business_hours if {
    input.env.hour >= 7
    input.env.hour < 19
    not input.env.day in ["saturday", "sunday"]
}

high_risk_context if {
    input.user.risk_score > 70
}

high_risk_context if {
    input.resource.sensitivity == "production"
    input.tool.effect_class != "read_only"
}
```

### 5.2 Inter-agent envelope policy

```rego
package aikonos.envelope_send

default allow := false

# Users can send tasks to direct reports (ReBAC check delegates to FGA)
allow if {
    input.fga_send_decision == "allow"
    input.envelope.task.required_skills == subset_of_sender_skills
    input.envelope.delegation.attenuated_scopes == subset_of_sender_scopes
    input.envelope.depth < 5
}

# Auto-accept criteria for recipient (evaluated on recipient side)
auto_accept if {
    input.relationship == "direct_report"
    input.envelope.task.effect_class in ["read_only", "write_local"]
    input.envelope.task.cost_budget <= 100
    input.sender.risk_score < 40
}

# Always require explicit acceptance for cross-group delegation
require_acceptance if {
    input.sender.group != input.recipient.group
}
```

### 5.3 Plan validation policy

```rego
package aikonos.plan_validation

default allow := false

allow if {
    count(violations) == 0
}

violations contains msg if {
    some step in input.plan.steps
    step.effect_class == "destructive"
    not step.justification
    msg := sprintf("step %d destructive without justification", [step.seq])
}

violations contains msg if {
    total_cost := sum([s.estimated_cost | s := input.plan.steps[_]])
    total_cost > input.task.cost_budget
    msg := sprintf("plan cost %d exceeds budget %d", [total_cost, input.task.cost_budget])
}

violations contains msg if {
    some step in input.plan.steps
    step.tool_id == "shell.execute"
    input.task.effect_class_ceiling != "destructive"
    msg := "shell.execute not permitted at this effect ceiling"
}

violations contains msg if {
    # Prevent data exfil pattern: read sensitive + write external in same plan
    sensitive_reads := [s | s := input.plan.steps[_]; s.reads_sensitive]
    external_writes := [s | s := input.plan.steps[_]; s.effect_class == "write_external"]
    count(sensitive_reads) > 0
    count(external_writes) > 0
    not input.plan.has_dlp_attestation
    msg := "plan reads sensitive data and writes externally without DLP attestation"
}
```

## 6. Effect Classes

Every skill/tool declares its effect class. This is the primary risk taxonomy.

| Class | Meaning | Default policy |
|---|---|---|
| `read_only` | No state change (list, get, query, search) | Allow if FGA allows |
| `write_local` | Writes inside user workspace only | Allow if FGA allows |
| `write_internal` | Writes to tenant-shared resources | Allow if FGA allows, audit |
| `write_external` | Writes outside Aikonos (external API, email, Slack) | Require approval |
| `network_egress` | Arbitrary external network access | Require approval, DLP scan |
| `credential_access` | Reads secrets, tokens, keys | Require approval + step-up auth |
| `destructive` | Deletes, truncates, disables | Require approval + step-up auth + cooldown |
| `infrastructure` | Modifies Aikonos itself (fw, policies, users) | Control-plane identity only |

Effect classes are **declared in the skill manifest and verified at publish time** — you can't lie about your effect class and slip through.

## 7. Capability Scope Grammar

Capabilities issued to sandboxes use a structured scope format:

```
<domain>:<action>:<resource_selector>[:<constraints>]
```

Examples:
- `siem:query:incidents/q3:severity>=medium`
- `github:read:repo/acme-inc/*:branches=main`
- `email:send:external:recipients<=10,attachments=false`
- `fs:write:workspace/alice/scratch/*`

Scope parsing, matching, and attenuation happen in a shared library used by Broker, Tool Proxy, and sandbox stubs. Single source of truth.

## 8. Policy Authoring & Lifecycle

**Storage**: Git repo, one per tenant (or shared with tenant-specific overrides).

**Structure**:
```
policies/
├── rbac/
│   ├── groups.yaml            # group definitions
│   ├── role_bindings.yaml     # group → permissions
│   └── skill_assignments.yaml # skill → groups
├── fga/
│   ├── model.fga              # authorization model
│   └── tuples/                # relationship tuples (managed data)
├── opa/
│   ├── tool_invocation.rego
│   ├── envelope_send.rego
│   ├── plan_validation.rego
│   └── tests/
│       ├── tool_invocation_test.rego
│       └── ...
└── attributes/
    ├── risk_scoring.yaml      # how risk scores compute
    └── sensitivity_labels.yaml
```

**Lifecycle**:

1. PR with policy change
2. CI runs: syntax check, unit tests, policy linter (Regal for OPA), simulation against historical decisions
3. Reviewed by security team + resource owner
4. Merge → policies bind-mounted into the `opa` compose service; recreate to apply (`docker compose up -d --force-recreate opa broker`)
5. Canary rollout with shadow mode (evaluate new + old, compare, alert on divergence)
6. Promote to production
7. Old policy versions retained for audit replay

**Testing example** (OPA):

```rego
package aikonos.tool_invocation_test

import data.aikonos.tool_invocation

test_read_only_allowed if {
    tool_invocation.allow with input as {
        "user": {"risk_score": 20, "roles": ["developer"]},
        "tool": {"effect_class": "read_only", "id": "siem.query"},
        "fga_decision": "allow",
        "env": {"hour": 14, "day": "monday"}
    }
}

test_destructive_requires_approval if {
    tool_invocation.require_approval with input as {
        "user": {"risk_score": 20},
        "tool": {"effect_class": "destructive"},
        "fga_decision": "allow",
    }
}

test_high_risk_user_denied_writes if {
    tool_invocation.deny_reason == "user risk score elevated" with input as {
        "user": {"risk_score": 80},
        "tool": {"effect_class": "write_local"},
        "fga_decision": "allow",
    }
}
```

## 9. Risk Scoring

Risk score is a per-user, per-session dynamic value [0,100] computed by a separate risk service and fed into OPA as an attribute.

Inputs:
- Recent auth anomalies (new device, impossible travel)
- Historical action patterns (deviation from baseline)
- Outstanding security findings on user's workspace
- Time since last credential rotation
- Recent denied actions (noisy user = higher risk)

Output consumed by policies. Risk score updates invalidate cached policy decisions for that user.

## 10. Approvals

Approval workflow is first-class:

- Approval request includes: task summary, plan steps, risk assessment, requester, resource impact
- Approver set resolved dynamically (via FGA `can_approve` relation)
- Four-eyes required for `destructive` + `infrastructure` classes
- Approvers see diff if the plan is an amendment of a previously-approved plan
- Approvals are themselves audited — who approved what, when, with what context
- Time-limited: approval expires if task hasn't started within N minutes

## 11. Delegation Semantics (Manager → Report)

When Alice (manager) sends a task to Bob (report):

1. FGA: `check(user:alice, can_delegate_to_user, user:bob)` — must be allow
2. OPA `envelope_send`: validates scope attenuation, depth, context
3. Broker mints delegated capability: Alice's scope ∩ policy-permitted subset
4. Envelope routed to Bob's inbox
5. Bob's policy evaluates auto-accept vs. manual accept
6. On execution, Bob's agent runs with Bob's identity + the delegated capability
7. Audit records both: "Alice delegated to Bob", and "Bob executed on behalf of Alice"

**Crucial**: Bob cannot re-delegate to someone outside Alice's delegation scope. Biscuit attenuation plus a delegation depth counter enforces this.

## 12. Special Identities

Control-plane identities (`aikonos-netsec`, `aikonos-tlsinspect`, `aikonos-iam`, `aikonos-audit`) bypass normal user RBAC but are tightly constrained:

- Can only authenticate via workload SVID (SPIFFE), never password/OIDC
- All actions require signed commits in the GitOps repo (no ad-hoc calls)
- Four-eyes on every change via PR review
- Separate audit stream, shorter retention on immutable store but never deleted

## 13. Escape Hatches

Real-world operations require occasional exceptions. Design them in, don't let them be invented ad-hoc:

- **Break-glass user**: pre-provisioned, MFA + physical token, generates alert on use, session fully recorded, time-limited (auto-revokes in 1h)
- **Policy override flag**: on a task, with justification, two-person approval, loud audit
- **Maintenance mode**: per-tenant, disables agent execution except for meta-agents

## 14. Policy Debuggability

Every denial returns:
- Policy rule ID that matched
- Input that was evaluated (redacted where sensitive)
- Human-readable reason

Users and admins can query: "why was my action denied?" and get an actionable answer. Without this, policy becomes a black box and operators bypass it.

## 15. Open Questions

1. **Risk score model**: hand-tuned rules vs ML model. Start with rules; ML later only if clearly beneficial and explainable.
2. **Policy language ergonomics for non-security staff**: Rego is powerful but not friendly. Consider a YAML-based DSL on top for common cases, compiling to Rego.
3. **Cross-tenant federation policies**: when Tenant A federates with Tenant B, whose policies apply? Proposal: intersection (both must allow), with explicit federation agreement.
4. **Emergency policy push**: how fast can we push a "deny all tool X" rule cluster-wide? Target: < 30 seconds.
5. **Dry-run mode for new policies**: baked in from day one.
