<script setup>
import { ref } from "vue";
import { get } from "../../api/client.js";
import Icon from "../../components/Icon.vue";
import EmptyState from "../../components/ui/EmptyState.vue";

const taskIdInput = ref("");
const steps       = ref([]);
const loading     = ref(false);
const forbidden   = ref(false);
const searched    = ref(false);

async function explain() {
  if (!taskIdInput.value.trim()) return;
  forbidden.value = false;
  loading.value   = true;
  steps.value     = [];
  searched.value  = false;
  try {
    const resp = await get("/admin/decisions/" + encodeURIComponent(taskIdInput.value.trim()));
    if (resp.forbidden) { forbidden.value = true; return; }
    steps.value    = resp.steps ?? [];
    searched.value = true;
  } finally {
    loading.value = false;
  }
}

function outcomeBadgeClass(outcome) {
  const o = (outcome ?? "").toUpperCase();
  if (o === "DENY")   return "deny";
  if (o === "ALLOW")  return "ok";
  if (o === "INFO")   return "muted";
  return "warn";
}
</script>

<template>
  <div class="view">
    <div class="view-header">
      <Icon name="audit" class="view-icon" />
      <h1>Decision Trace</h1>
    </div>

    <!-- not-an-admin empty-state -->
    <EmptyState
      v-if="forbidden"
      data-testid="forbidden"
      icon="admin"
      message="You are not a tenant admin."
    />

    <template v-else>
      <!-- ── Input form ── -->
      <div class="search-form">
        <div class="form-row">
          <label>Task ID</label>
          <input
            data-testid="task-id-input"
            type="text"
            v-model="taskIdInput"
            placeholder="Enter task UUID…"
            class="form-input"
            @keyup.enter="explain"
          />
        </div>
        <div class="form-actions">
          <button
            data-testid="explain-btn"
            class="btn-primary"
            :disabled="loading"
            @click="explain"
          >
            <Icon name="audit" /> Explain
          </button>
        </div>
      </div>

      <div v-if="loading" class="muted">Loading…</div>

      <!-- ── Empty result ── -->
      <EmptyState
        v-if="searched && steps.length === 0"
        data-testid="no-trace"
        icon="audit"
        message="No decision trace found for this task."
      />

      <!-- ── Steps ── -->
      <div v-if="steps.length > 0" class="steps">
        <div
          v-for="step in steps"
          :key="step.seq"
          data-testid="step-row"
          class="step-card"
        >
          <div class="step-header">
            <span class="step-seq">Step {{ step.seq }}</span>
            <span class="step-tool mono">{{ step.tool_id }}</span>
            <span class="step-effect muted">{{ step.effect_class }}</span>
            <span class="badge" :class="outcomeBadgeClass(step.decision)">{{ step.decision }}</span>
          </div>
          <div v-if="step.justification" class="step-justification muted">{{ step.justification }}</div>

          <!-- Gates -->
          <div v-if="step.gates && step.gates.length > 0" class="gates">
            <div
              v-for="gate in step.gates"
              :key="gate.gate"
              data-testid="gate-row"
              class="gate-row"
            >
              <span class="gate-name mono">{{ gate.gate }}</span>
              <span class="badge" :class="outcomeBadgeClass(gate.outcome)">{{ gate.outcome }}</span>
              <span v-if="gate.rule_id" class="gate-rule-id mono muted">{{ gate.rule_id }}</span>
              <span v-if="gate.reason" class="gate-reason muted">{{ gate.reason }}</span>
              <span v-if="gate.detail" class="gate-detail muted">{{ gate.detail }}</span>
            </div>
          </div>
        </div>
      </div>
    </template>
  </div>
</template>

<style scoped>
.view { padding: 24px 32px; max-width: 900px; }
.view-header { display: flex; align-items: center; gap: 10px; margin-bottom: 20px; }
.view-header h1 { margin: 0; font-size: 20px; font-weight: 600; }
.view-icon { color: var(--text-muted); width: 22px; height: 22px; }

.search-form {
  background: var(--bg-elevated); border: 1px solid var(--border);
  border-radius: var(--radius); padding: 16px 20px; margin-bottom: 20px;
  display: flex; flex-direction: column; gap: 12px;
}
.form-row { display: flex; align-items: center; gap: 10px; flex-wrap: wrap; }
label { font-size: 12px; color: var(--text-muted); white-space: nowrap; }
.form-input {
  background: var(--bg); color: var(--text);
  border: 1px solid var(--border); border-radius: var(--radius-sm);
  padding: 6px 9px; font-size: 13px; min-width: 280px;
}
.form-actions { display: flex; gap: 10px; }

.btn-primary {
  display: inline-flex; align-items: center; gap: 4px;
  background: var(--accent); color: var(--text-on-accent);
  border: none; border-radius: var(--radius-sm);
  padding: 6px 14px; font-size: 13px; cursor: pointer;
}
.btn-primary:disabled { opacity: 0.5; cursor: default; }

.steps { display: flex; flex-direction: column; gap: 12px; }
.step-card {
  background: var(--bg-elevated); border: 1px solid var(--border);
  border-radius: var(--radius); padding: 14px 18px;
}
.step-header {
  display: flex; align-items: center; gap: 10px; flex-wrap: wrap;
  margin-bottom: 6px;
}
.step-seq { font-size: 11px; color: var(--text-muted); font-weight: 600; }
.step-tool { font-size: 13px; font-weight: 500; }
.step-effect { font-size: 11px; }
.step-justification { font-size: 12px; margin-bottom: 10px; }

.gates { display: flex; flex-direction: column; gap: 4px; margin-top: 8px; }
.gate-row {
  display: flex; align-items: center; gap: 8px; flex-wrap: wrap;
  padding: 4px 8px; background: var(--bg); border-radius: var(--radius-sm);
  font-size: 12px;
}
.gate-name { font-weight: 500; min-width: 120px; }
.gate-rule-id { font-size: 11px; }
.gate-reason { flex: 1; }
.gate-detail { font-size: 11px; color: var(--text-muted); }

.badge {
  font-size: 11px; padding: 1px 8px; border-radius: 999px; border: 1px solid var(--border);
}
.badge.ok     { color: var(--ok);     border-color: var(--ok); }
.badge.warn   { color: var(--accent); border-color: var(--accent); }
.badge.deny   { color: var(--danger); border-color: var(--danger); }
.badge.muted  { color: var(--text-muted); border-color: var(--border); }

.mono  { font-family: var(--font-mono); }
.muted { color: var(--text-muted); font-size: 13px; }
</style>
