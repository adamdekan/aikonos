<script setup>
import { ref } from "vue";
import { get } from "../../api/client.js";
import Icon from "../../components/Icon.vue";
import DataTable from "../../components/ui/DataTable.vue";
import EmptyState from "../../components/ui/EmptyState.vue";
import FormField from "../../components/ui/FormField.vue";
import Spinner from "../../components/ui/Spinner.vue";
import DecisionBadge from "../../components/audit/DecisionBadge.vue";
import EventDetail from "../../components/audit/EventDetail.vue";
import { fmtTimeHistory, actor } from "../../components/audit/audit-format.js";
import { toJson, toCsv } from "./auditExport.js";

// ── Search form state ─────────────────────────────────────────────────────────
const filterStart     = ref("");
const filterEnd       = ref("");
const filterActor     = ref("");
const filterEventType = ref("");
const filterDecision  = ref("");

// ── Result state ──────────────────────────────────────────────────────────────
const events     = ref([]);
const nextCursor = ref("");
const loading    = ref(false);
const forbidden  = ref(false);

// ── Verify state ──────────────────────────────────────────────────────────────
const verifyResult  = ref(null);   // VerifyAuditChainResponse shape or null
const verifyLoading = ref(false);

// ── Side inspector ────────────────────────────────────────────────────────────
const selectedEvent = ref(null);

// The last filter params used for the initial search — reused on "Load more".
let lastParams = null;

function buildParams(cursor = "") {
  const p = new URLSearchParams();
  if (filterStart.value)     p.set("start",      filterStart.value);
  if (filterEnd.value)       p.set("end",        filterEnd.value);
  if (filterActor.value)     p.set("actor",      filterActor.value);
  if (filterEventType.value) p.set("event_type", filterEventType.value);
  if (filterDecision.value)  p.set("decision",   filterDecision.value);
  if (cursor)                p.set("cursor",     cursor);
  return p.toString();
}

async function search() {
  forbidden.value = false;
  loading.value   = true;
  events.value    = [];
  nextCursor.value = "";
  verifyResult.value = null;
  selectedEvent.value = null;
  lastParams = buildParams();
  try {
    const qs   = lastParams ? `?${lastParams}` : "";
    const resp = await get(`/admin/audit/query${qs}`);
    if (resp.forbidden) { forbidden.value = true; return; }
    events.value     = resp.events     ?? [];
    nextCursor.value = resp.nextCursor ?? "";
  } finally {
    loading.value = false;
  }
}

async function loadMore() {
  if (!nextCursor.value) return;
  loading.value = true;
  try {
    const base   = lastParams ? `${lastParams}&` : "";
    const qs     = `?${base}cursor=${encodeURIComponent(nextCursor.value)}`;
    const resp   = await get(`/admin/audit/query${qs}`);
    if (resp.forbidden) { forbidden.value = true; return; }
    events.value     = [...events.value, ...(resp.events ?? [])];
    nextCursor.value = resp.nextCursor ?? "";
  } finally {
    loading.value = false;
  }
}

async function verify() {
  forbidden.value    = false;
  verifyResult.value = null;
  verifyLoading.value = true;
  try {
    const resp = await get("/admin/audit/verify");
    if (resp.forbidden) { forbidden.value = true; return; }
    verifyResult.value = resp;
  } finally {
    verifyLoading.value = false;
  }
}

function selectRow(ev) {
  selectedEvent.value = selectedEvent.value?.event_id === ev.event_id ? null : ev;
}

function closeInspector() {
  selectedEvent.value = null;
}

// Delegate row-click from DataTable tbody: find the nearest [data-testid='audit-history-row']
// ancestor of the click target, then match it to events by index.
function onTableClick(e) {
  const row = e.target.closest("[data-testid='audit-history-row']");
  if (!row) return;
  // Find the row's index within its tbody
  const tbody = row.parentElement;
  if (!tbody) return;
  const idx = Array.from(tbody.children).indexOf(row);
  if (idx >= 0 && idx < events.value.length) {
    selectRow(events.value[idx]);
  }
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
  downloadBlob(toJson(events.value), "audit-history.json", "application/json");
}

function exportCsv() {
  downloadBlob(toCsv(events.value), "audit-history.csv", "text/csv");
}

// clipboard copy for verify card chips
const copiedId = ref(null);
function copyId(id) {
  navigator.clipboard?.writeText(id).then(() => {
    copiedId.value = id;
    setTimeout(() => { copiedId.value = null; }, 1000);
  }).catch(() => {});
}

const TABLE_COLS = [
  { key: "time",     label: "Time",     width: "140px" },
  { key: "event",    label: "Event",    width: "220px" },
  { key: "actor",    label: "Actor",    width: "160px" },
  { key: "resource", label: "Resource" },
  { key: "decision", label: "Decision", width: "90px" },
];
</script>

