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
