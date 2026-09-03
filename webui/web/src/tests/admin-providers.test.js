// Tests for views/admin/Providers.vue — the type-first provider wizard
//: step flow, per-family fields, per-model
// mode + unit-discriminated pricing, the defaults panel, and mode-aware tests.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import { createRouter, createMemoryHistory } from "vue-router";
import { useUserStore } from "../store/user.js";

vi.mock("../api/admin.js", () => ({
  listLlmProviders:       vi.fn(),
  upsertLlmProvider:      vi.fn(),
  deleteLlmProvider:      vi.fn(),
  setDefaultProviderFor:  vi.fn(),
  testLlmProvider:        vi.fn(),
}));

import Providers from "../views/admin/Providers.vue";
import * as adminApi from "../api/admin.js";
import { useToast } from "../components/ui/useToast.js";

function makeRouter() {
  return createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: "/admin/providers", component: Providers },
      { path: "/", component: { template: "<div/>" } },
    ],
  });
}

async function mountView() {
  const router = makeRouter();
  await router.push("/admin/providers");
  const w = mount(Providers, { global: { plugins: [router] }, attachTo: document.body });
  await flushPromises();
  return w;
}

// The modal is teleported to <body>, so its fields are queried via the DOM.
function q(testid) {
  return document.querySelector(`[data-testid='${testid}']`);
}

function setField(testid, value, evt = "input") {
  const el = q(testid);
  el.value = value;
  el.dispatchEvent(new Event(evt));
}

async function click(testid) {
  q(testid).click();
  await flushPromises();
}

// openWizard walks create → family pick → step "details".
async function openWizard(w, api = "openai-completions") {
  await w.find("[data-testid='provider-create-btn']").trigger("click");
  await flushPromises();
  await click(`family-${api}`);
}

function fillDetails({ id = "p", name = "P", endpoint = "https://a/v1" } = {}) {
  setField("provider-id", id);
  setField("provider-name", name);
  setField("provider-endpoint", endpoint);
}

// toModels advances from "details" to the models step and names one model.
async function toModels(modelId = "gpt-4o") {
  await click("wizard-next-btn");
  setField("model-id-0", modelId);
  await flushPromises();
}

function setupPinia() {
  setActivePinia(createPinia());
  useUserStore().setFromProfile({ email: "admin@example.com", sub: "admin@example.com" });
  vi.clearAllMocks();
  const { toasts } = useToast();
  toasts.splice(0);
}

function teardown() {
  document.body.innerHTML = "";
  vi.restoreAllMocks();
}

describe("Providers.vue — wizard steps", () => {
  beforeEach(setupPinia);
  afterEach(teardown);

  it("create opens at the family step and shows all five families", async () => {
    adminApi.listLlmProviders.mockResolvedValue({ providers: [] });
    const w = await mountView();
    await w.find("[data-testid='provider-create-btn']").trigger("click");
    await flushPromises();

    for (const api of [
      "openai-completions",
      "anthropic-messages",
      "azure-openai",
      "google-gemini",
      "aws-bedrock",
    ]) {
      expect(q(`family-${api}`), api).not.toBeNull();
    }
    // Connection fields belong to the next step.
    expect(q("provider-id")).toBeNull();
    // Next is inert until a family is picked.
    expect(q("wizard-next-btn").disabled).toBe(true);
  });

  it("picking a family advances to details and seeds that family's endpoint placeholder", async () => {
    adminApi.listLlmProviders.mockResolvedValue({ providers: [] });
    const w = await mountView();
    await openWizard(w, "google-gemini");

    expect(q("provider-id")).not.toBeNull();
    expect(q("provider-endpoint").placeholder).toBe(
      "https://generativelanguage.googleapis.com/v1beta/openai",
    );
    expect(q("family-locked").textContent).toContain("Google Gemini");
  });

  it("details → Next reaches the models step; Back returns to details", async () => {
    adminApi.listLlmProviders.mockResolvedValue({ providers: [] });
    const w = await mountView();
    await openWizard(w);
    fillDetails();

    await click("wizard-next-btn");
    expect(q("model-id-0")).not.toBeNull();
    expect(q("provider-save-btn")).not.toBeNull();

    await click("wizard-back-btn");
    expect(q("provider-id")).not.toBeNull();
    expect(q("model-id-0")).toBeNull();
  });

  it("blocks the details step until id/name/endpoint are filled", async () => {
    adminApi.listLlmProviders.mockResolvedValue({ providers: [] });
    const w = await mountView();
    await openWizard(w);

    await click("wizard-next-btn");
    expect(q("model-id-0")).toBeNull();
    expect(q("provider-form-error").textContent).toContain("ID is required");
  });

  it("edit opens at details with the family locked and the id frozen", async () => {
    adminApi.listLlmProviders.mockResolvedValue({
      providers: [{ id: "az", name: "Az", endpoint: "https://r.openai.azure.com", api: "azure-openai", apiVersion: "2024-10-21", enabled: true }],
    });
    const w = await mountView();
    await w.find("[data-testid='provider-edit-az']").trigger("click");
    await flushPromises();

    expect(q("provider-id").value).toBe("az");
    expect(q("provider-id").disabled).toBe(true);
    expect(q("family-locked").textContent).toContain("Azure OpenAI");
    // No family step behind the details step in edit mode.
    expect(q("wizard-back-btn")).toBeNull();
    expect(q("family-azure-openai")).toBeNull();
  });
});

