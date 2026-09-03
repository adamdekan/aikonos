<script setup>
// LLM provider catalog admin. The create
// flow is a type-first wizard — family → connection details → models — because
// every field below the family (endpoint shape, api-version, region, probe
// route) is family-dependent, and a flat form made the admin guess which of
// them applied. Editing skips step 1: the family is stored and immutable.
import { ref, computed, onMounted } from "vue";
import {
  listLlmProviders,
  upsertLlmProvider,
  deleteLlmProvider,
  setDefaultProviderFor,
  testLlmProvider,
} from "../../api/admin.js";
import { toMicros, fromMicros } from "../../lib/money.js";
import Icon from "../../components/Icon.vue";
import DataTable from "../../components/ui/DataTable.vue";
import EmptyState from "../../components/ui/EmptyState.vue";
import Modal from "../../components/ui/Modal.vue";
import FormField from "../../components/ui/FormField.vue";
import ToggleSwitch from "../../components/ui/ToggleSwitch.vue";
import { useToast } from "../../components/ui/useToast.js";

const { push: toast } = useToast();

// FAMILIES drives step 1's cards, the endpoint placeholder, and the locked
// label shown when editing. `api` is the stored wire value — deliberately not
// renamed (openai-completions covers every OpenAI-compatible server).
const FAMILIES = [
  {
    api: "openai-completions",
    label: "OpenAI-compatible",
    desc: "OpenAI, Groq, Together, Fireworks, DeepSeek, xAI, Mistral, OpenRouter, and self-hosted Ollama/vLLM (enter any placeholder key for keyless servers).",
    endpoint: "https://api.openai.com/v1",
  },
  {
    api: "anthropic-messages",
    label: "Anthropic",
    desc: "Claude models; used natively by vision/reason; interactive chat runs on OpenAI-compatible wire.",
    endpoint: "https://api.anthropic.com",
  },
  {
    api: "azure-openai",
    label: "Azure OpenAI",
    desc: "deployment-name routing; requires API version.",
    endpoint: "https://<resource>.openai.azure.com",
  },
  {
    api: "google-gemini",
    label: "Google Gemini",
    desc: "via the OpenAI-compatible endpoint; note: legacy API keys are rejected from Sep 2026 — use service-account-bound keys.",
    endpoint: "https://generativelanguage.googleapis.com/v1beta/openai",
  },
  {
    api: "aws-bedrock",
    label: "AWS Bedrock",
    desc: "bearer API key via the OpenAI-compatible endpoint; region-scoped.",
    endpoint: "https://bedrock-runtime.<region>.amazonaws.com/openai/v1",
  },
];

const MODES = [
  "chat",
  "embedding",
  "image_generation",
  "audio_speech",
  "audio_transcription",
  "rerank",
  "ocr",
  "moderation",
];

const UNITS = [
  "per_mtok",
  "per_image",
  "per_megapixel",
  "per_mchar",
  "per_minute",
  "per_second",
  "per_page",
  "per_1k_queries",
  "per_request",
  "free",
];

// The unit an admin almost always wants for a given modality. Only a starting
// point — the unit select stays free (a per-page OCR model and a per-request
// one both exist).
const UNIT_FOR_MODE = {
  chat: "per_mtok",
  embedding: "per_mtok",
  image_generation: "per_image",
  audio_speech: "per_mchar",
  audio_transcription: "per_minute",
  rerank: "per_1k_queries",
  ocr: "per_page",
  moderation: "free",
};

// Capabilities a default can be pinned to. chat/vision/fallback always render;
// a modality row appears only once some provider could serve it.
const ALWAYS_CAPABILITIES = ["chat", "vision", "fallback"];
const MODALITY_CAPABILITIES = [
  "embedding",
  "image_generation",
  "audio_speech",
  "audio_transcription",
  "rerank",
  "ocr",
];

const CAPABILITY_LABELS = {
  chat: "Chat",
  vision: "Vision",
  fallback: "Fallback",
  embedding: "Embedding",
  image_generation: "Image generation",
  audio_speech: "Text to speech",
  audio_transcription: "Transcription",
  rerank: "Rerank",
  ocr: "OCR",
};

const providers = ref([]);
const defaults  = ref({});
const forbidden = ref(false);
const loading   = ref(false);
const error     = ref("");

// wizard state — editId non-null = edit mode; step drives both body and footer
const showModal  = ref(false);
const step       = ref("type"); // "type" | "details" | "models"
const editId     = ref(null);
const editHasKey = ref(false);
const fId        = ref("");
const fName      = ref("");
const fEndpoint  = ref("");
const fApi       = ref("");
const fApiVersion = ref("");
const fRegion    = ref("");
const fEnabled   = ref(true);
const fVisionCapable = ref(false);
// Flat per-provider fallback pricing (per 1M tokens), used by EmitLlmUsage when
// a call reports no cost of its own and its model carries no pricing block.
const fPriceIn   = ref(0);
const fPriceOut  = ref(0);
const fModels    = ref([]);
const fApiKey    = ref("");
const formError  = ref("");
// Last endpoint value this form wrote from the region field — while the
// endpoint still matches it (or is blank), region edits keep re-deriving it.
const autofilled = ref("");

