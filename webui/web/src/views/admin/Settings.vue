<script setup>
import { computed, markRaw } from "vue";
import { useRoute, useRouter } from "vue-router";
import Icon from "../../components/Icon.vue";
import GeneralSettings from "./GeneralSettings.vue";
import OrgInstructions from "./OrgInstructions.vue";
import Automation from "./Automation.vue";
import WorkflowGovernance from "./WorkflowGovernance.vue";
import Capabilities from "./Capabilities.vue";
import Observability from "./Observability.vue";
import InstallAllowlist from "./InstallAllowlist.vue";
import M365Connection from "./M365Connection.vue";
import WebSearchConfig from "./WebSearchConfig.vue";
import Network from "./Network.vue";
import RateLimits from "./RateLimits.vue";
import SpendCaps from "./SpendCaps.vue";

// Settings is a category-tabbed shell. "General" holds the raw config table;
// the org-governance pages (previously top-level Admin nav items) are folded in
// here as categories. Each pane component is unchanged — it keeps its own
// explanatory lede, load/save wiring, and forbidden empty-state.
const TABS = Object.freeze([
  { key: "general",             label: "General",             comp: markRaw(GeneralSettings) },
  { key: "org-instructions",    label: "Org Instructions",    comp: markRaw(OrgInstructions) },
  { key: "automation",          label: "Automation Controls", comp: markRaw(Automation) },
  { key: "workflow-governance", label: "Workflow Governance", comp: markRaw(WorkflowGovernance) },
  { key: "capabilities",        label: "Capabilities",        comp: markRaw(Capabilities) },
  { key: "network",             label: "Network Access",      comp: markRaw(Network) },
  { key: "rate-limits",         label: "Rate Limits",         comp: markRaw(RateLimits) },
  { key: "spend-caps",          label: "Spend Caps",          comp: markRaw(SpendCaps) },
  { key: "connector-allowlist", label: "Connector Allowlist", comp: markRaw(InstallAllowlist) },
  { key: "m365",                label: "Microsoft 365",       comp: markRaw(M365Connection) },
  { key: "websearch",           label: "Web Search",          comp: markRaw(WebSearchConfig) },
  { key: "observability",       label: "Observability",       comp: markRaw(Observability) },
]);

const KEYS = TABS.map((t) => t.key);

const route  = useRoute();
const router = useRouter();

// Default to General when ?tab is absent or unrecognized — no redirect, so a
// bare /admin/settings renders immediately (the config table mounts on load).
const activeKey = computed(() =>
  KEYS.includes(route.query.tab) ? route.query.tab : "general"
);

const activeComponent = computed(
  () => TABS.find((t) => t.key === activeKey.value).comp
);

function selectTab(key) {
  router.push("/admin/settings?tab=" + key);
}
</script>

<template>
  <div class="settings-tabs">
    <div class="tab-view">
      <div class="view-header">
        <Icon name="settings" class="view-icon" />
        <h1>Settings</h1>
      </div>
      <div class="tab-strip">
        <button
          v-for="t in TABS"
          :key="t.key"
          :data-testid="'settings-tab-' + t.key"
          :class="['tab-btn', { active: t.key === activeKey }]"
          @click="selectTab(t.key)"
        >{{ t.label }}</button>
      </div>
    </div>
    <component :is="activeComponent" />
  </div>
</template>

<style scoped>
.settings-tabs {
  display: flex;
  flex-direction: column;
  flex: 1;
  min-height: 0;
}

.tab-view {
  padding: 24px 2rem 0;
  flex-shrink: 0;
}

.view-header { display: flex; align-items: center; gap: 10px; margin-bottom: 16px; }
.view-header h1 { margin: 0; font-size: 20px; font-weight: 600; }
.view-icon { color: var(--text-muted); width: 22px; height: 22px; }

.tab-strip {
  display: flex;
  flex-wrap: wrap;
  gap: 2px;
  border-bottom: 1px solid var(--border);
}

.tab-btn {
  background: none;
  border: none;
  border-bottom: 2px solid transparent;
  color: var(--text-muted);
  cursor: pointer;
  font-size: 14px;
  padding: 8px 16px;
  margin-bottom: -1px;
  transition: color 0.12s, border-color 0.12s;
}

.tab-btn:hover { color: var(--text); }

.tab-btn.active {
  color: var(--text);
  border-bottom-color: var(--accent);
  font-weight: 600;
}
</style>
