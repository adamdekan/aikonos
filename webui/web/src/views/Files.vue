<script setup>
import { ref, computed, onMounted } from "vue";
import { storeToRefs } from "pinia";
import Icon from "../components/Icon.vue";
import ErrorBanner from "../components/ui/ErrorBanner.vue";
import { listFiles, uploadFile, downloadFile, deleteFile, moveFile, createDir } from "../api/files.js";
import { fileToBase64 } from "../lib/base64.js";
import { useWorkspaceStore } from "../store/workspace.js";

// Active-backend indicator + reconnect banner — read-only display over the workspace store; the broker routes File
// explorer/upload/mutation calls transparently by the caller's pref, so no
// listing/mutation logic below needs to change.
const workspaceStore = useWorkspaceStore();
const {
  backend: wsBackend,
  onedriveFolderPath: wsFolderPath,
  onedriveStatus: wsStatus,
  loaded: wsLoaded,
} = storeToRefs(workspaceStore);

const backendLabel = computed(() =>
  wsBackend.value === "onedrive" ? `OneDrive · /${wsFolderPath.value}` : "Local workspace",
);

const showReconnectBanner = computed(
  () => wsBackend.value === "onedrive" && wsStatus.value === "reconnect_needed",
);

const entries = ref([]);
const cwd = ref("");
const error = ref("");
const dragActive = ref(false);
const loading = ref(true);
const pendingDelete = ref(null);

// Mirrors the broker's workspacefs cap (broker/internal/workspacefs/store.go MaxFileBytes).
const MAX_UPLOAD_BYTES = 10 * 1024 * 1024;

const newFolderOpen = ref(false);
const newFolderName = ref("");
const renamingPath = ref(null);
const renameName = ref("");

// Belt-and-suspenders: hide any path whose segment starts with "." even if the gateway
// ever returns one (gateway already excludes dot-prefixed paths by default).
function isHidden(path) {
  return path.split("/").some(seg => seg.startsWith("."));
}

function basename(path) {
  return path.split("/").pop();
}

function cwdJoin(name) {
  return cwd.value ? `${cwd.value}/${name}` : name;
}

// entries already holds only cwd's immediate children (fetched per-directory —
// see load()), so no path-prefix filtering step is needed anymore.
const visibleEntries = computed(() => entries.value.filter(f => !isHidden(f.path)));

// F63: case-insensitive plain substring over this directory's entries only
// (no include/-exclude grammar — recorded decision). Cleared on navigation —
// see openFolder/goToCrumb.
const folderFilter = ref("");

const currentEntries = computed(() => {
  const q = folderFilter.value.trim().toLowerCase();
  const matched = q
    ? visibleEntries.value.filter(f => basename(f.path).toLowerCase().includes(q))
    : visibleEntries.value;
  return [...matched].sort((a, b) => {
    if (a.isDir !== b.isDir) return a.isDir ? -1 : 1;
    return a.path.localeCompare(b.path);
  });
});

const breadcrumbs = computed(() => {
  const segs = cwd.value ? cwd.value.split("/") : [];
  const crumbs = [{ label: "Files", path: "" }];
  let acc = "";
  for (const seg of segs) {
    acc = acc ? `${acc}/${seg}` : seg;
    crumbs.push({ label: seg, path: acc });
  }
  return crumbs;
});

function formatSize(bytes) {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}

// Fetches dir's immediate children only (shallow, non-recursive) and replaces
// entries wholesale — entries always holds exactly the currently-viewed dir's
// children, never the whole tree. "" (workspace root) is sent as "." because an
// empty path means "full legacy recursive listing" server-side (wire-compat
// invariant), not "shallow root".
async function load(dir = cwd.value) {
  error.value = "";
  try {
    const data = await listFiles({ dir: dir || ".", recursive: false });
    entries.value = data.files ?? [];
  } catch (e) {
    error.value = e.message || "Failed to load files";
  } finally {
    loading.value = false;
  }
}

// Best-effort refetch of the current dir used to recover from a failed mutation,
// without touching `error` (the caller has already set the user-facing message).
async function refetchCwd() {
  try {
    const data = await listFiles({ dir: cwd.value || ".", recursive: false });
    entries.value = data.files ?? [];
  } catch {
    // Recovery is best-effort; the mutation's own error stays on screen.
  }
}