// Test-connection state. testResult: null = untested, else { ok, message }.
const testing    = ref(false);
const testResult = ref(null);
const testMode   = ref("");

const family = computed(() => FAMILIES.find((f) => f.api === fApi.value) ?? null);

// Editing opens at "details" with the family locked, so there is nothing behind
// it to go back to.
const hasBack = computed(() => step.value === "models" || (step.value === "details" && !editId.value));

function emptyPricing() {
  return { unit: "per_mtok", in: 0, out: 0, cacheRead: 0, cacheWrite: 0, tiers: [] };
}

// A model row: id/mode/maxTokens/contextWindow have inputs, `priced` gates the
// pricing sub-form, and the four price* floats are legacy dead fields carried
// through untouched so a save never wipes what an earlier release stored.
function emptyModel() {
  return {
    id: "",
    mode: "chat",
    maxTokens: 0,
    contextWindow: 0,
    priced: false,
    pricing: emptyPricing(),
    priceIn: 0,
    priceOut: 0,
    priceCacheRead: 0,
    priceCacheWrite: 0,
  };
}

async function load() {
  loading.value   = true;
  error.value     = "";
  forbidden.value = false;
  try {
    const data = await listLlmProviders();
    if (data.forbidden) { forbidden.value = true; return; }
    providers.value = data.providers ?? [];
    defaults.value  = data.defaults ?? {};
  } catch (e) {
    error.value = e.message;
  } finally {
    loading.value = false;
  }
}

onMounted(load);

function resetForm() {
  editHasKey.value = false;
  fId.value       = "";
  fName.value     = "";
  fEndpoint.value = "";
  fApiVersion.value = "";
  fRegion.value   = "";
  fEnabled.value  = true;
  fVisionCapable.value = false;
  fPriceIn.value  = 0;
  fPriceOut.value = 0;
  fModels.value   = [emptyModel()];
  fApiKey.value   = "";
  formError.value = "";
  autofilled.value = "";
  testResult.value = null;
  testMode.value  = "";
}

function openCreate() {
  editId.value = null;
  fApi.value   = "";
  resetForm();
  step.value = "type";
  showModal.value = true;
}

function openEdit(p) {
  editId.value     = p.id;
  fApi.value       = p.api ?? "openai-completions";
  resetForm();
  editHasKey.value = p.hasKey ?? p.has_key ?? false;
  fId.value        = p.id;
  fName.value      = p.name;
  fEndpoint.value  = p.endpoint;
  fApiVersion.value = p.apiVersion ?? p.api_version ?? "";
  fRegion.value    = (p.config ?? {}).region ?? "";
  fEnabled.value   = p.enabled ?? true;
  fVisionCapable.value = p.visionCapable ?? p.vision_capable ?? false;
  fPriceIn.value   = fromMicros(p.priceInMicrosPerMtok  ?? p.price_in_micros_per_mtok  ?? 0);
  fPriceOut.value  = fromMicros(p.priceOutMicrosPerMtok ?? p.price_out_micros_per_mtok ?? 0);
  fModels.value    = (p.models ?? []).map(modelToForm);
  if (fModels.value.length === 0) fModels.value = [emptyModel()];
  step.value = "details";
  showModal.value = true;
}

function modelToForm(m) {
  const pr = m.pricing ?? null;
  return {
    id:            m.id ?? "",
    mode:          m.mode || "chat",
    maxTokens:     m.maxTokens ?? m.max_tokens ?? 0,
    contextWindow: m.contextWindow ?? m.context_window ?? 0,
    priced:        !!pr,
    pricing: pr
      ? {
          unit:       pr.unit || "per_mtok",
          in:         fromMicros(pr.inMicros         ?? pr.in_micros          ?? 0),
          out:        fromMicros(pr.outMicros        ?? pr.out_micros         ?? 0),
          cacheRead:  fromMicros(pr.cacheReadMicros  ?? pr.cache_read_micros  ?? 0),
          cacheWrite: fromMicros(pr.cacheWriteMicros ?? pr.cache_write_micros ?? 0),
          tiers: (pr.tiers ?? []).map((t) => ({
            minContextTokens: t.minContextTokens ?? t.min_context_tokens ?? 0,
            in:  fromMicros(t.inMicros  ?? t.in_micros  ?? 0),
            out: fromMicros(t.outMicros ?? t.out_micros ?? 0),
          })),
        }
      : emptyPricing(),
    // Carried through untouched — no input exists for these, and rebuilding a
    // model row without them wiped whatever was stored on every save.
    priceIn:         m.priceIn         ?? m.price_in          ?? 0,
    priceOut:        m.priceOut        ?? m.price_out         ?? 0,
    priceCacheRead:  m.priceCacheRead  ?? m.price_cache_read  ?? 0,
    priceCacheWrite: m.priceCacheWrite ?? m.price_cache_write ?? 0,
  };
}

