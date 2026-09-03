// Tests for the semantic-recall IO module (memory-semantic.ts,
//  CP3).
//
// WHY these tests exist: semanticRecall is the one place a broken/misbehaving
// embedding provider could either (a) leak into a chat turn as a thrown error
// (breaking chat) or (b) silently rank the wrong concept as relevant. Every
// skip/failure path must degrade to [] (keyword-only, unchanged) — never
// throw — and the ranking math (cosine floor, tiebreak) must be exact.
import { test } from "node:test";
import assert from "node:assert/strict";
import pino from "pino";
import {
  semanticRecall,
  VectorCache,
  type SemanticRecallSouth,
  type SemanticRecallParams,
  type SemanticEmbeddingProvider,
} from "../src/pi/memory-semantic.js";
import { MEMORY_RECALL_CAP, type MemoryConceptLike } from "../src/pi/memory-match.js";
import type { EmbeddingCallResult } from "../src/llm/embed.js";

const log = pino({ level: "silent" });

function concept(overrides: Partial<MemoryConceptLike> = {}): MemoryConceptLike {
  return {
    id: overrides.id ?? "c1",
    scope: overrides.scope ?? "user",
    groupId: overrides.groupId ?? "",
    title: overrides.title ?? "deploy runbook",
    description: overrides.description ?? "how we ship",
    tags: overrides.tags ?? ["deploy"],
    status: overrides.status ?? "stable",
    trustTier: overrides.trustTier ?? "unverified",
    stale: overrides.stale ?? false,
  };
}

const EMBEDDING_PROVIDER: SemanticEmbeddingProvider = {
  id: "embed-provider",
  enabled: true,
  api: "openai-completions",
  endpoint: "https://embeddings.example.test",
  apiKey: "dummy",
  models: [{ id: "text-embedding-3-small", mode: "embedding" }],
};

function fakeSouth(overrides: Partial<SemanticRecallSouth> = {}): SemanticRecallSouth {
  return {
    getLlmProviders: async () => ({ providers: [EMBEDDING_PROVIDER], defaults: {} }),
    emitLlmUsage: async () => {},
    ...overrides,
  };
}

function baseParams(
  overrides: Partial<SemanticRecallParams<MemoryConceptLike>> = {},
): SemanticRecallParams<MemoryConceptLike> {
  return {
    enabled: true,
    prompt: "how do we deploy the service?",
    concepts: [concept()],
    keywordMatchCount: 0,
    tenantId: "tenant-1",
    userId: "alice",
    agentId: "",
    runId: "run-1",
    timeoutMs: 5000,
    south: fakeSouth(),
    rateLimitChecker: async () => {},
    log,
    cache: new VectorCache(),
    ...overrides,
  };
}

// ── skip conditions (no network call) ───────────────────────────────────────

test("semanticRecall: knob off skips before any south call", async () => {
  let called = false;
  const result = await semanticRecall(
    baseParams({ enabled: false, south: fakeSouth({ getLlmProviders: async () => { called = true; return { providers: [], defaults: {} }; } }) }),
  );
  assert.deepEqual(result, []);
  assert.equal(called, false, "getLlmProviders must not be called when the knob is off");
});

test("semanticRecall: no recallable concepts skips before any south call", async () => {
  let called = false;
  const result = await semanticRecall(
    baseParams({
      concepts: [concept({ status: "deprecated" })],
      south: fakeSouth({ getLlmProviders: async () => { called = true; return { providers: [], defaults: {} }; } }),
    }),
  );
  assert.deepEqual(result, []);
  assert.equal(called, false);
});

test("semanticRecall: keyword matches already at cap skips before any south call", async () => {
  let called = false;
  const result = await semanticRecall(
    baseParams({
      keywordMatchCount: MEMORY_RECALL_CAP,
      south: fakeSouth({ getLlmProviders: async () => { called = true; return { providers: [], defaults: {} }; } }),
    }),
  );
  assert.deepEqual(result, []);
  assert.equal(called, false);
});

test("semanticRecall: no embedding candidate resolvable degrades to []", async () => {
  const result = await semanticRecall(
    baseParams({ south: fakeSouth({ getLlmProviders: async () => ({ providers: [], defaults: {} }) }) }),
  );
  assert.deepEqual(result, []);
});

// ── failure paths (fail-open) ────────────────────────────────────────────────