// Replaces the entry at `path` with `patch` (or appends it if new). entries is
// scoped to cwd's children, so this only ever matches/appends within cwd.
function upsertEntry(path, patch) {
  const idx = entries.value.findIndex(f => f.path === path);
  if (idx === -1) {
    entries.value = [...entries.value, patch];
  } else {
    entries.value = entries.value.map((f, i) => (i === idx ? patch : f));
  }
}

// Rewrites `path` to `newPath` in place. Renaming only ever targets a same-dir
// name (see confirmRename's cwdJoin), and entries now holds only cwd's immediate
// children — a renamed folder's descendants live in a directory that isn't
// currently fetched, so there's nothing left here to reparent.
function reparentEntries(path, newPath, patch) {
  entries.value = entries.value.map(f => (f.path === path ? { ...f, ...patch, path: newPath } : f));
}

// Deletion is restricted to files and empty dirs (broker workspacefs), so `path`
// never has descendants among cwd's currently-loaded children either.
function removeEntry(path) {
  entries.value = entries.value.filter(f => f.path !== path);
}

async function openFolder(path) {
  // A folder filter rarely means the next folder too — clear it on navigation.
  folderFilter.value = "";
  cwd.value = path;
  await load(path);
}

async function goToCrumb(path) {
  folderFilter.value = "";
  cwd.value = path;
  await load(path);
}

// Exposed for testing — accepts a File object directly, uploads into the current dir.
async function handleFileUpload(file, targetDir = cwd.value) {
  error.value = "";
  if (file.size > MAX_UPLOAD_BYTES) {
    error.value = `${file.name} exceeds the 10 MiB upload limit`;
    return;
  }
  try {
    const base64 = await fileToBase64(file);
    const path = targetDir ? `${targetDir}/${file.name}` : file.name;
    const resp = await uploadFile(path, base64);
    upsertEntry(path, resp.file ?? { path, size: file.size, modified: null, isDir: false });
  } catch (e) {
    error.value = e.message || "Upload failed";
    await refetchCwd();
  }
}

function onFileInputChange(e) {
  const file = e.target.files?.[0];
  if (file) handleFileUpload(file);
  // Reset input so re-selecting the same file triggers change again.
  // Deliberately not awaiting handleFileUpload above: the reset must run
  // synchronously and not block on the network upload.
  e.target.value = "";
}

// Drag-drop batch upload: uploads each valid file directly (bypassing
// handleFileUpload's per-file local patch) and does ONE refetch of the target
// dir at the end, rather than N in-memory patches — a directory-scoped listing
// makes "just refetch the dir we changed" both cheap and simplest.
async function uploadFiles(fileList, targetDir) {
  const files = Array.from(fileList ?? []);
  const oversized = files.filter(f => f.size > MAX_UPLOAD_BYTES);
  const valid = files.filter(f => f.size <= MAX_UPLOAD_BYTES);
  for (const file of valid) {
    try {
      const base64 = await fileToBase64(file);
      const path = targetDir ? `${targetDir}/${file.name}` : file.name;
      await uploadFile(path, base64);
    } catch (e) {
      error.value = e.message || "Upload failed";
    }
  }
  if (valid.length > 0 && targetDir === cwd.value) {
    await refetchCwd();
  }
  // Set after all uploads finish so a later success in the same batch can't clear it.
  if (oversized.length > 0) {
    error.value = `${oversized.map(f => f.name).join(", ")} exceeds the 10 MiB upload limit`;
  }
}

function onDragOver(e) {
  e.preventDefault();
  dragActive.value = true;
}

function onDragLeave() {
  dragActive.value = false;
}

async function onDrop(e, targetDir = cwd.value) {
  e.preventDefault();
  dragActive.value = false;
  await uploadFiles(e.dataTransfer?.files, targetDir);
}

function openNewFolder() {
  newFolderOpen.value = true;
  newFolderName.value = "";
}

async function confirmNewFolder() {
  const name = newFolderName.value.trim();
  newFolderOpen.value = false;
  if (!name) return;
  error.value = "";
  const path = cwdJoin(name);
  try {
    await createDir(path);
    upsertEntry(path, { path, size: 0, modified: null, isDir: true });
  } catch (e) {
    error.value = e.message || "Create folder failed";
    await refetchCwd();
  }
}

function startRename(path) {
  renamingPath.value = path;
  renameName.value = basename(path);
}