<template>
  <div class="view">
    <!-- ── Header ─────────────────────────────────────────────────────── -->
    <div class="view-header">
      <Icon name="audit" class="view-icon" />
      <h1>Audit History</h1>
    </div>

    <!-- ── Not-an-admin empty-state ──────────────────────────────────── -->
    <EmptyState
      v-if="forbidden"
      data-testid="forbidden"
      icon="admin"
      message="You are not a tenant admin."
    />

    <template v-else>
      <!-- ── Toolbar (filter bar) ───────────────────────────────────── -->
      <div class="toolbar">
        <div class="filter-grid">
          <FormField label="Start">
            <input
              data-testid="filter-start"
              type="datetime-local"
              v-model="filterStart"
              class="form-input"
            />
          </FormField>
          <FormField label="End">
            <input
              data-testid="filter-end"
              type="datetime-local"
              v-model="filterEnd"
              class="form-input"
            />
          </FormField>
          <FormField label="Actor">
            <input
              data-testid="filter-actor"
              type="text"
              v-model="filterActor"
              placeholder="user@example.com"
              class="form-input"
            />
          </FormField>
          <FormField label="Event type">
            <input
              data-testid="filter-event-type"
              type="text"
              v-model="filterEventType"
              placeholder="aikonos.broker.…"
              class="form-input"
            />
          </FormField>
          <FormField label="Decision">
            <select data-testid="filter-decision" v-model="filterDecision" class="form-input narrow">
              <option value="">All</option>
              <option value="1">ALLOW</option>
              <option value="2">DENY</option>
              <option value="3">APPROVAL</option>
              <option value="4">STEP-UP</option>
            </select>
          </FormField>
        </div>

        <div class="toolbar-actions">
          <button
            data-testid="search-btn"
            class="btn-primary"
            :disabled="loading"
            @click="search"
          >
            <Icon name="audit" :size="14" /> Search
          </button>
          <button
            data-testid="verify-btn"
            class="btn-ghost"
            :disabled="verifyLoading"
            @click="verify"
          >
            <Spinner v-if="verifyLoading" size="sm" />
            Verify integrity
          </button>
          <button class="btn-ghost" @click="exportJson" title="Export loaded events as JSON">
            <Icon name="download" :size="14" /> JSON
          </button>
          <button class="btn-ghost" @click="exportCsv" title="Export loaded events as CSV">
            <Icon name="download" :size="14" /> CSV
          </button>
        </div>
      </div>

      <!-- ── Verify result card ─────────────────────────────────────── -->
      <div
        v-if="verifyResult"
        data-testid="verify-result"
        :class="['verify-card', verifyResult.ok ? 'ok' : 'fail']"
      >
        <div class="verify-status">
          <Icon :name="verifyResult.ok ? 'check' : 'close'" :size="15" />
          <template v-if="verifyResult.ok">
            Chain intact · {{ verifyResult.total }} events
            <span v-if="verifyResult.signed"> · signatures verified</span>
          </template>
          <template v-else>
            Chain NOT intact · {{ verifyResult.total }} events ·
            {{ verifyResult.breaks?.length ?? 0 }} break(s) ·
            {{ verifyResult.signatureFailures?.length ?? 0 }} sig failure(s)
          </template>
        </div>
        <template v-if="!verifyResult.ok">
          <div v-if="verifyResult.breaks?.length" class="id-list">
            <span class="id-list-label">Breaks:</span>
            <button
              v-for="b in verifyResult.breaks"
              :key="b.eventId"
              class="event-id-chip"
              :class="{ copied: copiedId === b.eventId }"
              @click="copyId(b.eventId)"
            >{{ b.eventId }}</button>
          </div>
          <div v-if="verifyResult.signatureFailures?.length" class="id-list">
            <span class="id-list-label">Sig failures:</span>
            <button
              v-for="f in verifyResult.signatureFailures"
              :key="f.eventId"
              class="event-id-chip"
              :class="{ copied: copiedId === f.eventId }"
              @click="copyId(f.eventId)"
            >{{ f.eventId }}</button>
          </div>
        </template>
      </div>

      <!-- ── Table + side inspector ──────────────────────────────────── -->
      <div class="content-area">
        <div class="table-wrap" @click.capture="onTableClick">
          <DataTable
            :columns="TABLE_COLS"
            :rows="events"
            :loading="loading"
            empty-text=""
            :row-attrs="{ 'data-testid': 'audit-history-row' }"
          >
            <template #row="{ row: ev }">
              <td class="muted mono">{{ fmtTimeHistory(ev) }}</td>
              <td class="accent mono">{{ ev.event_type }}</td>
              <td class="mono">{{ actor(ev) }}</td>
              <td class="muted overflow" :title="ev.resource_ref">{{ ev.resource_ref }}</td>
              <td><DecisionBadge :decision="ev.decision" /></td>
            </template>
          </DataTable>

          <!-- Load more -->
          <button
            v-if="nextCursor && !loading"
            data-testid="load-more-btn"
            class="btn-ghost load-more"
            @click="loadMore"
          >
            <Spinner v-if="loading" size="sm" />
            Load more
          </button>
        </div>

        <!-- Side inspector -->
        <div v-if="selectedEvent" class="side-inspector">
          <div class="inspector-header">
            <span class="inspector-title">Event</span>
            <button class="close-btn" @click="closeInspector" title="Close">
              <Icon name="close" :size="14" />
            </button>
          </div>
          <EventDetail :ev="selectedEvent" />
        </div>
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

