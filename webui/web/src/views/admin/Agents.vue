<script setup>
import { ref, computed, nextTick, watch, onMounted } from "vue";
import {
  listAgents,
  createAgent,
  updateAgent,
  deleteAgent,
  assignRole,
  revokeRole,
  listMcpConnections,
  mintAgentApiKey,
  listAgentApiKeys,
  revokeAgentApiKey,
  listLlmProviders,
  listSkills,
} from "../../api/admin.js";
import { getAgentSoul, setAgentSoul } from "../../api/agents.js";
import { subjectRef } from "../../sections.js";
import Icon from "../../components/Icon.vue";
import DataTable from "../../components/ui/DataTable.vue";
import EmptyState from "../../components/ui/EmptyState.vue";
import Modal from "../../components/ui/Modal.vue";
import FormField from "../../components/ui/FormField.vue";
import Spinner from "../../components/ui/Spinner.vue";
import { useToast } from "../../components/ui/useToast.js";
import FormSection from "./FormSection.vue";
import CheckList from "./CheckList.vue";
import ToggleSwitch from "../../components/ui/ToggleSwitch.vue";
import ByteCounter from "./ByteCounter.vue";

const { push: toast } = useToast();

const agents           = ref([]);
const mcpOptions       = ref([]);
const providerOptions  = ref([]);
const skillOptions     = ref([]);
const forbidden        = ref(false);
const loading          = ref(false);
const error            = ref("");

// modal state — editId non-null = edit mode
const showModal            = ref(false);
const editId               = ref(null);
const fName                = ref("");
// "" = inherit the tenant default; otherwise "<providerId>::<modelId>".
const fModelPair           = ref("");
const fApproval            = ref("needs_approval");
const fSkills              = ref([]);
const fMcpServers          = ref([]);
const fAllowedProviders    = ref([]);
const fSoul                = ref("");
const soulBytes            = computed(() => new Blob([fSoul.value]).size);
const fGatewayEnabled      = ref(false);
const formError            = ref("");
const nameError            = computed(() =>
  formError.value === "Name is required." ? formError.value : ""
);
const saving               = ref(false);

// assign-to state (per-agent inline)
const assignSubject = ref("");
const assignError   = ref("");

// API keys state (shown in edit modal for the open agent)
const apiKeys       = ref([]);
const keysLoading   = ref(false);
const keysError     = ref("");
const mintLabel     = ref("");
const newRawKey     = ref("");   // shown once after minting; cleared on close

// autofocus ref
const nameInputRef  = ref(null);

// section id 4 = AGENT_RELATION (mirrors sections.js id for agents)
const AGENT_RELATION = 4;

// Derived item lists for CheckList
const skillItems    = computed(() =>
  skillOptions.value.map(s => ({ id: s.toolId, label: s.displayName || s.toolId }))
);
const mcpItems      = computed(() =>
  mcpOptions.value.map(m => ({ id: m.id, label: m.name }))
);

// Model and provider are one choice, not two. The gateway reads an agent's
// preferred provider only inside the branch that also requires a model
// (llm/provider-fallback.ts), so a provider set on its own is inert, and a model
// binds only when the provider serving it lists that model. The provider is
// therefore a property of the chosen model rather than a separate decision, and
// every option carries the pair. Encoding the pair (rather than looking the
// provider up from the model id) is also what keeps the same model id offered by
// two providers unambiguous.
const MODEL_PAIR_SEP = "::";

function modelPair(providerId, modelId) {
  return `${providerId}${MODEL_PAIR_SEP}${modelId}`;
}

// Splits on the first separator only: provider ids are slugs, but model ids
// legitimately contain "/" and ":" (e.g. "anthropic/claude-sonnet-4.6").
function splitModelPair(pair) {
  const at = pair.indexOf(MODEL_PAIR_SEP);
  if (at < 0) return { providerId: "", modelId: "" };
  return { providerId: pair.slice(0, at), modelId: pair.slice(at + MODEL_PAIR_SEP.length) };
}

