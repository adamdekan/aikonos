<script setup>
import { ref, computed, onMounted, onUnmounted, watch } from "vue";
import Icon from "../../components/Icon.vue";
import EmptyState from "../../components/ui/EmptyState.vue";
import DecisionBadge from "../../components/audit/DecisionBadge.vue";
import EventDetail from "../../components/audit/EventDetail.vue";
import { fmtTimeLive, actor, detail, decision } from "../../components/audit/audit-format.js";
import { toJson, toCsv } from "./auditExport.js";

// eventSourceFactory is injectable for tests: pass a function () => EventSource-like.
// When null/undefined, the view renders a "not configured" empty-state.
const props = defineProps({
  eventSourceFactory: {
    type: Function,
    default: null,
  },
});

const events           = ref([]);
const connected        = ref(false);
const notConfigured    = ref(false);   // factory absent, or a probe of /audit/stream returned 501
const reconnecting     = ref(false);   // transient failure — retrying with backoff
const disconnected     = ref(false);   // retries exhausted — manual Reconnect available
const filter           = ref("");
const tenantFilter     = ref("");      // "" = All tenants
const expanded         = ref(new Set()); // _id values of expanded rows
const MAX              = 1000;

// Live-stream affordances
const paused   = ref(false);
const pending  = ref([]);  // buffer while paused
const atTop    = ref(true); // whether scroll position is at top (tail-lock)
const scrollEl = ref(null); // template ref for scroll container

let es               = null;
let retryTimer       = null;
let retryAttempt     = 0;
let probeController  = null;
let destroyed        = false;
// Bumped on every connect() so a slow classifyAndHandleFailure() probe that
// resolves after a newer connection has already started (e.g. a tenant-filter
// change) can detect it's stale and no-op instead of disrupting the fresh
// connection.
let generation       = 0;

const MAX_RETRIES    = 5;
const BASE_DELAY_MS  = 1000;
const MAX_DELAY_MS   = 30000;

function add(ev) {
  ev._id = (ev.event_id || "") + ":" + Date.now() + Math.random();
  events.value.unshift(ev);
  if (events.value.length > MAX) events.value.pop();
}

function clearRetryTimer() {
  if (retryTimer) { clearTimeout(retryTimer); retryTimer = null; }
}

function closeStream() {
  if (es) {
    es.onopen = null; es.onerror = null; es.onmessage = null;
    es.close();
    es = null;
  }
}

function scheduleRetry() {
  if (retryAttempt >= MAX_RETRIES) {
    reconnecting.value = false;
    disconnected.value = true;
    return;
  }
  reconnecting.value = true;
  disconnected.value = false;
  const delay = Math.min(MAX_DELAY_MS, BASE_DELAY_MS * 2 ** retryAttempt);
  retryAttempt += 1;
  retryTimer = setTimeout(() => {
    retryTimer = null;
    connect();
  }, delay);
}

// Classify a connection failure: probe /audit/stream and abort right after the
// response headers arrive. HTTP 501 = the proxy has no OBSERVABILITY_URL configured
// (terminal, no point retrying). Anything else — network error, 5xx, aborted probe —
// is treated as transient and retried with capped exponential backoff.
async function classifyAndHandleFailure() {
  const gen = generation;
  closeStream();
  connected.value = false;

  probeController = new AbortController();
  let status = null;
  try {
    const resp = await fetch("/audit/stream", { signal: probeController.signal });
    status = resp.status;
  } catch {
    status = null;
  } finally {
    probeController?.abort();
    probeController = null;
  }

  if (destroyed) return;
  // A newer connect() (e.g. a tenant-filter change) already superseded this
  // probe — don't let a stale resolution flip state on the fresh connection.
  if (gen !== generation) return;

  if (status === 501) {
    notConfigured.value = true;
    reconnecting.value  = false;
    disconnected.value  = false;
    clearRetryTimer();
    return;
  }

  scheduleRetry();
}

