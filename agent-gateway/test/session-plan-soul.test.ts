// CP3 tests: soul injection into buildSystemPrompt + resolveSessionPlan.
//
// WHY these tests exist:
//   buildSystemPrompt must append the SOUL block only for non-empty soul strings,
//   keeping the governance preamble first and unmodified (invariant 1 of the spec).
//
//   resolveSessionPlan must fetch the soul from south.getAgentSpec for named
//   agents ("agent:<uuid>") and surface it in the systemPrompt, while leaving
//   the prompt unchanged for default agents ("default:…") and ticker ids.
import { test } from "node:test";
import assert from "node:assert/strict";
import {
  resolveSessionPlan,
  buildSystemPrompt,
  type ResolveIdentity,
  type ResolveSessionDeps,
  type ResolveSouth,
} from "../src/pi/session-plan.js";

// ── Shared constants ───────────────────────────────────────────────────────────

const PREAMBLE_SNIPPET = "You are a Aikonos agent";
const SOUL_DELIMITER_START = "--- Agent personality (author-provided) ---";
const SOUL_DELIMITER_END = "--- End agent personality ---";

const BASE_CFG = {
  llmModel: "anthropic/claude-sonnet-4.6",
  defaultTenantId: "11111111-1111-1111-1111-111111111111",
};

// makeSouthClient returns a ResolveSouth stub where getAgentSpec is configurable.
function makeSouthClient(opts: {
  getAgentSpecResult?: { found: boolean; soul?: string };
  getAgentSpecCalled?: { count: number };
  skills?: string[];
}): ResolveSouth {
  const result = opts.getAgentSpecResult ?? { found: false };
  const spy = opts.getAgentSpecCalled;

  return {
    getLlmProviders: async () => ({ providers: [] }),
    getTenantModel: async () => ({ model: BASE_CFG.llmModel }),
    listUserSkills: async () => ({ skills: opts.skills ?? ["web.fetch"] }),
    listUserAgentSkills: async () => ({ bundles: [] }),
    listAccessibleMcpServersForAgent: async () => ({ connections: [] }),
    listMcpServerToolsSouth: async () => ({ tools: [] }),
    getAgentSpec: async () => {
      if (spy) spy.count++;
      return result;
    },
  };
}

// ── buildSystemPrompt ──────────────────────────────────────────────────────────

test("buildSystemPrompt: governance preamble is first and present when soul is provided", () => {
  const prompt = buildSystemPrompt(["web_fetch"], "Be a helpful pirate.");

  const preambleIdx = prompt.indexOf(PREAMBLE_SNIPPET);
  const delimiterIdx = prompt.indexOf(SOUL_DELIMITER_START);

  assert.ok(preambleIdx !== -1, "governance preamble must be present");
  assert.ok(delimiterIdx !== -1, "soul delimiter must be present");
  assert.ok(preambleIdx < delimiterIdx, "governance preamble must appear before the soul block");
});

test("buildSystemPrompt: soul text appears between the delimiters", () => {
  const soul = "Be a helpful pirate.";
  const prompt = buildSystemPrompt(["web_fetch"], soul);

  assert.ok(prompt.includes(soul), "soul text must appear in the prompt");
  assert.ok(prompt.includes(SOUL_DELIMITER_START), "start delimiter must be present");
  assert.ok(prompt.includes(SOUL_DELIMITER_END), "end delimiter must be present");
  assert.ok(
    prompt.indexOf(SOUL_DELIMITER_START) < prompt.indexOf(soul),
    "start delimiter must precede soul text",
  );
  assert.ok(
    prompt.indexOf(soul) < prompt.indexOf(SOUL_DELIMITER_END),
    "soul text must precede end delimiter",
  );
});

test("buildSystemPrompt: no delimiter when soul is empty string", () => {
  const prompt = buildSystemPrompt(["web_fetch"], "");

  assert.ok(!prompt.includes(SOUL_DELIMITER_START), "no delimiter when soul is ''");
  assert.ok(prompt.includes(PREAMBLE_SNIPPET), "governance preamble must still be present");
});

