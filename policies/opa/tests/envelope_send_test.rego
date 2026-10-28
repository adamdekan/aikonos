package aikonos.envelope_send_test

import future.keywords.if
import future.keywords.in

import data.aikonos.envelope_send

# M-17 / AG-11: envelope_send had no tests. Input mirrors what the broker sends
# (policy.Engine.DecideEnvelopeSend).

base := {
    "fga_send_decision": "allow",
    "relationship": "peer",
    "envelope": {
        "task": {"effect_class": "write_external"},
        "delegation": {
            "depth": 1,
            "attenuated_scopes": ["siem:read"],
            "max_cost_units": 50,
        },
    },
    "sender": {
        "capability_scopes": ["siem:read", "slack:write"],
        "risk_score": 0,
        "remaining_cost_budget": 1000,
        "is_suspended": false,
        "group_id": "",
    },
    "recipient": {"group_id": ""},
}

with_delegation(d) := object.union(base, {"envelope": {"task": base.envelope.task, "delegation": object.union(base.envelope.delegation, d)}})

with_sender(s) := object.union(base, {"sender": object.union(base.sender, s)})

# ── allow ────────────────────────────────────────────────────────────────────

test_attenuated_subset_allowed if {
    envelope_send.allow with input as base
}

test_empty_scopes_allowed if {
    envelope_send.allow with input as with_delegation({"attenuated_scopes": []})
}

# ── attenuation ──────────────────────────────────────────────────────────────

test_scope_outside_sender_denied if {
    inp := with_delegation({"attenuated_scopes": ["siem:read", "doc:write"]})
    not envelope_send.allow with input as inp
    "Scope 'doc:write' is not in sender's capability set and cannot be delegated." in envelope_send.scope_violations with input as inp
}

test_sender_with_no_scopes_cannot_delegate if {
    not envelope_send.allow with input as with_sender({"capability_scopes": []})
}

# ── relationship (FGA) ───────────────────────────────────────────────────────

test_fga_deny_denies if {
    not envelope_send.allow with input as object.union(base, {"fga_send_decision": "deny"})
}

test_missing_fga_decision_denies if {
    not envelope_send.allow with input as object.remove(base, ["fga_send_decision"])
}

# ── depth ────────────────────────────────────────────────────────────────────

test_depth_four_allowed if {
    envelope_send.allow with input as with_delegation({"depth": 4})
}

test_depth_five_denied if {
    inp := with_delegation({"depth": 5})
    not envelope_send.allow with input as inp
    "Maximum delegation depth (5) reached. No further delegation permitted." in envelope_send.deny_reasons with input as inp
}

# ── cost ─────────────────────────────────────────────────────────────────────

test_cost_over_remaining_budget_denied if {
    inp := with_delegation({"max_cost_units": 1001})
    not envelope_send.allow with input as inp
    "Delegation cost budget exceeds sender's remaining budget." in envelope_send.deny_reasons with input as inp
}

test_cost_equal_to_budget_allowed if {
    envelope_send.allow with input as with_delegation({"max_cost_units": 1000})
}

# ── sender risk ──────────────────────────────────────────────────────────────

test_elevated_risk_denied if {
    not envelope_send.allow with input as with_sender({"risk_score": 71})
}

# ── malformed input fails closed ─────────────────────────────────────────────

test_empty_input_denied if {
    not envelope_send.allow with input as {}
}

test_missing_delegation_denied if {
    inp := json.remove(base, ["envelope/delegation"])
    not envelope_send.allow with input as inp
}

# ── auto-accept ──────────────────────────────────────────────────────────────

auto_base := object.union(base, {
    "relationship": "direct_report",
    "envelope": {
        "task": {"effect_class": "read_only"},
        "delegation": base.envelope.delegation,
    },
})

test_auto_accept_direct_report_low_risk if {
    envelope_send.auto_accept with input as auto_base
}

test_peer_never_auto_accepted if {
    not envelope_send.auto_accept with input as base
}

test_write_external_not_auto_accepted if {
    inp := object.union(auto_base, {"envelope": {"task": {"effect_class": "write_external"}, "delegation": base.envelope.delegation}})
    not envelope_send.auto_accept with input as inp
}

test_suspended_sender_not_auto_accepted if {
    not envelope_send.auto_accept with input as object.union(auto_base, {"sender": object.union(base.sender, {"is_suspended": true})})
}

test_cross_group_requires_manual_acceptance if {
    envelope_send.require_manual_acceptance with input as with_sender({"group_id": "g1"})
}
