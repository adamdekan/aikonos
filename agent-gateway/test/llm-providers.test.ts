import { test } from "node:test";
import assert from "node:assert/strict";
import { AuthStorage, ModelRegistry } from "@earendil-works/pi-coding-agent";
import { resolveProviderModel } from "../src/pi/session";
import type { LlmModel, LlmProvider } from "../gen/ts/proto/broker";

function mkModel(id: string, priceIn = 0, priceOut = 0): LlmModel {
  return {
    id,
    mode: "",
    priceIn,
    priceOut,
    priceCacheRead: 0,
    priceCacheWrite: 0,
    contextWindow: 0,
    maxTokens: 0,
  };
}

function mkProvider(p: Partial<LlmProvider> & { id: string; models: LlmModel[] }): LlmProvider {
  return {
    name: p.id,
    endpoint: "https://example.com/v1",
    api: "openai-completions",
    enabled: true,
    isDefault: false,
    hasKey: true,
    updatedBy: "",
    apiKey: "k",
    apiVersion: "",
    visionCapable: false,
    isDefaultVision: false,
    isFallback: false,
    priceInMicrosPerMtok: 0,
    priceOutMicrosPerMtok: 0,
    config: {},
    ...p,
  };
}

// ── resolveProviderModel ────────────────────────────────────────────────────

test("resolveProviderModel: agent preferred provider + model wins", () => {
  const providers = [
    mkProvider({ id: "openrouter", isDefault: true, models: [mkModel("sonnet")] }),
    mkProvider({ id: "openai", models: [mkModel("gpt-4o")] }),
  ];
  const got = resolveProviderModel(providers, {
    model: "gpt-4o",
    approvalMode: "auto",
    skills: [],
    preferredProvider: "openai",
  });
  assert.deepEqual(got, { providerId: "openai", modelId: "gpt-4o" });
});

test("resolveProviderModel: falls back to default provider when preference unset", () => {
  const providers = [
    mkProvider({ id: "openrouter", isDefault: true, models: [mkModel("sonnet")] }),
    mkProvider({ id: "openai", models: [mkModel("gpt-4o")] }),
  ];
  const got = resolveProviderModel(providers, { model: "", approvalMode: "auto", skills: [] });
  assert.deepEqual(got, { providerId: "openrouter", modelId: "sonnet" });
});

test("resolveProviderModel: default provider honours the agent model when it lists it", () => {
  const providers = [
    mkProvider({ id: "openrouter", isDefault: true, models: [mkModel("sonnet"), mkModel("haiku")] }),
  ];
  const got = resolveProviderModel(providers, { model: "haiku", approvalMode: "auto", skills: [] });
  assert.deepEqual(got, { providerId: "openrouter", modelId: "haiku" });
});

test("resolveProviderModel: preference to a disabled provider falls back to default", () => {
  const providers = [
    mkProvider({ id: "openrouter", isDefault: true, models: [mkModel("sonnet")] }),
    mkProvider({ id: "openai", enabled: false, models: [mkModel("gpt-4o")] }),
  ];
  const got = resolveProviderModel(providers, {
    model: "gpt-4o",
    approvalMode: "auto",
    skills: [],
    preferredProvider: "openai",
  });
  // disabled providers are filtered before this call; even if passed, it is not selectable
  assert.deepEqual(got, { providerId: "openrouter", modelId: "sonnet" });
});

// ── registerProviderModels: prices land in the model cost block ──────────────

test("registered provider prices become the model cost block (Pi prices from this)", () => {
  // Inlines the provider-registration shape (name/baseUrl/apiKey/models/cost)
  // and asserts the cost block Pi actually prices turns from.
  const registry = ModelRegistry.inMemory(AuthStorage.inMemory());
  const provider = mkProvider({
    id: "openrouter",
    isDefault: true,
    models: [mkModel("sonnet", 0.000003, 0.000015)],
  });
  registry.registerProvider(provider.id, {
    name: provider.name,
    baseUrl: provider.endpoint,
    apiKey: provider.apiKey,
    api: provider.api,
    authHeader: true,
    models: provider.models.map((m) => ({
      id: m.id,
      name: `${provider.name} ${m.id}`,
      reasoning: false,
      input: ["text"],
      cost: { input: m.priceIn, output: m.priceOut, cacheRead: m.priceCacheRead, cacheWrite: m.priceCacheWrite },
      contextWindow: 200000,
      maxTokens: 8192,
    })),
  });
  registry.refresh();
  const model = registry.find("openrouter", "sonnet");
  assert.ok(model);
  assert.equal(model.cost.input, 0.000003);
  assert.equal(model.cost.output, 0.000015);
});