async function confirmRename(path) {
  // Guards double-fire: blur fires after Enter (input unmounts) and after Esc
  // (which nulls renamingPath to cancel). Only the first call for a given path
  // proceeds; the trailing blur is a no-op — so Esc cancels cleanly and Enter
  // never renames twice.
  if (renamingPath.value !== path) return;
  const name = renameName.value.trim();
  renamingPath.value = null;
  if (!name || name === basename(path)) return;
  error.value = "";
  const newPath = cwdJoin(name);
  try {
    const resp = await moveFile(path, newPath);
    reparentEntries(path, newPath, resp.file ?? {});
  } catch (e) {
    error.value = e.message || "Rename failed";
    await refetchCwd();
  }
}

async function triggerDownload(path) {
  error.value = "";
  try {
    const data = await downloadFile(path);
    const bytes = atob(data.contentBase64 ?? "");
    const byteArr = new Uint8Array(bytes.length);
    for (let i = 0; i < bytes.length; i++) byteArr[i] = bytes.charCodeAt(i);
    const blob = new Blob([byteArr], { type: data.mime || "application/octet-stream" });
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = path.split("/").pop();
    document.body.appendChild(a);
    a.click();
    document.body.removeChild(a);
    URL.revokeObjectURL(url);
  } catch (e) {
    error.value = e.message || "Download failed";
  }
}

function requestDelete(path) {
  pendingDelete.value = path;
}

function cancelDelete() {
  pendingDelete.value = null;
}

async function confirmDelete() {
  const path = pendingDelete.value;
  if (!path) return;
  error.value = "";
  try {
    await deleteFile(path);
    removeEntry(path);
    pendingDelete.value = null;
  } catch (e) {
    error.value = e.message || "Delete failed";
    await refetchCwd();
  }
}

onMounted(() => {
  load();
  workspaceStore.load();
});

// Expose for test harness (handleFileUpload is called directly in tests).
defineExpose({ handleFileUpload });
</script>

