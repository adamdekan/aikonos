<script setup>
import { computed, markRaw, onMounted } from "vue";
import { useRoute, useRouter } from "vue-router";
import Audit from "./Audit.vue";
import AuditHistory from "./AuditHistory.vue";
import DecisionTrace from "./DecisionTrace.vue";
import PolicySimulator from "./PolicySimulator.vue";
import Alerts from "./Alerts.vue";

const ESF = (url) => new EventSource(url);

const TABS = Object.freeze([
  { key: "audit",      label: "Audit",         comp: markRaw(Audit) },
  { key: "history",    label: "Audit History",  comp: markRaw(AuditHistory) },
  { key: "decisions",  label: "Decisions",      comp: markRaw(DecisionTrace) },
  { key: "simulator",  label: "Simulator",      comp: markRaw(PolicySimulator) },
  { key: "alerts",     label: "Alerts",         comp: markRaw(Alerts) },
]);

const KEYS = TABS.map((t) => t.key);

const route  = useRoute();
const router = useRouter();

const activeKey = computed(() =>
  KEYS.includes(route.query.tab) ? route.query.tab : "audit"
);

const activeComponent = computed(
  () => TABS.find((t) => t.key === activeKey.value).comp
);

onMounted(() => {
  if (!KEYS.includes(route.query.tab)) {
    router.replace("/admin/policy?tab=audit");
  }
});

function selectTab(key) {
  router.push("/admin/policy?tab=" + key);
}
</script>

<template>
  <div class="policy-tabs">
    <div class="tab-view">
      <div class="tab-strip">
        <button
          v-for="t in TABS"
          :key="t.key"
          :data-testid="'policy-tab-' + t.key"
          :class="['tab-btn', { active: t.key === activeKey }]"
          @click="selectTab(t.key)"
        >{{ t.label }}</button>
      </div>
    </div>
    <component
      :is="activeComponent"
      v-bind="activeKey === 'audit' ? { eventSourceFactory: ESF } : {}"
    />
  </div>
</template>

<style scoped>
.policy-tabs {
  display: flex;
  flex-direction: column;
  flex: 1;
  min-height: 0;
}

/* The tab-strip lives inside a view-padded wrapper so its underline aligns with
   the tab content column; the wrapper supplies the horizontal inset, so the strip
   itself needs no padding. */
.tab-view {
  padding: 24px 2rem 0;
  flex-shrink: 0;
}

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