function closeModal() {
  showModal.value = false;
}

function pickFamily(api) {
  fApi.value = api;
  formError.value = "";
  step.value = "details";
}

function addModelRow() {
  fModels.value.push(emptyModel());
}

function removeModelRow(idx) {
  fModels.value.splice(idx, 1);
}

// Switching a model's modality re-seeds its billing unit — an image model
// priced per 1M tokens is almost always a leftover, not an intent.
function onModeChange(m) {
  m.pricing.unit = UNIT_FOR_MODE[m.mode] ?? "per_mtok";
}

// Only per_mtok has a token basis to tier or a cache/output side; every other
// unit bills one amount per unit (the broker reads it from in_micros).
function isPerMTok(m) {
  return m.pricing.unit === "per_mtok";
}

function addTier(m) {
  m.pricing.tiers.push({ minContextTokens: "", in: 0, out: 0 });
}

function removeTier(m, idx) {
  m.pricing.tiers.splice(idx, 1);
}

// A region change re-derives the bedrock endpoint while the admin has not
// typed their own — a bedrock endpoint is mechanical from the region.
function onRegionInput() {
  if (fApi.value !== "aws-bedrock") return;
  if (fEndpoint.value !== "" && fEndpoint.value !== autofilled.value) return;
  const r = fRegion.value.trim();
  const next = r ? `https://bedrock-runtime.${r}.amazonaws.com/openai/v1` : "";
  fEndpoint.value = next;
  autofilled.value = next;
}

function validateDetails() {
  if (!fApi.value)             return "Select a provider type.";
  if (!fId.value.trim())       return "ID is required.";
  if (!fName.value.trim())     return "Name is required.";
  if (!fEndpoint.value.trim()) return "Endpoint is required.";
  if (fApi.value === "azure-openai" && !fApiVersion.value.trim()) {
    return "API Version is required for Azure providers.";
  }
  return "";
}

function validateModels() {
  const rows = fModels.value.filter((m) => m.id.trim());
  if (rows.length === 0) return "At least one model ID is required.";
  for (const m of rows) {
    if (!m.priced) continue;
    const tiers = usedTiers(m);
    let prev = 0;
    for (const t of tiers) {
      const min = Number(t.minContextTokens);
      if (!(min > 0)) return `Model ${m.id.trim()}: tier min context tokens must be greater than 0.`;
      if (min <= prev) return `Model ${m.id.trim()}: pricing tiers must ascend by min context tokens.`;
      prev = min;
    }
  }
  return "";
}

// usedTiers drops rows the admin added but left blank; anything with a value
// entered is validated rather than silently discarded.
function usedTiers(m) {
  if (!isPerMTok(m)) return [];
  return m.pricing.tiers.filter((t) => String(t.minContextTokens).trim() !== "");
}

function nextStep() {
  formError.value = "";
  if (step.value === "type") {
    if (!fApi.value) { formError.value = "Select a provider type."; return; }
    step.value = "details";
    return;
  }
  const err = validateDetails();
  if (err) { formError.value = err; return; }
  step.value = "models";
}

function backStep() {
  formError.value = "";
  step.value = step.value === "models" ? "details" : "type";
}

// buildProvider assembles the wire payload. isDefault*/isFallback/hasKey are
// authoritative server-side, so they are sent as false (the broker preserves
// the real values on update).
function buildProvider() {
  return {
    id:         fId.value.trim(),
    name:       fName.value.trim(),
    endpoint:   fEndpoint.value.trim(),
    api:        fApi.value,
    apiVersion: fApi.value === "azure-openai" ? fApiVersion.value.trim() : "",
    config:     fApi.value === "aws-bedrock" && fRegion.value.trim()
      ? { region: fRegion.value.trim() }
      : {},
    enabled:    fEnabled.value,
    isDefault:  false,
    isDefaultVision: false,
    isFallback: false,
    visionCapable: fVisionCapable.value,
    priceInMicrosPerMtok:  toMicros(fPriceIn.value),
    priceOutMicrosPerMtok: toMicros(fPriceOut.value),
    hasKey:     false,
    models:     fModels.value.filter((m) => m.id.trim()).map(buildModel),
  };
}

