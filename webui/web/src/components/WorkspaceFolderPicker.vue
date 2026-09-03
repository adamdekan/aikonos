<script setup>
// Breadcrumb folder browser over the tenant's OneDrive drive root — lets the
// user pick a working folder. Mirrors
// RunWorkflowModal's modal pattern: props {visible}, emits close + select(path).
import { ref, computed, watch, onUnmounted } from "vue";
import Icon from "./Icon.vue";
import ErrorBanner from "./ui/ErrorBanner.vue";
import { listOneDriveFolders } from "../api/workspace.js";

const props = defineProps({
  visible: { type: Boolean, default: false },
});

const emit = defineEmits(["close", "select"]);

const currentDir = ref("");
const folders = ref([]);
const loading = ref(false);
const error = ref(null);

const breadcrumbs = computed(() => {
  const segs = currentDir.value ? currentDir.value.split("/") : [];
  const crumbs = [{ label: "OneDrive", path: "" }];
  let acc = "";
  for (const seg of segs) {
    acc = acc ? `${acc}/${seg}` : seg;
    crumbs.push({ label: seg, path: acc });
  }
  return crumbs;
});

async function load(dir) {
  loading.value = true;
  error.value = null;
  try {
    const resp = await listOneDriveFolders(dir);
    folders.value = resp.folders ?? [];
  } catch (err) {
    error.value = err?.message ?? "Failed to load folders";
  } finally {
    loading.value = false;
  }
}

// Reset to the drive root every time the picker opens — never resume the
// previously-browsed directory from a prior open.
watch(
  () => props.visible,
  (visible) => {
    if (!visible) return;
    currentDir.value = "";
    folders.value = [];
    error.value = null;
    load("");
  },
  { immediate: true },
);

function descend(folder) {
  currentDir.value = folder.path;
  load(folder.path);
}

function goToCrumb(path) {
  currentDir.value = path;
  load(path);
}

function retry() {
  load(currentDir.value);
}

function confirm() {
  emit("select", currentDir.value);
}

function close() {
  emit("close");
}

// Esc-close: a document-level listener, not a keydown on the backdrop div —
// the backdrop is never focused (nothing inside it holds focus on open), so a
// keydown handler bound there would never fire. Mirrors ui/Modal.vue.
function onKey(e) {
  if (e.key === "Escape") close();
}
function attachKey() { document.addEventListener("keydown", onKey); }
function detachKey() { document.removeEventListener("keydown", onKey); }

watch(
  () => props.visible,
  (visible) => {
    if (visible) attachKey();
    else detachKey();
  },
  { immediate: true },
);

onUnmounted(detachKey);
</script>

<template>
  <div v-if="visible" class="picker-backdrop">
    <div class="picker-modal" role="dialog" aria-modal="true" aria-labelledby="picker-title">

      <header class="picker-header">
        <Icon name="drive" :size="16" />
        <span id="picker-title" class="picker-name">Choose a OneDrive folder</span>
        <button class="picker-close" aria-label="Close" data-testid="picker-close" @click="close">
          <Icon name="close" :size="14" />
        </button>
      </header>

      <hr class="picker-divider" />

      <nav class="breadcrumbs" data-testid="picker-breadcrumbs">
        <template v-for="(crumb, i) in breadcrumbs" :key="crumb.path">
          <span v-if="i > 0" class="breadcrumb-sep">/</span>
          <button
            class="breadcrumb"
            :data-testid="`picker-breadcrumb-${i}`"
            @click="goToCrumb(crumb.path)"
          >{{ crumb.label }}</button>
        </template>
      </nav>

      <ErrorBanner v-if="error" :message="error">
        <template #action>
          <button class="btn-cancel" data-testid="picker-retry-btn" @click="retry">Retry</button>
        </template>
      </ErrorBanner>

      <div class="picker-list" data-testid="picker-list">
        <div v-if="loading" class="picker-empty" data-testid="picker-loading">Loading…</div>
        <div v-else-if="!error && folders.length === 0" class="picker-empty">No folders here.</div>
        <ul v-else-if="!loading" class="folder-list">
          <li
            v-for="folder in folders"
            :key="folder.path"
            class="folder-row"
            data-testid="picker-folder"
            @click="descend(folder)"
          >
            <Icon name="files" :size="15" />
            <span class="folder-name">{{ folder.name }}</span>
          </li>
        </ul>
      </div>

      <footer class="picker-footer">
        <button class="btn-cancel" data-testid="picker-cancel-btn" @click="close">Cancel</button>
        <button class="btn-run" data-testid="picker-confirm-btn" @click="confirm">
          Use this folder
        </button>
      </footer>
    </div>
  </div>