const modelGroups = computed(() =>
  providerOptions.value
    .filter(p => p.enabled !== false)
    .map(p => ({
      providerId: p.id,
      providerName: p.name || p.id,
      models: (p.models ?? [])
        .map(m => m.id)
        .filter(Boolean)
        .map(id => ({ id, pair: modelPair(p.id, id) })),
    }))
    .filter(g => g.models.length > 0)
);

// A stored pair no enabled provider offers — kept as a selectable option so
// opening the modal cannot silently blank an agent's existing configuration.
const orphanPair = computed(() => {
  const pair = fModelPair.value;
  if (!pair) return null;
  if (modelGroups.value.some(g => g.models.some(m => m.pair === pair))) return null;
  return { pair, ...splitModelPair(pair) };
});

// With the provider carried by the selection, a model can no longer be paired
// with a provider that does not offer it. The only remaining way to hold an
// inert model is a stored one that has since disappeared from every provider.
const modelWarning = computed(() => {
  const orphan = orphanPair.value;
  if (!orphan) return "";
  return `No enabled provider offers ${orphan.modelId}, so it will be ignored until one does.`;
});

// A model stored without a provider predates this control. Bind it to a provider
// that actually offers it — tenant default first — so the pairing becomes
// explicit and takes effect; before, it worked only when the tenant default
// happened to list it, and was silently ignored otherwise.
function providerForStoredModel(modelId) {
  const owners = modelGroups.value.filter(g => g.models.some(m => m.id === modelId));
  if (owners.length === 0) return "";
  const dflt = providerOptions.value.find(p => p.isDefault ?? p.is_default);
  const onDefault = dflt && owners.find(g => g.providerId === dflt.id);
  return (onDefault ?? owners[0]).providerId;
}

async function load() {
  loading.value   = true;
  error.value     = "";
  forbidden.value = false;
  try {
    const [agData, mcpData, provData, skillData] = await Promise.all([
      listAgents(),
      listMcpConnections(),
      listLlmProviders().catch(() => ({ providers: [] })),
      listSkills().catch(() => ({ skills: [] })),
    ]);
    if (agData.forbidden) { forbidden.value = true; return; }
    agents.value          = agData.agents       ?? [];
    mcpOptions.value      = mcpData.connections ?? [];
    providerOptions.value = provData.providers  ?? [];
    skillOptions.value    = (skillData.skills ?? [])
      .filter(s => s.enabled !== false && !s.toolId.startsWith("mcp:"))
      .sort((a, b) => a.toolId.localeCompare(b.toolId))
      .map(s => ({ toolId: s.toolId, displayName: s.displayName }));
  } catch (e) {
    error.value = e.message;
  } finally {
    loading.value = false;
  }
}

onMounted(load);

function openCreate() {
  editId.value             = null;
  fName.value              = "";
  fModelPair.value         = "";
  fApproval.value          = "needs_approval";
  fSkills.value            = [];
  fMcpServers.value        = [];
  fAllowedProviders.value  = [];
  fSoul.value              = "";
  fGatewayEnabled.value    = false;
  formError.value          = "";
  assignSubject.value      = "";
  assignError.value        = "";
  apiKeys.value            = [];
  keysError.value          = "";
  mintLabel.value          = "";
  newRawKey.value          = "";
  showModal.value          = true;
}

