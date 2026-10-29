package aikonos.plan_validation_test

import future.keywords.if
import future.keywords.in

import data.aikonos.plan_validation

base_task := {
    "cost_budget": 1000,
    "effect_class_ceiling": "write_local",
}

base_actor := {
    "user_id": "alice@corp",
    "is_control_plane": false,
}

clean_plan := {
    "plan_id": "plan-001",
    "steps": [
        {
            "seq": 1,
            "tool_id": "siem.query",
            "effect_class": "read_only",
            "estimated_cost": 50,
            "reads_sensitive": false,
            "justification": "",
            "args": {"index": "logs-*", "query": "error"},
        },
        {
            "seq": 2,
            "tool_id": "doc.write",
            "effect_class": "write_local",
            "estimated_cost": 100,
            "reads_sensitive": false,
            "justification": "",
            "args": {"format": "markdown"},
        },
    ],
    "has_dlp_attestation": false,
}

test_clean_plan_allowed if {
    plan_validation.allow with input as {
        "plan": clean_plan,
        "task": base_task,
        "actor": base_actor,
    }
}

test_budget_exceeded_violation if {
    expensive_plan := json.patch(clean_plan, [
        {"op": "replace", "path": "/steps/0/estimated_cost", "value": 600},
        {"op": "replace", "path": "/steps/1/estimated_cost", "value": 600},
    ])
    violations := plan_validation.violations with input as {"plan": expensive_plan, "task": base_task, "actor": base_actor}
    some v in violations
    contains(v, "exceeds task budget")
}

test_destructive_without_justification_violation if {
    bad_plan := json.patch(clean_plan, [
        {"op": "replace", "path": "/steps/0/effect_class", "value": "destructive"},
        {"op": "replace", "path": "/steps/0/justification", "value": ""},
    ])
    violations := plan_validation.violations with input as {"plan": bad_plan, "task": base_task, "actor": base_actor}
    some v in violations
    contains(v, "requires a justification")
}

test_destructive_with_justification_no_violation if {
    good_plan := json.patch(clean_plan, [
        {"op": "replace", "path": "/steps/0/effect_class", "value": "destructive"},
        {"op": "replace", "path": "/steps/0/justification", "value": "Rotating stale API key per security policy."},
    ])
    # This should NOT produce a justification violation (but will produce others)
    violations := plan_validation.violations with input as {"plan": good_plan, "task": base_task, "actor": base_actor}
    justification_violations := [v | v := violations[_]; contains(v, "requires a justification")]
    count(justification_violations) == 0
}

test_exfil_pattern_without_dlp_violation if {
    exfil_plan := json.patch(clean_plan, [
        {"op": "replace", "path": "/steps/0/reads_sensitive", "value": true},
        {"op": "replace", "path": "/steps/1/effect_class", "value": "write_external"},
    ])
    violations := plan_validation.violations with input as {"plan": exfil_plan, "task": base_task, "actor": base_actor}
    some v in violations
    contains(v, "DLP attestation required")
}

test_exfil_pattern_with_dlp_no_violation if {
    exfil_plan_with_dlp := json.patch(clean_plan, [
        {"op": "replace", "path": "/steps/0/reads_sensitive", "value": true},
        {"op": "replace", "path": "/steps/1/effect_class", "value": "write_external"},
        {"op": "replace", "path": "/has_dlp_attestation", "value": true},
    ])
    violations := plan_validation.violations with input as {"plan": exfil_plan_with_dlp, "task": base_task, "actor": base_actor}
    exfil_violations := [v | v := violations[_]; contains(v, "DLP attestation")]
    count(exfil_violations) == 0
}

test_duplicate_steps_violation if {
    loop_plan := json.patch(clean_plan, [
        {"op": "add", "path": "/steps/-", "value": {
            "seq": 3,
            "tool_id": "siem.query",
            "effect_class": "read_only",
            "estimated_cost": 50,
            "reads_sensitive": false,
            "justification": "",
            "args": {"index": "logs-*", "query": "error"},
        }},
    ])
    violations := plan_validation.violations with input as {"plan": loop_plan, "task": base_task, "actor": base_actor}
    some v in violations
    contains(v, "identical")
}