function connect() {
  generation += 1;
  const factory = props.eventSourceFactory;
  if (!factory) { notConfigured.value = true; return; }
  closeStream();
  try {
    const url = tenantFilter.value
      ? `/audit/stream?tenant=${encodeURIComponent(tenantFilter.value)}`
      : "/audit/stream";
    es = factory(url);
    es.onopen  = () => {
      connected.value     = true;
      reconnecting.value  = false;
      disconnected.value  = false;
      notConfigured.value = false;
      retryAttempt        = 0;
    };
    es.onerror = () => { classifyAndHandleFailure(); };
    es.onmessage = (m) => {
      try {
        const ev = JSON.parse(m.data);
        if (paused.value) {
          ev._id = (ev.event_id || "") + ":" + Date.now() + Math.random();
          pending.value.unshift(ev);
          if (pending.value.length > MAX) pending.value.pop();
        } else {
          add(ev);
        }
      } catch { /* ignore parse errors */ }
    };
  } catch {
    classifyAndHandleFailure();
  }
}

// Manual Reconnect: resets backoff state and retries immediately.
function reconnect() {
  retryAttempt        = 0;
  clearRetryTimer();
  disconnected.value  = false;
  notConfigured.value = false;
  reconnecting.value  = false;
  connect();
}

// F44: a tenant-filter change re-establishes the SSE connection scoped to the
// selected tenant (or unfiltered when cleared) via the existing connect path —
// no separate connection state machine.
watch(tenantFilter, () => {
  retryAttempt        = 0;
  clearRetryTimer();
  disconnected.value  = false;
  notConfigured.value = false;
  connect();
});

onMounted(connect);
onUnmounted(() => {
  destroyed = true;
  clearRetryTimer();
  probeController?.abort();
  closeStream();
});

function toggleExpand(id) {
  const s = new Set(expanded.value);
  if (s.has(id)) s.delete(id); else s.add(id);
  expanded.value = s;
}

// Pause / resume
function togglePause() {
  if (paused.value) {
    // flush pending into events (prepend in order — pending is newest-first)
    for (const ev of [...pending.value].reverse()) {
      events.value.unshift(ev);
    }
    if (events.value.length > MAX) events.value.splice(MAX);
    pending.value = [];
    paused.value = false;
  } else {
    paused.value = true;
  }
}

function jumpToLatest() {
  paused.value && togglePause();
  if (scrollEl.value) scrollEl.value.scrollTop = 0;
  atTop.value = true;
}

function onScroll(e) {
  atTop.value = e.target.scrollTop < 40;
}

function downloadBlob(content, filename, mime) {
  const url = URL.createObjectURL(new Blob([content], { type: mime }));
  const a   = Object.assign(document.createElement("a"), { href: url, download: filename });
  document.body.appendChild(a);
  a.click();
  a.remove();
  URL.revokeObjectURL(url);
}

function exportJson() {
  downloadBlob(toJson(filtered.value), "audit-events.json", "application/json");
}

function exportCsv() {
  downloadBlob(toCsv(filtered.value), "audit-events.csv", "text/csv");
}

const counts = computed(() => {
  const c = { ALLOW: 0, DENY: 0, APPROVAL: 0 };
  for (const e of events.value) {
    const d = decision(e);
    if (d?.label === "ALLOW") c.ALLOW++;
    else if (d?.label === "DENY") c.DENY++;
    else if (d?.label === "APPROVAL" || d?.label === "STEP-UP") c.APPROVAL++;
  }
  return c;
});

const filtered = computed(() => {
  const q  = filter.value.trim().toLowerCase();
  const tf = tenantFilter.value;
  let result = events.value;
  if (tf) result = result.filter((e) => e.tenant_id === tf);
  if (!q) return result;
  const terms   = q.split(/\s+/).filter(Boolean);
  const include = terms.filter((t) => !t.startsWith("-"));
  const exclude = terms.filter((t) => t.startsWith("-") && t.length > 1).map((t) => t.slice(1));
  return result.filter((e) => {
    const d = decision(e);
    const hay = [e.event_type, actor(e), e.resource_ref, detail(e), d?.label ?? ""]
      .join(" ").toLowerCase();
    if (include.some((t) => !hay.includes(t))) return false;
    if (exclude.some((t) => hay.includes(t)))  return false;
    return true;
  });
});