<template>
  <div class="view">
    <header class="view-header">
      <Icon name="files" :size="20" />
      <h2 class="view-title">Files</h2>
      <span v-if="wsLoaded" class="backend-chip" data-testid="backend-indicator">{{ backendLabel }}</span>
      <button class="btn-ghost" data-testid="new-folder-btn" @click="openNewFolder">
        <Icon name="plus" :size="15" />
        New folder
      </button>
      <label class="btn-accent upload-label">
        <Icon name="upload" :size="15" />
        Upload
        <input type="file" class="file-input-hidden" @change="onFileInputChange" />
      </label>
    </header>

    <nav class="breadcrumbs">
      <template v-for="(crumb, i) in breadcrumbs" :key="crumb.path">
        <span v-if="i > 0" class="breadcrumb-sep">/</span>
        <button
          class="breadcrumb"
          :data-testid="`breadcrumb-${i}`"
          @click="goToCrumb(crumb.path)"
        >{{ crumb.label }}</button>
      </template>
    </nav>

    <input
      v-model="folderFilter"
      type="text"
      class="folder-filter-input"
      data-testid="folder-filter-input"
      placeholder="filter this folder…"
      aria-label="Filter files in this folder"
    />

    <div v-if="newFolderOpen" class="inline-form">
      <input
        class="inline-input"
        data-testid="new-folder-input"
        v-model="newFolderName"
        placeholder="Folder name"
        autofocus
        @keyup.enter="confirmNewFolder"
        @keyup.esc="newFolderOpen = false"
      />
    </div>

    <div v-if="showReconnectBanner" data-testid="reconnect-banner">
      <ErrorBanner message="OneDrive connection needs to be refreshed — it reconnects automatically on your next sign-in." />
    </div>

    <ErrorBanner v-if="error" :message="error" />

    <div
      class="dropzone"
      data-testid="dropzone"
      :class="{ 'drag-active': dragActive }"
      @dragover="onDragOver"
      @dragleave="onDragLeave"
      @drop="onDrop($event)"
    >
      <div v-if="loading" class="empty" data-testid="loading">Loading…</div>
      <div v-else-if="currentEntries.length === 0 && !error" class="empty">This folder is empty.</div>

      <ul v-if="!loading && currentEntries.length > 0" class="file-list">
        <li
          v-for="f in currentEntries"
          :key="f.path"
          :class="f.isDir ? 'file-row folder-row' : 'file-row'"
          :data-testid="f.isDir ? 'folder-row' : 'file-row'"
          @click="f.isDir && renamingPath !== f.path ? openFolder(f.path) : null"
          @dragover.stop="f.isDir ? onDragOver($event) : null"
          @dragleave.stop="f.isDir ? onDragLeave() : null"
          @drop.stop="f.isDir ? onDrop($event, f.path) : null"
        >
          <template v-if="renamingPath === f.path">
            <Icon :name="f.isDir ? 'files' : 'file'" :size="15" />
            <input
              class="inline-input rename-inline"
              data-testid="rename-input"
              v-model="renameName"
              autofocus
              @keyup.enter="confirmRename(f.path)"
              @keyup.esc="renamingPath = null"
              @blur="confirmRename(f.path)"
              @click.stop
            />
          </template>
          <template v-else>
            <component
              :is="f.isDir ? 'button' : 'span'"
              :type="f.isDir ? 'button' : undefined"
              class="file-path-btn"
              :class="{ 'is-dir': f.isDir }"
            >
              <Icon :name="f.isDir ? 'files' : 'file'" :size="15" />
              <span class="file-path">{{ basename(f.path) }}</span>
            </component>
            <span v-if="!f.isDir" class="file-size">{{ formatSize(f.size) }}</span>
            <button
              class="btn-icon"
              :data-testid="`rename-btn-${f.path}`"
              :aria-label="`Rename ${f.path}`"
              @click.stop="startRename(f.path)"
            >
              <Icon name="customize" :size="15" />
            </button>
            <button
              v-if="!f.isDir"
              class="btn-icon"
              :data-testid="`download-${f.path}`"
              :aria-label="`Download ${f.path}`"
              @click.stop="triggerDownload(f.path)"
            >
              <Icon name="download" :size="15" />
            </button>
            <button
              class="btn-icon btn-icon-danger"
              :data-testid="`delete-${f.path}`"
              :aria-label="`Delete ${f.path}`"
              @click.stop="requestDelete(f.path)"
            >
              <Icon name="trash" :size="15" />
            </button>
          </template>
        </li>
      </ul>
    </div>

    <!-- Delete confirmation — mirrors Workflows.vue's del-backdrop idiom. -->
    <div v-if="pendingDelete" class="del-backdrop" data-testid="delete-confirm">
      <div class="del-modal" role="dialog" aria-modal="true" aria-labelledby="del-title">
        <header class="del-header">
          <Icon name="trash" :size="16" />
          <span id="del-title" class="del-title">Delete</span>
        </header>
        <p class="del-body">
          Delete <strong>{{ basename(pendingDelete) }}</strong>? This cannot be undone.
        </p>
        <footer class="del-footer">
          <button class="btn-cancel" data-testid="cancel-delete" @click="cancelDelete">Cancel</button>
          <button class="btn-danger-solid" data-testid="confirm-delete" @click="confirmDelete">Delete</button>
        </footer>
      </div>
    </div>
  </div>
</template>

<style scoped>
.view {
  padding: 2rem;
  max-width: 720px;
  margin: 0 auto;
  display: flex;
  flex-direction: column;
  gap: 1.5rem;
}

.view-header {
  display: flex;
  align-items: center;
  gap: 0.5rem;
  color: var(--text);
}

.view-title {
  flex: 1;
  font-family: var(--font-sans);
  font-size: 1.25rem;
  font-weight: 500;
  margin: 0;
}

.backend-chip {
  font-size: 0.75rem;
  color: var(--text-muted);
  font-family: var(--font-mono);
  padding: 0.1875rem 0.5rem;
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
}

