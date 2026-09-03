<script setup>
import { ref, onMounted } from "vue";
import {
  getM365Connection,
  upsertM365Connection,
  deleteM365Connection,
  testM365Connection,
} from "../../api/admin.js";
import Icon from "../../components/Icon.vue";
import EmptyState from "../../components/ui/EmptyState.vue";
import FormField from "../../components/ui/FormField.vue";
import ToggleSwitch from "../../components/ui/ToggleSwitch.vue";
import { useToast } from "../../components/ui/useToast.js";

const { push: toast } = useToast();

const forbidden = ref(false);
const loading   = ref(false);
const error     = ref("");
const saving    = ref(false);
const deleting  = ref(false);

const hasSecret = ref(false);
const updatedBy = ref("");
const updatedAt = ref("");

const fTenantId = ref("");
const fClientId = ref("");
const fSecret   = ref("");
const fEnabled  = ref(false);
const formError = ref("");

// Test-connection state. testResult: null = untested, else { ok, message }.
const testing    = ref(false);
const testResult = ref(null);

// applyConnection populates the form from a {connection} response (or resets
// it to the empty/unconfigured state on delete). The secret field is never
// pre-filled — hasSecret drives the placeholder instead (write-only contract).
function applyConnection(c) {
  hasSecret.value = c?.hasSecret ?? false;
  updatedBy.value = c?.updatedBy ?? "";
  updatedAt.value = c?.updatedAt ?? "";
  fTenantId.value = c?.entraTenantId ?? "";
  fClientId.value = c?.clientId ?? "";
  fEnabled.value  = c?.enabled ?? false;
  fSecret.value   = "";
}

async function load() {
  loading.value   = true;
  error.value     = "";
  forbidden.value = false;
  try {
    const data = await getM365Connection();
    if (data.forbidden) { forbidden.value = true; return; }
    applyConnection(data.connection);
  } catch (e) {
    error.value = e.message;
  } finally {
    loading.value = false;
  }
}

onMounted(load);

// validateForm returns an error string, or "" when the form is valid.
function validateForm() {
  if (!fTenantId.value.trim()) return "Entra tenant ID is required.";
  if (!fClientId.value.trim()) return "Application (client) ID is required.";
  return "";
}

// buildPayload assembles the wire payload. clientSecret sent "" when left
// blank — the broker preserves the stored secret on an edit (same contract
// as the LLM provider API key).
function buildPayload() {
  return {
    entraTenantId: fTenantId.value.trim(),
    clientId: fClientId.value.trim(),
    clientSecret: fSecret.value,
    enabled: fEnabled.value,
  };
}

async function submit() {
  formError.value = "";
  testResult.value = null;
  const err = validateForm();
  if (err) { formError.value = err; return; }

  saving.value = true;
  try {
    const resp = await upsertM365Connection(buildPayload());
    if (resp.forbidden) { formError.value = resp.error || "You are not a tenant admin."; return; }
    applyConnection(resp.connection);
    toast("ok", "M365 connection saved.");
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
    const res = await testM365Connection(buildPayload());
    if (res.forbidden) { formError.value = res.error || "You are not a tenant admin."; return; }
    testResult.value = { ok: res.ok, message: res.detail || (res.ok ? "OBO exchange succeeded." : "Connection failed.") };
  } catch (e) {
    testResult.value = { ok: false, message: e.message };
  } finally {
    testing.value = false;
  }
}

async function disconnect() {
  if (!confirm("Disconnect Microsoft 365? Every user's OneDrive working-folder access stops until this is reconfigured.")) return;
  error.value = "";
  deleting.value = true;
  try {
    await deleteM365Connection();
    applyConnection(null);
    testResult.value = null;
    toast("ok", "M365 connection removed.");
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
      <Icon name="drive" class="view-icon" />
      <h1>Microsoft 365</h1>
    </div>

    <EmptyState
      v-if="forbidden"
      data-testid="forbidden"
      icon="settings"
      message="You are not a tenant admin."
    />

    <template v-else>
      <p class="lede">
        Connects this tenant to your Azure AD org so every user's OneDrive is reachable through
        on-behalf-of (OBO) delegated access — no per-user connect step.
      </p>

      <div class="help-box" data-testid="m365-help">
        <p>
          Use the <strong>same Entra app registration</strong> your users sign in with. Extend it
          with delegated Microsoft Graph permissions <code>Files.ReadWrite</code> and
          <code>offline_access</code>, add a client secret, then have a
          <strong>tenant admin grant admin consent</strong> in the Entra portal — this app cannot
          request consent on its own.
        </p>
        <p>
          <strong>Test connection</strong> runs a real OBO exchange with your own sign-in and
          reports exactly what's missing: <code>AADSTS65001</code> means admin consent hasn't
          been granted, <code>AADSTS7000215</code> means the client secret is wrong or expired,
          and <code>AADSTS500011</code> means this app registration doesn't match your tenant.
        </p>
      </div>

      <div v-if="error" data-testid="error-banner" class="banner-err">{{ error }}</div>

      <div class="form-card">
        <FormField label="Entra tenant ID" required>
          <input
            v-model="fTenantId"
            placeholder="00000000-0000-0000-0000-000000000000"
            data-testid="m365-tenant-id"
            :disabled="loading"
          />
        </FormField>

        <FormField label="Application (client) ID" required>
          <input
            v-model="fClientId"
            placeholder="00000000-0000-0000-0000-000000000000"
            data-testid="m365-client-id"
            :disabled="loading"
          />
        </FormField>

        <FormField label="Client secret">
          <input
            v-model="fSecret"
            type="password"
            :placeholder="hasSecret ? '•••••• (unchanged)' : 'Enter client secret'"
            data-testid="m365-secret"
            autocomplete="new-password"
            :disabled="loading"
          />
        </FormField>

        <FormField label="Enabled">
          <label class="checkbox-item">
            <ToggleSwitch
              v-model="fEnabled"
              data-testid="m365-enabled"
              aria-label="Enabled"
              :disabled="loading"
            />
            Enabled
          </label>
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
          data-testid="m365-test-result"
        >
          <Icon :name="testResult.ok ? 'check' : 'close'" />
          {{ testResult.message }}
        </div>

        <div class="actions">
          <button
            class="btn-secondary"
            type="button"
            data-testid="m365-test-btn"
            :disabled="testing || loading"
            @click="testConnection"
          >
            {{ testing ? "Testing…" : "Test connection" }}
          </button>
          <button
            class="btn-danger-sm"
            type="button"
            data-testid="m365-disconnect-btn"
            :disabled="deleting || loading"
            @click="disconnect"
          >
            Disconnect
          </button>
          <button
            class="btn-primary"
            type="button"
            data-testid="m365-save-btn"
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

.help-box {
  background: var(--bg-elevated); border: 1px solid var(--border);
  border-radius: var(--radius); padding: 14px 16px; margin-bottom: 20px;
}
.help-box p { margin: 0 0 10px; font-size: 13px; line-height: 1.6; color: var(--text-muted); max-width: 68ch; }
.help-box p:last-child { margin-bottom: 0; }
.help-box code {
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

.checkbox-item {
  display: flex; align-items: center; gap: 8px;
  font-size: 13px; color: var(--text); cursor: pointer;
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

input {
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
