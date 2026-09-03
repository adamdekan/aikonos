<script setup>
import { ref, onMounted } from "vue";
import {
  listSkillBundles,
  uploadSkillBundle,
  updateSkillBundle,
  deleteSkillBundle,
  grantSkillBundle,
} from "../../api/admin.js";
import Icon from "../../components/Icon.vue";
import EmptyState from "../../components/ui/EmptyState.vue";
import { useToast } from "../../components/ui/useToast.js";

const { push: toast } = useToast();

// ── data ──────────────────────────────────────────────────────────────────────

const bundles    = ref([]);
const loading    = ref(false);
const error      = ref("");
const forbidden  = ref(false);

// ── upload / edit ─────────────────────────────────────────────────────────────

const uploadContent  = ref("");
const uploading      = ref(false);

// edit mode — null means create mode; set to the bundle id being edited
const editingId      = ref(null);

// comma-separated keywords text for the bundle being edited; empty = clears keywords
const editKeywords   = ref("");

function parseKeywords(text) {
  return text.split(",").map((k) => k.trim()).filter(Boolean);
}

function buildSkillMd(bundle) {
  const tools = (bundle.allowedTools ?? []).map((t) => `  - ${t}`).join("\n");
  const toolsSection = tools ? `\nallowed-tools:\n${tools}` : "";
  const disableFlag = bundle.disableModelInvocation
    ? "\ndisable-model-invocation: true"
    : "";
  const forkFlag = bundle.contextFork ? "\ncontext: fork" : "";
  // JSON.stringify produces a valid double-quoted YAML scalar — safe for values
  // containing colons, leading hashes, or embedded quotes.
  const name = JSON.stringify(bundle.name ?? "");
  const desc = JSON.stringify(bundle.description ?? "");
  return `---\nname: ${name}\ndescription: ${desc}${toolsSection}${disableFlag}${forkFlag}\n---\n${bundle.body ?? ""}`;
}

function doEdit(bundle) {
  editingId.value    = bundle.id;
  uploadContent.value = buildSkillMd(bundle);
  editKeywords.value  = (bundle.keywords ?? []).join(", ");
}

function cancelEdit() {
  editingId.value    = null;
  uploadContent.value = "";
  editKeywords.value  = "";
}

// ── grant per-bundle ──────────────────────────────────────────────────────────

// Map of bundleId → group name string (the input value)
const grantGroup = ref({});
const granting   = ref({});

// ── load ──────────────────────────────────────────────────────────────────────

async function load() {
  loading.value  = true;
  error.value    = "";
  forbidden.value = false;
  try {
    const resp = await listSkillBundles();
    if (resp.forbidden) { forbidden.value = true; return; }
    bundles.value = resp.bundles ?? [];
  } catch (e) {
    error.value = e.message;
  } finally {
    loading.value = false;
  }
}

onMounted(load);

// ── upload / update handler ───────────────────────────────────────────────────

async function doUpload() {
  const content = uploadContent.value.trim();
  if (!content) return;
  uploading.value = true;
  error.value = "";
  try {
    if (editingId.value) {
      await updateSkillBundle(editingId.value, content, "text/markdown", parseKeywords(editKeywords.value));
      editingId.value = null;
      toast("ok", "Skill bundle updated.");
    } else {
      await uploadSkillBundle(content, "text/markdown");
      toast("ok", "Skill bundle uploaded.");
    }
    uploadContent.value = "";
    editKeywords.value = "";
    await load();
  } catch (e) {
    error.value = e.message;
  } finally {
    uploading.value = false;
  }
}

// ── file picker handler ───────────────────────────────────────────────────────

