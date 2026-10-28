<script setup>
import { ref, onMounted } from "vue";
import { get, put } from "../../api/client.js";
import Icon from "../../components/Icon.vue";
import EmptyState from "../../components/ui/EmptyState.vue";
import Spinner from "../../components/ui/Spinner.vue";
import { fmtTimeHistory } from "../../components/audit/audit-format.js";

// Identity enforcement switches (audit | enforce) and the AG-02 would-deny
// feed: admins fix grants from it before flipping the user bound to enforce.
const MODES = [
  {
    key: "identity.enforce_user_bound",
    testid: "identity-user-bound",
    label: "Bound agents by their user",
    help: "Enforce: an agent may only use a skill or MCP connector the person it acts for may use. Audit: record would-deny events below and allow.",
  },
  {
    key: "identity.obo",
    testid: "identity-obo",
    label: "On-behalf-of checks",
    help: "Enforce: deny capability tokens, grants, api keys and schedules whose subject or actor is missing or does not match the task.",
  },
];
const WOULD_DENY_EVENT = "aikonos.broker.user_bound.would_deny";

const enforce   = ref({});
const saving    = ref({});
const errors    = ref({});
const forbidden = ref(false);
const loading   = ref(false);
const events    = ref([]);
const eventsErr = ref("");

async function loadEvents() {
  eventsErr.value = "";
  try {
    const resp = await get(`/admin/audit/query?event_type=${encodeURIComponent(WOULD_DENY_EVENT)}&limit=50`);
    events.value = resp?.events ?? [];
  } catch (e) {
    eventsErr.value = String(e).replace(/^Error:\s*/, "");
  }
}

onMounted(async () => {
  loading.value = true;
  try {
    const resp = await get("/admin/config");
    if (resp.forbidden) { forbidden.value = true; return; }
    for (const e of resp.entries ?? []) {
      if (MODES.some((m) => m.key === e.key)) enforce.value[e.key] = e.value === "enforce";
    }
    await loadEvents();
  } finally {
    loading.value = false;
  }
});

async function toggle(key, on) {
  saving.value[key] = true;
  errors.value[key] = "";
  try {
    await put("/admin/config", { body: { key, value: on ? "enforce" : "audit" } });
    enforce.value[key] = on;
  } catch (err) {
    errors.value[key] = String(err).replace(/^Error:\s*/, "");
  } finally {
    saving.value[key] = false;
  }
}

function ctx(ev) { return ev.context || {}; }
</script>

<template>
  <div class="view">
    <div class="view-header">
      <Icon name="shield" class="view-icon" />
      <h1>Identity enforcement</h1>
    </div>

    <EmptyState v-if="forbidden" data-testid="forbidden" icon="admin" message="You are not a tenant admin." />
    <div v-else-if="loading" class="loading-row"><Spinner size="md" /></div>

    <template v-else>
      <div class="modes">
        <label v-for="m in MODES" :key="m.key" class="mode-row">
          <input
            type="checkbox"
            :data-testid="`${m.testid}-toggle`"
            :checked="enforce[m.key]"
            :disabled="saving[m.key]"
            @change="toggle(m.key, $event.target.checked)"
          />
          <span class="mode-text">
            <span class="mode-label">{{ m.label }}</span>
            <span class="mono muted">{{ m.key }} = {{ enforce[m.key] ? "enforce" : "audit" }}</span>
            <span class="muted">{{ m.help }}</span>
            <span v-if="errors[m.key]" :data-testid="`${m.testid}-error`" class="row-error">{{ errors[m.key] }}</span>
          </span>
        </label>
      </div>

      <div class="feed-header">
        <h2>Would-deny events</h2>
        <button class="btn-ghost" data-testid="would-deny-refresh" @click="loadEvents">Refresh</button>
      </div>
      <p class="muted">
        Calls the user bound would refuse once enforced. Grant the user the skill or the connector
        (MCP connections → Access), or mark the agent as a service agent, then enforce.
      </p>
      <p v-if="eventsErr" class="row-error">{{ eventsErr }}</p>
      <p v-else-if="!events.length" data-testid="would-deny-empty" class="muted">No would-deny events.</p>
      <table v-else class="feed">
        <thead>
          <tr><th>Time</th><th>User</th><th>Agent</th><th>Connector / tool</th><th>Stage</th></tr>
        </thead>
        <tbody>
          <tr v-for="ev in events" :key="ev.event_id" data-testid="would-deny-row">
            <td>{{ fmtTimeHistory(ev) }}</td>
            <td class="mono">{{ ctx(ev).user }}</td>
            <td class="mono">{{ ctx(ev).agent }}</td>
            <td class="mono">{{ ctx(ev).connector || ctx(ev).tool_id }}</td>
            <td>{{ ctx(ev).stage }}</td>
          </tr>
        </tbody>
      </table>
    </template>
  </div>
</template>

<style scoped>
.view { padding: 24px 32px; max-width: 900px; }
.view-header { display: flex; align-items: center; gap: 10px; margin-bottom: 20px; }
.view-header h1 { margin: 0; font-size: 20px; font-weight: 600; }
.view-icon { color: var(--text-muted); width: 22px; height: 22px; }
.modes { display: flex; flex-direction: column; gap: 12px; margin-bottom: 28px; }
.mode-row {
  display: flex; gap: 12px; align-items: flex-start;
  background: var(--bg-elevated); border: 1px solid var(--border);
  border-radius: var(--radius); padding: 14px 18px; cursor: pointer;
}
.mode-text { display: flex; flex-direction: column; gap: 4px; font-size: 13px; }
.mode-label { font-weight: 500; font-size: 14px; }
.feed-header { display: flex; align-items: center; justify-content: space-between; }
.feed-header h2 { margin: 0; font-size: 16px; font-weight: 600; }
.feed { width: 100%; border-collapse: collapse; font-size: 13px; }
.feed th, .feed td { text-align: left; padding: 6px 8px; border-bottom: 1px solid var(--border); }
.btn-ghost {
  background: transparent; color: var(--text); border: 1px solid var(--border);
  border-radius: var(--radius-sm); padding: 5px 12px; font-size: 13px; cursor: pointer;
}
.row-error { margin: 0; font-size: 12px; color: var(--danger); }
.mono  { font-family: var(--font-mono); word-break: break-all; }
.muted { color: var(--text-muted); font-size: 13px; }
.loading-row { display: flex; padding: 32px 0; }
</style>