describe("Providers.vue — Azure", () => {
  beforeEach(setupPinia);
  afterEach(teardown);

  it("shows the API Version field only for the azure family", async () => {
    adminApi.listLlmProviders.mockResolvedValue({ providers: [] });
    const w = await mountView();
    await openWizard(w, "openai-completions");
    expect(q("provider-api-version")).toBeNull();

    await click("wizard-back-btn");
    await click("family-azure-openai");
    expect(q("provider-api-version")).not.toBeNull();
  });

  it("blocks the details step for azure-openai when api_version is empty", async () => {
    adminApi.listLlmProviders.mockResolvedValue({ providers: [] });
    const w = await mountView();
    await openWizard(w, "azure-openai");
    fillDetails({ id: "azure-foundry", name: "Azure Foundry", endpoint: "https://r.openai.azure.com" });

    await click("wizard-next-btn");

    expect(q("model-id-0")).toBeNull();
    expect(q("provider-form-error").textContent).toContain("API Version is required");
  });

  it("sends apiVersion in the upsert payload for azure-openai", async () => {
    adminApi.listLlmProviders.mockResolvedValue({ providers: [] });
    adminApi.upsertLlmProvider.mockResolvedValue({});
    const w = await mountView();
    await openWizard(w, "azure-openai");
    fillDetails({ id: "azure-foundry", name: "Azure Foundry", endpoint: "https://r.openai.azure.com" });
    setField("provider-api-version", "2024-08-01-preview");
    await toModels("gpt-4o-deployment");

    await click("provider-save-btn");

    expect(adminApi.upsertLlmProvider).toHaveBeenCalledTimes(1);
    const [provider] = adminApi.upsertLlmProvider.mock.calls[0];
    expect(provider.api).toBe("azure-openai");
    expect(provider.apiVersion).toBe("2024-08-01-preview");
  });
});

