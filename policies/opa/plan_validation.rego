package aikonos.plan_validation

import future.keywords.contains
import future.keywords.if
import future.keywords.in
import future.keywords.every

# ── Main decision ─────────────────────────────────────────────────────────────

default allow := false

allow if {
    count(violations) == 0
}

# ── Violations ────────────────────────────────────────────────────────────────
# Each violation is a string returned to the agent for replanning.
# Never include internal policy details in violation messages.

violations contains msg if {
    some step in input.plan.steps
    step.effect_class in ["destructive", "credential_access"]
    # Treat a missing OR empty justification as absent (not step.justification
    # only catches a missing key, not "").
    object.get(step, "justification", "") == ""
    msg := sprintf("Step %d (%s) requires a justification field.", [step.seq, step.tool_id])
}

violations contains msg if {
    total_cost := sum([s.estimated_cost | s := input.plan.steps[_]])
    total_cost > input.task.cost_budget
    msg := sprintf(
        "Plan estimated cost (%d units) exceeds task budget (%d units). Reduce scope or request a higher budget.",
        [total_cost, input.task.cost_budget]
    )
}

violations contains msg if {
    some step in input.plan.steps
    step.tool_id == "shell.execute"
    input.task.effect_class_ceiling != "destructive"
    msg := "shell.execute is not permitted at this task's effect class ceiling."
}

# Prompt-injection exfil pattern: read sensitive + write external in same plan
violations contains msg if {
    sensitive_reads := [s | s := input.plan.steps[_]; s.reads_sensitive == true]
    external_writes := [s | s := input.plan.steps[_]; s.effect_class in ["write_external", "network_egress"]]
    count(sensitive_reads) > 0
    count(external_writes) > 0
    not input.plan.has_dlp_attestation
    msg := "Plan reads sensitive data and writes externally. DLP attestation required before this plan can be approved."
}

violations contains msg if {
    count(input.plan.steps) > 50
    msg := sprintf("Plan has %d steps, exceeding the maximum of 50.", [count(input.plan.steps)])
}

violations contains msg if {
    some step in input.plan.steps
    step.effect_class == "infrastructure"
    not input.actor.is_control_plane
    msg := sprintf("Step %d requests infrastructure access. Only control-plane identities may perform infrastructure actions.", [step.seq])
}

violations contains msg if {
    # Detect duplicate tool calls with identical args in same plan (loop indicator)
    some i, j
    i < j
    input.plan.steps[i].tool_id == input.plan.steps[j].tool_id
    input.plan.steps[i].args == input.plan.steps[j].args
    msg := sprintf(
        "Steps %d and %d are identical (%s with same args). This may indicate an agent loop.",
        [input.plan.steps[i].seq, input.plan.steps[j].seq, input.plan.steps[i].tool_id]
    )
}