test("semanticRecall: a rate-limit denial degrades to [] without calling embed", async () => {
  let embedCalls = 0;
  const result = await semanticRecall(
    baseParams({
      rateLimitChecker: async () => { throw new Error("rate limit exceeded"); },
      embed: async () => { embedCalls++; return { embeddings: [[1, 0]], tokensIn: 1 }; },
    }),
  );
  assert.deepEqual(result, []);
  assert.equal(embedCalls, 0);
});

test("semanticRecall: getLlmProviders throwing degrades to []", async () => {
  const result = await semanticRecall(
    baseParams({ south: fakeSouth({ getLlmProviders: async () => { throw new Error("broker down"); } }) }),
  );
  assert.deepEqual(result, []);
});

test("semanticRecall: an embed provider error/timeout degrades to []", async () => {
  const result = await semanticRecall(
    baseParams({ embed: async () => { throw new Error("timed out after 5000ms"); } }),
  );
  assert.deepEqual(result, []);
});

// ── cache ────────────────────────────────────────────────────────────────────

test("semanticRecall: a cached concept vector is not re-embedded (call count + inputs assertion)", async () => {
  const c = concept({ id: "cached-one" });
  const cache = new VectorCache();
  // Pre-populate as semanticRecall's own cacheKey would (id|title|description|tags).
  const { createHash } = await import("node:crypto");
  const key = createHash("sha256").update(`${c.id}|${c.title}|${c.description}|${c.tags.join(",")}`).digest("hex");
  cache.set(key, [1, 0]);

  let calls = 0;
  let lastInputs: string[] = [];
  const result = await semanticRecall(
    baseParams({
      concepts: [c],
      cache,
      embed: async (opts) => {
        calls++;
        lastInputs = opts.inputs;
        return { embeddings: opts.inputs.map(() => [1, 0]), tokensIn: 3 };
      },
    }),
  );
  assert.equal(calls, 1, "one batched call per turn, never per concept");
  assert.deepEqual(lastInputs, ["how do we deploy the service?"], "the cached concept's text must not be re-sent");
  assert.deepEqual(result.map((r) => r.id), ["cached-one"], "the cached vector still ranks the concept");
});

// ── ranking: cosine floor + tiebreak ────────────────────────────────────────

test("semanticRecall: ranks by cosine desc, applies the 0.30 floor, tiebreaks by id asc", async () => {
  const high = concept({ id: "high", title: "t-high" });
  const low = concept({ id: "low", title: "t-low" }); // below floor
  const tieB = concept({ id: "tie-b", title: "t-tie" });
  const tieA = concept({ id: "tie-a", title: "t-tie-2" });

  const vectorFor: Record<string, number[]> = {
    "how do we deploy the service?": [1, 0],
    "t-high\nhow we ship\ndeploy": [1, 0], // cosine 1.0
    "t-low\nhow we ship\ndeploy": [0, 1], // cosine 0.0 — below floor
    "t-tie\nhow we ship\ndeploy": [0.6, 0.6], // equal score, tiebreak by id
    "t-tie-2\nhow we ship\ndeploy": [0.6, 0.6],
  };

  const result = await semanticRecall(
    baseParams({
      concepts: [low, tieB, high, tieA],
      embed: async (opts) => ({
        embeddings: opts.inputs.map((text) => vectorFor[text] ?? [0, 0]),
        tokensIn: 10,
      }),
    }),
  );

  assert.deepEqual(result.map((r) => r.id), ["high", "tie-a", "tie-b"]);
});

// ── usage emission ───────────────────────────────────────────────────────────

test("semanticRecall: emits usage with source:embedding and the response's tokensIn on success", async () => {
  const usageCalls: unknown[] = [];
  await semanticRecall(
    baseParams({
      tenantId: "tenant-x",
      userId: "bob",
      agentId: "agent-1",
      runId: "run-42",
      south: fakeSouth({
        emitLlmUsage: async (req) => { usageCalls.push(req); },
      }),
      embed: async (opts): Promise<EmbeddingCallResult> => ({
        embeddings: opts.inputs.map(() => [1, 0]),
        tokensIn: 17,
      }),
    }),
  );
  assert.equal(usageCalls.length, 1);
  assert.deepEqual(usageCalls[0], {
    tenantId: "tenant-x",
    userId: "bob",
    agentId: "agent-1",
    provider: "embed-provider",
    model: "text-embedding-3-small",
    tokensIn: 17,
    tokensOut: 0,
    cacheRead: 0,
    cacheWrite: 0,
    cost: 0,
    runId: "run-42",
    sessionId: "",
    source: "embedding",
    quantity: 0,
    unit: "",
  });
});
