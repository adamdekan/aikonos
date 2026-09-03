<script setup>
import { ref, onMounted } from "vue";
import {
  getWebSearchConfig,
  upsertWebSearchConfig,
  deleteWebSearchConfig,
  testWebSearchConfig,
} from "../../api/admin.js";
import Icon from "../../components/Icon.vue";
import EmptyState from "../../components/ui/EmptyState.vue";
import FormField from "../../components/ui/FormField.vue";
import { useToast } from "../../components/ui/useToast.js";

const { push: toast } = useToast();

const forbidden = ref(false);
const loading   = ref(false);
const error     = ref("");
const saving    = ref(false);
const deleting  = ref(false);

const hasKey    = ref(false);
const updatedBy = ref("");
const updatedAt = ref("");

const fEngine     = ref("brave");
const fMaxResults = ref(10);
const fApiKey     = ref("");
const formError   = ref("");

// Test-probe state. testResult: null = untested, else { ok, message }.
const testing    = ref(false);
const testResult = ref(null);

// applyConfig populates the form from a {config} response (or resets it to the
// empty/unconfigured state on delete). The key field is never pre-filled —
// hasKey drives the placeholder instead (write-only contract).
function applyConfig(c) {
  hasKey.value      = c?.hasKey ?? false;
  updatedBy.value   = c?.updatedBy ?? "";
  updatedAt.value   = c?.updatedAt ?? "";
  fEngine.value     = c?.engine || "brave";
  fMaxResults.value = c?.maxResults || 10;
  fApiKey.value     = "";
}

async function load() {
  loading.value   = true;
  error.value     = "";
  forbidden.value = false;
  try {
    const data = await getWebSearchConfig();
    if (data.forbidden) { forbidden.value = true; return; }
    applyConfig(data.config);
  } catch (e) {
    error.value = e.message;
  } finally {
    loading.value = false;
  }
}

onMounted(load);

// validateForm returns an error string, or "" when the form is valid.
function validateForm() {
  if (!fEngine.value) return "Engine is required.";
  if (!Number.isInteger(fMaxResults.value) || fMaxResults.value < 1) {
    return "Max results must be a positive integer.";
  }
  return "";
}

// buildPayload assembles the wire payload. apiKey sent "" when left blank —
// the broker preserves the stored key on an edit (same contract as the M365
// client secret / LLM provider API key).
function buildPayload() {
  return {
    engine: fEngine.value,
    maxResults: fMaxResults.value,
    apiKey: fApiKey.value,
  };
}

async function submit() {
  formError.value = "";
  testResult.value = null;
  const err = validateForm();
  if (err) { formError.value = err; return; }

  saving.value = true;
  try {
    const resp = await upsertWebSearchConfig(buildPayload());
    if (resp.forbidden) { formError.value = resp.error || "You are not a tenant admin."; return; }
    applyConfig(resp.config);
    toast("ok", "Web search config saved.");
  } catch (e) {
    formError.value = e.message;
  } finally {
    saving.value = false;
  }
}

async function testConnection() {
  formError.value = "";
  testResult.value = null;
  const err = validateForm();
  if (err) { formError.value = err; return; }

  testing.value = true;
  try {
    const res = await testWebSearchConfig(buildPayload());
    if (res.forbidden) { formError.value = res.error || "You are not a tenant admin."; return; }
    testResult.value = { ok: res.ok, message: res.detail || (res.ok ? "Search probe succeeded." : "Search probe failed.") };
  } catch (e) {
    testResult.value = { ok: false, message: e.message };
  } finally {
    testing.value = false;
  }
}

async function removeConfig() {
  if (!confirm("Delete the web search configuration? Agents will no longer be able to use web.search until this is reconfigured.")) return;
  error.value = "";
  deleting.value = true;
  try {
    await deleteWebSearchConfig();
    applyConfig(null);
    testResult.value = null;
    toast("ok", "Web search config deleted.");
  } catch (e) {
    error.value = e.message;
  } finally {
    deleting.value = false;
  }
}
</script>