// Distinct tenant_ids seen in the full buffer for the tenant filter <select>.
const tenantIds = computed(() => {
  const seen = new Set();
  for (const e of events.value) if (e.tenant_id) seen.add(e.tenant_id);
  return [...seen].sort();
});

const pendingCount = computed(() => pending.value.length);
const showJumpPill = computed(() => paused.value ? pendingCount.value > 0 : !atTop.value);
const jumpLabel    = computed(() =>
  paused.value ? `▲ ${pendingCount.value} new` : "▼ Jump to latest"
);
</script>

<template>
  <div class="view">
    <!-- ── Header ─────────────────────────────────────────────────────── -->
    <div class="view-header">
      <Icon name="audit" class="view-icon" />
      <h1>Audit Stream</h1>
      <!-- Connection status pill -->
      <span
        v-if="!notConfigured && !disconnected"
        class="conn-status"
        :class="connected ? 'live' : (reconnecting ? 'reconnecting' : 'connecting')"
      >
        <span class="conn-dot" />
        {{ connected ? "live" : (reconnecting ? "reconnecting…" : "connecting…") }}
      </span>
    </div>

    <!-- ── Not-configured empty-state ────────────────────────────────── -->
    <EmptyState v-if="notConfigured" data-testid="not-configured" icon="audit">
      <p>Audit stream not configured.</p>
      <span class="hint">
        Set <code>OBSERVABILITY_URL</code> in <code>server.mjs</code> to connect to the observability
        service SSE feed.
      </span>
      <button class="tool-btn" data-testid="reconnect-btn" @click="reconnect">Reconnect</button>
    </EmptyState>

    <template v-else>
      <!-- ── Disconnected banner (retries exhausted) ───────────────── -->
      <div v-if="disconnected" data-testid="disconnected-banner" class="banner-err">
        Audit stream disconnected. Displayed events are from before the disconnect.
        <button class="tool-btn" data-testid="reconnect-btn" @click="reconnect">Reconnect</button>
      </div>

      <!-- ── Reconnecting banner (transient failure, retrying) ─────── -->
      <div v-else-if="reconnecting" data-testid="reconnecting-banner" class="banner-warn">
        Audit stream connection lost — reconnecting…
      </div>

      <!-- ── Toolbar ───────────────────────────────────────────────── -->
      <div class="toolbar">
        <!-- Left: decision count chips -->
        <div class="toolbar-counts">
          <span class="count-chip ok">{{ counts.ALLOW }} allow</span>
          <span class="count-chip warn">{{ counts.APPROVAL }} approval</span>
          <span class="count-chip deny">{{ counts.DENY }} deny</span>
          <span class="count-chip muted">{{ events.length }} total</span>
        </div>

        <!-- Center: tenant + regex filter -->
        <div class="toolbar-center">
          <select
            v-model="tenantFilter"
            data-testid="tenant-filter"
            class="filter-select"
          >
            <option value="">All tenants</option>
            <option v-for="tid in tenantIds" :key="tid" :value="tid">{{ tid }}</option>
          </select>
          <input
            v-model="filter"
            placeholder="filter events… (-term excludes)"
            class="filter-input"
            title="Space-separated terms; prefix with - to exclude"
          />
        </div>

        <!-- Right: pause + export -->
        <div class="toolbar-actions">
          <button class="tool-btn" :class="{ active: paused }" @click="togglePause" :title="paused ? 'Resume stream' : 'Pause stream'">
            <Icon :name="paused ? 'play' : 'pause'" :size="14" />
            {{ paused ? "Resume" : "Pause" }}
          </button>
          <button class="tool-btn" @click="exportJson" title="Export JSON">
            <Icon name="download" :size="14" />
            JSON
          </button>
          <button class="tool-btn" @click="exportCsv" title="Export CSV">
            <Icon name="download" :size="14" />
            CSV
          </button>
        </div>
      </div>

      <!-- ── Table scroll region ───────────────────────────────────── -->
      <div class="table-scroll scroll-min" ref="scrollEl" @scroll="onScroll">
        <!-- New-events / jump-to-latest pill -->
        <button
          v-if="showJumpPill"
          class="jump-pill"
          :aria-live="'polite'"
          @click="jumpToLatest"
        >
          {{ jumpLabel }}
        </button>

        <!-- Empty waiting state -->
        <div v-if="filtered.length === 0" class="empty-section">
          Waiting for broker activity…
        </div>

        <table v-else class="audit-table">
          <colgroup>
            <col style="width:110px" />
            <col style="width:200px" />
            <col style="width:160px" />
            <col />
            <col style="width:90px" />
            <col style="width:1.2fr" />
          </colgroup>
          <thead>
            <tr class="thead-row">
              <th>Time</th>
              <th>Event</th>
              <th>Actor</th>
              <th>Resource</th>
              <th>Decision</th>
              <th>Detail</th>
            </tr>
          </thead>
          <tbody>
            <template v-for="ev in filtered" :key="ev._id">
              <tr
                data-testid="audit-row"
                class="audit-row"
                tabindex="0"
                @click="toggleExpand(ev._id)"
                @keydown.enter.prevent="toggleExpand(ev._id)"
                @keydown.space.prevent="toggleExpand(ev._id)"
              >
                <td class="cell-mono cell-muted">{{ fmtTimeLive(ev) }}</td>
                <td class="cell-mono cell-accent">{{ ev.event_type }}</td>
                <td class="cell-mono">{{ actor(ev) }}</td>
                <td class="cell-overflow cell-muted" :title="ev.resource_ref">{{ ev.resource_ref }}</td>
                <td><DecisionBadge :decision="ev.decision" /></td>
                <td class="cell-overflow cell-muted" :title="detail(ev)">{{ detail(ev) }}</td>
              </tr>
              <tr
                v-if="expanded.has(ev._id)"
                data-testid="audit-detail"
                class="detail-row"
              >
                <td colspan="6">
                  <EventDetail :ev="ev" />
                </td>
              </tr>
            </template>
          </tbody>
        </table>
      </div>
    </template>
  </div>
