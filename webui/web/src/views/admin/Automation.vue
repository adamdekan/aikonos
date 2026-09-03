<script setup>
import { ref, onMounted } from "vue";
import { getOrgSettings, updateOrgSettings } from "../../api/admin.js";
import Icon from "../../components/Icon.vue";
import EmptyState from "../../components/ui/EmptyState.vue";
import ToggleSwitch from "../../components/ui/ToggleSwitch.vue";
import { useToast } from "../../components/ui/useToast.js";

const { push: toast } = useToast();

const forbidden = ref(false);
const loading = ref(false);
const error = ref("");
const saving = ref(false);

const unattendedAllowed = ref(true);
const updatedBy = ref("");

async function load() {
  loading.value = true;
  error.value = "";
  forbidden.value = false;
  try {
    const data = await getOrgSettings();
    if (data.forbidden) { forbidden.value = true; return; }
    const s = data.settings ?? {};
    unattendedAllowed.value = s.unattendedAllowed ?? true;
    updatedBy.value = s.updatedBy ?? "";
  } catch (e) {
    error.value = e.message;
  } finally {
    loading.value = false;
  }
}

onMounted(load);

async function setUnattended(next) {
  const prev = unattendedAllowed.value;
  unattendedAllowed.value = next; // optimistic
  saving.value = true;
  error.value = "";
  try {
    const resp = await updateOrgSettings({ unattendedAllowed: next });
    if (resp.forbidden) {
      unattendedAllowed.value = prev;
      error.value = resp.error || "You are not a tenant admin.";
      return;
    }
    unattendedAllowed.value = resp.settings?.unattendedAllowed ?? next;
    updatedBy.value = resp.settings?.updatedBy ?? updatedBy.value;
    toast("ok", next ? "Unattended mode allowed." : "Unattended mode disabled org-wide.");
  } catch (e) {
    unattendedAllowed.value = prev; // rollback
    error.value = e.message;
  } finally {
    saving.value = false;
  }
}
</script>

<template>
  <div class="view">
    <div class="view-header">
      <Icon name="bot" class="view-icon" />
      <h1>Automation Controls</h1>
    </div>

    <EmptyState
      v-if="forbidden"
      data-testid="forbidden"
      icon="settings"
      message="You are not a tenant admin."
    />

    <template v-else>
      <p class="lede">
        Org-wide controls over how much your agents may act without a human in the loop.
        These settings sit above per-agent configuration — when a control is off here, no
        agent can opt back into it.
      </p>

      <div v-if="error" data-testid="error-banner" class="banner-err">{{ error }}</div>

      <div class="setting-row" data-testid="unattended-row">
        <div class="setting-text">
          <div class="setting-title">Allow unattended (auto-approve) mode</div>
          <p class="setting-desc">
            When enabled, agents configured with <code>approval_mode: auto</code> may invoke
            tools without per-action approval during interactive chat. This increases exposure
            to prompt injection — content from a fetched page or connector could steer the
            agent to act without a person confirming. Turn this off to force every interactive
            run through human-in-the-loop approval, regardless of agent configuration.
          </p>
          <p v-if="updatedBy" class="setting-meta">
            Last changed by <span class="mono">{{ updatedBy }}</span>
          </p>
        </div>
        <ToggleSwitch
          :model-value="unattendedAllowed"
          :disabled="saving || loading"
          aria-label="Allow unattended auto-approve mode"
          data-testid="unattended-toggle"
          @update:model-value="setUnattended"
        />
      </div>
    </template>
  </div>
</template>

<style scoped>
.view { padding: 24px 32px; max-width: 800px; }
.view-header { display: flex; align-items: center; gap: 10px; margin-bottom: 16px; }
.view-header h1 { margin: 0; font-size: 20px; font-weight: 600; }
.view-icon { color: var(--text-muted); width: 22px; height: 22px; }

.lede {
  color: var(--text-muted); font-size: 13px; line-height: 1.6;
  margin: 0 0 20px; max-width: 68ch;
}

.banner-err {
  background: var(--fill-danger); border: 1px solid var(--danger);
  border-radius: var(--radius-sm); padding: 10px 14px;
  color: var(--danger); font-size: 13px; margin-bottom: 16px;
}

.setting-row {
  display: flex; align-items: flex-start; gap: 20px;
  padding: 18px 20px;
  background: var(--bg-elevated);
  border: 1px solid var(--border);
  border-radius: var(--radius);
}
.setting-text { flex: 1; }
.setting-title { font-size: 14px; font-weight: 600; margin-bottom: 6px; }
.setting-desc {
  color: var(--text-muted); font-size: 13px; line-height: 1.6; margin: 0; max-width: 62ch;
}
.setting-desc code {
  font-family: var(--font-mono); font-size: 12px;
  background: var(--fill-muted); padding: 1px 5px; border-radius: 4px;
}
.setting-meta { color: var(--text-faint); font-size: 12px; margin: 8px 0 0; }
.mono { font-family: var(--font-mono); }
</style>