async function openEdit(agent) {
  const targetId           = agent.id;
  editId.value             = targetId;
  fName.value              = agent.name;
  fApproval.value          = agent.approvalMode ?? agent.approval_mode ?? "needs_approval";
  fSkills.value            = [...(agent.skills ?? [])];
  fMcpServers.value        = [...(agent.mcpServers ?? agent.mcp_servers ?? [])];
  fAllowedProviders.value  = [...(agent.allowedProviders ?? agent.allowed_providers ?? [])];
  {
    const storedModel    = agent.llmModel ?? agent.llm_model ?? "";
    const storedProvider = agent.preferredProvider ?? agent.preferred_provider ?? "";
    fModelPair.value = storedModel
      ? modelPair(storedProvider || providerForStoredModel(storedModel), storedModel)
      : "";
  }
  fSoul.value              = "";
  fGatewayEnabled.value    = agent.gatewayEnabled ?? agent.gateway_enabled ?? false;
  formError.value          = "";
  assignSubject.value      = "";
  assignError.value        = "";
  apiKeys.value            = [];
  keysError.value          = "";
  mintLabel.value          = "";
  newRawKey.value          = "";
  showModal.value          = true;
  loadKeys(targetId);
  const r = await getAgentSoul(targetId);
  // guard: a newer openEdit call may have already set editId to a different agent
  if (editId.value !== targetId) return;
  fSoul.value = r.forbidden ? "" : (r.soul ?? "");
}

// Autofocus the name input when modal opens
watch(showModal, (val) => {
  if (val) nextTick(() => nameInputRef.value?.focus());
});

function closeModal() {
  showModal.value = false;
  newRawKey.value = "";
}

function toggleSkill(id) {
  const idx = fSkills.value.indexOf(id);
  if (idx >= 0) fSkills.value.splice(idx, 1);
  else fSkills.value.push(id);
}

function toggleMcp(id) {
  const idx = fMcpServers.value.indexOf(id);
  if (idx >= 0) fMcpServers.value.splice(idx, 1);
  else fMcpServers.value.push(id);
}

async function loadKeys(agentId) {
  keysLoading.value = true;
  keysError.value   = "";
  try {
    const data     = await listAgentApiKeys(agentId);
    apiKeys.value  = data.keys ?? [];
  } catch (e) {
    keysError.value = e.message;
  } finally {
    keysLoading.value = false;
  }
}

async function mintKey(agentId) {
  keysError.value = "";
  newRawKey.value = "";
  try {
    const data      = await mintAgentApiKey(agentId, mintLabel.value.trim());
    newRawKey.value = data.rawKey ?? "";
    mintLabel.value = "";
    toast("ok", "API key minted — copy it now.");
    await loadKeys(agentId);
  } catch (e) {
    keysError.value = e.message;
  }
}

async function revokeKey(agentId, keyId) {
  keysError.value = "";
  newRawKey.value = "";
  try {
    await revokeAgentApiKey(agentId, keyId);
    toast("ok", "API key revoked.");
    await loadKeys(agentId);
  } catch (e) {
    keysError.value = e.message;
  }
}

async function submit() {
  formError.value = "";
  if (!fName.value.trim()) { formError.value = "Name is required."; return; }
  if (soulBytes.value > 4096) {
    formError.value = `Personality exceeds 4096 bytes (${soulBytes.value}).`;
    return;
  }
  // One selection supplies both wire fields. Blank = inherit, which clears any
  // stored preferred provider — that combination was inert anyway, so dropping it
  // removes a value that looked like a setting without being one.
  const { providerId, modelId } = splitModelPair(fModelPair.value);
  const body = {
    name:              fName.value.trim(),
    llmModel:          modelId,
    approvalMode:      fApproval.value,
    skills:            fSkills.value,
    mcpServers:        fMcpServers.value,
    allowedProviders:  fAllowedProviders.value,
    preferredProvider: providerId,
    gatewayEnabled:    fGatewayEnabled.value,
  };
  saving.value = true;
  try {
    if (editId.value) {
      await updateAgent(editId.value, body);
      await setAgentSoul(editId.value, fSoul.value);
      toast("ok", "Agent updated.");
      closeModal();
    } else {
      const { agent: newAgent } = await createAgent(body);
      if (fSoul.value.trim()) {
        try {
          await setAgentSoul(newAgent.id, fSoul.value);
        } catch (soulErr) {
          formError.value = `Agent created but personality failed to save: ${soulErr.message}`;
          await load();
          return;
        }
      }
      toast("ok", "Agent created.");
      closeModal();
    }
    await load();
  } catch (e) {
    formError.value = e.message;
  } finally {
    saving.value = false;
  }
}