test_too_many_steps_violation if {
    # Generate 51 steps
    many_steps := [{"seq": i, "tool_id": "siem.query", "effect_class": "read_only",
                    "estimated_cost": 1, "reads_sensitive": false, "justification": "",
                    "args": {"n": i}} | i := numbers.range(1, 51)[_]]
    big_plan := json.patch(clean_plan, [{"op": "replace", "path": "/steps", "value": many_steps}])
    violations := plan_validation.violations with input as {"plan": big_plan, "task": base_task, "actor": base_actor}
    some v in violations
    contains(v, "exceeding the maximum")
}

test_shell_execute_denied_below_ceiling if {
    shell_plan := json.patch(clean_plan, [
        {"op": "replace", "path": "/steps/0/tool_id", "value": "shell.execute"},
    ])
    violations := plan_validation.violations with input as {"plan": shell_plan, "task": base_task, "actor": base_actor}
    some v in violations
    contains(v, "shell.execute")
}

test_infrastructure_from_non_control_plane_denied if {
    infra_plan := json.patch(clean_plan, [
        {"op": "replace", "path": "/steps/0/effect_class", "value": "infrastructure"},
    ])
    violations := plan_validation.violations with input as {"plan": infra_plan, "task": base_task, "actor": base_actor}
    some v in violations
    contains(v, "infrastructure access")
}

# ── Fail closed on malformed input ────────────────────────────────────────────

test_empty_input_denied if {
    not plan_validation.allow with input as {}
}

test_missing_steps_denied if {
    violations := plan_validation.violations with input as {"plan": {"plan_id": "p"}, "task": base_task, "actor": base_actor}
    "Plan steps are missing or malformed." in violations
}

test_non_array_steps_denied if {
    not plan_validation.allow with input as {"plan": {"steps": "x"}, "task": base_task, "actor": base_actor}
}

test_missing_cost_budget_denied if {
    violations := plan_validation.violations with input as {"plan": clean_plan, "task": {"effect_class_ceiling": "write_local"}, "actor": base_actor}
    "Task cost budget is missing or malformed." in violations
}

test_string_cost_budget_denied if {
    not plan_validation.allow with input as {"plan": clean_plan, "task": {"cost_budget": "1000", "effect_class_ceiling": "write_local"}, "actor": base_actor}
}

test_missing_effect_class_ceiling_denied if {
    violations := plan_validation.violations with input as {"plan": clean_plan, "task": {"cost_budget": 1000}, "actor": base_actor}
    "Task effect class ceiling is missing or malformed." in violations
}

test_missing_step_cost_denied if {
    bad_plan := json.patch(clean_plan, [{"op": "remove", "path": "/steps/1/estimated_cost"}])
    violations := plan_validation.violations with input as {"plan": bad_plan, "task": base_task, "actor": base_actor}
    "Step 2 has a missing or malformed estimated cost." in violations
}

test_negative_cost_cannot_offset_budget if {
    # -1000000 + 1000100 sums to 100, under the 1000 budget: the negative step
    # itself must be the violation.
    netting_plan := json.patch(clean_plan, [
        {"op": "replace", "path": "/steps/0/estimated_cost", "value": -1000000},
        {"op": "replace", "path": "/steps/1/estimated_cost", "value": 1000100},
    ])
    violations := plan_validation.violations with input as {"plan": netting_plan, "task": base_task, "actor": base_actor}
    "Step 1 has a negative estimated cost." in violations
    not plan_validation.allow with input as {"plan": netting_plan, "task": base_task, "actor": base_actor}
}

test_negative_cost_without_seq_denied if {
    no_seq := json.patch(clean_plan, [
        {"op": "remove", "path": "/steps/0/seq"},
        {"op": "replace", "path": "/steps/0/estimated_cost", "value": -1},
    ])
    not plan_validation.allow with input as {"plan": no_seq, "task": base_task, "actor": base_actor}
}