describe("Providers.vue — Bedrock region", () => {
  beforeEach(setupPinia);
  afterEach(teardown);

  it("autofills the endpoint from the region and sends it as config.region", async () => {
    adminApi.listLlmProviders.mockResolvedValue({ providers: [] });
    adminApi.upsertLlmProvider.mockResolvedValue({});
    const w = await mountView();
    await openWizard(w, "aws-bedrock");

    setField("provider-id", "bedrock");
    setField("provider-name", "Bedrock");
    setField("provider-region", "us-east-1");
    await flushPromises();
    expect(q("provider-endpoint").value).toBe("https://bedrock-runtime.us-east-1.amazonaws.com/openai/v1");

    await toModels("anthropic.claude-sonnet-4");
    await click("provider-save-btn");

    const [provider] = adminApi.upsertLlmProvider.mock.calls[0];
    expect(provider.config).toEqual({ region: "us-east-1" });
    expect(provider.endpoint).toBe("https://bedrock-runtime.us-east-1.amazonaws.com/openai/v1");
  });

  it("leaves an endpoint the admin typed alone when the region changes", async () => {
    adminApi.listLlmProviders.mockResolvedValue({ providers: [] });
    const w = await mountView();
    await openWizard(w, "aws-bedrock");

    setField("provider-endpoint", "https://my-proxy.internal/openai/v1");
    setField("provider-region", "eu-central-1");
    await flushPromises();

    expect(q("provider-endpoint").value).toBe("https://my-proxy.internal/openai/v1");
  });

  it("sends an empty config for a family with no config keys", async () => {
    adminApi.listLlmProviders.mockResolvedValue({ providers: [] });
    adminApi.upsertLlmProvider.mockResolvedValue({});
    const w = await mountView();
    await openWizard(w, "openai-completions");
    fillDetails();
    await toModels();

    await click("provider-save-btn");

    const [provider] = adminApi.upsertLlmProvider.mock.calls[0];
    expect(provider.config).toEqual({});
  });
});

describe("Providers.vue — per-model mode and pricing", () => {
  beforeEach(setupPinia);
  afterEach(teardown);

  it("defaults a model to chat mode and omits pricing when the toggle is off", async () => {
    adminApi.listLlmProviders.mockResolvedValue({ providers: [] });
    adminApi.upsertLlmProvider.mockResolvedValue({});
    const w = await mountView();
    await openWizard(w);
    fillDetails();
    await toModels();

    expect(q("pricing-unit-0")).toBeNull();
    await click("provider-save-btn");

    const [provider] = adminApi.upsertLlmProvider.mock.calls[0];
    expect(provider.models[0].mode).toBe("chat");
    expect(provider.models[0].pricing).toBeUndefined();
  });

  it("preselects the unit from the model's mode", async () => {
    adminApi.listLlmProviders.mockResolvedValue({ providers: [] });
    const w = await mountView();
    await openWizard(w);
    fillDetails();
    await toModels("dall-e-3");

    setField("model-mode-0", "image_generation", "change");
    await flushPromises();
    await click("model-pricing-toggle-0");

    expect(q("pricing-unit-0").value).toBe("per_image");
    // A single per-unit amount — no output/cache side for a non-token unit.
    expect(q("pricing-out-0")).toBeNull();
    expect(q("pricing-cache-read-0")).toBeNull();
  });

  it("sends a per_mtok pricing object with micro-unit amounts", async () => {
    adminApi.listLlmProviders.mockResolvedValue({ providers: [] });
    adminApi.upsertLlmProvider.mockResolvedValue({});
    const w = await mountView();
    await openWizard(w);
    fillDetails();
    await toModels();

    await click("model-pricing-toggle-0");
    setField("pricing-in-0", "2.5");
    setField("pricing-out-0", "10");
    setField("pricing-cache-read-0", "0.25");
    setField("pricing-cache-write-0", "3.125");

    await click("provider-save-btn");

    const [provider] = adminApi.upsertLlmProvider.mock.calls[0];
    expect(provider.models[0].pricing).toEqual({
      unit: "per_mtok",
      inMicros: 2_500_000,
      outMicros: 10_000_000,
      cacheReadMicros: 250_000,
      cacheWriteMicros: 3_125_000,
      tiers: [],
    });
  });

  it("sends a single-amount pricing object for a non-token unit", async () => {
    adminApi.listLlmProviders.mockResolvedValue({ providers: [] });
    adminApi.upsertLlmProvider.mockResolvedValue({});
    const w = await mountView();
    await openWizard(w);
    fillDetails();
    await toModels("dall-e-3");

    setField("model-mode-0", "image_generation", "change");
    await flushPromises();
    await click("model-pricing-toggle-0");
    setField("pricing-in-0", "0.04");

    await click("provider-save-btn");

    const [provider] = adminApi.upsertLlmProvider.mock.calls[0];
    expect(provider.models[0].mode).toBe("image_generation");
    expect(provider.models[0].pricing).toEqual({
      unit: "per_image",
      inMicros: 40_000,
      outMicros: 0,
      cacheReadMicros: 0,
      cacheWriteMicros: 0,
      tiers: [],
    });
  });

  it("sends a free-unit pricing object with no amounts and no inputs", async () => {
    adminApi.listLlmProviders.mockResolvedValue({ providers: [] });
    adminApi.upsertLlmProvider.mockResolvedValue({});
    const w = await mountView();
    await openWizard(w);
    fillDetails();
    await toModels("omni-moderation");

    setField("model-mode-0", "moderation", "change");
    await flushPromises();
    await click("model-pricing-toggle-0");
    expect(q("pricing-unit-0").value).toBe("free");
    expect(q("pricing-in-0")).toBeNull();

    await click("provider-save-btn");

    const [provider] = adminApi.upsertLlmProvider.mock.calls[0];
    expect(provider.models[0].pricing.unit).toBe("free");
    expect(provider.models[0].pricing.inMicros).toBe(0);
  });

  it("adds context tiers and converts them to micro units", async () => {
    adminApi.listLlmProviders.mockResolvedValue({ providers: [] });
    adminApi.upsertLlmProvider.mockResolvedValue({});
    const w = await mountView();
    await openWizard(w);
    fillDetails();
    await toModels();

    await click("model-pricing-toggle-0");
    await click("tier-add-0");
    setField("tier-min-0-0", "200000");
    setField("tier-in-0-0", "5");
    setField("tier-out-0-0", "20");

    await click("provider-save-btn");

    const [provider] = adminApi.upsertLlmProvider.mock.calls[0];
    expect(provider.models[0].pricing.tiers).toEqual([
      { minContextTokens: 200000, inMicros: 5_000_000, outMicros: 20_000_000 },
    ]);
  });

  it("rejects tiers that do not ascend by min context tokens", async () => {
    adminApi.listLlmProviders.mockResolvedValue({ providers: [] });
    const w = await mountView();
    await openWizard(w);
    fillDetails();
    await toModels();

    await click("model-pricing-toggle-0");
    await click("tier-add-0");
    await click("tier-add-0");
    setField("tier-min-0-0", "200000");
    setField("tier-min-0-1", "100000");

    await click("provider-save-btn");

    expect(adminApi.upsertLlmProvider).not.toHaveBeenCalled();
    expect(q("provider-form-error").textContent).toContain("ascend");
  });

  it("blocks save when no model id was entered", async () => {
    adminApi.listLlmProviders.mockResolvedValue({ providers: [] });
    const w = await mountView();
    await openWizard(w);
    fillDetails();
    await click("wizard-next-btn");

    await click("provider-save-btn");

    expect(adminApi.upsertLlmProvider).not.toHaveBeenCalled();
    expect(q("provider-form-error").textContent).toContain("At least one model ID");
  });
});

