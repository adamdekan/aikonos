<script setup>
// Personal skills — Skills view.
// My skills: owner-managed rows (Share/Delete/Import). Granted skills: the
// read-only admin-bundle half of GET /skills — no share/edit/delete, ever.
// A 403 (no skill:personal-skills grant) is not an error: the nav entry stays
// (per spec), and this view renders an informative no-access panel instead.
import { computed, onMounted, ref } from "vue";
import Icon from "../components/Icon.vue";
import ErrorBanner from "../components/ui/ErrorBanner.vue";
import EmptyState from "../components/ui/EmptyState.vue";
import ShareSkillModal from "../components/ShareSkillModal.vue";
import { useToast } from "../components/ui/useToast.js";
import { listSkills, importSkill, deleteSkill } from "../api/skills.js";

const { push: toast } = useToast();

const skills = ref([]);
const granted = ref([]);
const grantedUnavailable = ref(false);
const forbidden = ref(false);
const loading = ref(false);
const error = ref("");

async function load() {
  loading.value = true;
  error.value = "";
  try {
    const data = await listSkills();
    if (data.forbidden) {
      forbidden.value = true;
      skills.value = [];
      granted.value = [];
      return;
    }
    forbidden.value = false;
    skills.value = data.skills ?? [];
    granted.value = data.granted ?? [];
    grantedUnavailable.value = data.grantedUnavailable ?? false;
  } catch (e) {
    error.value = e.message || "Failed to load skills";
  } finally {
    loading.value = false;
  }
}

onMounted(load);

// ── name filter (Workflows.vue/Files.vue precedent: case-insensitive plain
// substring, name-only) — applies to both sections. ────────────────────────
const nameFilter = ref("");

function matchesFilter(name) {
  const q = nameFilter.value.trim().toLowerCase();
  return !q || (name ?? "").toLowerCase().includes(q);
}

const filteredSkills = computed(() => skills.value.filter((s) => matchesFilter(s.name)));
const filteredGranted = computed(() => granted.value.filter((g) => matchesFilter(g.name)));

function formatSize(bytes) {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}

// ── Import ─────────────────────────────────────────────────────────────────
const importing = ref(false);
const importError = ref("");
// Set only on a 409 name conflict — the gateway import route has no rename-
// on-import override, so there is nothing to "retry": this is informational,
// pointing the user at the free name the broker would install under instead.
const importConflict = ref(null);

const ZIP_MAGIC = [0x50, 0x4b, 0x03, 0x04];

function isZipBuffer(buf) {
  const sig = new Uint8Array(buf.slice(0, 4));
  return ZIP_MAGIC.every((b, i) => sig[i] === b);
}

// FileReader, not file.arrayBuffer() — mirrors SkillBundles.vue's onFileChange
// (jsdom's File/Blob has no .arrayBuffer(), only FileReader is polyfilled).
function readAsArrayBuffer(file) {
  return new Promise((resolve, reject) => {
    const fr = new FileReader();
    fr.onload = (e) => resolve(e.target.result);
    fr.onerror = reject;
    fr.readAsArrayBuffer(file);
  });
}

// Exposed for testing (Files.vue's handleFileUpload precedent) — accepts a
// File object directly, so tests never need to fake a DOM change event.
async function importFile(file) {
  importing.value = true;
  importError.value = "";
  importConflict.value = null;
  try {
    const buf = await readAsArrayBuffer(file);
    if (isZipBuffer(buf)) {
      await importSkill(buf, "application/zip");
    } else {
      await importSkill(new TextDecoder("utf-8").decode(buf), "text/markdown");
    }
    toast("ok", "Skill imported.");
    await load();
  } catch (e) {
    if (e.data?.suggested_name) {
      importConflict.value = { message: e.message, suggestedName: e.data.suggested_name };
    } else {
      importError.value = e.message || "Import failed";
    }
  } finally {
    importing.value = false;
  }
}

function onImportFileChange(e) {
  const file = e.target.files?.[0];
  e.target.value = "";
  if (file) importFile(file);
}

// ── Delete confirm ─────────────────────────────────────────────────────────
const deleteTarget = ref(null);
const deletePending = ref(false);

function openDeleteConfirm(skill) {
  deleteTarget.value = skill;
}

function closeDeleteConfirm() {
  if (deletePending.value) return;
  deleteTarget.value = null;
}

async function confirmDelete() {
  if (!deleteTarget.value) return;
  deletePending.value = true;
  try {
    await deleteSkill(deleteTarget.value.name);
    toast("ok", `Skill "${deleteTarget.value.name}" deleted.`);
    deleteTarget.value = null;
    await load();
  } catch (e) {
    toast("error", e.message || "Failed to delete skill");
  } finally {
    deletePending.value = false;
  }
}

// ── Share modal ────────────────────────────────────────────────────────────
const shareTarget = ref(null);
const shareVisible = ref(false);

