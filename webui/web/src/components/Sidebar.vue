<script setup>
import { ref, computed, onMounted, onUnmounted } from "vue";
import { useRoute, useRouter } from "vue-router";
import Icon from "./Icon.vue";
import UserSwitcher from "./UserSwitcher.vue";
import SessionsNav from "./SessionsNav.vue";

import { useUserStore } from "../store/user.js";
import { useChatStore } from "../store/chat.js";
import { useInboxStore } from "../store/inbox.js";
import { useAgentsStore } from "../store/agents.js";
import { refreshAdminStatus } from "../api/adminStatus.js";
import { listConnectorProviders } from "../api/connectors.js";
import { listUserSkills } from "../api/admin.js";
import { readCollapsed, writeCollapsed } from "../lib/collapsePref.js";

const ADMIN_KEY = "aikonos.sidebar.adminCollapsed";

const collapsed = ref(false);
const adminCollapsed = ref(readCollapsed(ADMIN_KEY));
// Connections is shown only when the deployment has at least one connector
// provider configured (OAuth creds present). No configured provider → the nav
// item is hidden, since nothing can be connected.
const hasConnectorProviders = ref(false);

// Skill-gated feature nav (Schedules, Workflows, …) is hidden until the user's
// granted skills are known, and stays hidden when the grant is absent —
// deny-by-default, matching the broker. Purely cosmetic: every RPC is still
// FGA-gated server-side.
const grantedSkills = ref([]);

function toggleAdmin() {
  adminCollapsed.value = !adminCollapsed.value;
  writeCollapsed(ADMIN_KEY, adminCollapsed.value);
}
const route = useRoute();
const router = useRouter();
const userStore = useUserStore();
const chatStore = useChatStore();
const inboxStore = useInboxStore();
const agentsStore = useAgentsStore();

function newChat() {
  const agentId = route.query.agent || "";
  chatStore.clearActiveSession();
  chatStore.reset();
  router.push(agentId ? `/chat?agent=${agentId}` : "/chat");
}

function toggle() {
  collapsed.value = !collapsed.value;
}

let pollTimer = null;

onMounted(() => {
  refreshAdminStatus();
  inboxStore.refresh();
  agentsStore.refresh();
  listConnectorProviders()
    .then((data) => {
      hasConnectorProviders.value = (data.providers ?? []).length > 0;
    })
    .catch(() => {
      hasConnectorProviders.value = false;
    });
  listUserSkills()
    .then((data) => {
      grantedSkills.value = data.skills ?? [];
    })
    .catch(() => {
      grantedSkills.value = [];
    });
  pollTimer = setInterval(() => {
    inboxStore.refresh();
  }, 15000);
});

onUnmounted(() => {
  if (pollTimer !== null) {
    clearInterval(pollTimer);
    pollTimer = null;
  }
});

// requiredSkill gates a nav item on an FGA capability-skill grant. New
// skill-gated features add their entry here with the matching skill id.
const workspaceItems = computed(() =>
  [
    { label: "Chat", to: "/chat", icon: "chat" },
    { label: "Files", to: "/files", icon: "files" },
    ...(hasConnectorProviders.value
      ? [{ label: "Connections", to: "/connections", icon: "connections" }]
      : []),
    { label: "Schedules", to: "/schedules", icon: "schedules", requiredSkill: "scheduler" },
    { label: "Workflows", to: "/workflows", icon: "play-circle", requiredSkill: "workflows" },
    // No requiredSkill: the nav entry stays present even when the caller lacks
    // skill:personal-skills — Skills.vue renders
    // its own no-access panel on a 403 instead of the item disappearing.
    { label: "My Skills", to: "/skills", icon: "book" },
    { label: "Inbox", to: "/inbox", icon: "inbox" },
  ].filter((item) => !item.requiredSkill || grantedSkills.value.includes(item.requiredSkill)),
);