async function remove(agent) {
  error.value = "";
  try {
    await deleteAgent(agent.id);
    toast("ok", "Agent deleted.");
    await load();
  } catch (e) {
    error.value = e.message;
  }
}

async function assign(agent) {
  assignError.value = "";
  const raw = assignSubject.value.trim();
  if (!raw) { assignError.value = "Subject is required."; return; }
  try {
    await assignRole({
      user:     subjectRef(raw),
      relation: "usable_by",
      object:   `agent:${agent.id}`,
      section:  AGENT_RELATION,
    });
    toast("ok", "Assignment saved.");
    assignSubject.value = "";
    await load();
  } catch (e) {
    assignError.value = e.message;
  }
}

async function revoke(tuple) {
  error.value = "";
  try {
    await revokeRole(tuple);
    toast("ok", "Assignment revoked.");
    await load();
  } catch (e) {
    error.value = e.message;
  }
}

function assignedTo(agent) {
  return agent.usable_by ?? agent.usableBy ?? [];
}

const TABLE_COLS = [
  { key: "name",         label: "Name" },
  { key: "llm_model",    label: "Model" },
  { key: "approval_mode",label: "Approval" },
  { key: "_skills",      label: "Skills" },
  { key: "_mcp",         label: "MCP" },
  { key: "_assigned",    label: "Assigned to" },
  { key: "_actions",     label: "",            width: "160px" },
];
</script>