function openShare(skill) {
  shareTarget.value = skill;
  shareVisible.value = true;
}

function closeShare() {
  shareVisible.value = false;
  shareTarget.value = null;
}

// ── Granted detail (body is already in the list payload — no extra fetch) ──
const expandedGrantedId = ref(null);

function toggleGrantedDetail(id) {
  expandedGrantedId.value = expandedGrantedId.value === id ? null : id;
}

defineExpose({ deleteTarget, importFile });
</script>

<template>
  <div class="view">
    <header class="view-header">
      <Icon name="book" :size="20" />
      <h2 class="view-title">Skills</h2>
    </header>

    <ErrorBanner v-if="error" :message="error" />

    <EmptyState
      v-if="forbidden"
      data-testid="forbidden"
      icon="admin"
      message="You do not have access to personal skills. Ask an admin to grant skill:personal-skills."
    />

    <template v-else>
      <input
        v-if="skills.length > 0 || granted.length > 0"
        v-model="nameFilter"
        type="text"
        class="skill-filter-input"
        data-testid="skill-filter-input"
        placeholder="Filter skills by name…"
        aria-label="Filter skills by name"
      />

      <div v-if="loading && skills.length === 0 && granted.length === 0" class="empty" data-testid="loading">
        Loading…
      </div>

      <!-- My skills -->
      <section class="section">
        <h3 class="section-title">My skills</h3>

        <div class="import-row">
          <label class="btn-sm import-btn">
            <Icon name="upload" :size="14" />
            {{ importing ? "Importing…" : "Import" }}
            <input
              type="file"
              accept=".zip,.skill,.md"
              class="import-input-hidden"
              data-testid="import-input"
              :disabled="importing"
              @change="onImportFileChange"
            />
          </label>
        </div>

        <p v-if="importError" class="skill-error" role="alert" data-testid="import-error">{{ importError }}</p>
        <p v-if="importConflict" class="skill-error" role="alert" data-testid="import-conflict">
          {{ importConflict.message }} Suggested name: "{{ importConflict.suggestedName }}" (rename inside the
          bundle and re-import to use it).
        </p>

        <ul v-if="filteredSkills.length > 0" class="skill-list">
          <li v-for="s in filteredSkills" :key="s.name" class="skill-card" data-testid="skill-row">
            <div class="card-body">
              <div class="card-name">{{ s.name }}</div>
              <div class="card-meta">
                <span class="badge" :class="s.valid ? 'badge-ok' : 'badge-danger'" data-testid="skill-status">
                  {{ s.valid ? "valid" : "invalid" }}
                </span>
                <span v-if="!s.valid && s.warning" class="missing-req" data-testid="skill-warning">
                  {{ s.warning }}
                </span>
                <span class="badge badge-muted">{{ formatSize(s.sizeBytes) }}</span>
              </div>
            </div>
            <div class="card-actions">
              <button class="btn-sm" data-testid="btn-share" @click="openShare(s)">Share</button>
              <button class="btn-sm btn-danger" data-testid="btn-delete" @click="openDeleteConfirm(s)">Delete</button>
            </div>
          </li>
        </ul>

        <div v-else-if="!loading" class="empty" data-testid="empty-my-skills">
          No skills yet. Author a SKILL.md under Skills/&lt;name&gt;/ or import a bundle above.
        </div>
      </section>

      <!-- Granted skills (read-only) -->
      <section v-if="granted.length > 0 || grantedUnavailable" class="section">
        <h3 class="section-title">Granted skills</h3>

        <ErrorBanner
          v-if="grantedUnavailable"
          message="Granted skills are temporarily unavailable."
          data-testid="granted-unavailable"
        />

        <ul v-if="filteredGranted.length > 0" class="skill-list">
          <li v-for="g in filteredGranted" :key="g.id" data-testid="granted-row">
            <div class="skill-card">
              <div class="card-body">
                <div class="card-name">{{ g.name }}</div>
                <div class="card-desc">{{ g.description }}</div>
              </div>
              <div class="card-actions">
                <button class="btn-sm" data-testid="btn-view-granted" @click="toggleGrantedDetail(g.id)">
                  {{ expandedGrantedId === g.id ? "Hide" : "View" }}
                </button>
              </div>
            </div>
            <pre v-if="expandedGrantedId === g.id" class="granted-body" data-testid="granted-detail">{{ g.body }}</pre>
          </li>
        </ul>
      </section>

      <div
        v-if="
          !loading &&
          (skills.length > 0 || granted.length > 0) &&
          filteredSkills.length === 0 &&
          filteredGranted.length === 0
        "
        class="empty"
        data-testid="no-matches"
      >
        No skills match "{{ nameFilter }}".
      </div>
    </template>

    <ShareSkillModal
      :skill-name="shareTarget?.name ?? ''"
      :visible="shareVisible"
      @close="closeShare"
    />

    <!-- Delete confirmation -->
    <div v-if="deleteTarget" class="del-backdrop" data-testid="delete-confirm">
      <div class="del-modal" role="dialog" aria-modal="true" aria-labelledby="del-title">
        <header class="del-header">
          <Icon name="trash" :size="16" />
          <span id="del-title" class="del-title">Delete skill</span>
        </header>
        <p class="del-body">
          Delete <strong>{{ deleteTarget.name }}</strong>? This removes the whole Skills/{{ deleteTarget.name }}/
          folder and cannot be undone.
        </p>
        <footer class="del-footer">
          <button class="btn-cancel" :disabled="deletePending" @click="closeDeleteConfirm">Cancel</button>
          <button
            class="btn-danger-solid"
            :disabled="deletePending"
            data-testid="btn-delete-confirm"
            @click="confirmDelete"
          >{{ deletePending ? "Deleting…" : "Delete" }}</button>
        </footer>
      </div>
    </div>
  </div>