async function onFileChange(event) {
  const file = event.target.files?.[0];
  if (!file) return;

  uploading.value = true;
  error.value = "";

  try {
    const buf = await new Promise((resolve, reject) => {
      const fr = new FileReader();
      fr.onload = (e) => resolve(e.target.result);
      fr.onerror = reject;
      fr.readAsArrayBuffer(file);
    });

    // File upload is the bundle path: a .skill/.zip is a binary archive sent as
    // application/zip (the gateway routes that Content-Type to the zip parser).
    // It is intentionally separate from the textarea, which is the copy-paste
    // path for bare SKILL.md text (text/markdown). The file's bytes never enter
    // the textarea — that is what produced the garbled binary previously.
    //
    // Guard with the zip magic (PK\x03\x04): a non-zip file picked here is a
    // mistake (likely a bare SKILL.md), so point the user at the paste box
    // rather than uploading bytes the zip parser will reject.
    const sig = new Uint8Array(buf.slice(0, 4));
    const isZip =
      sig[0] === 0x50 && sig[1] === 0x4b && sig[2] === 0x03 && sig[3] === 0x04;
    if (!isZip) {
      event.target.value = "";
      throw new Error(
        "Not a .skill/.zip bundle. To upload a plain SKILL.md, paste its text into the box above instead.",
      );
    }

    if (editingId.value) {
      await updateSkillBundle(editingId.value, buf, "application/zip", parseKeywords(editKeywords.value));
      editingId.value = null;
      toast("ok", "Skill bundle updated.");
    } else {
      await uploadSkillBundle(buf, "application/zip");
      toast("ok", "Skill bundle uploaded.");
    }
    event.target.value = "";
    editKeywords.value = "";
    await load();
  } catch (e) {
    error.value = e.message;
  } finally {
    uploading.value = false;
  }
}

// ── delete handler ────────────────────────────────────────────────────────────

async function doDelete(bundle) {
  if (!confirm(`Delete skill bundle "${bundle.name}"? This cannot be undone.`)) return;
  error.value = "";
  try {
    await deleteSkillBundle(bundle.id);
    toast("ok", `Bundle "${bundle.name}" deleted.`);
    await load();
  } catch (e) {
    error.value = e.message;
  }
}

// ── grant handler ─────────────────────────────────────────────────────────────

async function doGrant(bundle) {
  const group = (grantGroup.value[bundle.id] ?? "").trim();
  if (!group) return;
  granting.value = { ...granting.value, [bundle.id]: true };
  error.value = "";
  try {
    await grantSkillBundle(bundle.id, group);
    grantGroup.value = { ...grantGroup.value, [bundle.id]: "" };
    toast("ok", `Granted "${bundle.name}" to group:${group}.`);
  } catch (e) {
    error.value = e.message;
  } finally {
    granting.value = { ...granting.value, [bundle.id]: false };
  }
}
</script>

