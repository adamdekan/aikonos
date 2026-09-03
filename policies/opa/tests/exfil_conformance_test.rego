package aikonos.exfil_conformance_test

# Golden-vector conformance suite (F6) — asserts the exfil/DLP rule fires
# identically across both rego rules for the shared 13-vector matrix in
# policies/opa/conformance/exfil_dlp_vectors.json. See
# .

import future.keywords.every
import future.keywords.if
import future.keywords.in

import data.aikonos.plan_validation
import data.aikonos.tool_invocation

vectors := data.conformance.exfil_dlp.vectors

tool_invocation_dlp_msg := "Plan reads sensitive data and writes externally without DLP attestation. DLP review required."

plan_validation_dlp_msg := "Plan reads sensitive data and writes externally. DLP attestation required before this plan can be approved."

# ── Count guard ───────────────────────────────────────────────────────────────

test_vector_count_is_13 if {
    count(vectors) == 13
}

# ── tool_invocation: per-call realization ────────────────────────────────────

tool_invocation_input(vector) := {
    "user": {"id": "alice@corp", "risk_score": 10, "is_on_call": false},
    "tool": {"id": "conformance.tool", "effect_class": vector.effect_class},
    "resource": {"ref": "aikonos:conformance:vector", "sensitivity": "internal"},
    "plan": {
        "reads_sensitive": vector.reads_sensitive,
        "has_dlp_attestation": vector.has_dlp_attestation,
    },
    "approval": {"is_approved": false},
    "actor": {"spiffe_id": "spiffe://aikonos.com/broker"},
    "fga_decision": "allow",
    "env": {"hour": 14, "day": "monday"},
}

test_tool_invocation_fires_iff_vector_says_so if {
    every vector in vectors {
        deny_reasons := tool_invocation.deny_reasons with input as tool_invocation_input(vector)
        vector.exfil_rule_fires == (tool_invocation_dlp_msg in deny_reasons)
    }
}

# ── plan_validation: two-step plan realization ───────────────────────────────

plan_validation_input(vector) := {
    "plan": {
        "plan_id": "conformance-plan",
        "steps": [
            {
                "seq": 1,
                "tool_id": "siem.query",
                "effect_class": "read_only",
                "estimated_cost": 1,
                "reads_sensitive": vector.reads_sensitive,
                "justification": "",
                "args": {},
            },
            {
                "seq": 2,
                "tool_id": "conformance.tool",
                "effect_class": vector.effect_class,
                "estimated_cost": 1,
                "reads_sensitive": false,
                "justification": "conformance vector step",
                "args": {},
            },
        ],
        "has_dlp_attestation": vector.has_dlp_attestation,
    },
    "task": {"cost_budget": 1000, "effect_class_ceiling": "destructive"},
    "actor": {"user_id": "alice@corp", "is_control_plane": true},
}

test_plan_validation_fires_iff_vector_says_so if {
    every vector in vectors {
        violations := plan_validation.violations with input as plan_validation_input(vector)
        vector.exfil_rule_fires == (plan_validation_dlp_msg in violations)
    }
}
