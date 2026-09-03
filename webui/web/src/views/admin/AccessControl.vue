<script setup>
import { ref, onMounted, provide } from "vue";
import AdvancedTuples from "./AdvancedTuples.vue";
import Provisioning from "./Provisioning.vue";
import Members from "./Members.vue";
import UsersTab from "./UsersTab.vue";
import GroupsTab from "./GroupsTab.vue";
import ToolsTab from "./ToolsTab.vue";
import AgentsTab from "./AgentsTab.vue";
import Icon from "../../components/Icon.vue";
import EmptyState from "../../components/ui/EmptyState.vue";
import { useAccessControl, ACCESS_CTX } from "./useAccessControl.js";

// ── tab-local selection refs declared first so the composable can close over them ──

const selectedUser  = ref(null);   // principal object
const selectedGroup = ref(null);   // group object from derivedGroups
const selectedAgent = ref(null);

// ── shared composable — instantiated once, provided to future tab children ────

const ctx = useAccessControl({ selectedUser, selectedGroup, selectedAgent });
provide(ACCESS_CTX, ctx);

const {
  fgaEnabled, warnings, forbidden,
  error, mutError,
  load,
} = ctx;

// ── tabs ──────────────────────────────────────────────────────────────────────

const TABS = ["Members", "Users", "Groups", "Tools", "Agents", "Provisioning", "Advanced"];
const activeTab = ref("Members");

onMounted(load);

// Reset selection when tab changes — selectedUser/Group/Agent are shell refs
// injected by tab children; they survive unmount so must be cleared explicitly.
function setTab(tab) {
  activeTab.value     = tab;
  selectedUser.value  = null;
  selectedGroup.value = null;
  selectedAgent.value = null;
}


</script>

<template>
  <div class="view">
    <div class="view-header">
      <Icon name="users" class="view-icon" />
      <h1>Access Control</h1>
    </div>

    <!-- not-an-admin empty-state -->
    <EmptyState
      v-if="forbidden"
      data-testid="forbidden"
      icon="admin"
      message="You are not a tenant admin."
    />

    <template v-else>
      <!-- dev allow-all banner -->
      <div v-if="!fgaEnabled" data-testid="fga-disabled-banner" class="banner-warn">
        OpenFGA is disabled — dev allow-all mode. Role assignments are not enforced.
      </div>

      <!-- warnings -->
      <div v-for="(w, i) in warnings" :key="i" class="banner-warn small">{{ w }}</div>

      <!-- load error -->
      <div v-if="error" data-testid="error-banner" class="banner-err">{{ error }}</div>

      <!-- mutation error -->
      <div v-if="mutError" class="banner-err">{{ mutError }}</div>

      <!-- tab strip -->
      <div class="tab-strip" data-testid="tab-strip">
        <button
          v-for="tab in TABS"
          :key="tab"
          :class="['tab-btn', { active: activeTab === tab }]"
          :data-testid="`tab-${tab}`"
          @click="setTab(tab)"
        >{{ tab }}</button>
      </div>

      <!-- ── Members tab — embed Members (keeps its Export CSV header action) ─ -->
      <div v-if="activeTab === 'Members'" class="embedded-pane keep-actions" data-testid="pane-Members">
        <Members />
      </div>

      <!-- ── Users tab ───────────────────────────────────────────────────────── -->
      <UsersTab v-else-if="activeTab === 'Users'" />

      <!-- ── Groups tab ──────────────────────────────────────────────────────── -->
      <GroupsTab v-else-if="activeTab === 'Groups'" />

      <!-- ── Tools tab ─────────────────────────────────────────────────────── -->
      <ToolsTab v-else-if="activeTab === 'Tools'" />

      <!-- ── Agents tab ─────────────────────────────────────────────────────── -->
      <AgentsTab v-else-if="activeTab === 'Agents'" />

      <!-- ── Provisioning tab — embed Provisioning ───────────────────────── -->
      <div v-else-if="activeTab === 'Provisioning'" class="embedded-pane">
        <Provisioning />
      </div>

      <!-- ── Advanced tab — embed AdvancedTuples ─────────────────────────── -->
      <div v-else-if="activeTab === 'Advanced'" class="advanced-pane">
        <AdvancedTuples />
      </div>
    </template>
  </div>
</template>

<style scoped>
.view { padding: 24px 32px; max-width: 1100px; }
.view-header { display: flex; align-items: center; gap: 10px; margin-bottom: 20px; }
.view-header h1 { margin: 0; font-size: 20px; font-weight: 600; }
.view-icon { color: var(--text-muted); width: 22px; height: 22px; }

.banner-warn {
  background: var(--fill-accent); border: 1px solid var(--accent);
  border-radius: var(--radius-sm); padding: 10px 14px;
  color: var(--accent); font-size: 13px; margin-bottom: 12px;
}
.banner-warn.small { font-size: 12px; padding: 6px 10px; }
.banner-err {
  background: var(--fill-danger); border: 1px solid var(--danger);
  border-radius: var(--radius-sm); padding: 10px 14px;
  color: var(--danger); font-size: 13px; margin-bottom: 12px;
}

/* ── tab strip ─────────────────────────────────────────────────────── */
.tab-strip {
  display: flex; flex-wrap: wrap; gap: 2px; margin-bottom: 20px;
  border-bottom: 1px solid var(--border);
}
.tab-btn {
  background: none; border: none; border-bottom: 2px solid transparent;
  color: var(--text-muted); cursor: pointer; font-size: 14px;
  padding: 8px 16px; margin-bottom: -1px; transition: color 0.12s, border-color 0.12s;
}
.tab-btn:hover { color: var(--text); }
.tab-btn.active { color: var(--text); border-bottom-color: var(--accent); font-weight: 600; }

/* ── placeholder / advanced ─────────────────────────────────────────── */
.advanced-pane { margin-top: -8px; }
/* Provisioning.vue carries its own .view padding/header; pull it back flush
   with the tab strip and let it own its internal layout. */
.embedded-pane :deep(.view) { padding: 0; max-width: none; }
.embedded-pane:not(.keep-actions) :deep(.view-header) { display: none; }
/* Members.vue's header carries the Export CSV action — keep the header row,
   hide only the icon + title the tab strip already provides. */
.embedded-pane.keep-actions :deep(.view-header .view-icon),
.embedded-pane.keep-actions :deep(.view-header h1) { display: none; }
</style>