// Org Instructions / Automation Controls / Workflow Governance / Capabilities
// now live as categories inside Settings (see Settings.vue tab strip), so they
// are intentionally absent from this top-level Admin nav.
// Members lives as the first tab inside Access Control; Connector Allowlist,
// Microsoft 365, Network Access, Rate Limits and Spend Caps as categories
// inside Settings — none get a top-level item.
const adminItems = [
  { label: "Access Control", to: "/admin/roles",         icon: "users" },
  { label: "Tools",          to: "/admin/skills",        icon: "tool" },
  { label: "Skills",         to: "/admin/skill-bundles", icon: "book" },
  { label: "MCP",            to: "/admin/mcp",           icon: "server" },
  { label: "Agents",         to: "/admin/agents",        icon: "bot" },
  { label: "LLM Providers",  to: "/admin/providers",     icon: "cpu" },
  { label: "Runs",           to: "/admin/runs",          icon: "play-circle" },
  { label: "Policy",         to: "/admin/policy",        icon: "scale" },
  { label: "Settings",       to: "/admin/settings",      icon: "settings" },
];

function isActive(path) {
  if (path === "/") return route.path === "/";
  return route.path === path || route.path.startsWith(path + "/");
}
</script>

<template>
  <aside :class="['sidebar', { rail: collapsed }]">
    <div class="sidebar-header">
      <span v-if="!collapsed" class="brand-text">aikonos</span>
      <button
        class="toggle-btn"
        data-testid="sidebar-toggle"
        @click="toggle"
        :title="collapsed ? 'Expand sidebar' : 'Collapse sidebar'"
      >
        <Icon name="sidebar" :size="18" />
      </button>
    </div>

    <div class="sidebar-scroll scroll-min" v-autoscroll>
      <button class="new-chat-btn" data-testid="new-chat-btn" @click="newChat">
        <Icon name="plus" :size="16" />
        <span v-if="!collapsed" class="btn-label">New chat</span>
      </button>

      <nav class="nav-section">
        <div v-if="!collapsed" class="section-label">Workspace</div>
        <router-link
          v-for="item in workspaceItems"
          :key="item.to"
          :to="item.to"
          :class="['nav-item', { active: isActive(item.to) }]"
          :title="item.label"
        >
          <Icon :name="item.icon" :size="18" />
          <span v-if="!collapsed" class="item-label">{{ item.label }}</span>
          <span
            v-if="item.to === '/inbox' && inboxStore.count > 0"
            class="nav-badge"
            data-testid="inbox-badge"
          >{{ inboxStore.count }}</span>
        </router-link>
        <router-link
          v-for="a in agentsStore.assigned"
          :key="a.id"
          :to="`/chat?agent=${a.id}`"
          :class="['nav-item', { active: route.fullPath === `/chat?agent=${a.id}` }]"
          :title="a.name"
          data-testid="assigned-agent"
        >
          <Icon name="chat" :size="18" />
          <span v-if="!collapsed" class="item-label">{{ a.name }}</span>
        </router-link>
      </nav>

      <SessionsNav v-if="!collapsed" />

      <nav v-if="userStore.isAdmin" class="nav-section">
        <div
          v-if="!collapsed"
          class="section-label section-label--collapsible"
          @click="toggleAdmin"
        >
          Admin
          <Icon
            name="chevron-down"
            :size="12"
            :class="['collapse-chevron', { 'collapse-chevron--closed': adminCollapsed }]"
          />
        </div>
        <template v-if="collapsed || !adminCollapsed">
          <router-link
            v-for="item in adminItems"
            :key="item.to"
            :to="item.to"
            :class="['nav-item', { active: isActive(item.to) }]"
            :title="item.label"
          >
            <Icon :name="item.icon" :size="18" />
            <span v-if="!collapsed" class="item-label">{{ item.label }}</span>
          </router-link>
        </template>
      </nav>
    </div>

    <div class="sidebar-footer">
      <UserSwitcher :collapsed="collapsed" />
    </div>
  </aside>
</template>

<style scoped>
.sidebar {
  width: 220px;
  height: 100vh;
  overflow: hidden;
  background: var(--bg-sidebar);
  border-right: 1px solid var(--border);
  display: flex;
  flex-direction: column;
  flex-shrink: 0;
  transition: width 0.2s ease;
}