function buildModel(m) {
  const out = {
    id:              m.id.trim(),
    mode:            m.mode || "chat",
    maxTokens:       Number(m.maxTokens) || 0,
    contextWindow:   Number(m.contextWindow) || 0,
    priceIn:         Number(m.priceIn)   || 0,
    priceOut:        Number(m.priceOut)  || 0,
    priceCacheRead:  Number(m.priceCacheRead)  || 0,
    priceCacheWrite: Number(m.priceCacheWrite) || 0,
  };
  // No pricing key at all when custom pricing is off — that is what tells the
  // broker to fall back to the provider's flat rates.
  if (!m.priced) return out;
  const perMTok = isPerMTok(m);
  out.pricing = {
    unit:             m.pricing.unit,
    inMicros:         m.pricing.unit === "free" ? 0 : toMicros(m.pricing.in),
    outMicros:        perMTok ? toMicros(m.pricing.out) : 0,
    cacheReadMicros:  perMTok ? toMicros(m.pricing.cacheRead)  : 0,
    cacheWriteMicros: perMTok ? toMicros(m.pricing.cacheWrite) : 0,
    tiers: usedTiers(m).map((t) => ({
      minContextTokens: Number(t.minContextTokens) || 0,
      inMicros:  toMicros(t.in),
      outMicros: toMicros(t.out),
    })),
  };
  return out;
}

async function submit() {
  formError.value = "";
  const err = validateDetails() || validateModels();
  if (err) { formError.value = err; return; }

  try {
    // send "" when blank — broker leaves the stored key unchanged
    await upsertLlmProvider(buildProvider(), fApiKey.value);
    toast("ok", editId.value ? "Provider updated." : "Provider created.");
    closeModal();
    await load();
  } catch (e) {
    formError.value = e.message;
  }
}

// Distinct modalities across the entered models. One mode = probe it silently;
// several = the admin picks which one Test connection exercises.
const testModes = computed(() => {
  const seen = [];
  for (const m of fModels.value) {
    if (!m.id.trim()) continue;
    const mode = m.mode || "chat";
    if (!seen.includes(mode)) seen.push(mode);
  }
  return seen;
});

// Always resolves to a mode the current models actually declare, so removing a
// model can never leave the select (or the probe) pointing at a stale one.
const testModeSel = computed({
  get: () => (testModes.value.includes(testMode.value) ? testMode.value : testModes.value[0] ?? ""),
  set: (v) => { testMode.value = v; },
});

async function testConnection() {
  formError.value = "";
  testResult.value = null;
  const err = validateDetails() || validateModels();
  if (err) { formError.value = err; return; }

  testing.value = true;
  try {
    const res = await testLlmProvider(buildProvider(), fApiKey.value, testModeSel.value);
    testResult.value = res.ok
      ? { ok: true, message: `Connection succeeded (${res.latencyMs ?? 0} ms).` }
      : { ok: false, message: res.error || "Connection failed." };
  } catch (e) {
    testResult.value = { ok: false, message: e.message };
  } finally {
    testing.value = false;
  }
}

async function remove(p) {
  error.value = "";
  try {
    await deleteLlmProvider(p.id);
    toast("ok", "Provider deleted.");
    await load();
  } catch (e) {
    error.value = e.message;
  }
}

// ── Defaults panel ───────────────────────────────────────────────────────────

// eligible mirrors the broker's capabilityEligible so the select does not offer
// a pick that fails closed server-side. UX only — the broker re-checks.
function eligible(capability, p) {
  if (capability === "vision") return !!(p.visionCapable ?? p.vision_capable);
  const models = p.models ?? [];
  if (capability === "chat" || capability === "fallback") {
    return (p.enabled ?? true) && models.length > 0;
  }
  return models.some((m) => (m.mode || "chat") === capability);
}

function optionsFor(capability) {
  const current = defaults.value[capability] ?? "";
  return providers.value.filter((p) => eligible(capability, p) || p.id === current);
}

const shownCapabilities = computed(() => [
  ...ALWAYS_CAPABILITIES,
  ...MODALITY_CAPABILITIES.filter((c) => providers.value.some((p) => eligible(c, p))),
]);

async function onDefaultChange(capability, providerId) {
  const prev = defaults.value[capability] ?? "";
  if (providerId === prev) return;
  const clear = providerId === "";
  const id = clear ? prev : providerId;
  if (!id) return;
  error.value = "";
  try {
    await setDefaultProviderFor(id, capability, clear);
    toast("ok", clear
      ? `${CAPABILITY_LABELS[capability]} default cleared.`
      : `${CAPABILITY_LABELS[capability]} default set to ${id}.`);
  } catch (e) {
    error.value = e.message;
  }
  await load();
}

function modelSummary(p) {
  const ms = p.models ?? [];
  if (ms.length === 0) return "—";
  return ms.map((m) => m.id).join(", ");
}

// unpriced flags a provider that would accrue 0 spend forever: with no flat
// per-token rate and no per-model pricing block, a usage event carrying no cost
// of its own contributes nothing, silently defeating spend caps.
function unpriced(row) {
  const in_ = Number(row.priceInMicrosPerMtok  ?? row.price_in_micros_per_mtok  ?? 0);
  const out = Number(row.priceOutMicrosPerMtok ?? row.price_out_micros_per_mtok ?? 0);
  if (in_ > 0 || out > 0) return false;
  return !(row.models ?? []).some((m) => m.pricing);
}