<template>
  <div class="view">
    <div class="view-header">
      <Icon name="book" class="view-icon" />
      <h1>Skills</h1>
    </div>

    <EmptyState
      v-if="forbidden"
      data-testid="forbidden"
      icon="admin"
      message="You are not a tenant admin."
    />

    <template v-else>
      <!-- error banner -->
      <div v-if="error" data-testid="error-banner" class="banner-err">{{ error }}</div>

      <!-- upload / edit panel -->
      <div class="upload-panel">
        <div data-testid="upload-panel-label" class="section-label">
          {{ editingId ? "Edit Skill Bundle" : "Upload SKILL.md" }}
        </div>
        <div class="hint small">
          {{ editingId
            ? "Edit the SKILL.md text below, or upload a .skill / .zip bundle to replace it."
            : "Paste SKILL.md text below, or upload a .skill / .zip bundle file." }}
        </div>
        <input
          data-testid="file-input"
          type="file"
          accept=".skill,.zip"
          class="file-input"
          :disabled="uploading"
          @change="onFileChange"
        />
        <textarea
          v-model="uploadContent"
          data-testid="upload-input"
          class="upload-textarea"
          placeholder="---&#10;name: my-skill&#10;description: ...&#10;allowed-tools:&#10;  - web.fetch&#10;---&#10;# System prompt body"
          rows="8"
          :disabled="uploading"
        />
        <template v-if="editingId">
          <div class="hint small">
            Auto-load keywords (comma-separated). Empty clears keywords — the bundle never auto-loads.
          </div>
          <input
            v-model="editKeywords"
            data-testid="keywords-input"
            class="keywords-input"
            placeholder="billing, invoice, refund"
            :disabled="uploading"
          />
        </template>
        <div class="add-row">
          <button
            data-testid="upload-btn"
            class="btn-primary-sm"
            :disabled="uploading || !uploadContent.trim()"
            @click="doUpload"
          >{{ uploading ? "Uploading…" : (editingId ? "Save changes" : "Upload bundle") }}</button>
          <button
            v-if="editingId"
            data-testid="cancel-edit-btn"
            class="btn-ghost-sm"
            :disabled="uploading"
            @click="cancelEdit"
          >Cancel</button>
        </div>
      </div>

      <!-- bundles table -->
      <div class="section-label" style="margin-top: 24px;">Bundles ({{ bundles.length }})</div>

      <div v-if="loading" class="list-loading">Loading…</div>

      <EmptyState
        v-else-if="!loading && bundles.length === 0"
        data-testid="empty-bundles"
        icon="admin"
        message="No skill bundles uploaded yet."
      />

      <table v-else class="bundles-table">
        <thead>
          <tr>
            <th>Name</th>
            <th>Description</th>
            <th>Files</th>
            <th>Keywords</th>
            <th>Flags</th>
            <th>Grant to group</th>
            <th></th>
            <th></th>
          </tr>
        </thead>
        <tbody>
          <tr
            v-for="b in bundles"
            :key="b.id"
            data-testid="bundle-row"
          >
            <td class="mono">{{ b.name }}</td>
            <td>{{ b.description }}</td>
            <td class="files-cell">
              <span v-if="b.filePaths?.length">{{ b.filePaths.length }}</span>
              <span v-else class="no-items">—</span>
            </td>
            <td class="tools-cell">
              <span
                v-for="k in b.keywords"
                :key="k"
                class="chip"
              >{{ k }}</span>
              <span v-if="!b.keywords?.length" class="no-items">—</span>
            </td>
            <td>
              <span v-if="b.disableModelInvocation" class="flag-badge">no-model-invoke</span>
              <span v-if="b.contextFork" class="flag-badge">context-fork</span>
            </td>
            <td>
              <div class="grant-row">
                <input
                  :value="grantGroup[b.id] ?? ''"
                  :data-testid="`grant-group-${b.id}`"
                  class="inline-input"
                  placeholder="group-name"
                  :disabled="granting[b.id]"
                  @input="(e) => { grantGroup[b.id] = e.target.value; }"
                />
                <button
                  :data-testid="`grant-btn-${b.id}`"
                  class="btn-ghost-sm"
                  :disabled="granting[b.id] || !(grantGroup[b.id] ?? '').trim()"
                  @click="doGrant(b)"
                >Grant</button>
              </div>
            </td>
            <td class="right">
              <button
                :data-testid="`edit-${b.id}`"
                class="btn-ghost-sm"
                @click="doEdit(b)"
              >Edit</button>
            </td>
            <td class="right">
              <button
                :data-testid="`delete-${b.id}`"
                class="btn-danger-sm"
                @click="doDelete(b)"
              >Delete</button>
            </td>
          </tr>
        </tbody>
      </table>
    </template>
  </div>
</template>

<style scoped>
.view { padding: 24px 32px; max-width: 1100px; }
.view-header { display: flex; align-items: center; gap: 10px; margin-bottom: 20px; }
.view-header h1 { margin: 0; font-size: 20px; font-weight: 600; }
.view-icon { color: var(--text-muted); width: 22px; height: 22px; }

.banner-err {
  background: var(--fill-danger); border: 1px solid var(--danger);
  border-radius: var(--radius-sm); padding: 10px 14px;
  color: var(--danger); font-size: 13px; margin-bottom: 12px;
}

.section-label {
  font-size: 12px; font-weight: 600; text-transform: uppercase;
  letter-spacing: 0.06em; color: var(--text-muted); margin-bottom: 8px;
}

.hint       { color: var(--text-muted); font-size: 12px; margin-bottom: 6px; }
.hint.small { font-size: 11px; }