</template>

<style scoped>
.view {
  padding: 2rem;
  max-width: 800px;
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
  font-family: var(--font-sans);
  font-size: 1.25rem;
  font-weight: 500;
  margin: 0;
}

.skill-filter-input {
  padding: 0.4375rem 0.75rem;
  background: var(--bg-elevated);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  color: var(--text);
  font-family: var(--font-sans);
  font-size: 0.875rem;
}

.section {
  display: flex;
  flex-direction: column;
  gap: 0.75rem;
}

.section-title {
  font-size: 0.8125rem;
  font-weight: 500;
  color: var(--text-muted);
  text-transform: uppercase;
  letter-spacing: 0.05em;
  margin: 0;
}

.import-row {
  display: flex;
}

.import-btn {
  display: inline-flex;
  align-items: center;
  gap: 0.375rem;
  cursor: pointer;
  width: fit-content;
}

.import-input-hidden {
  position: absolute;
  width: 1px;
  height: 1px;
  opacity: 0;
  overflow: hidden;
}

.skill-error {
  margin: 0;
  font-size: 0.8125rem;
  color: var(--danger);
}

.skill-list {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 0.5rem;
}

.skill-card {
  display: flex;
  align-items: flex-start;
  gap: 1rem;
  padding: 0.875rem 1rem;
  background: var(--bg-elevated);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
}

.card-body {
  flex: 1;
  display: flex;
  flex-direction: column;
  gap: 0.25rem;
  min-width: 0;
}

.card-name {
  font-size: 0.9375rem;
  color: var(--text);
}

.card-desc {
  font-size: 0.8125rem;
  color: var(--text-muted);
}

.card-meta {
  display: flex;
  align-items: center;
  gap: 0.5rem;
  flex-wrap: wrap;
}

.missing-req {
  font-size: 0.75rem;
  color: var(--text-muted);
  font-style: italic;
}

.badge {
  font-size: 0.6875rem;
  padding: 0.125rem 0.5rem;
  border-radius: 999px;
  font-weight: 500;
}

.badge-ok {
  background: var(--fill-accent);
  color: var(--ok);
}

.badge-danger {
  background: var(--fill-danger);
  color: var(--danger);
}

.badge-muted {
  background: var(--fill-muted);
  color: var(--text-muted);
}

.card-actions {
  display: flex;
  align-items: center;
  gap: 0.375rem;
  flex-shrink: 0;
  flex-wrap: wrap;
}

.granted-body {
  margin: 0.5rem 0 0;
  padding: 0.75rem;
  background: var(--fill-muted);
  border-radius: var(--radius-sm);
  font-family: var(--font-mono);
  font-size: 0.8125rem;
  white-space: pre-wrap;
  word-break: break-word;
  max-height: 240px;
  overflow-y: auto;
}

.btn-sm {
  padding: 0.25rem 0.625rem;
  font-size: 0.8125rem;
  font-family: var(--font-sans);
  background: var(--bg);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  color: var(--text-muted);
  cursor: pointer;
  transition: background 0.15s, color 0.15s;
}

.btn-sm:not(:disabled):hover {
  background: var(--fill-muted);
  color: var(--text);
}

.btn-sm:disabled {
  cursor: not-allowed;
  opacity: 0.5;
}

.btn-sm.btn-danger {
  color: var(--danger);
}

.btn-sm.btn-danger:not(:disabled):hover {
  background: var(--fill-danger);
  color: var(--danger);
  border-color: var(--danger);
}

.empty {
  color: var(--text-faint);
  font-size: 0.875rem;
}

/* ── Delete confirmation modal (Workflows.vue precedent) ─────────────────── */
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

.btn-cancel:not(:disabled):hover {
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

.btn-danger-solid:not(:disabled):hover {
  filter: brightness(1.1);
}

.btn-danger-solid:disabled,
.btn-cancel:disabled {
  cursor: not-allowed;
  opacity: 0.5;
}
</style>