describe("Providers.vue — provider fallback pricing", () => {
  beforeEach(setupPinia);
  afterEach(teardown);

  it("converts major-unit fallback pricing inputs to micro-unit ints on submit", async () => {
    adminApi.listLlmProviders.mockResolvedValue({ providers: [] });
    adminApi.upsertLlmProvider.mockResolvedValue({});
    const w = await mountView();
    await openWizard(w);
    fillDetails();
    setField("provider-price-in", "3.50");
    setField("provider-price-out", "15");
    await toModels();

    await click("provider-save-btn");

    const [provider] = adminApi.upsertLlmProvider.mock.calls[0];
    expect(provider.priceInMicrosPerMtok).toBe(3_500_000);
    expect(provider.priceOutMicrosPerMtok).toBe(15_000_000);
  });

  it("defaults unset fallback pricing to 0 (unpriced)", async () => {
    adminApi.listLlmProviders.mockResolvedValue({ providers: [] });
    adminApi.upsertLlmProvider.mockResolvedValue({});
    const w = await mountView();
    await openWizard(w);
    fillDetails();
    await toModels();

    await click("provider-save-btn");

    const [provider] = adminApi.upsertLlmProvider.mock.calls[0];
    expect(provider.priceInMicrosPerMtok).toBe(0);
    expect(provider.priceOutMicrosPerMtok).toBe(0);
  });

  it("edit mode pre-fills fallback pricing converted back to major units", async () => {
    adminApi.listLlmProviders.mockResolvedValue({
      providers: [
        {
          id: "p3", name: "P3", endpoint: "https://c", enabled: true,
          priceInMicrosPerMtok: 3_500_000, priceOutMicrosPerMtok: 15_000_000,
        },
      ],
    });
    const w = await mountView();
    await w.find("[data-testid='provider-edit-p3']").trigger("click");
    await flushPromises();

    expect(q("provider-price-in").value).toBe("3.5");
    expect(q("provider-price-out").value).toBe("15");
  });

  it("sends visionCapable in the upsert payload when toggled on", async () => {
    adminApi.listLlmProviders.mockResolvedValue({ providers: [] });
    adminApi.upsertLlmProvider.mockResolvedValue({});
    const w = await mountView();
    await openWizard(w);
    fillDetails();
    await click("provider-vision-capable");
    await toModels();

    await click("provider-save-btn");

    const [provider] = adminApi.upsertLlmProvider.mock.calls[0];
    expect(provider.visionCapable).toBe(true);
  });
});