.upload-panel {
  border: 1px solid var(--border); border-radius: var(--radius-sm);
  padding: 16px; margin-bottom: 8px;
}

.file-input {
  display: block; margin-bottom: 8px; font-size: 12px; color: var(--text-muted);
}

.upload-textarea {
  width: 100%; box-sizing: border-box;
  background: var(--bg); color: var(--text);
  border: 1px solid var(--border); border-radius: var(--radius-sm);
  padding: 8px 10px; font-size: 12px; font-family: var(--font-mono);
  resize: vertical; margin-bottom: 8px; display: block;
}

.keywords-input {
  width: 100%; box-sizing: border-box;
  background: var(--bg); color: var(--text);
  border: 1px solid var(--border); border-radius: var(--radius-sm);
  padding: 6px 10px; font-size: 12px; font-family: var(--font-mono);
  margin-bottom: 8px; display: block;
}

.add-row {
  display: flex; gap: 6px; align-items: center; flex-wrap: wrap;
}

.list-loading { padding: 12px 0; color: var(--text-muted); font-size: 13px; }

.bundles-table { width: 100%; border-collapse: collapse; font-size: 13px; }
.bundles-table th {
  text-align: left; font-size: 11px; font-weight: 600;
  text-transform: uppercase; letter-spacing: 0.05em;
  color: var(--text-muted); padding: 4px 8px; border-bottom: 1px solid var(--border);
}
.bundles-table td { padding: 8px; border-bottom: 1px solid var(--border); vertical-align: middle; }
.bundles-table tr:last-child td { border-bottom: none; }

.tools-cell { display: table-cell; }

/* chips */
.chip {
  display: inline-flex; align-items: center; gap: 4px;
  background: var(--fill-muted); color: var(--text);
  border: 1px solid var(--border); border-radius: 12px;
  padding: 2px 8px; font-size: 11px; margin-right: 4px;
}
.flag-badge {
  display: inline-block; background: var(--fill-muted); color: var(--text-muted);
  border: 1px solid var(--border); border-radius: var(--radius-sm);
  padding: 1px 6px; font-size: 11px; margin-right: 4px; font-family: var(--font-mono);
}

.grant-row { display: flex; gap: 6px; align-items: center; }

.inline-input {
  background: var(--bg); color: var(--text);
  border: 1px solid var(--border); border-radius: var(--radius-sm);
  padding: 4px 8px; font-size: 12px; font-family: var(--font-mono); min-width: 140px;
}

.no-items { color: var(--text-muted); font-size: 12px; font-style: italic; }

.right { text-align: right; }

.btn-primary-sm {
  display: inline-flex; align-items: center; gap: 4px;
  background: var(--accent); color: var(--text-on-accent);
  border: none; border-radius: var(--radius-sm);
  padding: 5px 10px; font-size: 12px; cursor: pointer; white-space: nowrap;
}
.btn-primary-sm:hover:not(:disabled) { background: var(--accent-hover); }
.btn-primary-sm:disabled { opacity: 0.5; cursor: default; }

.btn-ghost-sm {
  background: transparent; color: var(--text); border: 1px solid var(--border);
  border-radius: var(--radius-sm); padding: 4px 10px; font-size: 12px; cursor: pointer; white-space: nowrap;
}
.btn-ghost-sm:hover:not(:disabled) { background: var(--bg-hover); }
.btn-ghost-sm:disabled { opacity: 0.5; cursor: default; }

.btn-danger-sm {
  display: inline-flex; align-items: center; gap: 4px;
  background: transparent; color: var(--danger);
  border: 1px solid var(--danger); border-radius: var(--radius-sm);
  padding: 3px 10px; cursor: pointer; font-size: 12px; opacity: 0.8;
}
.btn-danger-sm:hover:not(:disabled) { background: var(--fill-danger); opacity: 1; }
.btn-danger-sm:disabled { opacity: 0.3; cursor: default; }

.mono { font-family: var(--font-mono); }

@media (max-width: 860px) {
  .view { padding: 16px; }
  .bundles-table th:nth-child(3),
  .bundles-table td:nth-child(3) { display: none; }
}
</style>