</template>

<style scoped>
.picker-backdrop {
  position: fixed;
  inset: 0;
  background: var(--overlay);
  display: flex;
  align-items: center;
  justify-content: center;
  z-index: 100;
}

.picker-modal {
  background: var(--bg-elevated);
  border: 1px solid var(--border);
  border-radius: var(--radius);
  padding: var(--space-4) var(--space-5);
  min-width: 360px;
  max-width: 480px;
  width: 100%;
  max-height: min(80vh, 560px);
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
  overflow: hidden;
}

.picker-header {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  font-weight: 500;
  color: var(--text);
  flex-shrink: 0;
}

.picker-name {
  flex: 1;
  font-size: 0.9375rem;
}

.picker-close {
  background: transparent;
  border: none;
  cursor: pointer;
  color: var(--text-muted);
  padding: var(--space-1);
  display: flex;
  align-items: center;
}

.picker-divider {
  border: none;
  border-top: 1px solid var(--border);
  margin: 0;
  flex-shrink: 0;
}

.breadcrumbs {
  display: flex;
  align-items: center;
  gap: 0.375rem;
  font-size: 0.8125rem;
  color: var(--text-muted);
  flex-shrink: 0;
}

.breadcrumb {
  background: transparent;
  border: none;
  padding: 0;
  color: var(--text-muted);
  cursor: pointer;
  font-family: var(--font-sans);
  font-size: 0.8125rem;
}

.breadcrumb:hover {
  color: var(--text);
  text-decoration: underline;
}

.breadcrumb-sep {
  color: var(--text-faint);
}

.picker-list {
  overflow-y: auto;
  flex: 1 1 auto;
  min-height: 0;
}

.picker-empty {
  color: var(--text-faint);
  font-size: 0.875rem;
}

.folder-list {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 0.25rem;
}

.folder-row {
  display: flex;
  align-items: center;
  gap: 0.5rem;
  padding: 0.5rem 0.625rem;
  border-radius: var(--radius-sm);
  cursor: pointer;
  color: var(--text);
}

.folder-row:hover {
  background: var(--bg-hover);
}

.folder-name {
  font-family: var(--font-mono);
  font-size: 0.8125rem;
}

.picker-footer {
  display: flex;
  gap: var(--space-2);
  justify-content: flex-end;
  padding-top: var(--space-3);
  border-top: 1px solid var(--border);
  flex-shrink: 0;
}

.btn-run {
  display: inline-flex;
  align-items: center;
  gap: var(--space-2);
  padding: var(--space-2) var(--space-4);
  border-radius: var(--radius-sm);
  font-family: var(--font-sans);
  font-size: 0.875rem;
  font-weight: 500;
  cursor: pointer;
  background: var(--accent);
  color: var(--text-on-accent);
  border: 1px solid transparent;
  transition: background 0.15s;
}

.btn-run:hover {
  filter: brightness(1.1);
}

.btn-cancel {
  display: inline-flex;
  align-items: center;
  padding: var(--space-2) var(--space-4);
  border-radius: var(--radius-sm);
  font-family: var(--font-sans);
  font-size: 0.875rem;
  font-weight: 500;
  cursor: pointer;
  background: transparent;
  color: var(--text);
  border: 1px solid var(--border);
  transition: background 0.15s;
}

.btn-cancel:hover {
  background: var(--bg-hover);
}
</style>
