package aikonos.tool_invocation_test

import future.keywords.if
import future.keywords.in

import data.aikonos.tool_invocation

# ── Helpers ───────────────────────────────────────────────────────────────────

base_input := {
    "user": {
        "id": "alice@corp",
        "risk_score": 10,
        "is_on_call": false,
    },
    "tool": {
        "id": "siem.query",
        "effect_class": "read_only",
    },
    "resource": {
        "ref": "aikonos:incidents:q3",
        "sensitivity": "internal",
    },
    "plan": {
        "reads_sensitive": false,
        "has_dlp_attestation": false,
    },
    "approval": {
        "is_approved": false,
    },
    "actor": {
        "spiffe_id": "spiffe://aikonos.com/sandbox/alice/task-001",
    },
    "fga_decision": "allow",
    "env": {
        "hour": 14,
        "day": "monday",
    },
}

# ── Allow cases ───────────────────────────────────────────────────────────────

test_read_only_allowed if {
    tool_invocation.allow with input as base_input
}

test_write_local_allowed if {
    tool_invocation.allow with input as json.patch(base_input, [
        {"op": "replace", "path": "/tool/effect_class", "value": "write_local"},
        {"op": "replace", "path": "/tool/id", "value": "doc.write"},
    ])
}

# ── Office document tools classification (checkpoint 7) ───────────────────────
# write_local ids auto-approve identically to test_write_local_allowed above.

test_docx_create_write_local_allowed if {
    tool_invocation.allow with input as json.patch(base_input, [
        {"op": "replace", "path": "/tool/effect_class", "value": "write_local"},
        {"op": "replace", "path": "/tool/id", "value": "docx.create"},
    ])
}

test_docx_edit_write_local_allowed if {
    tool_invocation.allow with input as json.patch(base_input, [
        {"op": "replace", "path": "/tool/effect_class", "value": "write_local"},
        {"op": "replace", "path": "/tool/id", "value": "docx.edit"},
    ])
}

test_xlsx_create_write_local_allowed if {
    tool_invocation.allow with input as json.patch(base_input, [
        {"op": "replace", "path": "/tool/effect_class", "value": "write_local"},
        {"op": "replace", "path": "/tool/id", "value": "xlsx.create"},
    ])
}

test_xlsx_edit_write_local_allowed if {
    tool_invocation.allow with input as json.patch(base_input, [
        {"op": "replace", "path": "/tool/effect_class", "value": "write_local"},
        {"op": "replace", "path": "/tool/id", "value": "xlsx.edit"},
    ])
}

test_xlsx_recalc_write_local_allowed if {
    tool_invocation.allow with input as json.patch(base_input, [
        {"op": "replace", "path": "/tool/effect_class", "value": "write_local"},
        {"op": "replace", "path": "/tool/id", "value": "xlsx.recalc"},
    ])
}

test_pptx_create_write_local_allowed if {
    tool_invocation.allow with input as json.patch(base_input, [
        {"op": "replace", "path": "/tool/effect_class", "value": "write_local"},
        {"op": "replace", "path": "/tool/id", "value": "pptx.create"},
    ])
}

test_pptx_edit_write_local_allowed if {
    tool_invocation.allow with input as json.patch(base_input, [
        {"op": "replace", "path": "/tool/effect_class", "value": "write_local"},
        {"op": "replace", "path": "/tool/id", "value": "pptx.edit"},
    ])
}

test_pptx_thumbnail_write_local_allowed if {
    tool_invocation.allow with input as json.patch(base_input, [
        {"op": "replace", "path": "/tool/effect_class", "value": "write_local"},
        {"op": "replace", "path": "/tool/id", "value": "pptx.thumbnail"},
    ])
}

test_pdf_create_write_local_allowed if {
    tool_invocation.allow with input as json.patch(base_input, [
        {"op": "replace", "path": "/tool/effect_class", "value": "write_local"},
        {"op": "replace", "path": "/tool/id", "value": "pdf.create"},
    ])
}

test_pdf_transform_write_local_allowed if {
    tool_invocation.allow with input as json.patch(base_input, [
        {"op": "replace", "path": "/tool/effect_class", "value": "write_local"},
        {"op": "replace", "path": "/tool/id", "value": "pdf.transform"},
    ])
}

test_office_convert_write_local_allowed if {
    tool_invocation.allow with input as json.patch(base_input, [
        {"op": "replace", "path": "/tool/effect_class", "value": "write_local"},
        {"op": "replace", "path": "/tool/id", "value": "office.convert"},
    ])
}