<template>
  <div class="view">
    <div class="view-header">
      <Icon name="bot" class="view-icon" />
      <h1>Agents</h1>
    </div>

    <EmptyState
      v-if="forbidden"
      data-testid="forbidden"
      icon="admin"
      message="You are not a tenant admin."
    />

    <template v-else>
      <div v-if="error" data-testid="error-banner" class="banner-err">{{ error }}</div>

      <div class="toolbar">
        <button class="btn-primary" data-testid="agent-create-btn" @click="openCreate">
          <Icon name="plus" /> New Agent
        </button>
      </div>

      <DataTable
        :columns="TABLE_COLS"
        :rows="agents"
        :loading="loading"
        empty-text="No agents yet."
        :row-attrs="{ 'data-testid': 'agent-row' }"
      >
        <template #row="{ row }">
          <td class="mono">{{ row.name }}</td>
          <td class="muted">{{ row.llm_model || row.llmModel || "default" }}</td>
          <td>
            <span class="badge">{{ row.approval_mode || row.approvalMode || "needs_approval" }}</span>
            <span v-if="row.gatewayEnabled || row.gateway_enabled" class="badge badge-ext" data-testid="gateway-enabled-badge">ext</span>
          </td>
          <td>{{ (row.skills ?? []).length }}</td>
          <td>{{ (row.mcp_servers ?? row.mcpServers ?? []).length }}</td>
          <td>{{ assignedTo(row).length }}</td>
          <td class="right actions">
            <button
              :data-testid="`agent-edit-${row.id}`"
              class="btn-secondary-sm"
              @click="openEdit(row)"
            >
              Edit
            </button>
            <button
              :data-testid="`agent-delete-${row.id}`"
              class="btn-danger-sm"
              data-testid-base="agent-delete-btn"
              @click="remove(row)"
            >
              <Icon name="trash" /> Delete
            </button>
          </td>
        </template>
      </DataTable>
    </template>

    <!-- Create / Edit modal -->
    <Modal
      :open="showModal"
      size="wide"
      :title="editId ? 'Edit Agent' : 'New Agent'"
      @close="closeModal"
    >
      <div data-testid="agent-modal" class="agent-modal-body">

        <!-- General formError banner (non-name errors shown here) -->
        <div
          v-if="formError && formError !== 'Name is required.'"
          class="banner-err banner-err--modal"
          role="alert"
        >
          {{ formError }}
        </div>

        <!-- ── Section 1: Identity ── -->
        <FormSection title="Identity" icon="bot">
          <FormField label="Name" :error="nameError" :required="true">
            <input
              ref="nameInputRef"
              v-model="fName"
              placeholder="My Agent"
              data-testid="agent-name"
              aria-required="true"
            />
          </FormField>

          <FormField
            label="Model"
            hint="Grouped by the provider that serves it — choosing a model also pins its provider. Leave blank to inherit the tenant default."
          >
            <select v-model="fModelPair" data-testid="agent-model">
              <option value="">— inherit default —</option>
              <option v-if="orphanPair" :value="orphanPair.pair">
                {{ orphanPair.modelId }} (no longer offered)
              </option>
              <optgroup
                v-for="g in modelGroups"
                :key="g.providerId"
                :label="g.providerName"
              >
                <option
                  v-for="m in g.models"
                  :key="m.pair"
                  :value="m.pair"
                >
                  {{ m.id }}
                </option>
              </optgroup>
            </select>
            <p
              v-if="modelWarning"
              class="muted small model-warning"
              data-testid="agent-model-warning"
            >
              {{ modelWarning }}
            </p>
          </FormField>
        </FormSection>

        <!-- ── Section 2: Capabilities ── -->

        <FormSection title="Capabilities" icon="tool">
          <FormField label="Skills">
            <CheckList
              :items="skillItems"
              :selected="fSkills"
              testid-prefix="skill-"
              :searchable="true"
              empty-text="No skills configured."
              @toggle="toggleSkill"
            />
          </FormField>

          <FormField label="MCP servers">
            <CheckList
              :items="mcpItems"
              :selected="fMcpServers"
              testid-prefix="mcp-"
              :searchable="true"
              empty-text="No MCP connections configured."
              @toggle="toggleMcp"
            />
          </FormField>
        </FormSection>

        <!-- ── Section 3: Behavior ── -->
        <FormSection title="Behavior" icon="settings">
          <FormField label="Approval mode">
            <select v-model="fApproval" data-testid="agent-approval">
              <option value="needs_approval">needs_approval</option>
              <option value="auto">auto</option>
            </select>
          </FormField>

          <FormField label="External API access">
            <label class="checkbox-item">
              <ToggleSwitch
                v-model="fGatewayEnabled"
                data-testid="agent-gateway-enabled"
                aria-label="Allow invocation via external :8090 surface"
              />
              Allow invocation via external :8090 surface (gateway_enabled)
            </label>
          </FormField>

          <FormField
            label="Personality"
            :error="soulBytes > 4096 ? `Exceeds 4096 bytes (${soulBytes})` : ''"
          >
            <textarea
              v-model="fSoul"
              data-testid="agent-soul"
              rows="8"
              style="font-family: var(--font-mono); resize: vertical;"
              placeholder="Optional freeform personality / system-prompt appendix (markdown)."
            />
            <ByteCounter :value="soulBytes" :max="4096" />
          </FormField>
        </FormSection>

        <!-- ── Section 4: Access (edit only) ── -->
        <FormSection v-if="editId" title="Access" icon="users">
          <FormField label="Assign to (user:email or group:name)">
            <div class="assign-row">
              <input
                v-model="assignSubject"
                placeholder="user:alice@example.com"
                data-testid="assign-subject"
              />
              <button
                class="btn-secondary-sm"
                data-testid="assign-btn"
                @click="assign(agents.find(a => a.id === editId))"
              >
                Assign
              </button>
            </div>
            <div v-if="assignError" class="field-error">{{ assignError }}</div>
          </FormField>

          <div class="assignments">
            <div
              v-for="subj in assignedTo(agents.find(a => a.id === editId) ?? {})"
              :key="subj"
              class="assignment-row"
            >
              <span class="mono">{{ subj }}</span>
              <button
                class="btn-danger-sm"
                data-testid="revoke-assignment-btn"
                @click="revoke({ user: subj, relation: 'usable_by', object: `agent:${editId}`, section: AGENT_RELATION })"
              >
                <Icon name="trash" /> Revoke
              </button>
            </div>
          </div>
        </FormSection>

        <!-- ── Section 5: API keys (edit only) ── -->
        <FormSection v-if="editId" title="API keys" icon="code">
          <!-- Raw key shown once after minting -->
          <div v-if="newRawKey" data-testid="new-raw-key" class="banner-key">
            <span class="key-note">Copy it now — it will not be shown again.</span>
            <div class="key-value mono">{{ newRawKey }}</div>
            <button
              class="btn-secondary-sm"
              data-testid="copy-key-btn"
              @click="navigator.clipboard.writeText(newRawKey)"
            >
              Copy
            </button>
          </div>

          <!-- Existing keys list -->
          <div v-if="keysError" class="field-error">{{ keysError }}</div>
          <div v-if="keysLoading" class="muted">Loading keys…</div>
          <div
            v-for="key in apiKeys"
            :key="key.id"
            class="assignment-row"
            data-testid="api-key-row"
          >
            <div class="key-info">
              <span class="mono key-prefix">{{ key.keyPrefix }}…</span>
              <span v-if="key.label" class="badge key-label">{{ key.label }}</span>
            </div>
            <button
              class="btn-danger-sm"
              :data-testid="`revoke-key-${key.id}`"
              @click="revokeKey(editId, key.id)"
            >
              <Icon name="trash" /> Revoke
            </button>
          </div>
          <div v-if="!keysLoading && apiKeys.length === 0" class="muted">
            No API keys yet.
          </div>

          <!-- Mint a new key -->
          <div class="mint-row">
            <input
              v-model="mintLabel"
              placeholder="Label (optional)"
              data-testid="mint-key-label"
              class="mint-label-input"
            />
            <button
              class="btn-secondary-sm"
              data-testid="mint-key-btn"
              @click="mintKey(editId)"
            >
              Mint key
            </button>
          </div>
        </FormSection>

      </div>

      <template #footer>
        <button class="btn-ghost" @click="closeModal">Cancel</button>
        <button
          class="btn-primary"
          data-testid="agent-save-btn"
          :disabled="soulBytes > 4096 || saving"
          @click="submit"
        >
          <Spinner v-if="saving" size="sm" />
          {{ editId ? "Save" : "Create" }}
        </button>
      </template>
    </Modal>
  </div>