test("buildSystemPrompt: no delimiter when soul is undefined", () => {
  const prompt = buildSystemPrompt(["web_fetch"], undefined);

  assert.ok(!prompt.includes(SOUL_DELIMITER_START), "no delimiter when soul is undefined");
  assert.ok(prompt.includes(PREAMBLE_SNIPPET), "governance preamble must still be present");
});

test("buildSystemPrompt: no delimiter when soul is only whitespace", () => {
  const prompt = buildSystemPrompt(["web_fetch"], "   ");

  assert.ok(!prompt.includes(SOUL_DELIMITER_START), "no delimiter when soul is whitespace-only");
});

test("buildSystemPrompt: output is identical to no-soul call when soul omitted (regression guard)", () => {
  const withoutSoul = buildSystemPrompt(["web_fetch"]);
  const withEmptySoul = buildSystemPrompt(["web_fetch"], "");

  assert.equal(withoutSoul, withEmptySoul, "empty soul must produce byte-identical output to omitted soul");
});

// ── workflow guidance injection ────────────────────────────────────────────────
// WHY: agents with the workflows skill are told to use workflow_* tools and not
// to dump markdown via doc.write or misuse delegate for sharing.

// Stable substring that only the guidance paragraph emits — not present in the
// tool-name list that the base prompt already interpolates.
const WORKFLOW_GUIDANCE_MARKER = "Do not save a workflow as a file";

test("buildSystemPrompt: workflow guidance present when workflow_save is in activeToolNames", () => {
  const prompt = buildSystemPrompt(
    ["workflow_save", "workflow_run", "workflow_list", "workflow_publish", "doc_write"],
    undefined,
  );

  assert.ok(
    prompt.includes(WORKFLOW_GUIDANCE_MARKER),
    "no-markdown instruction must be present when workflows skill is active",
  );
  assert.ok(
    prompt.includes("workflow_publish"),
    "workflow_publish must be referenced in the guidance body",
  );
  assert.ok(
    prompt.includes("workflow_save"),
    "workflow_save must be referenced in the guidance body",
  );
});

test("buildSystemPrompt: workflow guidance absent when workflow_save is NOT in activeToolNames", () => {
  const prompt = buildSystemPrompt(["doc_write", "web_fetch"], undefined);

  assert.ok(
    !prompt.includes(WORKFLOW_GUIDANCE_MARKER),
    "no-markdown instruction must NOT appear when workflows skill is absent",
  );
});

// ── analyze_image guidance injection (F7/CP6) ─────────────────────────────────
// WHY: the model must be steered to call analyze_image when the user's message
// references an image via a #<path> mention, but never forced — the model
// decides. Guidance must appear only when the tool is actually offered.

const ANALYZE_IMAGE_GUIDANCE_MARKER = "analyze_image";

test("buildSystemPrompt: analyze_image guidance present when analyze_image is in activeToolNames", () => {
  const prompt = buildSystemPrompt(["analyze_image", "doc_write"], undefined);

  assert.ok(
    prompt.includes(ANALYZE_IMAGE_GUIDANCE_MARKER),
    "analyze_image guidance must be present when the tool is active",
  );
  assert.ok(
    prompt.toLowerCase().includes("#<path>") || prompt.includes("#"),
    "guidance must mention the #<path> reference convention",
  );
});

test("buildSystemPrompt: analyze_image guidance absent when analyze_image is NOT in activeToolNames", () => {
  const prompt = buildSystemPrompt(["doc_write", "web_fetch"], undefined);

  assert.ok(
    !prompt.includes(ANALYZE_IMAGE_GUIDANCE_MARKER),
    "analyze_image guidance must NOT appear when the tool is absent",
  );
});

// ── resolveSessionPlan soul injection ─────────────────────────────────────────

