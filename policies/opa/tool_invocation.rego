package aikonos.tool_invocation

import future.keywords.contains
import future.keywords.if
import future.keywords.in

# BEGIN generated effectclass routing — do not edit by hand (go generate ./broker/internal/effectclass/...)
auto_classes := {"read_only", "write_local"}

approval_classes := {"credential_access", "destructive", "infrastructure", "network_egress", "write_external"}

stepup_classes := {"credential_access", "destructive", "infrastructure"}

# END generated effectclass routing

# ── Main decisions ────────────────────────────────────────────────────────────

default allow := false

default require_approval := false

default require_step_up := false

allow if {
	input.fga_decision == "allow"
	input.tool.effect_class in auto_classes
	count(deny_reasons) == 0
}

require_approval if {
	input.fga_decision == "allow"
	input.tool.effect_class in approval_classes
	count(deny_reasons) == 0
}

require_step_up if {
	input.fga_decision == "allow"
	input.tool.effect_class in stepup_classes
}

# ── Deny reasons ──────────────────────────────────────────────────────────────
# These produce human-readable explanations safe to surface to the user/agent.
# Never expose policy rule internals.

deny_reasons contains msg if {
	input.user.risk_score > 70
	input.tool.effect_class != "read_only"
	msg := "Your account's risk score is elevated. Read-only actions permitted only."
}

deny_reasons contains msg if {
	not business_hours
	input.tool.effect_class in ["destructive", "infrastructure"]
	not input.user.is_on_call
	msg := "Destructive and infrastructure actions are restricted to business hours unless on-call."
}

deny_reasons contains msg if {
	input.resource.sensitivity == "production"
	input.tool.effect_class in ["destructive", "infrastructure"]
	not input.approval.is_approved
	msg := "Production resource modifications require prior approval."
}

deny_reasons contains msg if {
	# Prevent prompt-injection data exfil pattern:
	# if a task reads sensitive data AND writes external, require DLP attestation
	input.plan.reads_sensitive == true
	input.tool.effect_class in ["write_external", "network_egress"]
	not input.plan.has_dlp_attestation
	msg := "Plan reads sensitive data and writes externally without DLP attestation. DLP review required."
}

deny_reasons contains msg if {
	# Infrastructure actions must come from a control-plane identity, never a
	# user sandbox (sandbox SVIDs are spiffe://aikonos.com/sandbox/<id>).
	input.tool.effect_class == "infrastructure"
	startswith(input.actor.spiffe_id, "spiffe://aikonos.com/sandbox/")
	msg := "Infrastructure actions cannot be initiated from a user sandbox."
}

deny_reasons contains msg if {
	# User does not have can_invoke on this skill in OpenFGA.
	# Guard on "deny" (not != "allow") so legacy callers that pass empty — which
	# buildToolInput defaults to "allow" before OPA sees it — are never affected.
	input.fga_decision == "deny"
	msg := sprintf("You do not have access to tool %q. Ask a tenant admin to grant skill access via a group.", [input.tool.id])
}

# ── Context helpers ───────────────────────────────────────────────────────────

business_hours if {
	input.env.hour >= 7
	input.env.hour < 19
	not input.env.day in ["saturday", "sunday"]
}

# ── Deny if any deny reason exists ───────────────────────────────────────────
deny if {
	count(deny_reasons) > 0
}
