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

const sharingAllowed = ref(true);
const updatedBy = ref("");

async function load() {
  loading.value = true;
  error.value = "";
  forbidden.value = false;
  try {
    const data = await getOrgSettings();
    if (data.forbidden) { forbidden.value = true; return; }
    const s = data.settings ?? {};
    sharingAllowed.value = s.workflowSharingAllowed ?? true;
    updatedBy.value = s.updatedBy ?? "";
  } catch (e) {
    error.value = e.message;
  } finally {
    loading.value = false;
  }
}

onMounted(load);

async function setSharing(next) {
  const prev = sharingAllowed.value;
  sharingAllowed.value = next;
  saving.value = true;
  error.value = "";
  try {
    const resp = await updateOrgSettings({ workflowSharingAllowed: next });
    if (resp.forbidden) {
      sharingAllowed.value = prev;
      error.value = resp.error || "You are not a tenant admin.";
      return;
    }
    sharingAllowed.value = resp.settings?.workflowSharingAllowed ?? next;
    updatedBy.value = resp.settings?.updatedBy ?? updatedBy.value;
    toast("ok", next ? "Workflow sharing allowed." : "Workflow sharing disabled org-wide.");
  } catch (e) {
    sharingAllowed.value = prev;
    error.value = e.message;
  } finally {
    saving.value = false;
  }
}
</script>

<template>
  <div class="view">
    <div class="view-header">
      <Icon name="book" class="view-icon" />
      <h1>Workflow Governance</h1>
    </div>

    <EmptyState
      v-if="forbidden"
      data-testid="forbidden"
      icon="settings"
      message="You are not a tenant admin."
    />

    <template v-else>
      <p class="lede">
        Controls over reusable, user-authored workflows — Aikonos's shareable automation
        artifact. Authoring and running workflows stays governed by the per-user
        <code>skill:workflows</code> grant in Access Control; this page adds org-wide masters
        on top of that.
      </p>

      <div v-if="error" data-testid="error-banner" class="banner-err">{{ error }}</div>

      <div class="setting-row" data-testid="workflow-sharing-row">
        <div class="setting-text">
          <div class="setting-title">Allow workflow sharing</div>
          <p class="setting-desc">
            When enabled, a member may publish a proven, success-rated workflow to their
            groups so teammates can run it. Turn this off to keep every workflow private to
            its author org-wide — <code>workflow_publish</code> is refused for everyone,
            regardless of individual grants. Existing published workflows remain, but no new
            shares can be made.
          </p>
          <p v-if="updatedBy" class="setting-meta">
            Last changed by <span class="mono">{{ updatedBy }}</span>
          </p>
        </div>
        <ToggleSwitch
          :model-value="sharingAllowed"
          :disabled="saving || loading"
          aria-label="Allow workflow sharing"
          data-testid="workflow-sharing-toggle"
          @update:model-value="setSharing"
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
  padding: 18px 20px;
  background: var(--bg-elevated);
  border: 1px solid var(--border);
  border-radius: var(--radius);
}
.setting-text { flex: 1; }
.setting-title { font-size: 14px; font-weight: 600; margin-bottom: 6px; }
.setting-desc { color: var(--text-muted); font-size: 13px; line-height: 1.6; margin: 0; max-width: 62ch; }
.setting-meta { color: var(--text-faint); font-size: 12px; margin: 8px 0 0; }
.mono { font-family: var(--font-mono); }
</style>