describe("Providers.vue — edit round-trip", () => {
  beforeEach(setupPinia);
  afterEach(teardown);

  // Regression: rebuilding a model row from the fields that have inputs zeroed
  // everything else, so opening a provider and pressing Save silently wiped the
  // stored contextWindow and the legacy price floats. Only the fields with
  // inputs may change; the rest must round-trip untouched.
  it("edit + save round-trips model fields, mode, and pricing", async () => {
    adminApi.listLlmProviders.mockResolvedValue({
      providers: [
        {
          id: "p9", name: "P9", endpoint: "https://d", enabled: true, api: "openai-completions",
          models: [{
            id: "gpt-5.6-terra", mode: "chat",
            priceIn: 2.2, priceOut: 13.19,
            maxTokens: 32000, contextWindow: 400000,
            priceCacheRead: 0.25, priceCacheWrite: 2.75,
            pricing: {
              unit: "per_mtok",
              inMicros: 2_200_000, outMicros: 13_190_000,
              cacheReadMicros: 250_000, cacheWriteMicros: 2_750_000,
              tiers: [{ minContextTokens: 200000, inMicros: 4_400_000, outMicros: 26_380_000 }],
            },
          }],
        },
      ],
    });
    adminApi.upsertLlmProvider.mockResolvedValue({});
    const w = await mountView();
    await w.find("[data-testid='provider-edit-p9']").trigger("click");
    await flushPromises();

    // Details step first — advance to models to see the round-tripped row.
    await click("wizard-next-btn");
    expect(q("model-max-tokens-0").value).toBe("32000");
    expect(q("model-context-window-0").value).toBe("400000");
    expect(q("model-mode-0").value).toBe("chat");
    // An existing pricing block opens the sub-form pre-filled in major units.
    expect(q("pricing-unit-0").value).toBe("per_mtok");
    expect(q("pricing-in-0").value).toBe("2.2");
    expect(q("pricing-cache-write-0").value).toBe("2.75");
    expect(q("tier-min-0-0").value).toBe("200000");

    await click("provider-save-btn");

    expect(adminApi.upsertLlmProvider).toHaveBeenCalledTimes(1);
    const [provider] = adminApi.upsertLlmProvider.mock.calls[0];
    expect(provider.models[0]).toMatchObject({
      id: "gpt-5.6-terra",
      mode: "chat",
      maxTokens: 32000,
      contextWindow: 400000,
      // Legacy per-model float fields: dead, carried through so a save that
      // never mentioned them does not zero them.
      priceIn: 2.2,
      priceOut: 13.19,
      priceCacheRead: 0.25,
      priceCacheWrite: 2.75,
      pricing: {
        unit: "per_mtok",
        inMicros: 2_200_000,
        outMicros: 13_190_000,
        cacheReadMicros: 250_000,
        cacheWriteMicros: 2_750_000,
        tiers: [{ minContextTokens: 200000, inMicros: 4_400_000, outMicros: 26_380_000 }],
      },
    });
  });

  it("turning custom pricing off drops the pricing key entirely", async () => {
    adminApi.listLlmProviders.mockResolvedValue({
      providers: [
        {
          id: "p8", name: "P8", endpoint: "https://e", enabled: true,
          models: [{ id: "m1", pricing: { unit: "per_mtok", inMicros: 1_000_000 } }],
        },
      ],
    });
    adminApi.upsertLlmProvider.mockResolvedValue({});
    const w = await mountView();
    await w.find("[data-testid='provider-edit-p8']").trigger("click");
    await flushPromises();
    await click("wizard-next-btn");

    await click("model-pricing-toggle-0");
    await click("provider-save-btn");

    const [provider] = adminApi.upsertLlmProvider.mock.calls[0];
    expect(provider.models[0].pricing).toBeUndefined();
  });
});

