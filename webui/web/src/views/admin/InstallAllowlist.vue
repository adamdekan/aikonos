<script setup>
import { ref, onMounted } from "vue";
import { getOrgSettings, updateOrgSettings } from "../../api/admin.js";
import Icon from "../../components/Icon.vue";
import EmptyState from "../../components/ui/EmptyState.vue";
import ToggleSwitch from "../../components/ui/ToggleSwitch.vue";
import { useToast } from "../../components/ui/useToast.js";

const { push: toast } = useToast();

// The connector provider set is fixed in the broker (connector.Provider). Ids
// must match the broker's string values used by the A7 allowlist check.
const PROVIDERS = [
  { id: "google_drive", label: "Google Drive" },
  { id: "onedrive", label: "OneDrive" },
];

const forbidden = ref(false);
const loading = ref(false);
const error = ref("");
const saving = ref(false);

const enabled = ref(false);
const allowed = ref(new Set());
const updatedBy = ref("");

async function load() {
  loading.value = true;
  error.value = "";
  forbidden.value = false;
  try {
    const data = await getOrgSettings();
    if (data.forbidden) { forbidden.value = true; return; }
    const s = data.settings ?? {};
    enabled.value = s.connectorAllowlistEnabled ?? false;
    allowed.value = new Set(s.connectorAllowlist ?? []);
    updatedBy.value = s.updatedBy ?? "";
  } catch (e) {
    error.value = e.message;
  } finally {
    loading.value = false;
  }
}

onMounted(load);

async function persist() {
  saving.value = true;
  error.value = "";
  try {
    const resp = await updateOrgSettings({
      connectorAllowlistEnabled: enabled.value,
      connectorAllowlist: [...allowed.value],
    });
    if (resp.forbidden) { error.value = resp.error || "You are not a tenant admin."; await load(); return; }
    const s = resp.settings ?? {};
    enabled.value = s.connectorAllowlistEnabled ?? false;
    allowed.value = new Set(s.connectorAllowlist ?? []);
    updatedBy.value = s.updatedBy ?? updatedBy.value;
    toast("ok", "Connector allowlist saved.");
  } catch (e) {
    error.value = e.message;
    await load();
  } finally {
    saving.value = false;
  }
}

function toggleEnabled(next) {
  enabled.value = next;
  persist();
}

function toggleProvider(id) {
  if (allowed.value.has(id)) allowed.value.delete(id);
  else allowed.value.add(id);
  // reassign to trigger reactivity on the Set
  allowed.value = new Set(allowed.value);
  persist();
}
</script>

<template>
  <div class="view">
    <div class="view-header">
      <Icon name="connections" class="view-icon" />
      <h1>Connector Allowlist</h1>
    </div>

    <EmptyState
      v-if="forbidden"
      data-testid="forbidden"
      icon="settings"
      message="You are not a tenant admin."
    />

    <template v-else>
      <p class="lede">
        Control which third-party connectors members may link to their workspace. When the
        allowlist is off, any configured connector can be linked. When on, members can only
        begin OAuth for the providers you check below — every other
        <code>BeginConnectorAuth</code> is refused. MCP servers are separately admin-managed
        and are not affected by this list.
      </p>

      <div v-if="error" data-testid="error-banner" class="banner-err">{{ error }}</div>

      <div class="setting-row" data-testid="allowlist-enabled-row">
        <div class="setting-text">
          <div class="setting-title">Restrict connectors to the allowlist</div>
          <p class="setting-desc">When enabled, only the checked providers below may be linked.</p>
          <p v-if="updatedBy" class="setting-meta">
            Last changed by <span class="mono">{{ updatedBy }}</span>
          </p>
        </div>
        <ToggleSwitch
          :model-value="enabled"
          :disabled="saving || loading"
          aria-label="Restrict connectors to the allowlist"
          data-testid="allowlist-toggle"
          @update:model-value="toggleEnabled"
        />
      </div>

      <div v-if="enabled" class="providers" data-testid="provider-list">
        <div class="providers-head">Allowed providers</div>
        <label
          v-for="p in PROVIDERS"
          :key="p.id"
          class="provider-row"
          :data-testid="`provider-${p.id}`"
        >
          <ToggleSwitch
            :model-value="allowed.has(p.id)"
            :disabled="saving"
            :aria-label="p.label"
            @update:model-value="toggleProvider(p.id)"
          />
          <span>{{ p.label }}</span>
          <code class="pid">{{ p.id }}</code>
        </label>
        <p v-if="allowed.size === 0" class="warn">
          No providers checked — members cannot link any connector while the allowlist is on.
        </p>
      </div>
    </template>
  </div>
</template>

<style scoped>
.view { padding: 24px 32px; max-width: 800px; }
.view-header { display: flex; align-items: center; gap: 10px; margin-bottom: 16px; }
.view-header h1 { margin: 0; font-size: 20px; font-weight: 600; }
.view-icon { color: var(--text-muted); width: 22px; height: 22px; }

.lede { color: var(--text-muted); font-size: 13px; line-height: 1.6; margin: 0 0 20px; max-width: 68ch; }
.lede code, .setting-desc code {
  font-family: var(--font-mono); font-size: 12px;
  background: var(--fill-muted); padding: 1px 5px; border-radius: 4px;
}

.banner-err {
  background: var(--fill-danger); border: 1px solid var(--danger);
  border-radius: var(--radius-sm); padding: 10px 14px;
  color: var(--danger); font-size: 13px; margin-bottom: 16px;
}

.setting-row {
  display: flex; align-items: flex-start; gap: 20px;
  padding: 18px 20px; background: var(--bg-elevated);
  border: 1px solid var(--border); border-radius: var(--radius);
}
.setting-text { flex: 1; }
.setting-title { font-size: 14px; font-weight: 600; margin-bottom: 6px; }
.setting-desc { color: var(--text-muted); font-size: 13px; line-height: 1.6; margin: 0; max-width: 62ch; }
.setting-meta { color: var(--text-faint); font-size: 12px; margin: 8px 0 0; }
.mono { font-family: var(--font-mono); }

.providers {
  margin-top: 16px; padding: 16px 20px;
  background: var(--bg-elevated); border: 1px solid var(--border);
  border-radius: var(--radius);
}
.providers-head {
  font-size: 12px; text-transform: uppercase; letter-spacing: 0.04em;
  color: var(--text-faint); margin-bottom: 12px;
}
.provider-row {
  display: flex; align-items: center; gap: 10px;
  padding: 8px 0; font-size: 14px; cursor: pointer;
}
.pid { font-family: var(--font-mono); font-size: 12px; color: var(--text-faint); margin-left: auto; }
.warn { color: var(--danger); font-size: 12px; margin: 12px 0 0; }
</style>
