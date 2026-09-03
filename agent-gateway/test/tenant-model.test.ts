import { test } from "node:test";
import assert from "node:assert/strict";
import { resolveEnvModel } from "../src/pi/session";

// Minimal stub shaped like Pick<SouthClient, "getTenantModel">.
function stubSouth(model: string) {
  return { getTenantModel: async (_req: { tenantId: string }) => ({ model }) };
}

function failingSouth() {
  return {
    getTenantModel: async (_req: { tenantId: string }): Promise<{ model: string }> => {
      throw new Error("RPC unavailable");
    },
  };
}

const cfg = { llmModel: "anthropic/claude-sonnet-4.6" };

test("uses tenant config model when broker returns a non-empty model", async () => {
  const model = await resolveEnvModel(cfg, stubSouth("openai/gpt-4o"), "t1");
  assert.equal(model, "openai/gpt-4o");
});

test("falls back to env default when broker returns empty model", async () => {
  const model = await resolveEnvModel(cfg, stubSouth(""), "t1");
  assert.equal(model, "anthropic/claude-sonnet-4.6");
});

test("falls back to env default on RPC error", async () => {
  const model = await resolveEnvModel(cfg, failingSouth(), "t1");
  assert.equal(model, "anthropic/claude-sonnet-4.6");
});