describe("Providers.vue — unpriced badge", () => {
  beforeEach(setupPinia);
  afterEach(teardown);

  it("flags only a provider with no flat rate and no per-model pricing", async () => {
    adminApi.listLlmProviders.mockResolvedValue({
      providers: [
        { id: "unp", name: "Unpriced", endpoint: "https://u", enabled: true },
        {
          id: "halfp", name: "Half priced", endpoint: "https://h", enabled: true,
          priceInMicrosPerMtok: 3_500_000, priceOutMicrosPerMtok: 0,
        },
        {
          id: "priced", name: "Priced", endpoint: "https://p", enabled: true,
          price_in_micros_per_mtok: 3_500_000, price_out_micros_per_mtok: 15_000_000,
        },
        // Flat rates 0, but the model carries its own pricing — spend is tracked.
        {
          id: "modelp", name: "Model priced", endpoint: "https://m", enabled: true,
          models: [{ id: "m1", pricing: { unit: "per_mtok", inMicros: 2_500_000 } }],
        },
      ],
    });
    const w = await mountView();

    expect(w.find("[data-testid='unpriced-unp']").exists()).toBe(true);
    expect(w.find("[data-testid='unpriced-unp']").text()).toContain("spend not tracked");
    // Either flat price > 0 is enough to track spend.
    expect(w.find("[data-testid='unpriced-halfp']").exists()).toBe(false);
    expect(w.find("[data-testid='unpriced-priced']").exists()).toBe(false);
    expect(w.find("[data-testid='unpriced-modelp']").exists()).toBe(false);
  });
});

describe("Providers.vue — defaults panel", () => {
  const THREE = [
    { id: "chatty", name: "Chatty", endpoint: "https://a", enabled: true, visionCapable: false, models: [{ id: "m1", mode: "chat" }] },
    { id: "seer",   name: "Seer",   endpoint: "https://b", enabled: true, visionCapable: true,  models: [{ id: "m2", mode: "chat" }] },
    { id: "embed",  name: "Embed",  endpoint: "https://c", enabled: true, visionCapable: false, models: [{ id: "m3", mode: "embedding" }] },
  ];

  beforeEach(setupPinia);
  afterEach(teardown);

  it("seeds each select from the defaults map", async () => {
    adminApi.listLlmProviders.mockResolvedValue({
      providers: THREE,
      defaults: { chat: "chatty", vision: "seer", embedding: "embed" },
    });
    await mountView();

    expect(q("defaults-select-chat").value).toBe("chatty");
    expect(q("defaults-select-vision").value).toBe("seer");
    expect(q("defaults-select-embedding").value).toBe("embed");
    expect(q("defaults-select-fallback").value).toBe("");
  });

  it("changing a select calls setDefaultProviderFor with the capability", async () => {
    adminApi.listLlmProviders.mockResolvedValue({ providers: THREE, defaults: {} });
    adminApi.setDefaultProviderFor.mockResolvedValue({});
    await mountView();

    setField("defaults-select-fallback", "chatty", "change");
    await flushPromises();

    expect(adminApi.setDefaultProviderFor).toHaveBeenCalledTimes(1);
    expect(adminApi.setDefaultProviderFor).toHaveBeenCalledWith("chatty", "fallback", false);
  });

  it("choosing — clears the capability via the currently-set provider", async () => {
    adminApi.listLlmProviders.mockResolvedValue({ providers: THREE, defaults: { chat: "chatty" } });
    adminApi.setDefaultProviderFor.mockResolvedValue({});
    await mountView();

    setField("defaults-select-chat", "", "change");
    await flushPromises();

    expect(adminApi.setDefaultProviderFor).toHaveBeenCalledWith("chatty", "chat", true);
  });

  it("offers only vision-capable providers for the vision capability", async () => {
    adminApi.listLlmProviders.mockResolvedValue({ providers: THREE, defaults: {} });
    await mountView();

    const values = [...q("defaults-select-vision").options].map((o) => o.value);
    expect(values).toEqual(["", "seer"]);
    // chat needs a model, not vision — all three qualify.
    const chatValues = [...q("defaults-select-chat").options].map((o) => o.value);
    expect(chatValues).toEqual(["", "chatty", "seer", "embed"]);
  });

  it("hides a modality row when no provider serves that mode", async () => {
    adminApi.listLlmProviders.mockResolvedValue({ providers: THREE, defaults: {} });
    await mountView();

    expect(q("defaults-select-embedding")).not.toBeNull();
    expect(q("defaults-select-ocr")).toBeNull();
    expect(q("defaults-select-audio_speech")).toBeNull();
  });

  it("does not call the API when the select is set to its current value", async () => {
    adminApi.listLlmProviders.mockResolvedValue({ providers: THREE, defaults: { chat: "chatty" } });
    await mountView();

    setField("defaults-select-chat", "chatty", "change");
    await flushPromises();

    expect(adminApi.setDefaultProviderFor).not.toHaveBeenCalled();
  });
});

