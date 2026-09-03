<script setup>
import { computed, ref } from "vue";
import Spinner from "./Spinner.vue";
import EmptyState from "./EmptyState.vue";

const props = defineProps({
  columns:   { type: Array,  required: true },
  rows:      { type: Array,  default: () => [] },
  loading:   { type: Boolean, default: false },
  emptyText: { type: String, default: "No data." },
  pageSize:  { type: Number, default: 0 },
  rowAttrs:  { type: Object, default: () => ({}) },
});

const page = ref(1);

const paginated = computed(() => {
  if (!props.pageSize || props.pageSize <= 0) return props.rows;
  const start = (page.value - 1) * props.pageSize;
  return props.rows.slice(start, start + props.pageSize);
});

const totalPages = computed(() => {
  if (!props.pageSize || props.pageSize <= 0) return 1;
  return Math.max(1, Math.ceil(props.rows.length / props.pageSize));
});

function prev() { if (page.value > 1) page.value--; }
function next() { if (page.value < totalPages.value) page.value++; }
</script>

<template>
  <div class="dt-wrap">
    <div v-if="loading" class="dt-loading">
      <Spinner size="md" />
    </div>

    <template v-else-if="rows.length === 0">
      <EmptyState :message="emptyText" />
    </template>

    <template v-else>
      <table class="dt-table">
        <thead>
          <tr>
            <th
              v-for="col in columns"
              :key="col.key"
              :style="col.width ? { width: col.width } : {}"
            >{{ col.label }}</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="(row, index) in paginated" :key="index" v-bind="rowAttrs">
            <slot name="row" :row="row" :index="index" />
          </tr>
        </tbody>
      </table>

      <div v-if="pageSize > 0 && totalPages > 1" class="dt-pager">
        <button :disabled="page <= 1" @click="prev">Prev</button>
        <span>page {{ page }} of {{ totalPages }}</span>
        <button :disabled="page >= totalPages" @click="next">Next</button>
      </div>
    </template>
  </div>
</template>

<style scoped>
.dt-wrap {
  width: 100%;
}

.dt-loading {
  display: flex;
  justify-content: center;
  padding: 32px 0;
}

.dt-table {
  width: 100%;
  border-collapse: collapse;
  font-size: 0.875rem;
}

.dt-table th,
.dt-table td {
  padding: 8px 12px;
  text-align: left;
  border-bottom: 1px solid var(--border);
}

.dt-table th {
  color: var(--text-muted);
  font-weight: 500;
}

.dt-table tbody tr:hover {
  background: var(--bg-hover);
}

.dt-table tbody tr:last-child td {
  border-bottom: none;
}

.dt-pager {
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 8px 12px;
  font-size: 0.8rem;
  color: var(--text-muted);
  border-top: 1px solid var(--border);
  border-radius: 0 0 var(--radius-sm) var(--radius-sm);
}

.dt-pager button {
  background: none;
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  color: var(--text);
  padding: 2px 10px;
  cursor: pointer;
  font-size: 0.8rem;
}

.dt-pager button:disabled {
  opacity: 0.35;
  cursor: default;
}

.dt-pager button:not(:disabled):hover {
  background: var(--bg-hover);
}
</style>
