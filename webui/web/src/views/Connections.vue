<script setup>
import { ref, onMounted } from "vue";
import Icon from "../components/Icon.vue";
import { listConnectors, listConnectorProviders, beginConnectorAuth, revokeConnector } from "../api/connectors.js";

const connectors = ref([]);
// Providers the deployment has OAuth credentials for; fetched from the broker so
// unconfigured providers are never offered. Empty → the "Add connection" section
// is hidden entirely.
const providers = ref([]);
const error = ref("");
const busy = ref(false);

function providerLabel(providerEnum) {
  // Proto enum: 1 = GOOGLE_DRIVE, 2 = ONEDRIVE
  if (providerEnum === 1) return "Google Drive";
  if (providerEnum === 2) return "OneDrive";
  return String(providerEnum);
}

function statusClass(status) {
  // Broker writes 'connected' (healthy) and 'reconnect_needed' (F13); older
  // 'active' kept for back-compat.
  if (status === "connected" || status === "active") return "status-ok";
  if (status === "reconnect_needed") return "status-warn";
  return "status-muted";
}

async function load() {
  error.value = "";
  try {
    const [conn, provs] = await Promise.all([listConnectors(), listConnectorProviders()]);
    connectors.value = conn.connectors ?? [];
    providers.value = provs.providers ?? [];
  } catch (e) {
    error.value = e.message || "Failed to load connectors";
  }
}

async function connect(providerKey) {
  error.value = "";
  try {
    const data = await beginConnectorAuth(providerKey);
    window.location.assign(data.authorizeUrl);
  } catch (e) {
    error.value = e.message || "Failed to start OAuth";
  }
}

async function revoke(id) {
  error.value = "";
  try {
    await revokeConnector(id);
    await load();
  } catch (e) {
    error.value = e.message || "Failed to revoke connector";
  }
}

onMounted(load);
</script>

<template>
  <div class="view">
    <header class="view-header">
      <Icon name="connections" :size="20" />
      <h2 class="view-title">Connections</h2>
    </header>

    <div v-if="error" class="error-banner" data-testid="error-banner">
      <Icon name="close" :size="14" />
      {{ error }}
    </div>

    <section class="section">
      <h3 class="section-title">Connected accounts</h3>
      <div v-if="connectors.length === 0 && !error" class="empty">No connected accounts.</div>
      <ul class="connector-list">
        <li
          v-for="c in connectors"
          :key="c.connectorId"
          class="connector-row"
          data-testid="connector-row"
        >
          <Icon name="drive" :size="16" />
          <span class="connector-label">{{ providerLabel(c.provider) }}</span>
          <span class="connector-status" :class="statusClass(c.status)">
            {{ c.status }}
          </span>
          <span v-if="c.managed" class="managed-badge" data-testid="managed-badge">
            Managed by your organization
          </span>
          <button
            v-else
            class="btn-danger btn-sm"
            :data-testid="`revoke-${c.connectorId}`"
            @click="revoke(c.connectorId)"
          >
            <Icon name="trash" :size="14" />
            Revoke
          </button>
        </li>
      </ul>
    </section>

    <section v-if="providers.length > 0" class="section">
      <h3 class="section-title">Add connection</h3>
      <div class="provider-list">
        <button
          v-for="p in providers"
          :key="p.key"
          class="btn-secondary"
          :data-testid="`connect-${p.key}`"
          @click="connect(p.key)"
        >
          <Icon name="drive" :size="16" />
          Connect {{ p.displayName }}
        </button>
      </div>
    </section>
  </div>
</template>

<style scoped>
.view {
  padding: 2rem;
  max-width: 720px;
  margin: 0 auto;
  display: flex;
  flex-direction: column;
  gap: 2rem;
}

.view-header {
  display: flex;
  align-items: center;
  gap: 0.5rem;
  color: var(--text);
}

.view-title {
  font-family: var(--font-sans);
  font-size: 1.25rem;
  font-weight: 500;
  margin: 0;
}

.error-banner {
  display: flex;
  align-items: center;
  gap: 0.5rem;
  padding: 0.75rem 1rem;
  background: var(--fill-danger);
  border: 1px solid var(--danger);
  border-radius: var(--radius-sm);
  color: var(--danger);
  font-size: 0.875rem;
}

.section {
  display: flex;
  flex-direction: column;
  gap: 0.75rem;
}

.section-title {
  font-size: 0.8125rem;
  font-weight: 500;
  color: var(--text-muted);
  text-transform: uppercase;
  letter-spacing: 0.05em;
  margin: 0;
}

.empty {
  color: var(--text-faint);
  font-size: 0.875rem;
}

.connector-list {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 0.5rem;
}

.connector-row {
  display: flex;
  align-items: center;
  gap: 0.75rem;
  padding: 0.75rem 1rem;
  background: var(--bg-elevated);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
}

.connector-label {
  flex: 1;
  font-size: 0.9375rem;
  color: var(--text);
}

.connector-status {
  font-size: 0.75rem;
}

.status-ok {
  color: var(--ok);
}

.status-warn {
  color: var(--danger);
}

.status-muted {
  color: var(--text-muted);
}

.managed-badge {
  font-size: 0.75rem;
  color: var(--text-muted);
  background: var(--bg-elevated);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  padding: 0.25rem 0.625rem;
}

.provider-list {
  display: flex;
  gap: 0.75rem;
  flex-wrap: wrap;
}

.btn-secondary {
  display: inline-flex;
  align-items: center;
  gap: 0.375rem;
  padding: 0.5rem 1rem;
  background: var(--bg-elevated);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  color: var(--text);
  font-family: var(--font-sans);
  font-size: 0.875rem;
  cursor: pointer;
  transition: background 0.15s;
}

.btn-secondary:hover {
  background: var(--bg-hover);
}

.btn-danger {
  display: inline-flex;
  align-items: center;
  gap: 0.375rem;
  background: var(--fill-danger);
  border: 1px solid var(--danger);
  border-radius: var(--radius-sm);
  color: var(--danger);
  font-family: var(--font-sans);
  cursor: pointer;
  transition: background 0.15s;
}

.btn-danger:hover {
  background: var(--danger);
  color: var(--text-on-accent);
}

.btn-sm {
  padding: 0.25rem 0.625rem;
  font-size: 0.8125rem;
}
</style>