function familyLabel(api) {
  return FAMILIES.find((f) => f.api === api)?.label ?? api ?? "—";
}

const TABLE_COLS = [
  { key: "name",     label: "Name" },
  { key: "_family",  label: "Type",    width: "150px" },
  { key: "endpoint", label: "Endpoint" },
  { key: "_models",  label: "Models" },
  { key: "_enabled", label: "Enabled", width: "70px" },
  { key: "_haskey",  label: "Key",     width: "50px" },
  { key: "_actions", label: "",        width: "190px" },
];
</script>

<template>
  <div class="view">
    <div class="view-header">
      <Icon name="cpu" class="view-icon" />
      <h1>LLM Providers</h1>
    </div>

    <EmptyState
      v-if="forbidden"
      data-testid="forbidden"
      icon="settings"
      message="You are not a tenant admin."
    />

    <template v-else>
      <div v-if="error" data-testid="error-banner" class="banner-err">{{ error }}</div>

      <section v-if="providers.length" class="defaults-card" data-testid="defaults-panel">
        <h2>Defaults</h2>
        <p class="muted small">
          Which provider serves each capability. Options are limited to providers that can serve it.
        </p>
        <div class="defaults-grid">
          <label v-for="cap in shownCapabilities" :key="cap" class="defaults-row">
            <span class="defaults-label">{{ CAPABILITY_LABELS[cap] }}</span>
            <select
              :value="defaults[cap] ?? ''"
              :data-testid="`defaults-select-${cap}`"
              @change="onDefaultChange(cap, $event.target.value)"
            >
              <option value="">—</option>
              <option v-for="p in optionsFor(cap)" :key="p.id" :value="p.id">{{ p.name }}</option>
            </select>
          </label>
        </div>
      </section>

      <div class="toolbar">
        <button class="btn-primary" data-testid="provider-create-btn" @click="openCreate">
          <Icon name="plus" /> New Provider
        </button>
      </div>

      <DataTable
        :columns="TABLE_COLS"
        :rows="providers"
        :loading="loading"
        empty-text="No providers configured."
        :row-attrs="{ 'data-testid': 'provider-row' }"
      >
        <template #row="{ row }">
          <td class="mono">
            {{ row.name }}
            <span
              v-if="unpriced(row)"
              class="badge b-warn"
              :data-testid="`unpriced-${row.id}`"
            >unpriced — spend not tracked</span>
          </td>
          <td class="muted small">{{ familyLabel(row.api) }}</td>
          <td class="mono small">{{ row.endpoint }}</td>
          <td class="muted small">{{ modelSummary(row) }}</td>
          <td class="center">
            <span class="badge" :class="row.enabled ? 'b-ok' : 'b-off'">
              {{ row.enabled ? "yes" : "no" }}
            </span>
          </td>
          <td class="center">
            <span :class="(row.hasKey ?? row.has_key) ? 'key-yes' : 'key-no'">
              {{ (row.hasKey ?? row.has_key) ? "✓" : "✗" }}
            </span>
          </td>
          <td class="right actions">
            <button
              :data-testid="`provider-edit-${row.id}`"
              class="btn-secondary-sm"
              @click="openEdit(row)"
            >
              Edit
            </button>
            <button
              :data-testid="`provider-delete-${row.id}`"
              class="btn-danger-sm"
              @click="remove(row)"
            >
              <Icon name="trash" /> Delete
            </button>
          </td>
        </template>
      </DataTable>
    </template>

    <!-- Create wizard / Edit modal -->
    <Modal
      :open="showModal"
      :title="editId ? 'Edit Provider' : 'New Provider'"
      size="wide"
      @close="closeModal"
    >
      <div data-testid="provider-modal">
        <!-- Step 1: family -->
        <template v-if="step === 'type'">
          <p class="muted small step-intro">
            Pick the provider type. It decides the endpoint shape, the required fields, and how
            connection tests probe the server — and cannot be changed later.
          </p>
          <div class="family-list">
            <button
              v-for="f in FAMILIES"
              :key="f.api"
              type="button"
              class="family-card"
              :class="{ picked: fApi === f.api }"
              :data-testid="`family-${f.api}`"
              @click="pickFamily(f.api)"
            >
              <span class="family-name">{{ f.label }}</span>
              <span class="muted small">{{ f.desc }}</span>
            </button>
          </div>
        </template>

        <!-- Step 2: connection details -->
        <template v-else-if="step === 'details'">
          <FormField label="Type">
            <p class="locked-family" data-testid="family-locked">
              <strong>{{ family?.label ?? fApi }}</strong>
              <span class="muted small">{{ family?.desc }}</span>
            </p>
          </FormField>

          <FormField label="ID">
            <input
              v-model="fId"
              placeholder="openrouter"
              data-testid="provider-id"
              :disabled="!!editId"
            />
          </FormField>

          <FormField label="Name">
            <input v-model="fName" placeholder="OpenRouter" data-testid="provider-name" />
          </FormField>

          <FormField v-if="fApi === 'aws-bedrock'" label="Region">
            <input
              v-model="fRegion"
              placeholder="us-east-1"
              data-testid="provider-region"
              @input="onRegionInput"
            />
            <p class="muted small hint">
              Stored as the provider's <code>region</code> config and used to derive the
              endpoint below.
            </p>
          </FormField>

          <FormField label="Endpoint">
            <input
              v-model="fEndpoint"
              :placeholder="family?.endpoint ?? 'https://api.openai.com/v1'"
              data-testid="provider-endpoint"
            />
          </FormField>

          <FormField v-if="fApi === 'azure-openai'" label="API Version">
            <input
              v-model="fApiVersion"
              placeholder="2024-10-21"
              data-testid="provider-api-version"
            />
            <p class="muted small hint">
              Required for Azure. Each model ID is the Azure
              <strong>deployment name</strong>; requests go to
              <code>{endpoint}/openai/deployments/&lt;deployment&gt;/chat/completions?api-version=&lt;version&gt;</code>.
            </p>
          </FormField>

          <FormField label="API Key">
            <input
              v-model="fApiKey"
              type="password"
              :placeholder="editHasKey ? '•••• set — leave blank to keep' : 'Enter API key'"
              data-testid="provider-apikey"
              autocomplete="new-password"
            />
          </FormField>

          <FormField label="Enabled">
            <label class="checkbox-item">
              <ToggleSwitch v-model="fEnabled" data-testid="provider-enabled" aria-label="Enabled" />
              Enabled
            </label>
            <label class="checkbox-item">
              <ToggleSwitch
                v-model="fVisionCapable"
                data-testid="provider-vision-capable"
                aria-label="Vision capable"
              />
              Vision capable
            </label>
          </FormField>

          <FormField label="Fallback pricing (per 1M tokens)">
            <div class="pricing-row">
              <input
                v-model="fPriceIn"
                type="number"
                step="any"
                min="0"
                placeholder="input per 1M"
                data-testid="provider-price-in"
              />
              <input
                v-model="fPriceOut"
                type="number"
                step="any"
                min="0"
                placeholder="output per 1M"
                data-testid="provider-price-out"
              />
            </div>
            <p class="muted small hint">
              Used when a call reports no cost of its own and its model carries no pricing of its
              own. 0 = unpriced — tokens still recorded, cost 0.
            </p>
          </FormField>
        </template>

        <!-- Step 3: models -->
        <template v-else>
          <p class="muted small step-intro">
            Each model declares what it does (<em>mode</em>) and, optionally, its own pricing.
            Without per-model pricing, calls fall back to the provider rates from the previous step.
          </p>
          <div class="models-repeater">
            <div v-for="(m, idx) in fModels" :key="idx" class="model-card">
              <div class="model-row">
                <input
                  v-model="m.id"
                  placeholder="model-id"
                  class="model-id"
                  :data-testid="`model-id-${idx}`"
                />
                <select
                  v-model="m.mode"
                  class="model-mode"
                  :data-testid="`model-mode-${idx}`"
                  @change="onModeChange(m)"
                >
                  <option v-for="mode in MODES" :key="mode" :value="mode">{{ mode }}</option>
                </select>
                <button
                  v-if="fModels.length > 1"
                  class="btn-danger-sm row-remove"
                  type="button"
                  :data-testid="`model-remove-${idx}`"
                  @click="removeModelRow(idx)"
                >
                  <Icon name="trash" />
                </button>
              </div>
              <div class="model-row">
                <input
                  v-model="m.maxTokens"
                  type="number"
                  step="1"
                  min="0"
                  placeholder="max out tokens"
                  title="Max output tokens. 0 uses the server default. Raise it if a workflow reason step halts on a truncated response."
                  :data-testid="`model-max-tokens-${idx}`"
                />
                <input
                  v-model="m.contextWindow"
                  type="number"
                  step="1"
                  min="0"
                  placeholder="context window"
                  title="Total context window in tokens. 0 = unknown."
                  :data-testid="`model-context-window-${idx}`"
                />
              </div>
              <label class="checkbox-item">
                <ToggleSwitch
                  v-model="m.priced"
                  :data-testid="`model-pricing-toggle-${idx}`"
                  aria-label="Custom pricing"
                />
                Custom pricing
              </label>

              <div v-if="m.priced" class="pricing-box">
                <select v-model="m.pricing.unit" :data-testid="`pricing-unit-${idx}`">
                  <option v-for="u in UNITS" :key="u" :value="u">{{ u }}</option>
                </select>

                <template v-if="isPerMTok(m)">
                  <div class="pricing-row">
                    <input
                      v-model="m.pricing.in"
                      type="number" step="any" min="0" placeholder="input per 1M"
                      :data-testid="`pricing-in-${idx}`"
                    />
                    <input
                      v-model="m.pricing.out"
                      type="number" step="any" min="0" placeholder="output per 1M"
                      :data-testid="`pricing-out-${idx}`"
                    />
                  </div>
                  <div class="pricing-row">
                    <input
                      v-model="m.pricing.cacheRead"
                      type="number" step="any" min="0" placeholder="cache read per 1M"
                      :data-testid="`pricing-cache-read-${idx}`"
                    />
                    <input
                      v-model="m.pricing.cacheWrite"
                      type="number" step="any" min="0" placeholder="cache write per 1M"
                      :data-testid="`pricing-cache-write-${idx}`"
                    />
                  </div>
                  <div
                    v-for="(t, tIdx) in m.pricing.tiers"
                    :key="tIdx"
                    class="pricing-row"
                  >
                    <input
                      v-model="t.minContextTokens"
                      type="number" step="1" min="1" placeholder="from context tokens"
                      :data-testid="`tier-min-${idx}-${tIdx}`"
                    />
                    <input
                      v-model="t.in"
                      type="number" step="any" min="0" placeholder="input per 1M"
                      :data-testid="`tier-in-${idx}-${tIdx}`"
                    />
                    <input
                      v-model="t.out"
                      type="number" step="any" min="0" placeholder="output per 1M"
                      :data-testid="`tier-out-${idx}-${tIdx}`"
                    />
                    <button
                      class="btn-danger-sm row-remove"
                      type="button"
                      :data-testid="`tier-remove-${idx}-${tIdx}`"
                      @click="removeTier(m, tIdx)"
                    >
                      <Icon name="trash" />
                    </button>
                  </div>
                  <button
                    class="btn-secondary-sm"
                    type="button"
                    :data-testid="`tier-add-${idx}`"
                    @click="addTier(m)"
                  >
                    <Icon name="plus" /> Add context tier
                  </button>
                </template>

                <input
                  v-else-if="m.pricing.unit !== 'free'"
                  v-model="m.pricing.in"
                  type="number" step="any" min="0"
                  :placeholder="`price ${m.pricing.unit.replace('per_', 'per ')}`"
                  :data-testid="`pricing-in-${idx}`"
                />
                <p v-else class="muted small">Free — no charge is recorded for this model.</p>
              </div>
            </div>
            <button
              class="btn-secondary-sm"
              type="button"
              data-testid="model-add-btn"
              @click="addModelRow"
            >
              <Icon name="plus" /> Add model
            </button>
          </div>

          <FormField v-if="testModes.length > 1" label="Test as">
            <select v-model="testModeSel" data-testid="test-mode-select">
              <option v-for="mode in testModes" :key="mode" :value="mode">{{ mode }}</option>
            </select>
            <p class="muted small hint">
              Which modality Test connection probes — these models do not all serve the same one.
            </p>
          </FormField>
        </template>

        <div v-if="formError" class="field-error" data-testid="provider-form-error">{{ formError }}</div>
        <div
          v-if="testResult"
          class="test-result"
          :class="testResult.ok ? 'test-ok' : 'test-err'"
          data-testid="provider-test-result"
        >
          <Icon :name="testResult.ok ? 'check' : 'close'" />
          {{ testResult.message }}
        </div>
      </div>

      <template #footer>
        <button class="btn-ghost" type="button" @click="closeModal">Cancel</button>
        <button
          v-if="hasBack"
          class="btn-ghost"
          type="button"
          data-testid="wizard-back-btn"
          @click="backStep"
        >
          Back
        </button>

        <template v-if="step === 'models'">
          <button
            class="btn-secondary"
            type="button"
            data-testid="provider-test-btn"
            :disabled="testing"
            @click="testConnection"
          >
            {{ testing ? "Testing…" : "Test connection" }}
          </button>
          <button class="btn-primary" data-testid="provider-save-btn" @click="submit">
            {{ editId ? "Save" : "Create" }}
          </button>
        </template>
        <button
          v-else
          class="btn-primary"
          type="button"
          data-testid="wizard-next-btn"
          :disabled="step === 'type' && !fApi"
          @click="nextStep"
        >
          Next
        </button>
      </template>
    </Modal>
  </div>
