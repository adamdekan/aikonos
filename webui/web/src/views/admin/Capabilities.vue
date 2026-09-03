<script setup>
import { ref, onMounted } from "vue";
import { getOrgSettings, updateOrgSettings } from "../../api/admin.js";
import Icon from "../../components/Icon.vue";
import EmptyState from "../../components/ui/EmptyState.vue";
import ToggleSwitch from "../../components/ui/ToggleSwitch.vue";
import { useToast } from "../../components/ui/useToast.js";

const { push: toast } = useToast();

// The disableable effect classes. Names must match the broker's EffectClass
// enum (planv1.EffectClass.String()). READ_ONLY / WRITE_LOCAL are deliberately
// not exposed — disabling them would break ordinary agent operation.
const CAPABILITIES = [
  {
    id: "NETWORK_EGRESS",
    label: "Network egress",
    desc: "Tools that make arbitrary outbound network calls (e.g. web.fetch). Disabling stops agents from reaching external hosts.",
  },
  {
    id: "WRITE_EXTERNAL",
    label: "External writes",
    desc: "Tools that write outside Aikonos — email, external APIs, connector writes. Disabling makes all agents read-only toward third-party systems.",
  },
  {
    id: "CREDENTIAL_ACCESS",
    label: "Credential access",
    desc: "Tools that read secrets, tokens, or keys. Disabling blocks any tool classified as credential-reading.",
  },
  {
    id: "DESTRUCTIVE",
    label: "Destructive actions",
    desc: "Tools that delete, truncate, or disable resources. Disabling blocks destructive operations org-wide.",
  },
];

const forbidden = ref(false);
const loading = ref(false);
const error = ref("");
const saving = ref(false);

const disabled = ref(new Set());
const updatedBy = ref("");

async function load() {
  loading.value = true;
  error.value = "";
  forbidden.value = false;
  try {
    const data = await getOrgSettings();
    if (data.forbidden) { forbidden.value = true; return; }
    const s = data.settings ?? {};
    disabled.value = new Set(s.disabledEffectClasses ?? []);
    updatedBy.value = s.updatedBy ?? "";
  } catch (e) {
    error.value = e.message;
  } finally {
    loading.value = false;
  }
}

onMounted(load);

// Toggle semantics: the switch shows "allowed" (on). Turning it off disables
// the capability org-wide (adds the class to the disabled set).
async function setAllowed(classId, allowed) {
  const prev = new Set(disabled.value);
  if (allowed) disabled.value.delete(classId);
  else disabled.value.add(classId);
  disabled.value = new Set(disabled.value);
  saving.value = true;
  error.value = "";
  try {
    const resp = await updateOrgSettings({ disabledEffectClasses: [...disabled.value] });
    if (resp.forbidden) { disabled.value = prev; error.value = resp.error || "You are not a tenant admin."; return; }
    disabled.value = new Set(resp.settings?.disabledEffectClasses ?? []);
    updatedBy.value = resp.settings?.updatedBy ?? updatedBy.value;
    toast("ok", allowed ? "Capability enabled." : "Capability disabled org-wide.");
  } catch (e) {
    disabled.value = prev;
    error.value = e.message;
  } finally {
    saving.value = false;
  }
}
</script>

<template>
  <div class="view">
    <div class="view-header">
      <Icon name="shield" class="view-icon" />
      <h1>Capabilities</h1>
    </div>

    <EmptyState
      v-if="forbidden"
      data-testid="forbidden"
      icon="settings"
      message="You are not a tenant admin."
    />

    <template v-else>
      <p class="lede">
        Org-wide master switches over broad classes of tool behavior, keyed on each tool's
        authoritative <strong>effect class</strong>. Turning a capability off denies every tool
        in that class at the broker's invocation boundary — above and before per-user grants,
        agent skills, and approvals. This is a kill-switch, not a grant: it can only remove
        access, never add it.
      </p>

      <div v-if="error" data-testid="error-banner" class="banner-err">{{ error }}</div>

      <div class="caps">
        <div
          v-for="c in CAPABILITIES"
          :key="c.id"
          class="cap-row"
          :data-testid="`cap-${c.id}`"
        >
          <div class="cap-text">
            <div class="cap-title">
              {{ c.label }}
              <code class="cls">{{ c.id }}</code>
              <span v-if="disabled.has(c.id)" class="badge-off">Disabled</span>
            </div>
            <p class="cap-desc">{{ c.desc }}</p>
          </div>
          <ToggleSwitch
            :model-value="!disabled.has(c.id)"
            :disabled="saving || loading"
            :aria-label="`Allow ${c.label}`"
            :data-testid="`cap-toggle-${c.id}`"
            @update:model-value="(v) => setAllowed(c.id, v)"
          />
        </div>
      </div>

      <p v-if="updatedBy" class="setting-meta">
        Last changed by <span class="mono">{{ updatedBy }}</span>
      </p>
    </template>
  </div>
</template>

<style scoped>
.view { padding: 24px 32px; max-width: 800px; }
.view-header { display: flex; align-items: center; gap: 10px; margin-bottom: 16px; }
.view-header h1 { margin: 0; font-size: 20px; font-weight: 600; }
.view-icon { color: var(--text-muted); width: 22px; height: 22px; }

.lede { color: var(--text-muted); font-size: 13px; line-height: 1.6; margin: 0 0 20px; max-width: 68ch; }
.lede strong { color: var(--text); }

.banner-err {
  background: var(--fill-danger); border: 1px solid var(--danger);
  border-radius: var(--radius-sm); padding: 10px 14px;
  color: var(--danger); font-size: 13px; margin-bottom: 16px;
}

.caps { display: flex; flex-direction: column; gap: 10px; }
.cap-row {
  display: flex; align-items: flex-start; gap: 20px;
  padding: 16px 20px; background: var(--bg-elevated);
  border: 1px solid var(--border); border-radius: var(--radius);
}
.cap-text { flex: 1; }
.cap-title { font-size: 14px; font-weight: 600; display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
.cls { font-family: var(--font-mono); font-size: 11px; color: var(--text-faint); font-weight: 400; }
.badge-off {
  font-size: 11px; font-weight: 500; color: var(--danger);
  background: var(--fill-danger); border-radius: 4px; padding: 1px 7px;
}
.cap-desc { color: var(--text-muted); font-size: 13px; line-height: 1.6; margin: 6px 0 0; max-width: 60ch; }
.setting-meta { color: var(--text-faint); font-size: 12px; margin: 16px 0 0; }
.mono { font-family: var(--font-mono); }
</style>
