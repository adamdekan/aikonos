<script setup>
import { ref, onMounted } from "vue";
import { getObservability } from "../../api/admin.js";
import Icon from "../../components/Icon.vue";
import Spinner from "../../components/ui/Spinner.vue";
import ErrorBanner from "../../components/ui/ErrorBanner.vue";
import EmptyState from "../../components/ui/EmptyState.vue";

// Read-only telemetry-export state. The OTLP endpoint is AIKONOS_OTEL_ENDPOINT,
// a broker deploy-time env var consumed once at startup to build the
// MeterProvider — it is process-global (not tenant-scoped) and cannot be
// rewired at runtime, so this page displays it and never edits it.
const loading = ref(false);
const error = ref("");
const forbidden = ref(false);
const info = ref(null);

async function load() {
  loading.value = true;
  error.value = "";
  forbidden.value = false;
  try {
    const data = await getObservability();
    if (data.forbidden) { forbidden.value = true; return; }
    info.value = data;
  } catch (e) {
    error.value = e.message || "Failed to load observability info.";
  } finally {
    loading.value = false;
  }
}

onMounted(load);
</script>

<template>
  <div class="view">
    <p class="lede">
      Telemetry export from the broker to an OpenTelemetry collector. The endpoint is set at
      deploy time via the <span class="mono">AIKONOS_OTEL_ENDPOINT</span> environment variable and
      read once when the broker starts — it applies to the whole deployment, not a single tenant.
      Changing it requires editing the env and restarting the broker; it cannot be edited here.
    </p>

    <EmptyState
      v-if="forbidden"
      data-testid="forbidden"
      icon="admin"
      message="You are not a tenant admin."
    />

    <ErrorBanner v-else-if="error" :message="error" data-testid="obs-error">
      <template #action>
        <button class="btn-secondary" data-testid="obs-retry" @click="load">Retry</button>
      </template>
    </ErrorBanner>

    <div v-if="loading" class="loading-row"><Spinner size="md" /></div>

    <div v-else-if="info" class="info-card" data-testid="obs-card">
      <div class="info-row">
        <span class="info-label">Export status</span>
        <span
          :class="info.exportEnabled ? 'badge-on' : 'badge-off'"
          data-testid="obs-status"
        >{{ info.exportEnabled ? "Enabled" : "Disabled" }}</span>
      </div>

      <div class="info-row">
        <span class="info-label">OTLP endpoint</span>
        <span class="info-value mono" data-testid="obs-endpoint">
          {{ info.otelEndpoint || "— (not configured)" }}
        </span>
      </div>

      <div class="info-row">
        <span class="info-label">Source</span>
        <span class="info-value mono">AIKONOS_OTEL_ENDPOINT</span>
      </div>

      <div class="info-row">
        <span class="info-label">Apply a change</span>
        <span class="info-value">Edit the env var and restart the broker</span>
      </div>

      <div class="info-row">
        <span class="info-label">PromQL note</span>
        <span class="info-value">
          The collector scrape overwrites <span class="mono">job</span> → <span class="mono">otel-collector</span>;
          filter broker series by <span class="mono">exported_job="{{ info.exportedJob }}"</span>.
        </span>
      </div>

      <p v-if="!info.exportEnabled" class="hint">
        <Icon name="settings" :size="14" />
        No endpoint configured — set <span class="mono">AIKONOS_OTEL_ENDPOINT=otel-collector:4317</span>
        and bring up the <span class="mono">obs</span> profile to wire dashboards.
      </p>
    </div>
  </div>
</template>

<style scoped>
.view { padding: 24px 32px; max-width: 760px; }

.lede { color: var(--text-muted); font-size: 13px; line-height: 1.6; margin: 0 0 16px; max-width: 72ch; }

.loading-row { display: flex; padding: 32px 0; }

.info-card {
  background: var(--bg-elevated); border: 1px solid var(--border);
  border-radius: var(--radius); padding: 8px 20px;
}

.info-row {
  display: flex; align-items: baseline; gap: 16px;
  padding: 12px 0; border-bottom: 1px solid var(--border);
}
.info-row:last-of-type { border-bottom: none; }

.info-label {
  flex: 0 0 130px; font-size: 12px; font-weight: 600; color: var(--text-muted);
  text-transform: uppercase; letter-spacing: 0.04em;
}
.info-value { font-size: 13px; color: var(--text); line-height: 1.6; }

.mono { font-family: var(--font-mono); font-size: 12px; }

.badge-on, .badge-off {
  font-size: 11px; font-weight: 500; border-radius: 4px; padding: 2px 8px;
}
.badge-on  { color: var(--ok); background: var(--fill-ok); }
.badge-off { color: var(--text-faint); background: var(--fill-muted); }

.hint {
  display: flex; align-items: center; gap: 6px;
  margin: 12px 0 4px; font-size: 12px; color: var(--text-muted);
}

.btn-secondary {
  background: transparent; color: var(--text-muted);
  border: 1px solid var(--border); border-radius: var(--radius-sm);
  padding: 5px 11px; font-size: 12px; cursor: pointer;
}
.btn-secondary:hover { background: var(--bg-hover); color: var(--text); }
</style>