.sidebar.rail {
  width: 64px;
}

.sidebar.rail .sidebar-header {
  justify-content: center;
  padding-left: 0;
  padding-right: 0;
}

.sidebar-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 16px 14px 12px;
}

.brand-text {
  font-family: "Space Grotesk", "Helvetica Neue", Helvetica, Arial, sans-serif;
  font-weight: 700;
  font-size: 1.25rem;
  letter-spacing: -0.02em;
  color: var(--text);
  line-height: 46px;
}

.toggle-btn {
  background: none;
  border: none;
  color: var(--text-muted);
  cursor: pointer;
  padding: 4px;
  border-radius: var(--radius-sm);
  display: flex;
  align-items: center;
}

.toggle-btn:hover {
  background: var(--bg-hover);
  color: var(--text);
}

.new-chat-btn {
  display: flex;
  align-items: center;
  gap: 8px;
  margin: 0 10px 8px;
  padding: 8px 12px;
  background: var(--accent);
  color: var(--text-on-accent);
  border: 0;
  border-radius: var(--radius-sm);
  font-size: 13px;
  font-weight: 700;
  text-decoration: none;
  transition: background 0.15s;
}

.new-chat-btn:hover {
  background: var(--accent-hover);
  color: var(--text-on-accent);
}

.sidebar.rail .new-chat-btn {
  justify-content: center;
  margin: 0 8px 8px;
  padding: 8px;
}

.nav-section {
  flex: 0;
  padding: 4px 0;
}

.section-label {
  padding: 8px 16px 4px;
  font-size: 10px;
  font-weight: 600;
  text-transform: uppercase;
  letter-spacing: 0.08em;
  color: var(--text-muted);
}

.nav-item {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 8px 14px;
  margin: 1px 8px;
  border-radius: var(--radius-sm);
  color: var(--text-muted);
  text-decoration: none;
  font-size: 14px;
  transition: background 0.12s, color 0.12s;
  cursor: pointer;
}

.nav-item:hover {
  background: var(--bg-hover);
  color: var(--text);
}

.nav-item.active {
  background: var(--bg-active);
  color: var(--text);
  font-weight: 500;
}

.nav-item.active :deep(svg) {
  color: var(--accent);
}

.sidebar.rail .nav-item {
  justify-content: center;
  padding: 8px;
}

.sidebar-scroll {
  flex: 1;
  min-height: 0;
  /* Single scroll container for the whole nav (Workspace + Sessions + Admin),
     so every section stays reachable when all are expanded. Reserve the gutter
     so revealing the auto-hiding bar doesn't shift the nav items. */
  overflow-y: auto;
  scrollbar-gutter: stable;
}

.sidebar-footer {
  display: flex;
  align-items: center;
  flex-shrink: 0;
}

.nav-badge {
  margin-left: auto;
  min-width: 18px;
  height: 18px;
  padding: 0 5px;
  border-radius: 9px;
  background: var(--accent);
  color: var(--text-on-accent);
  font-size: 11px;
  font-weight: 600;
  line-height: 18px;
  text-align: center;
  flex-shrink: 0;
}

.sidebar.rail .nav-badge {
  position: absolute;
  top: 4px;
  right: 4px;
  min-width: 14px;
  height: 14px;
  padding: 0 3px;
  border-radius: 7px;
  font-size: 9px;
  line-height: 14px;
}

.sidebar.rail .nav-item {
  position: relative;
}

.section-label--collapsible {
  display: flex;
  align-items: center;
  justify-content: space-between;
  cursor: pointer;
  user-select: none;
}

.section-label--collapsible:hover {
  color: var(--text);
}

.collapse-chevron {
  flex-shrink: 0;
  transition: transform 0.2s ease;
  /* default: pointing down (expanded) */
}

.collapse-chevron--closed {
  /* pointing right (collapsed): rotate chevron-down by -90deg */
  transform: rotate(-90deg);
}
</style>
