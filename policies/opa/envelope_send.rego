package aikonos.envelope_send

import future.keywords.contains
import future.keywords.if
import future.keywords.in

# ── Send decision (evaluated on sender side by Broker) ────────────────────────

default allow := false

allow if {
    input.fga_send_decision == "allow"
    input.envelope.delegation.depth < 5
    count(scope_violations) == 0
    count(deny_reasons) == 0
}

# ── Attenuation check ─────────────────────────────────────────────────────────
# Sender cannot delegate scopes they don't hold.

scope_violations contains msg if {
    some scope in input.envelope.delegation.attenuated_scopes
    not scope in input.sender.capability_scopes
    msg := sprintf("Scope '%s' is not in sender's capability set and cannot be delegated.", [scope])
}

# ── Deny reasons ──────────────────────────────────────────────────────────────

deny_reasons contains msg if {
    input.envelope.delegation.max_cost_units > input.sender.remaining_cost_budget
    msg := "Delegation cost budget exceeds sender's remaining budget."
}

deny_reasons contains msg if {
    input.sender.risk_score > 70
    msg := "Sender's risk score is elevated. Delegation is suspended."
}

deny_reasons contains msg if {
    input.envelope.delegation.depth >= 5
    msg := "Maximum delegation depth (5) reached. No further delegation permitted."
}

# ── Auto-accept decision (evaluated on recipient side by Broker) ──────────────

default auto_accept := false

auto_accept if {
    input.relationship == "direct_report"
    input.envelope.task.effect_class in ["read_only", "write_local"]
    input.envelope.delegation.max_cost_units <= 100
    input.sender.risk_score < 40
    not input.sender.is_suspended
}

# ── Cross-group always requires manual acceptance ─────────────────────────────

require_manual_acceptance if {
    input.sender.group_id != input.recipient.group_id
}