</template>

<style scoped>
.view {
  padding: var(--space-6) var(--space-8);
  display: flex;
  flex-direction: column;
  height: 100%;
}

/* ── Header ── */
.view-header {
  display: flex;
  align-items: center;
  gap: var(--space-3);
  margin-bottom: var(--space-4);
  flex: 0 0 auto;
}
.view-header h1 { margin: 0; font-size: 20px; font-weight: 600; }
.view-icon { color: var(--text-muted); width: 22px; height: 22px; }

.conn-status {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  font-size: 12px;
  padding: 2px var(--space-2);
  border-radius: 999px;
  border: 1px solid var(--border);
  color: var(--text-muted);
}
.conn-status.live { color: var(--ok); border-color: var(--ok); }
.conn-status.connecting { color: var(--text-faint); }
.conn-status.reconnecting { color: var(--danger); border-color: var(--danger); }

.conn-dot {
  width: 6px;
  height: 6px;
  border-radius: 50%;
  background: currentColor;
  flex-shrink: 0;
}
.conn-status.connecting .conn-dot {
  animation: pulse 1.4s ease-in-out infinite;
}

@keyframes pulse {
  0%, 100% { opacity: 0.35; }
  50%       { opacity: 1;    }
}

/* ── Empty-state helpers ── */
.hint {
  font-size: 12px;
  text-align: center;
  max-width: 420px;
  color: var(--text-muted);
}
.hint code {
  background: var(--bg-elevated);
  border-radius: 4px;
  padding: 1px 5px;
  font-family: var(--font-mono);
  font-size: 12px;
}

/* ── Disconnected banner ── */
.banner-err {
  background: var(--fill-danger);
  border: 1px solid var(--danger);
  border-radius: var(--radius-sm);
  padding: var(--space-3) var(--space-4);
  color: var(--danger);
  font-size: 13px;
  margin-bottom: var(--space-2);
  flex: 0 0 auto;
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--space-3);
}

