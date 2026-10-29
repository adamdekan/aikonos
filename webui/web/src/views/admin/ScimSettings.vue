<script setup>
// Directory sync (AG-16): provisioning.mode, the SCIM tenant URL and the
// tenant's SCIM bearer tokens. A new token is shown once, then only its prefix.
import { ref, computed, onMounted } from "vue";
import { getScim, createScimToken, revokeScimToken, setProvisioningMode } from "../../api/admin.js";
import { useToast } from "../../components/ui/useToast.js";

const emit = defineEmits(["mode"]);
const { push: toast } = useToast();

const mode      = ref("jit");
const baseUrl   = ref("");
const tokens    = ref([]);
const rawToken  = ref("");   // shown once after create/rotate; cleared on dismiss
const error     = ref("");
const busy      = ref(false);
const forbidden = ref(false);

// No configured external base URL: guess the external surface on this host.
const scimUrl = computed(() => baseUrl.value || `${location.protocol}//${location.hostname}:8090/scim/v2`);

async function load() {
  error.value = "";
  try {
    const data = await getScim();
    if (data.forbidden) { forbidden.value = true; return; }
    mode.value    = data.mode || "jit";
    baseUrl.value = data.baseUrl || "";
    tokens.value  = data.tokens ?? [];
    emit("mode", mode.value);
  } catch (e) {
    error.value = e.message;
  }
}

onMounted(load);

async function changeMode(next) {
  if (next === mode.value) return;
  const msg = next === "scim"
    ? "Switch to SCIM? Email rules stop granting groups; the identity provider decides membership and deactivation."
    : "Switch back to email rules? The SCIM endpoint stops serving and email rules grant groups again.";
  if (!confirm(msg)) return;
  busy.value = true;
  error.value = "";
  try {
    const resp = await setProvisioningMode(next);
    if (resp?.forbidden) { error.value = resp.error || "You are not a tenant admin."; return; }
    mode.value = next;
    emit("mode", next);
    toast("ok", `Provisioning mode: ${next}.`);
  } catch (e) {
    error.value = e.message;
  } finally {
    busy.value = false;
  }
}

async function mint(replacesId = "") {
  if (replacesId && !confirm("Rotate this token? The old token stops working now; paste the new one into the identity provider.")) return;
  busy.value = true;
  error.value = "";
  try {
    const resp = await createScimToken(replacesId);
    if (resp.forbidden || resp.error) { error.value = resp.error || "You are not a tenant admin."; return; }
    rawToken.value = resp.rawToken ?? "";
    await load();
  } catch (e) {
    error.value = e.message;
  } finally {
    busy.value = false;
  }
}

async function revoke(t) {
  if (!confirm(`Revoke token ${t.prefix}…? Provisioning with it stops at once.`)) return;
  busy.value = true;
  error.value = "";
  try {
    const resp = await revokeScimToken(t.id);
    if (resp?.forbidden) { error.value = resp.error || "You are not a tenant admin."; return; }
    toast("ok", "Token revoked.");
    await load();
  } catch (e) {
    error.value = e.message;
  } finally {
    busy.value = false;
  }
}

function copy(text) {
  navigator.clipboard?.writeText(text).then(() => toast("ok", "Copied."));
}

function fmt(iso) {
  return iso ? new Date(iso).toLocaleString() : "never";
}
</script>