</template>

<style scoped>
.view { padding: 24px 32px; max-width: 1000px; }
.view-header { display: flex; align-items: center; gap: 10px; margin-bottom: 24px; }
.view-header h1 { margin: 0; font-size: 20px; font-weight: 600; }
.view-icon { color: var(--text-muted); width: 22px; height: 22px; }

.toolbar { margin-bottom: 16px; }

.banner-err {
  background: var(--fill-danger); border: 1px solid var(--danger);
  border-radius: var(--radius-sm); padding: 10px 14px;
  color: var(--danger); font-size: 13px; margin-bottom: 16px;
}

.muted { color: var(--text-muted); font-size: 13px; }
.small { font-size: 12px; }

.defaults-card {
  border: 1px solid var(--border); border-radius: var(--radius-sm);
  padding: 14px 16px; margin-bottom: 20px;
}
.defaults-card h2 { margin: 0 0 4px; font-size: 14px; font-weight: 600; }
.defaults-card p { margin: 0 0 12px; }
.defaults-grid {
  display: grid; gap: 10px;
  grid-template-columns: repeat(auto-fill, minmax(230px, 1fr));
}
.defaults-row { display: flex; flex-direction: column; gap: 4px; }
.defaults-label { font-size: 12px; color: var(--text-muted); font-weight: 500; }