describe("Providers.vue — test connection", () => {
  beforeEach(setupPinia);
  afterEach(teardown);

  it("sends the single model's mode and renders success", async () => {
    adminApi.listLlmProviders.mockResolvedValue({ providers: [] });
    adminApi.testLlmProvider.mockResolvedValue({ ok: true, latencyMs: 42 });
    const w = await mountView();
    await openWizard(w, "azure-openai");
    fillDetails({ id: "azure-foundry", name: "Azure Foundry", endpoint: "https://r.openai.azure.com" });
    setField("provider-api-version", "2024-08-01-preview");
    setField("provider-apikey", "sk-azure");
    await toModels("gpt-4o-deployment");

    await click("provider-test-btn");

    expect(adminApi.testLlmProvider).toHaveBeenCalledTimes(1);
    const [provider, apiKey, mode] = adminApi.testLlmProvider.mock.calls[0];
    expect(provider.api).toBe("azure-openai");
    expect(apiKey).toBe("sk-azure");
    expect(mode).toBe("chat");
    expect(q("provider-test-result").textContent).toContain("succeeded");
  });

  it("surfaces a failure message", async () => {
    adminApi.listLlmProviders.mockResolvedValue({ providers: [] });
    adminApi.testLlmProvider.mockResolvedValue({ ok: false, error: "upstream returned 401: bad key" });
    const w = await mountView();
    await openWizard(w);
    fillDetails();
    await toModels();

    await click("provider-test-btn");

    expect(q("provider-test-result").textContent).toContain("401");
  });

  it("offers a mode select when the models mix modes, and probes the picked one", async () => {
    adminApi.listLlmProviders.mockResolvedValue({ providers: [] });
    adminApi.testLlmProvider.mockResolvedValue({ ok: true, latencyMs: 5 });
    const w = await mountView();
    await openWizard(w);
    fillDetails();
    await toModels();

    // One chat model so far — nothing to choose between.
    expect(q("test-mode-select")).toBeNull();

    await click("model-add-btn");
    setField("model-id-1", "text-embedding-3-large");
    setField("model-mode-1", "embedding", "change");
    await flushPromises();

    const select = q("test-mode-select");
    expect(select).not.toBeNull();
    expect([...select.options].map((o) => o.value)).toEqual(["chat", "embedding"]);

    setField("test-mode-select", "embedding", "change");
    await flushPromises();
    await click("provider-test-btn");

    const [, , mode] = adminApi.testLlmProvider.mock.calls[0];
    expect(mode).toBe("embedding");
  });
});