.banner-warn {
  background: var(--bg-elevated);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  padding: var(--space-3) var(--space-4);
  color: var(--text-muted);
  font-size: 13px;
  margin-bottom: var(--space-2);
  flex: 0 0 auto;
}

/* ── Toolbar ── */
.toolbar {
  display: flex;
  align-items: center;
  gap: var(--space-3);
  padding: var(--space-2) 0;
  margin-bottom: var(--space-2);
  border-bottom: 1px solid var(--border);
  flex: 0 0 auto;
  flex-wrap: wrap;
}

.toolbar-counts {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  flex-shrink: 0;
}

.count-chip {
  font-size: 12px;
  padding: 2px var(--space-2);
  border-radius: 999px;
  border: 1px solid var(--border);
  white-space: nowrap;
}
.count-chip.ok   { color: var(--ok);     border-color: var(--ok); }
.count-chip.warn { color: var(--accent); border-color: var(--accent); }
.count-chip.deny { color: var(--danger); border-color: var(--danger); }
.count-chip.muted { color: var(--text-muted); }

.toolbar-center {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  flex: 1 1 auto;
}

.filter-select {
  background: var(--bg-elevated);
  color: var(--text);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  padding: 5px var(--space-2);
  font-size: 13px;
  width: 180px;
}

.filter-input {
  background: var(--bg-elevated);
  color: var(--text);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  padding: 5px var(--space-3);
  font-size: 13px;
  flex: 1 1 auto;
  min-width: 180px;
}

.toolbar-actions {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  flex-shrink: 0;
}

.tool-btn {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  font-size: 12px;
  padding: 4px var(--space-2);
  background: var(--bg-elevated);
  color: var(--text);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  cursor: pointer;
  white-space: nowrap;
}
.tool-btn:hover { border-color: var(--accent); color: var(--accent); }
.tool-btn.active { border-color: var(--accent); color: var(--accent); background: var(--fill-accent); }

/* ── Table scroll ── */
.table-scroll {
  flex: 1 1 auto;
  overflow-y: auto;
  position: relative;
}

/* ── Jump pill ── */
.jump-pill {
  position: sticky;
  top: var(--space-2);
  left: 50%;
  transform: translateX(-50%);
  z-index: 10;
  display: inline-flex;
  align-items: center;
  padding: var(--space-1) var(--space-3);
  font-size: 12px;
  background: var(--accent);
  color: var(--text-on-accent);
  border: none;
  border-radius: 999px;
  cursor: pointer;
  box-shadow: 0 2px 8px rgba(0,0,0,0.3);
  margin: var(--space-2) auto var(--space-1);
  display: block;
}
.jump-pill:hover { opacity: 0.9; }

.empty-section {
  padding: var(--space-10) var(--space-3);
  color: var(--text-muted);
  text-align: center;
  font-size: 13px;
}

/* ── Audit table ── */
.audit-table {
  width: 100%;
  border-collapse: collapse;
  font-size: 12px;
  table-layout: fixed;
}

.thead-row th {
  position: sticky;
  top: 0;
  background: var(--bg-elevated);
  color: var(--text-muted);
  font-size: 11px;
  font-weight: 500;
  text-transform: uppercase;
  letter-spacing: .04em;
  padding: var(--space-2) var(--space-3);
  text-align: left;
  border-bottom: 1px solid var(--border);
}

.audit-row td {
  padding: var(--space-1) var(--space-3);
  border-bottom: 1px solid var(--border);
  min-height: 28px;
  vertical-align: middle;
}

.audit-row {
  cursor: pointer;
}
.audit-row:hover td { background: var(--bg-hover); }
.audit-row:focus { outline: 2px solid var(--accent); outline-offset: -2px; }

.detail-row td {
  background: var(--bg-elevated);
  border-bottom: 1px solid var(--border);
  padding: 0;
}

.cell-mono   { font-family: var(--font-mono); }
.cell-muted  { color: var(--text-muted); }
.cell-accent { color: var(--accent); }
.cell-overflow {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
</style>