# ── Memory tools classification (checkpoint 4) ──────────────────────────────────
# read_only ids auto-approve identically to test_read_only_allowed above.

test_memory_read_read_only_allowed if {
    tool_invocation.allow with input as json.patch(base_input, [
        {"op": "replace", "path": "/tool/id", "value": "memory.read"},
    ])
}

test_memory_write_write_local_allowed if {
    tool_invocation.allow with input as json.patch(base_input, [
        {"op": "replace", "path": "/tool/effect_class", "value": "write_local"},
        {"op": "replace", "path": "/tool/id", "value": "memory.write"},
    ])
}

# read_only ids: base_input already carries effect_class "read_only".

test_docx_extract_read_only_allowed if {
    tool_invocation.allow with input as json.patch(base_input, [
        {"op": "replace", "path": "/tool/id", "value": "docx.extract"},
    ])
}

test_xlsx_extract_read_only_allowed if {
    tool_invocation.allow with input as json.patch(base_input, [
        {"op": "replace", "path": "/tool/id", "value": "xlsx.extract"},
    ])
}

test_pptx_extract_read_only_allowed if {
    tool_invocation.allow with input as json.patch(base_input, [
        {"op": "replace", "path": "/tool/id", "value": "pptx.extract"},
    ])
}

test_pdf_extract_read_only_allowed if {
    tool_invocation.allow with input as json.patch(base_input, [
        {"op": "replace", "path": "/tool/id", "value": "pdf.extract"},
    ])
}

test_web_search_read_only_allowed if {
    tool_invocation.allow with input as json.patch(base_input, [
        {"op": "replace", "path": "/tool/id", "value": "web.search"},
    ])
}

test_read_only_fga_deny_blocks if {
    not tool_invocation.allow with input as json.patch(base_input, [
        {"op": "replace", "path": "/fga_decision", "value": "deny"},
    ])
}

# ── Require approval cases ────────────────────────────────────────────────────

test_write_external_requires_approval if {
    tool_invocation.require_approval with input as json.patch(base_input, [
        {"op": "replace", "path": "/tool/effect_class", "value": "write_external"},
        {"op": "replace", "path": "/tool/id", "value": "email.send"},
    ])
}

test_network_egress_requires_approval if {
    tool_invocation.require_approval with input as json.patch(base_input, [
        {"op": "replace", "path": "/tool/effect_class", "value": "network_egress"},
    ])
}

test_credential_access_requires_approval if {
    tool_invocation.require_approval with input as json.patch(base_input, [
        {"op": "replace", "path": "/tool/effect_class", "value": "credential_access"},
    ])
}

test_destructive_requires_approval if {
    tool_invocation.require_approval with input as json.patch(base_input, [
        {"op": "replace", "path": "/tool/effect_class", "value": "destructive"},
    ])
}

test_destructive_requires_step_up if {
    tool_invocation.require_step_up with input as json.patch(base_input, [
        {"op": "replace", "path": "/tool/effect_class", "value": "destructive"},
    ])
}

test_fga_deny_destructive_not_require_step_up if {
    # fga_decision="deny" blocks require_step_up even for stepup-class tools;
    # the deny reason surfaces through deny_reasons instead.
    not tool_invocation.require_step_up with input as json.patch(base_input, [
        {"op": "replace", "path": "/fga_decision", "value": "deny"},
        {"op": "replace", "path": "/tool/effect_class", "value": "destructive"},
    ])
}

# ── Deny cases ────────────────────────────────────────────────────────────────

test_high_risk_user_denied_write if {
    not tool_invocation.allow with input as json.patch(base_input, [
        {"op": "replace", "path": "/user/risk_score", "value": 80},
        {"op": "replace", "path": "/tool/effect_class", "value": "write_local"},
    ])
}

test_high_risk_user_still_allowed_read_only if {
    # Read-only is allowed even with high risk score
    tool_invocation.allow with input as json.patch(base_input, [
        {"op": "replace", "path": "/user/risk_score", "value": 80},
    ])
}

test_out_of_hours_destructive_denied if {
    not tool_invocation.allow with input as json.patch(base_input, [
        {"op": "replace", "path": "/tool/effect_class", "value": "destructive"},
        {"op": "replace", "path": "/env/hour", "value": 23},
    ])
}