/* ── Toolbar ── */
.toolbar {
  display: flex;
  align-items: flex-end;
  gap: var(--space-4);
  padding: var(--space-2) 0 var(--space-3);
  margin-bottom: var(--space-3);
  border-bottom: 1px solid var(--border);
  flex: 0 0 auto;
  flex-wrap: wrap;
}

.filter-grid {
  display: flex;
  gap: var(--space-3);
  flex-wrap: wrap;
  flex: 1 1 auto;
}

.form-input {
  background: var(--bg-elevated);
  color: var(--text);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  padding: 5px var(--space-2);
  font-size: 13px;
  min-width: 160px;
}
.form-input.narrow { min-width: 100px; }

.toolbar-actions {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  flex-shrink: 0;
  flex-wrap: wrap;
}

.btn-primary {
  display: inline-flex;
  align-items: center;
  gap: var(--space-1);
  background: var(--accent);
  color: var(--text-on-accent);
  border: none;
  border-radius: var(--radius-sm);
  padding: 6px var(--space-3);
  font-size: 13px;
  cursor: pointer;
}
.btn-primary:disabled { opacity: 0.5; cursor: default; }

.btn-ghost {
  display: inline-flex;
  align-items: center;
  gap: var(--space-1);
  background: transparent;
  color: var(--text);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  padding: 6px var(--space-3);
  font-size: 13px;
  cursor: pointer;
}
.btn-ghost:disabled { opacity: 0.5; cursor: default; }
.btn-ghost:hover:not(:disabled) { border-color: var(--accent); color: var(--accent); }

/* ── Verify card ── */
.verify-card {
  border-radius: var(--radius-sm);
  padding: var(--space-3) var(--space-4);
  font-size: 13px;
  margin-bottom: var(--space-3);
  flex: 0 0 auto;
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
}
.verify-card.ok {
  background: var(--fill-ok);
  border: 1px solid var(--ok);
  color: var(--ok);
}
.verify-card.fail {
  background: var(--fill-danger);
  border: 1px solid var(--danger);
  color: var(--danger);
}

.verify-status {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  font-weight: 500;
}

.id-list {
  display: flex;
  align-items: center;
  gap: var(--space-1);
  flex-wrap: wrap;
  font-size: 12px;
}
.id-list-label {
  color: inherit;
  opacity: 0.75;
  white-space: nowrap;
}
.event-id-chip {
  font-family: var(--font-mono);
  background: var(--bg-elevated);
  border: none;
  border-radius: 3px;
  padding: 1px var(--space-2);
  cursor: pointer;
  font-size: 11px;
  color: inherit;
  transition: opacity 0.15s;
}
.event-id-chip:hover { opacity: 0.75; }
.event-id-chip.copied { opacity: 0.55; }

/* ── Content area (table + inspector) ── */
.content-area {
  display: flex;
  gap: var(--space-4);
  flex: 1 1 auto;
  overflow: hidden;
}

.table-wrap {
  flex: 1 1 auto;
  overflow-y: auto;
  min-width: 0;
}

.load-more {
  margin-top: var(--space-3);
  width: 100%;
  justify-content: center;
}

/* ── Side inspector ── */
.side-inspector {
  width: 420px;
  flex-shrink: 0;
  border-left: 1px solid var(--border);
  display: flex;
  flex-direction: column;
  overflow-y: auto;
  background: var(--bg-elevated);
  border-radius: 0 var(--radius-sm) var(--radius-sm) 0;
}

.inspector-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: var(--space-3) var(--space-4);
  border-bottom: 1px solid var(--border);
  flex-shrink: 0;
}
.inspector-title {
  font-size: 12px;
  font-weight: 600;
  text-transform: uppercase;
  letter-spacing: .05em;
  color: var(--text-muted);
}
.close-btn {
  background: none;
  border: none;
  cursor: pointer;
  color: var(--text-muted);
  display: flex;
  align-items: center;
  padding: 2px;
  border-radius: var(--radius-sm);
}
.close-btn:hover { color: var(--text); }

/* ── DataTable row styling (scoped, applies via :deep) ── */
:deep(.dt-table td) {
  font-size: 13px;
  min-height: 34px;
}
:deep(.dt-table tbody tr) {
  cursor: pointer;
}
:deep(.dt-table tbody tr.selected td) {
  background: var(--bg-active);
}

/* ── Shared cell helpers ── */
.mono    { font-family: var(--font-mono); }
.muted   { color: var(--text-muted); }
.accent  { color: var(--accent); }
.overflow { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
</style>
