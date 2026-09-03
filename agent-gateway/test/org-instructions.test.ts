// A1 tests: org-global instruction preamble.
//
// WHY these tests exist:
//   The org preamble is a governance-level block that must sit at the very TOP
//   of the system prompt — ahead of the base governance line, the agent soul,
//   and user instructions — because it carries the tenant admin's compliance
//   rules. It must be injected only when non-empty, capped in length, and it
//   must never break session resolution when the settings RPC is unavailable.
import { test } from "node:test";
import assert from "node:assert/strict";
import {
  buildSystemPrompt,
  ORG_PREAMBLE_MAX_CHARS,
} from "../src/pi/system-prompt.js";
import {
  resolveSessionPlan,
  type ResolveIdentity,
  type ResolveSessionDeps,
  type ResolveSouth,
} from "../src/pi/session-plan.js";

const BASE_PREAMBLE = "You are a Aikonos agent";
const ORG_START = "--- Organization instructions (set by your administrator; authoritative) ---";
const ORG_END = "--- End organization instructions ---";
const SOUL_START = "--- Agent personality (author-provided) ---";

const BASE_CFG = {
  llmModel: "anthropic/claude-sonnet-4.6",
  defaultTenantId: "11111111-1111-1111-1111-111111111111",
};

// ── buildSystemPrompt org preamble ──────────────────────────────────────────

test("buildSystemPrompt: org preamble appears before everything else", () => {
  const org = "Never include PII.";
  const prompt = buildSystemPrompt(["web_fetch"], "Be a pirate.", undefined, org);

  const orgIdx = prompt.indexOf(ORG_START);
  const baseIdx = prompt.indexOf(BASE_PREAMBLE);
  const soulIdx = prompt.indexOf(SOUL_START);

  assert.ok(orgIdx !== -1, "org block must be present");
  assert.ok(orgIdx < baseIdx, "org block must precede the base governance line");
  assert.ok(baseIdx < soulIdx, "base line must still precede the soul block");
  assert.ok(prompt.includes(org), "org instruction text must appear");
  assert.ok(prompt.includes(ORG_END), "org end delimiter must be present");
});

test("buildSystemPrompt: no org block when preamble empty / undefined / whitespace", () => {
  for (const val of ["", undefined, "   "]) {
    const prompt = buildSystemPrompt(["web_fetch"], undefined, undefined, val);
    assert.ok(!prompt.includes(ORG_START), `no org block for ${JSON.stringify(val)}`);
    assert.ok(prompt.includes(BASE_PREAMBLE), "base governance line must still be present");
  }
});

test("buildSystemPrompt: omitted org preamble is byte-identical to empty (regression guard)", () => {
  const omitted = buildSystemPrompt(["web_fetch"], "soul", "cat");
  const empty = buildSystemPrompt(["web_fetch"], "soul", "cat", "");
  assert.equal(omitted, empty, "omitted org preamble must match empty-string output");
});

test("buildSystemPrompt: org preamble is capped at ORG_PREAMBLE_MAX_CHARS", () => {
  // Use a marker char absent from the base prompt so the count is unambiguous.
  const long = "Z".repeat(ORG_PREAMBLE_MAX_CHARS + 500);
  const prompt = buildSystemPrompt(["web_fetch"], undefined, undefined, long);
  const zs = (prompt.match(/Z/g) ?? []).length;
  assert.equal(zs, ORG_PREAMBLE_MAX_CHARS, `preamble must be truncated to exactly ${ORG_PREAMBLE_MAX_CHARS} chars, got ${zs}`);
});

// ── resolveSessionPlan wiring ───────────────────────────────────────────────

function baseSouth(overrides: Partial<ResolveSouth> = {}): ResolveSouth {
  return {
    getLlmProviders: async () => ({ providers: [] }),
    getTenantModel: async () => ({ model: BASE_CFG.llmModel }),
    listUserSkills: async () => ({ skills: ["web.fetch"] }),
    listUserAgentSkills: async () => ({ bundles: [] }),
    listAccessibleMcpServersForAgent: async () => ({ connections: [] }),
    listMcpServerToolsSouth: async () => ({ tools: [] }),
    getAgentSpec: async () => ({ found: false }),
    ...overrides,
  };
}

test("resolveSessionPlan: org preamble from getOrgSettings appears in systemPrompt", async () => {
  const south = baseSouth({
    getOrgSettings: async () => ({ settings: { instructionPreamble: "COMPLIANCE RULE" } }),
  });
  const identity: ResolveIdentity = { tenantId: "t1", userId: "u", agentId: "default:u" };
  const plan = await resolveSessionPlan(identity, { south, cfg: BASE_CFG });

  assert.ok(plan.systemPrompt.includes("COMPLIANCE RULE"), "org preamble must appear in prompt");
  assert.ok(plan.systemPrompt.includes(ORG_START), "org delimiter must appear");
});

test("resolveSessionPlan: getOrgSettings absent → no org block, spawn succeeds", async () => {
  const south = baseSouth(); // no getOrgSettings on the stub
  const identity: ResolveIdentity = { tenantId: "t1", userId: "u", agentId: "default:u" };
  const plan = await resolveSessionPlan(identity, { south, cfg: BASE_CFG });

  assert.ok(!plan.systemPrompt.includes(ORG_START), "no org block when RPC unavailable");
  assert.ok(plan.systemPrompt.includes(BASE_PREAMBLE), "prompt still built");
});

test("resolveSessionPlan: getOrgSettings throws → no org block, spawn succeeds", async () => {
  const south = baseSouth({
    getOrgSettings: async () => { throw new Error("broker unavailable"); },
  });
  const identity: ResolveIdentity = { tenantId: "t1", userId: "u", agentId: "default:u" };
  const plan = await resolveSessionPlan(identity, { south, cfg: BASE_CFG });

  assert.ok(!plan.systemPrompt.includes(ORG_START), "no org block when RPC throws");
  assert.ok(plan.systemPrompt.includes(BASE_PREAMBLE), "prompt still built despite RPC error");
});