.breadcrumbs {
  display: flex;
  align-items: center;
  gap: 0.375rem;
  font-size: 0.8125rem;
  color: var(--text-muted);
  margin-top: -0.75rem;
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

.inline-form {
  display: flex;
}

.inline-input {
  flex: 1;
  padding: 0.4375rem 0.75rem;
  background: var(--bg-elevated);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  color: var(--text);
  font-family: var(--font-mono);
  font-size: 0.8125rem;
}

.rename-inline {
  flex: 1;
}

.folder-filter-input {
  padding: 0.4375rem 0.75rem;
  background: var(--bg-elevated);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  color: var(--text);
  font-family: var(--font-sans);
  font-size: 0.8125rem;
}

.dropzone {
  border-radius: var(--radius-sm);
  transition: background 0.15s, outline 0.15s;
  outline: 2px dashed transparent;
  outline-offset: 4px;
}

.dropzone.drag-active {
  outline-color: var(--accent);
  background: var(--fill-muted);
}

.empty {
  color: var(--text-faint);
  font-size: 0.875rem;
}

.file-list {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 0.375rem;
}

.file-row {
  display: flex;
  align-items: center;
  gap: 0.625rem;
  padding: 0.625rem 1rem;
  background: var(--bg-elevated);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
}

.file-path-btn {
  flex: 1;
  display: flex;
  align-items: center;
  gap: 0.625rem;
  min-width: 0;
  background: transparent;
  border: none;
  padding: 0;
  cursor: default;
  color: var(--text);
  text-align: left;
  font: inherit;
}

.file-path-btn.is-dir {
  cursor: pointer;
}

.file-path-btn.is-dir:hover .file-path {
  text-decoration: underline;
}

.file-path {
  flex: 1;
  color: var(--text);
  font-family: var(--font-mono);
  font-size: 0.8125rem;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.file-size {
  font-size: 0.75rem;
  color: var(--text-muted);
  flex-shrink: 0;
}

.btn-icon {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 1.75rem;
  height: 1.75rem;
  background: transparent;
  border: none;
  border-radius: var(--radius-sm);
  color: var(--text-muted);
  cursor: pointer;
  transition: background 0.15s, color 0.15s;
  flex-shrink: 0;
}

.btn-icon:hover {
  background: var(--fill-muted);
  color: var(--text);
}

.btn-icon-danger:hover {
  background: var(--fill-danger);
  color: var(--danger);
}

.btn-ghost {
  display: inline-flex;
  align-items: center;
  gap: 0.375rem;
  padding: 0.4375rem 0.875rem;
  background: transparent;
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  color: var(--text);
  font-family: var(--font-sans);
  font-size: 0.875rem;
  cursor: pointer;
  transition: background 0.15s;
}

.btn-ghost:hover {
  background: var(--fill-muted);
}

.btn-accent {
  display: inline-flex;
  align-items: center;
  gap: 0.375rem;
  padding: 0.4375rem 0.875rem;
  background: var(--accent);
  border: none;
  border-radius: var(--radius-sm);
  color: var(--text-on-accent);
  font-family: var(--font-sans);
  font-size: 0.875rem;
  cursor: pointer;
  transition: background 0.15s;
}

.btn-accent:hover {
  background: var(--accent-hover);
}

.upload-label {
  margin-left: 0;
}

.file-input-hidden {
  position: absolute;
  width: 0;
  height: 0;
  opacity: 0;
  overflow: hidden;
}

/* ── Delete confirmation modal — mirrors Workflows.vue's del-backdrop idiom ── */
.del-backdrop {
  position: fixed;
  inset: 0;
  background: var(--overlay);
  display: flex;
  align-items: center;
  justify-content: center;
  z-index: 100;
}

.del-modal {
  background: var(--bg-elevated);
  border: 1px solid var(--border);
  border-radius: var(--radius);
  padding: var(--space-4) var(--space-5);
  min-width: 320px;
  max-width: 420px;
  width: 100%;
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
}

.del-header {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  font-weight: 500;
  color: var(--text);
}

.del-title {
  font-size: 0.9375rem;
}

.del-body {
  margin: 0;
  font-size: 0.875rem;
  color: var(--text);
}

.del-footer {
  display: flex;
  gap: var(--space-2);
  justify-content: flex-end;
  margin-top: var(--space-2);
}

.btn-cancel {
  padding: var(--space-2) var(--space-4);
  border-radius: var(--radius-sm);
  font-family: var(--font-sans);
  font-size: 0.875rem;
  font-weight: 500;
  cursor: pointer;
  background: transparent;
  color: var(--text);
  border: 1px solid var(--border);
}

.btn-cancel:hover {
  background: var(--fill-muted);
}

.btn-danger-solid {
  padding: var(--space-2) var(--space-4);
  border-radius: var(--radius-sm);
  font-family: var(--font-sans);
  font-size: 0.875rem;
  font-weight: 500;
  cursor: pointer;
  background: var(--danger);
  color: var(--text-on-accent);
  border: 1px solid transparent;
}

.btn-danger-solid:hover {
  filter: brightness(1.1);
}

</style>