<template>
  <div class="view">
    <div class="view-header">
      <Icon name="search" class="view-icon" />
      <h1>Web Search</h1>
    </div>

    <EmptyState
      v-if="forbidden"
      data-testid="forbidden"
      icon="settings"
      message="You are not a tenant admin."
    />

    <template v-else>
      <p class="lede">
        Configures the org-wide search engine backing the <code>web.search</code> tool. Agents
        query this engine to discover URLs, then load results with <code>web.fetch</code>.
      </p>

      <div v-if="error" data-testid="error-banner" class="banner-err">{{ error }}</div>

      <div class="form-card">
        <FormField label="Engine" required>
          <select v-model="fEngine" data-testid="websearch-engine" :disabled="loading">
            <option value="brave">Brave</option>
            <option value="exa">Exa</option>
            <option value="tavily">Tavily</option>
          </select>
        </FormField>

        <FormField label="Max results" required>
          <input
            v-model.number="fMaxResults"
            type="number"
            min="1"
            data-testid="websearch-max-results"
            :disabled="loading"
          />
        </FormField>

        <FormField label="API key">
          <input
            v-model="fApiKey"
            type="password"
            :placeholder="hasKey ? '•••••• (unchanged)' : 'Enter API key'"
            data-testid="websearch-api-key"
            autocomplete="new-password"
            :disabled="loading"
          />
        </FormField>

        <p v-if="updatedBy" class="setting-meta">
          Last changed by <span class="mono">{{ updatedBy }}</span>
          <span v-if="updatedAt"> on {{ updatedAt }}</span>
        </p>

        <div v-if="formError" class="field-error">{{ formError }}</div>
        <div
          v-if="testResult"
          class="test-result"
          :class="testResult.ok ? 'test-ok' : 'test-err'"
          data-testid="websearch-test-result"
        >
          <Icon :name="testResult.ok ? 'check' : 'close'" />
          {{ testResult.message }}
        </div>

        <div class="actions">
          <button
            class="btn-secondary"
            type="button"
            data-testid="websearch-test-btn"
            :disabled="testing || loading"
            @click="testConnection"
          >
            {{ testing ? "Testing…" : "Test connection" }}
          </button>
          <button
            class="btn-danger-sm"
            type="button"
            data-testid="websearch-delete-btn"
            :disabled="deleting || loading"
            @click="removeConfig"
          >
            Delete
          </button>
          <button
            class="btn-primary"
            type="button"
            data-testid="websearch-save-btn"
            :disabled="saving || loading"
            @click="submit"
          >
            {{ saving ? "Saving…" : "Save" }}
          </button>
        </div>
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
  margin: 0 0 16px; max-width: 68ch;
}
.lede code {
  font-family: var(--font-mono); font-size: 12px;
  background: var(--fill-muted); padding: 1px 5px; border-radius: 4px;
}

.banner-err {
  background: var(--fill-danger); border: 1px solid var(--danger);
  border-radius: var(--radius-sm); padding: 10px 14px;
  color: var(--danger); font-size: 13px; margin-bottom: 16px;
}

.form-card {
  display: flex; flex-direction: column; gap: 14px;
  background: var(--bg-elevated); border: 1px solid var(--border);
  border-radius: var(--radius); padding: 20px;
}

.setting-meta { color: var(--text-faint); font-size: 12px; margin: 0; }
.mono { font-family: var(--font-mono); }

.field-error { color: var(--danger); font-size: 12px; }

.test-result {
  display: flex; align-items: center; gap: 6px;
  padding: 8px 10px;
  border-radius: var(--radius-sm); font-size: 12px;
  border: 1px solid transparent; word-break: break-word;
}
.test-ok  { background: var(--fill-muted); color: var(--ok);     border-color: var(--ok); }
.test-err { background: var(--fill-danger); color: var(--danger); border-color: var(--danger); }

.actions { display: flex; gap: 8px; justify-content: flex-end; }

input, select {
  width: 100%; box-sizing: border-box;
  background: var(--bg); color: var(--text);
  border: 1px solid var(--border); border-radius: var(--radius-sm);
  padding: 7px 9px; font-size: 13px;
}

.btn-primary {
  display: inline-flex; align-items: center; gap: 4px;
  background: var(--accent); color: var(--text-on-accent);
  border: none; border-radius: var(--radius-sm);
  padding: 7px 13px; font-size: 13px; cursor: pointer;
}
.btn-primary:hover { background: var(--accent-hover); }
.btn-primary:disabled { opacity: 0.6; cursor: default; }

.btn-secondary {
  display: inline-flex; align-items: center; gap: 4px;
  background: transparent; color: var(--text); border: 1px solid var(--border);
  border-radius: var(--radius-sm); padding: 7px 13px; font-size: 13px; cursor: pointer;
}
.btn-secondary:hover { background: var(--bg-hover); }
.btn-secondary:disabled { opacity: 0.6; cursor: default; }

.btn-danger-sm {
  display: inline-flex; align-items: center; gap: 4px;
  background: transparent; color: var(--danger);
  border: 1px solid var(--danger); border-radius: var(--radius-sm);
  padding: 7px 13px; cursor: pointer; font-size: 13px; opacity: 0.8;
}
.btn-danger-sm:hover { background: var(--fill-danger); opacity: 1; }
.btn-danger-sm:disabled { opacity: 0.4; cursor: default; }
</style>