.btn-primary {
  display: inline-flex; align-items: center; gap: 4px;
  background: var(--accent); color: var(--text-on-accent);
  border: none; border-radius: var(--radius-sm);
  padding: 7px 13px; font-size: 13px; cursor: pointer;
}
.btn-primary:hover { background: var(--accent-hover); }
.btn-primary:disabled { opacity: 0.5; cursor: default; }

.btn-ghost {
  background: transparent; color: var(--text); border: 1px solid var(--border);
  border-radius: var(--radius-sm); padding: 7px 13px; font-size: 13px; cursor: pointer;
}
.btn-ghost:hover { background: var(--bg-hover); }

.btn-secondary {
  display: inline-flex; align-items: center; gap: 4px;
  background: transparent; color: var(--text); border: 1px solid var(--border);
  border-radius: var(--radius-sm); padding: 7px 13px; font-size: 13px; cursor: pointer;
}
.btn-secondary:hover { background: var(--bg-hover); }
.btn-secondary:disabled { opacity: 0.6; cursor: default; }

.btn-secondary-sm {
  display: inline-flex; align-items: center; gap: 4px;
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
.center { text-align: center; }
.right { text-align: right; }
.actions { display: flex; gap: 6px; justify-content: flex-end; }

.badge {
  border-radius: var(--radius-sm); padding: 1px 7px;
  font-size: 12px; border: 1px solid transparent;
}
.b-ok  { background: var(--fill-muted); color: var(--ok);   border-color: var(--ok); }
.b-off { background: var(--fill-muted); color: var(--text-muted); border-color: var(--border); }
.b-warn { background: var(--fill-danger); color: var(--danger); border-color: var(--danger); white-space: nowrap; }

.key-yes { color: var(--ok);   font-weight: 600; font-size: 13px; }
.key-no  { color: var(--danger); font-weight: 600; font-size: 13px; }

select, input:not([type="checkbox"]):not([type="radio"]) {
  width: 100%; box-sizing: border-box;
  background: var(--bg); color: var(--text);
  border: 1px solid var(--border); border-radius: var(--radius-sm);
  padding: 7px 9px; font-size: 13px;
}

.checkbox-item {
  display: flex; align-items: center; gap: 8px;
  font-size: 13px; color: var(--text); cursor: pointer;
}

.field-error { color: var(--danger); font-size: 12px; margin-top: 8px; }

.hint { margin: 6px 0 0; line-height: 1.4; }
.hint code { font-family: var(--font-mono); font-size: 11px; word-break: break-all; }

.step-intro { margin: 0 0 14px; line-height: 1.5; }

.family-list { display: flex; flex-direction: column; gap: 8px; }
.family-card {
  display: flex; flex-direction: column; gap: 3px;
  text-align: left; cursor: pointer;
  background: transparent; color: var(--text);
  border: 1px solid var(--border); border-radius: var(--radius-sm);
  padding: 10px 12px;
}
.family-card:hover { background: var(--bg-hover); }
.family-card.picked { border-color: var(--accent); }
.family-name { font-size: 13px; font-weight: 600; }

.locked-family { display: flex; flex-direction: column; gap: 3px; margin: 0; font-size: 13px; }

.test-result {
  display: flex; align-items: center; gap: 6px;
  margin-top: 10px; padding: 8px 10px;
  border-radius: var(--radius-sm); font-size: 12px;
  border: 1px solid transparent; word-break: break-word;
}
.test-ok  { background: var(--fill-muted); color: var(--ok);     border-color: var(--ok); }
.test-err { background: var(--fill-danger); color: var(--danger); border-color: var(--danger); }

.pricing-row { display: flex; gap: 6px; }
.pricing-row input { flex: 1; }

.models-repeater { display: flex; flex-direction: column; gap: 10px; }

.model-card {
  display: flex; flex-direction: column; gap: 6px;
  border: 1px solid var(--border); border-radius: var(--radius-sm);
  padding: 10px 12px;
}

.model-row { display: flex; gap: 6px; align-items: center; }
.model-row input, .model-row select { flex: 1; }
.model-id   { flex: 2 !important; }
.model-mode { flex: 1 !important; min-width: 130px; }

.pricing-box {
  display: flex; flex-direction: column; gap: 6px;
  border-top: 1px dashed var(--border); padding-top: 8px;
}

.row-remove { flex: 0 0 auto; padding: 3px 7px; }
</style>
