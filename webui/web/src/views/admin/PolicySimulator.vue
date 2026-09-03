<script setup>
import { ref } from "vue";
import { post } from "../../api/client.js";
import Icon from "../../components/Icon.vue";
import EmptyState from "../../components/ui/EmptyState.vue";
import ToggleSwitch from "../../components/ui/ToggleSwitch.vue";

const subjectUserId = ref("");
const toolId        = ref("");
const effectClass   = ref("READ_ONLY");
const host          = ref("");
const readsSensitive = ref(false);
const fgaObject     = ref("");

const outcome   = ref(null);
const gates     = ref([]);
const loading   = ref(false);
const forbidden = ref(false);
const simulated = ref(false);

async function simulate() {
  if (!subjectUserId.value.trim() || !toolId.value.trim()) return;
  forbidden.value = false;
  loading.value   = true;
  outcome.value   = null;
  gates.value     = [];
  simulated.value = false;
  try {
    const body = {
      subjectUserId:  subjectUserId.value.trim(),
      toolId:         toolId.value.trim(),
      effectClass:    effectClass.value,
      readsSensitive: readsSensitive.value,
    };
    if (host.value.trim())      body.host      = host.value.trim();
    if (fgaObject.value.trim()) body.fgaObject = fgaObject.value.trim();

    const resp = await post("/admin/policy/simulate", { body });
    if (resp.forbidden) { forbidden.value = true; return; }
    outcome.value   = resp.outcome ?? null;
    gates.value     = resp.gates   ?? [];
    simulated.value = true;
  } finally {
    loading.value = false;
  }
}

function outcomeBadgeClass(o) {
  const v = (o ?? "").toUpperCase();
  if (v === "DENY")         return "deny";
  if (v === "ALLOW")        return "ok";
  if (v === "INFO")         return "muted";
  return "warn";
}
</script>

<template>
  <div class="view">
    <div class="view-header">
      <Icon name="audit" class="view-icon" />
      <h1>Policy Simulator</h1>
    </div>

    <!-- not-an-admin empty-state -->
    <EmptyState v-if="forbidden" data-testid="forbidden" icon="admin" message="You are not a tenant admin." />

    <template v-else>
      <!-- ── Input form ── -->
      <div class="sim-form">
        <div class="form-row">
          <label>Subject user ID</label>
          <input
            data-testid="subject-user-id"
            type="text"
            v-model="subjectUserId"
            placeholder="alice@example.com"
            class="form-input"
            @keyup.enter="simulate"
          />
        </div>

        <div class="form-row">
          <label>Tool ID</label>
          <input
            data-testid="tool-id"
            type="text"
            v-model="toolId"
            placeholder="web.fetch"
            class="form-input"
            @keyup.enter="simulate"
          />
        </div>

        <div class="form-row">
          <label>Effect class</label>
          <select
            data-testid="effect-class"
            v-model="effectClass"
            class="form-input form-select"
          >
            <option value="READ_ONLY">READ_ONLY</option>
            <option value="WRITE_LOCAL">WRITE_LOCAL</option>
            <option value="WRITE_EXTERNAL">WRITE_EXTERNAL</option>
            <option value="NETWORK_EGRESS">NETWORK_EGRESS</option>
            <option value="CREDENTIAL_ACCESS">CREDENTIAL_ACCESS</option>
            <option value="DESTRUCTIVE">DESTRUCTIVE</option>
            <option value="INFRASTRUCTURE">INFRASTRUCTURE</option>
          </select>
        </div>

        <div class="form-row">
          <label>Host <span class="optional">(optional, for web egress)</span></label>
          <input
            data-testid="host"
            type="text"
            v-model="host"
            placeholder="example.com"
            class="form-input"
            @keyup.enter="simulate"
          />
        </div>

        <div class="form-row">
          <label>FGA object <span class="optional">(optional)</span></label>
          <input
            data-testid="fga-object"
            type="text"
            v-model="fgaObject"
            placeholder="document:doc-123"
            class="form-input"
            @keyup.enter="simulate"
          />
        </div>

        <div class="form-row form-row-check">
          <label>
            <ToggleSwitch
              data-testid="reads-sensitive"
              v-model="readsSensitive"
              aria-label="Reads sensitive data"
            />
            Reads sensitive data
          </label>
        </div>

        <div class="form-actions">
          <button
            data-testid="simulate-btn"
            class="btn-primary"
            :disabled="loading"
            @click="simulate"
          >
            <Icon name="audit" /> Simulate
          </button>
        </div>
      </div>

      <div v-if="loading" class="muted">Simulating…</div>

      <!-- ── Result ── -->
      <template v-if="simulated">
        <div class="result-header">
          <span class="result-label">Aggregate outcome</span>
          <span
            data-testid="outcome-badge"
            class="badge"
            :class="outcomeBadgeClass(outcome)"
          >{{ outcome }}</span>
        </div>

        <div v-if="gates.length > 0" class="gates">
          <div
            v-for="(gate, index) in gates"
            :key="gate.gate + '-' + index"
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
      </template>
    </template>
  </div>
</template>

<style scoped>
.view { padding: 24px 32px; max-width: 900px; }
.view-header { display: flex; align-items: center; gap: 10px; margin-bottom: 20px; }
.view-header h1 { margin: 0; font-size: 20px; font-weight: 600; }
.view-icon { color: var(--text-muted); width: 22px; height: 22px; }

.empty-state {
  display: flex; flex-direction: column; align-items: center; gap: 12px;
  padding: 60px 0; color: var(--text-muted);
}
.empty-icon { width: 40px; height: 40px; }

.sim-form {
  background: var(--bg-elevated); border: 1px solid var(--border);
  border-radius: var(--radius); padding: 16px 20px; margin-bottom: 20px;
  display: flex; flex-direction: column; gap: 12px;
}
.form-row { display: flex; align-items: center; gap: 10px; flex-wrap: wrap; }
.form-row-check { align-items: center; }
label { font-size: 12px; color: var(--text-muted); white-space: nowrap; }
.optional { font-size: 11px; font-style: italic; }
.form-input {
  background: var(--bg); color: var(--text);
  border: 1px solid var(--border); border-radius: var(--radius-sm);
  padding: 6px 9px; font-size: 13px; min-width: 220px;
}
.form-select { min-width: 180px; }
.form-actions { display: flex; gap: 10px; margin-top: 4px; }

.btn-primary {
  display: inline-flex; align-items: center; gap: 4px;
  background: var(--accent); color: var(--text-on-accent);
  border: none; border-radius: var(--radius-sm);
  padding: 6px 14px; font-size: 13px; cursor: pointer;
}
.btn-primary:disabled { opacity: 0.5; cursor: default; }

.result-header {
  display: flex; align-items: center; gap: 10px;
  margin-bottom: 14px;
}
.result-label { font-size: 13px; color: var(--text-muted); font-weight: 500; }

.gates { display: flex; flex-direction: column; gap: 4px; }
.gate-row {
  display: flex; align-items: center; gap: 8px; flex-wrap: wrap;
  padding: 4px 8px; background: var(--bg-elevated); border: 1px solid var(--border);
  border-radius: var(--radius-sm); font-size: 12px;
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