</template>

<style scoped>
.view { padding: 24px 32px; max-width: 1100px; }
.view-header { display: flex; align-items: center; gap: 10px; margin-bottom: 24px; }
.view-header h1 { margin: 0; font-size: 20px; font-weight: 600; }
.view-icon { color: var(--text-muted); width: 22px; height: 22px; }

.toolbar { margin-bottom: 16px; }

.banner-err {
  background: var(--fill-danger); border: 1px solid var(--danger);
  border-radius: var(--radius-sm); padding: 10px 14px;
  color: var(--danger); font-size: 13px; margin-bottom: 16px;
}
.banner-err--modal {
  margin-bottom: var(--space-4);
}

.muted { color: var(--text-muted); font-size: 13px; }

.btn-primary {
  display: inline-flex; align-items: center; gap: var(--space-1);
  background: var(--accent); color: var(--text-on-accent);
  border: none; border-radius: var(--radius-sm);
  padding: 7px 13px; font-size: 13px; cursor: pointer;
}
.btn-primary:hover { background: var(--accent-hover); }
.btn-primary:disabled { opacity: 0.6; cursor: not-allowed; }

.btn-ghost {
  background: transparent; color: var(--text); border: 1px solid var(--border);
  border-radius: var(--radius-sm); padding: 7px 13px; font-size: 13px; cursor: pointer;
}
.btn-ghost:hover { background: var(--bg-hover); }

.btn-secondary-sm {
  background: transparent; color: var(--text-muted); border: 1px solid var(--border);
  border-radius: var(--radius-sm); padding: 3px 10px; cursor: pointer; font-size: 12px;
}
.btn-secondary-sm:hover { background: var(--bg-hover); color: var(--text); }

