<script setup>
// Fan-out timeline row-per-branch.
// Unlike SkillTimeline/MemoryTimeline (one-shot), branches are two-phase: a
// "running" row created at spawn is mutated in place to a terminal status by
// useAguiRun.js's onSubagentCompleted. This component is pure display over
// whatever branch list it is handed.
import Icon from "./Icon.vue";
import { toMicros, fmtAmountPrecise } from "../lib/money.js";

defineProps({
  // [{ index, task, role?, status: "running"|"ok"|"failure", failure?, cost }]
  branches: { type: Array, default: () => [] },
});

const FAILURE_LABEL = {
  error: "failed",
  timeout: "timed out",
  denied: "needs to run directly in chat",
  systemic: "system error",
};

function statusIcon(b) {
  if (b.status === "running") return "spinner";
  if (b.status === "ok") return "check";
  if (b.failure === "timeout") return "schedules";
  if (b.failure === "denied") return "pause";
  return "close"; // error / systemic / unrecognized failure kind
}

function statusLabel(b) {
  if (b.status === "running") return "running";
  if (b.status === "ok") return "done";
  return FAILURE_LABEL[b.failure] || "failed";
}

// cost is a major-unit float (proto double), not micro-units — route through
// money.js's own conversion so this renders like every other cost in the UI
// (SessionUsage.vue precedent) instead of a bespoke formatter.
function fmtCost(cost) {
  return fmtAmountPrecise(toMicros(cost ?? 0));
}

function allResolved(branches) {
  return branches.length > 0 && branches.every((b) => b.status !== "running");
}

function total(branches) {
  return branches.reduce((sum, b) => sum + (b.cost ?? 0), 0);
}
</script>

<template>
  <div class="subagent-timeline" data-testid="subagent-timeline">
    <div
      v-for="b in branches"
      :key="b.index"
      class="subagent-timeline-item"
      :class="[`subagent-timeline-item--${b.status}`, b.failure ? `subagent-timeline-item--${b.failure}` : null]"
    >
      <Icon :name="statusIcon(b)" :size="14" class="subagent-timeline-icon" />
      <div class="subagent-timeline-body">
        <span class="subagent-timeline-task">agent spawned to do {{ b.task }}</span>
        <span class="subagent-timeline-detail">
          {{ statusLabel(b) }}<template v-if="b.status !== 'running'"> · {{ fmtCost(b.cost) }}</template>
        </span>
      </div>
    </div>
    <div v-if="allResolved(branches)" class="subagent-timeline-total" data-testid="subagent-timeline-total">
      Total: {{ fmtCost(total(branches)) }}
    </div>
  </div>
</template>

<style scoped>
.subagent-timeline {
  display: flex;
  flex-direction: column;
  gap: 0.375rem;
  padding: 0.5rem 0.75rem;
  border-left: 2px solid var(--border);
}

.subagent-timeline-item {
  display: flex;
  align-items: flex-start;
  gap: 0.5rem;
  color: var(--text-muted);
}

.subagent-timeline-icon {
  flex-shrink: 0;
  margin-top: 1px;
  color: var(--text-muted);
}

.subagent-timeline-item--ok .subagent-timeline-icon {
  color: var(--ok);
}

.subagent-timeline-item--error .subagent-timeline-icon,
.subagent-timeline-item--systemic .subagent-timeline-icon {
  color: var(--danger);
}

.subagent-timeline-item--timeout .subagent-timeline-icon,
.subagent-timeline-item--denied .subagent-timeline-icon {
  color: var(--accent);
}

.subagent-timeline-body {
  display: flex;
  flex-direction: column;
}

.subagent-timeline-task {
  font-size: 0.8125rem;
  color: var(--text);
}

.subagent-timeline-detail {
  font-family: var(--font-mono);
  font-size: 0.75rem;
  color: var(--text-muted);
}

.subagent-timeline-total {
  font-size: 0.75rem;
  font-weight: 600;
  color: var(--text);
  padding-top: 0.25rem;
}
</style>
