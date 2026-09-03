<script setup>
import { ref, computed, onMounted } from "vue";
import { getOrgSettings, updateOrgSettings } from "../../api/admin.js";
import Icon from "../../components/Icon.vue";
import EmptyState from "../../components/ui/EmptyState.vue";
import ByteCounter from "./ByteCounter.vue";
import { useToast } from "../../components/ui/useToast.js";

const { push: toast } = useToast();

const MAX_CHARS = 4000;

const forbidden = ref(false);
const loading = ref(false);
const saving = ref(false);
const error = ref("");

const preamble = ref("");
const savedPreamble = ref("");
const updatedBy = ref("");
const updatedAt = ref("");

const dirty = computed(() => preamble.value !== savedPreamble.value);
const overLimit = computed(() => preamble.value.length > MAX_CHARS);

async function load() {
  loading.value = true;
  error.value = "";
  forbidden.value = false;
  try {
    const data = await getOrgSettings();
    if (data.forbidden) { forbidden.value = true; return; }
    const s = data.settings ?? {};
    preamble.value = s.instructionPreamble ?? "";
    savedPreamble.value = preamble.value;
    updatedBy.value = s.updatedBy ?? "";
    updatedAt.value = s.updatedAt ?? "";
  } catch (e) {
    error.value = e.message;
  } finally {
    loading.value = false;
  }
}

onMounted(load);

async function save() {
  if (overLimit.value) { error.value = `Instructions exceed the ${MAX_CHARS}-character limit.`; return; }
  error.value = "";
  saving.value = true;
  try {
    const resp = await updateOrgSettings({ instructionPreamble: preamble.value });
    if (resp.forbidden) { error.value = resp.error || "You are not a tenant admin."; return; }
    const s = resp.settings ?? {};
    savedPreamble.value = s.instructionPreamble ?? preamble.value;
    updatedBy.value = s.updatedBy ?? "";
    updatedAt.value = s.updatedAt ?? "";
    toast("ok", "Organization instructions saved.");
  } catch (e) {
    error.value = e.message;
  } finally {
    saving.value = false;
  }
}

function revert() {
  preamble.value = savedPreamble.value;
  error.value = "";
}

function fmtDate(iso) {
  if (!iso) return "";
  const d = new Date(iso);
  return isNaN(d) ? iso : d.toLocaleString();
}
</script>

<template>
  <div class="view">
    <div class="view-header">
      <Icon name="scale" class="view-icon" />
      <h1>Organization Instructions</h1>
    </div>

    <EmptyState
      v-if="forbidden"
      data-testid="forbidden"
      icon="settings"
      message="You are not a tenant admin."
    />

    <template v-else>
      <p class="lede">
        A governance preamble prepended to <strong>every</strong> agent session across your
        organization — chat, scheduled runs, and external invocations. Use it for compliance
        rules that must take priority over individual user and agent preferences (data handling,
        PII policy, allowed topics). These instructions shape behavior; they never grant tools —
        every tool call is still gated by the policy broker.
      </p>

      <div v-if="error" data-testid="error-banner" class="banner-err">{{ error }}</div>

      <div class="editor">
        <textarea
          v-model="preamble"
          data-testid="org-preamble"
          class="preamble"
          :class="{ over: overLimit }"
          rows="12"
          spellcheck="true"
          placeholder="e.g. Never include personally identifiable information (PII) in responses. Cite the source document for every factual claim drawn from a connector."
        ></textarea>

        <div class="editor-foot">
          <ByteCounter :value="preamble.length" :max="MAX_CHARS" />
          <span v-if="updatedBy" class="meta">
            Last updated by <span class="mono">{{ updatedBy }}</span>
            <span v-if="updatedAt"> · {{ fmtDate(updatedAt) }}</span>
          </span>
        </div>
      </div>

      <div class="actions">
        <button
          class="btn-primary"
          data-testid="save-org-instructions"
          :disabled="saving || overLimit || !dirty"
          @click="save"
        >
          <Icon name="check" /> {{ saving ? "Saving…" : "Save" }}
        </button>
        <button
          class="btn-secondary"
          :disabled="saving || !dirty"
          @click="revert"
        >
          Revert
        </button>
        <span v-if="dirty" class="unsaved">Unsaved changes</span>
      </div>
    </template>
  </div>
</template>

<style scoped>
.view { padding: 24px 32px; max-width: 800px; }
.view-header { display: flex; align-items: center; gap: 10px; margin-bottom: 16px; }
.view-header h1 { margin: 0; font-size: 20px; font-weight: 600; }
.view-icon { color: var(--text-muted); width: 22px; height: 22px; }

.lede {
  color: var(--text-muted); font-size: 13px; line-height: 1.6;
  margin: 0 0 20px; max-width: 68ch;
}
.lede strong { color: var(--text); }

.banner-err {
  background: var(--fill-danger); border: 1px solid var(--danger);
  border-radius: var(--radius-sm); padding: 10px 14px;
  color: var(--danger); font-size: 13px; margin-bottom: 16px;
}

.editor { display: flex; flex-direction: column; gap: 8px; }
.preamble {
  width: 100%; box-sizing: border-box;
  background: var(--bg-elevated); color: var(--text);
  border: 1px solid var(--border); border-radius: var(--radius-sm);
  padding: 12px 14px; font-size: 13px; line-height: 1.6;
  font-family: var(--font-sans); resize: vertical; min-height: 220px;
}
.preamble:focus { outline: none; border-color: var(--accent); }
.preamble.over { border-color: var(--danger); }

.editor-foot {
  display: flex; align-items: center; justify-content: space-between;
  flex-wrap: wrap; gap: 8px; font-size: 12px; color: var(--text-faint);
}
.meta { color: var(--text-faint); }
.mono { font-family: var(--font-mono); }

.actions { display: flex; align-items: center; gap: 10px; margin-top: 20px; }

.btn-primary {
  display: inline-flex; align-items: center; gap: 4px;
  background: var(--accent); color: var(--text-on-accent);
  border: none; border-radius: var(--radius-sm);
  padding: 8px 16px; font-size: 13px; cursor: pointer;
}
.btn-primary:hover:not(:disabled) { background: var(--accent-hover); }
.btn-primary:disabled { opacity: 0.5; cursor: not-allowed; }

.btn-secondary {
  background: transparent; color: var(--text-muted);
  border: 1px solid var(--border); border-radius: var(--radius-sm);
  padding: 8px 16px; font-size: 13px; cursor: pointer;
}
.btn-secondary:hover:not(:disabled) { background: var(--bg-hover); color: var(--text); }
.btn-secondary:disabled { opacity: 0.5; cursor: not-allowed; }

.unsaved { font-size: 12px; color: var(--accent); }
</style>