<template>
  <section v-if="!forbidden" class="scim" data-testid="scim-section">
    <h2>Directory sync (SCIM)</h2>
    <p class="muted">
      With SCIM the identity provider (Entra ID) creates users, deactivates leavers (a block) and
      sets group membership. Email rules below stop granting groups.
    </p>

    <div v-if="error" data-testid="scim-error" class="banner-err">{{ error }}</div>

    <div class="row">
      <label for="scim-mode">Provisioning mode</label>
      <select
        id="scim-mode"
        data-testid="scim-mode-select"
        :value="mode"
        :disabled="busy"
        @change="changeMode($event.target.value)"
      >
        <option value="jit">Email rules at first sign-in (jit)</option>
        <option value="scim">Identity provider via SCIM (scim)</option>
      </select>
    </div>

    <div class="row">
      <label for="scim-url">SCIM tenant URL</label>
      <input id="scim-url" data-testid="scim-base-url" class="mono url" :value="scimUrl" readonly />
      <button class="btn-ghost" data-testid="scim-copy-url" @click="copy(scimUrl)">Copy</button>
    </div>

    <div v-if="rawToken" class="banner-ok reveal" data-testid="scim-raw-token-panel">
      <div>Copy this token into the identity provider now. It is not shown again.</div>
      <code data-testid="scim-raw-token" class="mono">{{ rawToken }}</code>
      <div class="actions">
        <button class="btn-ghost" data-testid="scim-copy-token" @click="copy(rawToken)">Copy</button>
        <button class="btn-ghost" data-testid="scim-raw-dismiss" @click="rawToken = ''">Done</button>
      </div>
    </div>

    <div class="row">
      <span class="label">Secret tokens</span>
      <button class="btn-primary" data-testid="scim-create-token" :disabled="busy" @click="mint()">Create token</button>
    </div>
    <p v-if="!tokens.length" class="muted" data-testid="scim-no-tokens">No active SCIM tokens.</p>
    <table v-else class="tokens">
      <thead><tr><th>Token</th><th>Created</th><th>Last used</th><th>Expires</th><th></th></tr></thead>
      <tbody>
        <tr v-for="t in tokens" :key="t.id" data-testid="scim-token-row">
          <td class="mono">{{ t.prefix }}…</td>
          <td class="muted">{{ fmt(t.createdAt) }} · {{ t.createdBy }}</td>
          <td class="muted">{{ fmt(t.lastUsedAt) }}</td>
          <td class="muted" data-testid="scim-token-expires">{{ fmt(t.expiresAt) }}</td>
          <td class="right">
            <button class="btn-ghost" :data-testid="`scim-rotate-${t.id}`" :disabled="busy" @click="mint(t.id)">Rotate</button>
            <button class="btn-danger-sm" :data-testid="`scim-revoke-${t.id}`" :disabled="busy" @click="revoke(t)">Revoke</button>
          </td>
        </tr>
      </tbody>
    </table>
  </section>
</template>

<style scoped>
.scim { margin-bottom: 28px; padding-bottom: 20px; border-bottom: 1px solid var(--border); }
.scim h2 { font-size: 16px; font-weight: 600; margin: 0 0 6px; }
.muted { color: var(--text-muted); font-size: 13px; }
.row { display: flex; flex-wrap: wrap; align-items: center; gap: 8px; margin: 12px 0; }
.row label, .row .label { min-width: 140px; font-size: 13px; }
select, input {
  background: var(--bg-elevated); color: var(--text);
  border: 1px solid var(--border); border-radius: var(--radius-sm);
  padding: 7px 9px; font-size: 13px;
}
.url { flex: 1; min-width: 0; max-width: 460px; }
.mono { font-family: var(--font-mono); word-break: break-all; }
.reveal { display: flex; flex-direction: column; gap: 8px; }
.reveal code { color: var(--text); font-size: 13px; }
.actions { display: flex; gap: 6px; }
.banner-err {
  background: var(--fill-danger); border: 1px solid var(--danger);
  border-radius: var(--radius-sm); padding: 10px 14px;
  color: var(--danger); font-size: 13px; margin: 10px 0;
}
.banner-ok {
  background: var(--fill-muted); border: 1px solid var(--ok);
  border-radius: var(--radius-sm); padding: 10px 14px; font-size: 13px; margin: 10px 0;
}
.tokens { width: 100%; border-collapse: collapse; font-size: 13px; }
.tokens th { text-align: left; color: var(--text-muted); font-weight: 500; padding: 6px 4px; }
.tokens td { padding: 6px 4px; border-top: 1px solid var(--border); }
.right { text-align: right; white-space: nowrap; }
.btn-primary {
  background: var(--accent); color: var(--text-on-accent);
  border: none; border-radius: var(--radius-sm);
  padding: 7px 13px; font-size: 13px; cursor: pointer;
}
.btn-primary:hover { background: var(--accent-hover); }
.btn-ghost {
  background: transparent; color: var(--text);
  border: 1px solid var(--border); border-radius: var(--radius-sm);
  padding: 4px 10px; font-size: 12px; cursor: pointer;
}
.btn-danger-sm {
  background: transparent; color: var(--danger);
  border: 1px solid var(--danger); border-radius: var(--radius-sm);
  padding: 3px 10px; cursor: pointer; font-size: 12px; margin-left: 4px;
}
.btn-danger-sm:hover { background: var(--fill-danger); }
</style>