test_out_of_hours_on_call_still_denied_without_approval if {
    # On-call removes the business-hours deny reason, but destructive still
    # requires approval — so allow is still false
    not tool_invocation.allow with input as json.patch(base_input, [
        {"op": "replace", "path": "/tool/effect_class", "value": "destructive"},
        {"op": "replace", "path": "/env/hour", "value": 23},
        {"op": "replace", "path": "/user/is_on_call", "value": true},
    ])
    # But require_approval should fire
    tool_invocation.require_approval with input as json.patch(base_input, [
        {"op": "replace", "path": "/tool/effect_class", "value": "destructive"},
        {"op": "replace", "path": "/env/hour", "value": 23},
        {"op": "replace", "path": "/user/is_on_call", "value": true},
    ])
}

test_sensitive_read_plus_external_write_without_dlp_denied if {
    not tool_invocation.allow with input as json.patch(base_input, [
        {"op": "replace", "path": "/tool/effect_class", "value": "write_external"},
        {"op": "replace", "path": "/plan/reads_sensitive", "value": true},
        {"op": "replace", "path": "/plan/has_dlp_attestation", "value": false},
    ])
}

test_sensitive_read_plus_external_write_with_dlp_gets_approval if {
    # With DLP attestation the exfil deny reason clears, leaving only the
    # effect_class deny — which routes to require_approval (not hard deny)
    tool_invocation.require_approval with input as json.patch(base_input, [
        {"op": "replace", "path": "/tool/effect_class", "value": "write_external"},
        {"op": "replace", "path": "/plan/reads_sensitive", "value": true},
        {"op": "replace", "path": "/plan/has_dlp_attestation", "value": true},
    ])
}

test_sandbox_cannot_invoke_infrastructure if {
    "Infrastructure actions cannot be initiated from a user sandbox." in tool_invocation.deny_reasons
    with input as json.patch(base_input, [
        {"op": "replace", "path": "/tool/effect_class", "value": "infrastructure"},
        {"op": "replace", "path": "/actor/spiffe_id", "value": "spiffe://aikonos.com/sandbox/alice/task-001"},
    ])
}

test_infrastructure_from_control_plane_gets_approval if {
    # From a control-plane identity (not a sandbox), infrastructure still needs
    # approval but doesn't hit the actor deny reason
    tool_invocation.require_approval with input as json.patch(base_input, [
        {"op": "replace", "path": "/tool/effect_class", "value": "infrastructure"},
        {"op": "replace", "path": "/actor/spiffe_id", "value": "spiffe://aikonos.com/broker"},
    ])
}

# ── Deny reasons surface correctly ────────────────────────────────────────────

test_deny_reasons_for_high_risk if {
    count(tool_invocation.deny_reasons) > 0 with input as json.patch(base_input, [
        {"op": "replace", "path": "/user/risk_score", "value": 85},
        {"op": "replace", "path": "/tool/effect_class", "value": "write_local"},
    ])
}

test_no_deny_reasons_for_clean_read if {
    count(tool_invocation.deny_reasons) == 0 with input as base_input
}

# ── User skill gate (fga_decision="deny") ─────────────────────────────────────

# fga_decision="deny" + read_only → not allow, not require_approval, deny_reasons non-empty
test_fga_deny_read_only_not_allowed if {
    not tool_invocation.allow with input as json.patch(base_input, [
        {"op": "replace", "path": "/fga_decision", "value": "deny"},
    ])
}

test_fga_deny_read_only_not_require_approval if {
    not tool_invocation.require_approval with input as json.patch(base_input, [
        {"op": "replace", "path": "/fga_decision", "value": "deny"},
    ])
}

test_fga_deny_read_only_has_deny_reason if {
    count(tool_invocation.deny_reasons) > 0 with input as json.patch(base_input, [
        {"op": "replace", "path": "/fga_decision", "value": "deny"},
    ])
}

# deny reason message references the tool id
test_fga_deny_reason_names_tool if {
    some msg in tool_invocation.deny_reasons with input as json.patch(base_input, [
        {"op": "replace", "path": "/fga_decision", "value": "deny"},
        {"op": "replace", "path": "/tool/id", "value": "web.fetch"},
    ])
    contains(msg, "web.fetch")
}

# fga_decision="allow" is never affected by this rule
test_fga_allow_no_extra_deny_reason if {
    count(tool_invocation.deny_reasons) == 0 with input as base_input
}