.btn-danger-sm {
  display: inline-flex; align-items: center; gap: 4px;
  background: transparent; color: var(--danger);
  border: 1px solid var(--danger); border-radius: var(--radius-sm);
  padding: 3px 10px; cursor: pointer; font-size: 12px; opacity: 0.8;
}
.btn-danger-sm:hover { background: var(--fill-danger); opacity: 1; }

.mono { font-family: var(--font-mono); word-break: break-all; }
.right { text-align: right; }
.actions { display: flex; gap: 6px; justify-content: flex-end; }

.badge {
  border-radius: var(--radius-sm); padding: 1px 7px;
  font-size: 12px; background: var(--fill-muted);
  color: var(--text-muted); border: 1px solid var(--border);
}
.badge-ext {
  background: var(--fill-accent, var(--fill-muted));
  color: var(--accent, var(--text-muted));
  border-color: var(--accent, var(--border));
  margin-left: 4px;
}

.checkbox-item {
  display: flex; align-items: center; gap: var(--space-2);
  font-size: 13px; color: var(--text); cursor: pointer;
}

.assign-row {
  display: flex; gap: var(--space-2); align-items: center;
}
.assign-row input {
  flex: 1;
  background: var(--bg); color: var(--text);
  border: 1px solid var(--border); border-radius: var(--radius-sm);
  padding: 7px 9px; font-size: 13px; font-family: var(--font-mono);
}

.field-error { color: var(--danger); font-size: 12px; margin-top: 4px; }

.assignments { display: flex; flex-direction: column; gap: 6px; }
.assignment-row {
  display: flex; align-items: center; justify-content: space-between;
  background: var(--bg-elevated); border: 1px solid var(--border);
  border-radius: var(--radius-sm); padding: 6px 10px;
}

select {
  width: 100%; box-sizing: border-box;
  background: var(--bg); color: var(--text);
  border: 1px solid var(--border); border-radius: var(--radius-sm);
  padding: 7px 9px; font-size: 13px;
}

/* Modal body section stacking */
.agent-modal-body {
  display: flex;
  flex-direction: column;
  gap: var(--space-6);
}

.banner-key {
  background: var(--fill-muted); border: 1px solid var(--border);
  border-radius: var(--radius-sm); padding: 10px 12px;
  display: flex; flex-direction: column; gap: 6px;
}
.key-note { font-size: 12px; color: var(--danger); }
.key-value {
  word-break: break-all; font-size: 12px;
  background: var(--bg); padding: 6px 8px; border-radius: var(--radius-sm);
  border: 1px solid var(--border); user-select: all;
}

.key-info { display: flex; align-items: center; gap: var(--space-2); flex: 1; }
.key-prefix { font-size: 12px; }
.key-label { font-size: 11px; }

.mint-row {
  display: flex; gap: var(--space-2); align-items: center;
}
.mint-label-input {
  flex: 1;
  background: var(--bg); color: var(--text);
  border: 1px solid var(--border); border-radius: var(--radius-sm);
  padding: 5px 9px; font-size: 13px;
}

textarea {
  width: 100%; box-sizing: border-box;
  background: var(--bg); color: var(--text);
  border: 1px solid var(--border); border-radius: var(--radius-sm);
  padding: 7px 9px; font-size: 13px;
}

/* Identity name input — matches the dark select/textarea fields. Targeted by
   testid so the flex-row assign/mint inputs keep their own sizing. */
.agent-modal-body input[data-testid="agent-name"] {
  width: 100%; box-sizing: border-box;
  background: var(--bg); color: var(--text);
  border: 1px solid var(--border); border-radius: var(--radius-sm);
  padding: 7px 9px; font-size: 13px;
}
.agent-modal-body input[data-testid="agent-name"]:focus {
  outline: none; border-color: var(--accent);
}

.model-warning { margin: 6px 0 0; line-height: 1.4; }
</style>