test("resolveSessionPlan: named agent (agent:<uuid>) — soul from getAgentSpec appears in systemPrompt", async () => {
  const south = makeSouthClient({ getAgentSpecResult: { found: true, soul: "PIRATE" } });

  const identity: ResolveIdentity = {
    tenantId: "t1",
    userId: "user-a",
    agentId: "agent:uuid-123",
  };
  const deps: ResolveSessionDeps = { south, cfg: BASE_CFG };

  const plan = await resolveSessionPlan(identity, deps);

  assert.ok(plan.systemPrompt.includes("PIRATE"), "soul text must appear in systemPrompt");
  assert.ok(plan.systemPrompt.includes(SOUL_DELIMITER_START), "soul delimiter must appear when soul found");
});

test("resolveSessionPlan: named agent — found:false from getAgentSpec → no soul block", async () => {
  const south = makeSouthClient({ getAgentSpecResult: { found: false } });

  const identity: ResolveIdentity = {
    tenantId: "t1",
    userId: "user-a",
    agentId: "agent:uuid-456",
  };
  const deps: ResolveSessionDeps = { south, cfg: BASE_CFG };

  const plan = await resolveSessionPlan(identity, deps);

  assert.ok(!plan.systemPrompt.includes(SOUL_DELIMITER_START), "no soul block when found:false");
});

test("resolveSessionPlan: default agent (default:<user>) → getAgentSpec NOT called, no soul block", async () => {
  const called = { count: 0 };
  const south = makeSouthClient({
    getAgentSpecResult: { found: true, soul: "SHOULD NOT APPEAR" },
    getAgentSpecCalled: called,
  });

  const identity: ResolveIdentity = {
    tenantId: "t1",
    userId: "bob@x.com",
    agentId: "default:bob@x.com",
  };
  const deps: ResolveSessionDeps = { south, cfg: BASE_CFG };

  const plan = await resolveSessionPlan(identity, deps);

  assert.equal(called.count, 0, "getAgentSpec must NOT be called for default: agentId");
  assert.ok(!plan.systemPrompt.includes(SOUL_DELIMITER_START), "no soul block for default agent");
});

test("resolveSessionPlan: fast path — agentSpec.soul non-empty uses it without RPC call", async () => {
  const called = { count: 0 };
  const south = makeSouthClient({
    getAgentSpecResult: { found: true, soul: "RPC SOUL — must not appear" },
    getAgentSpecCalled: called,
  });

  const identity: ResolveIdentity = {
    tenantId: "t1",
    userId: "user-a",
    agentId: "agent:uuid-789",
  };
  const deps: ResolveSessionDeps = {
    south,
    cfg: BASE_CFG,
    agentSpec: {
      model: "gpt-4",
      approvalMode: "auto",
      skills: [],
      soul: "FAST PATH SOUL",
    },
  };

  const plan = await resolveSessionPlan(identity, deps);

  assert.equal(called.count, 0, "getAgentSpec must NOT be called when agentSpec.soul is already set");
  assert.ok(plan.systemPrompt.includes("FAST PATH SOUL"), "fast path soul must appear in systemPrompt");
  assert.ok(!plan.systemPrompt.includes("RPC SOUL"), "RPC soul must not appear");
});

test("resolveSessionPlan: getAgentSpec error → soul stays empty, spawn succeeds", async () => {
  const south: ResolveSouth = {
    getLlmProviders: async () => ({ providers: [] }),
    getTenantModel: async () => ({ model: BASE_CFG.llmModel }),
    listUserSkills: async () => ({ skills: ["web.fetch"] }),
    listUserAgentSkills: async () => ({ bundles: [] }),
    listAccessibleMcpServersForAgent: async () => ({ connections: [] }),
    listMcpServerToolsSouth: async () => ({ tools: [] }),
    getAgentSpec: async () => { throw new Error("broker unavailable"); },
  };

  const identity: ResolveIdentity = {
    tenantId: "t1",
    userId: "user-a",
    agentId: "agent:uuid-error",
  };
  const deps: ResolveSessionDeps = { south, cfg: BASE_CFG };

  // Must not throw despite getAgentSpec failing.
  const plan = await resolveSessionPlan(identity, deps);

  assert.ok(!plan.systemPrompt.includes(SOUL_DELIMITER_START), "no soul block when RPC throws");
  assert.ok(plan.systemPrompt.includes(PREAMBLE_SNIPPET), "governance preamble must still be present");
});
