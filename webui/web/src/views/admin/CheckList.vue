<script setup>
import { ref, computed } from "vue";
import { autoScrollbar } from "../../lib/autoScrollbar.js";
import ToggleSwitch from "../../components/ui/ToggleSwitch.vue";

const vAutoscroll = autoScrollbar;

const props = defineProps({
  items:        { type: Array,   required: true },   // [{ id, label }]
  selected:     { type: Array,   required: true },   // array ref of selected ids
  testidPrefix: { type: String,  default: "" },      // e.g. "skill-", "mcp-", "provider-allow-"
  searchable:   { type: Boolean, default: false },
  emptyText:    { type: String,  default: "No items." },
});

const emit = defineEmits(["toggle"]);

// Show search when searchable AND items exceed the threshold.
const SEARCH_THRESHOLD = 8;
const showSearch = computed(() => props.searchable && props.items.length > SEARCH_THRESHOLD);

const query = ref("");
const filtered = computed(() => {
  if (!query.value.trim()) return props.items;
  const q = query.value.toLowerCase();
  return props.items.filter(i => i.label.toLowerCase().includes(q));
});

const selectedCount = computed(() => props.selected.length);

function isChecked(id) {
  return props.selected.includes(id);
}

function selectAll() {
  for (const item of props.items) {
    if (!props.selected.includes(item.id)) emit("toggle", item.id);
  }
}

function selectNone() {
  for (const item of props.items) {
    if (props.selected.includes(item.id)) emit("toggle", item.id);
  }
}
</script>

<template>
  <div class="checklist">
    <div v-if="showSearch" class="checklist-toolbar">
      <input
        v-model="query"
        class="checklist-search"
        placeholder="Filter…"
        type="search"
      />
      <div class="checklist-actions">
        <button type="button" class="link-btn" @click="selectAll">All</button>
        <span class="sep">/</span>
        <button type="button" class="link-btn" @click="selectNone">None</button>
      </div>
    </div>

    <div class="checklist-count" v-if="selectedCount > 0">
      {{ selectedCount }} selected
    </div>

    <div v-if="items.length === 0" class="checklist-empty muted">{{ emptyText }}</div>
    <div v-else class="checklist-list scroll-min" v-autoscroll>
      <label
        v-for="item in filtered"
        :key="item.id"
        class="checkbox-item"
      >
        <ToggleSwitch
          :model-value="isChecked(item.id)"
          :data-testid="testidPrefix + item.id"
          :aria-label="item.label"
          @update:model-value="emit('toggle', item.id)"
        />
        {{ item.label }}
      </label>
      <span v-if="filtered.length === 0 && query" class="muted">No matches.</span>
    </div>
  </div>
</template>

<style scoped>
.checklist {
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
}

.checklist-toolbar {
  display: flex;
  align-items: center;
  gap: var(--space-2);
}

.checklist-search {
  flex: 1;
  background: var(--bg);
  color: var(--text);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  padding: 5px 9px;
  font-size: 12px;
}

.checklist-actions {
  display: flex;
  align-items: center;
  gap: 3px;
  font-size: 12px;
  color: var(--text-muted);
  flex-shrink: 0;
}

.link-btn {
  background: none;
  border: none;
  padding: 0;
  cursor: pointer;
  color: var(--accent);
  font-size: 12px;
}
.link-btn:hover { text-decoration: underline; }

.sep { color: var(--text-faint); }

.checklist-count {
  font-size: 11px;
  color: var(--text-muted);
}

.checklist-list {
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
  max-height: 180px;
  overflow-y: auto;
  padding-right: var(--space-1);
}

.checklist-empty {
  font-size: 13px;
}

.checkbox-item {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  font-size: 13px;
  color: var(--text);
  cursor: pointer;
  flex-shrink: 0;
}

.muted { color: var(--text-muted); font-size: 13px; }
</style>
