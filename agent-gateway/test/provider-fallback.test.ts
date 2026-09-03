// src/llm/provider-fallback.ts — the single provider-selection order shared by
// pi/session.ts, pi/session-plan.ts, llm/reason.ts and llm/vision.ts.
//
// The ordering tests are table-driven over provider fixtures: the four call
// sites differ only in what they do with the chain, so every ordering rule is
// proven once here rather than four times downstream.
import { test } from "node:test";
import assert from "node:assert/strict";
import {
  chatCandidates,
  visionCandidates,
  embeddingCandidates,
  shouldFailover,
  type Candidate,
  type ChatProviderLike,
  type SpecLike,
  type VisionProviderLike,
  type EmbeddingProviderLike,
} from "../src/llm/provider-fallback.js";

function chatProvider(p: Partial<ChatProviderLike> & { id: string }): ChatProviderLike {
  return { enabled: true, isDefault: false, isFallback: false, models: [{ id: `${p.id}-m1` }], ...p };
}

function visionProvider(p: Partial<VisionProviderLike> & { id: string }): VisionProviderLike {
  return {
    enabled: true,
    visionCapable: false,
    isDefaultVision: false,
    isFallback: false,
    models: [{ id: `${p.id}-m1` }],
    ...p,
  };
}

const ids = (candidates: Candidate<ChatProviderLike>[] | Candidate<VisionProviderLike>[]) =>
  candidates.map((c) => c.provider.id);

// ── chatCandidates ordering ──────────────────────────────────────────────────

const ASSIGNED_SPEC: SpecLike = { preferredProvider: "assigned", model: "assigned-m1" };

interface ChatCase {
  name: string;
  providers: ChatProviderLike[];
  spec?: SpecLike;
  expect: string[];
}

const chatCases: ChatCase[] = [
  {
    name: "assigned → default → fallback, all three present",
    providers: [
      chatProvider({ id: "assigned" }),
      chatProvider({ id: "def", isDefault: true }),
      chatProvider({ id: "fb", isFallback: true }),
    ],
    spec: ASSIGNED_SPEC,
    expect: ["assigned", "def", "fb"],
  },
  {
    name: "no spec: default → fallback",
    providers: [
      chatProvider({ id: "assigned" }),
      chatProvider({ id: "def", isDefault: true }),
      chatProvider({ id: "fb", isFallback: true }),
    ],
    expect: ["def", "fb"],
  },
  {
    name: "assigned provider disabled: skipped",
    providers: [
      chatProvider({ id: "assigned", enabled: false }),
      chatProvider({ id: "def", isDefault: true }),
      chatProvider({ id: "fb", isFallback: true }),
    ],
    spec: ASSIGNED_SPEC,
    expect: ["def", "fb"],
  },
  {
    name: "assigned provider does not list the assigned model: skipped",
    providers: [
      chatProvider({ id: "assigned", models: [{ id: "something-else" }] }),
      chatProvider({ id: "def", isDefault: true }),
    ],
    spec: ASSIGNED_SPEC,
    expect: ["def"],
  },
  {
    name: "no default: assigned → fallback",
    providers: [chatProvider({ id: "assigned" }), chatProvider({ id: "fb", isFallback: true })],
    spec: ASSIGNED_SPEC,
    expect: ["assigned", "fb"],
  },
  {
    name: "no fallback designated: chain stops at the default (no first-match lottery)",
    providers: [
      chatProvider({ id: "def", isDefault: true }),
      chatProvider({ id: "bystander" }),
    ],
    expect: ["def"],
  },
  {
    name: "nothing designated: empty, never an arbitrary enabled provider",
    providers: [chatProvider({ id: "bystander" }), chatProvider({ id: "other" })],
    expect: [],
  },
  {
    name: "default is also the fallback: deduped to one entry",
    providers: [chatProvider({ id: "both", isDefault: true, isFallback: true })],
    expect: ["both"],
  },
  {
    name: "assigned is also the default: deduped, assigned position wins",
    providers: [
      chatProvider({ id: "assigned", isDefault: true }),
      chatProvider({ id: "fb", isFallback: true }),
    ],
    spec: ASSIGNED_SPEC,
    expect: ["assigned", "fb"],
  },
  {
    name: "default with zero models is unusable: fallback carries the chain",
    providers: [
      chatProvider({ id: "def", isDefault: true, models: [] }),
      chatProvider({ id: "fb", isFallback: true }),
    ],
    expect: ["fb"],
  },
  {
    name: "disabled fallback: not reached",
    providers: [
      chatProvider({ id: "def", isDefault: true }),
      chatProvider({ id: "fb", isFallback: true, enabled: false }),
    ],
    expect: ["def"],
  },
  {
    name: "disabled default with no fallback: empty, the disabled default is never used",
    providers: [chatProvider({ id: "def", isDefault: true, enabled: false })],
    expect: [],
  },
  {
    name: "isFallback omitted entirely (narrower stub shape): treated as not-fallback",
    providers: [{ id: "def", enabled: true, isDefault: true, models: [{ id: "def-m1" }] }],
    expect: ["def"],
  },
];

for (const c of chatCases) {
  test(`chatCandidates: ${c.name}`, () => {
    assert.deepEqual(ids(chatCandidates(c.providers, c.spec)), c.expect);
  });
}

// ── chatCandidates model resolution ──────────────────────────────────────────

test("chatCandidates: a candidate keeps the spec model when it lists it, else its first model", () => {
  const providers: ChatProviderLike[] = [
    chatProvider({ id: "def", isDefault: true, models: [{ id: "shared" }, { id: "def-other" }] }),
    chatProvider({ id: "fb", isFallback: true, models: [{ id: "fb-first" }, { id: "fb-second" }] }),
  ];
  const got: Candidate<ChatProviderLike>[] = chatCandidates(providers, { model: "shared" });
  assert.deepEqual(
    got.map((c) => [c.provider.id, c.modelId]),
    [
      ["def", "shared"],
      // fb does not list "shared" — falls back to its own first model rather
      // than being dropped, so the chain stays usable.
      ["fb", "fb-first"],
    ],
  );
});

test("chatCandidates: the assigned candidate uses the assigned model", () => {
  const providers: ChatProviderLike[] = [
    chatProvider({ id: "assigned", models: [{ id: "assigned-m1" }, { id: "assigned-m2" }] }),
  ];
  const got: Candidate<ChatProviderLike>[] = chatCandidates(providers, {
    preferredProvider: "assigned",
    model: "assigned-m2",
  });
  assert.deepEqual(got, [{ provider: providers[0], modelId: "assigned-m2", maxTokens: 0 }]);
});

// A model that declares its own budget hands it to the call site, which is what
// lets an operator raise a reason step's ceiling from the Providers panel instead
// of rebuilding the gateway. Selection stays the concern here; what 0 means is
// the call site's.
test("chatCandidates: the selected model's own maxTokens rides along", () => {
  const providers: ChatProviderLike[] = [
    chatProvider({
      id: "assigned",
      isDefault: true,
      models: [{ id: "small", maxTokens: 2048 }, { id: "big", maxTokens: 32000 }],
    }),
  ];

  const assigned = chatCandidates(providers, { preferredProvider: "assigned", model: "big" });
  assert.deepEqual(assigned.map((c) => [c.modelId, c.maxTokens]), [["big", 32000]]);

  // No spec → first model, and its budget, not the largest one available.
  const unspecified = chatCandidates(providers);
  assert.deepEqual(unspecified.map((c) => [c.modelId, c.maxTokens]), [["small", 2048]]);
});

test("chatCandidates: a model with no declared budget reports 0, never undefined", () => {
  const providers: ChatProviderLike[] = [chatProvider({ id: "p", isDefault: true, models: [{ id: "m" }] })];
  const [candidate] = chatCandidates(providers);
  assert.equal(candidate.maxTokens, 0, "0 is the documented 'unset' signal the call sites branch on");
});

// ── visionCandidates ─────────────────────────────────────────────────────────

test("visionCandidates: default-vision → vision-capable fallback", () => {
  const providers: VisionProviderLike[] = [
    visionProvider({ id: "def", visionCapable: true, isDefaultVision: true }),
    visionProvider({ id: "fb", visionCapable: true, isFallback: true }),
  ];
  assert.deepEqual(ids(visionCandidates(providers)), ["def", "fb"]);
});

test("visionCandidates: a non-vision-capable fallback is skipped, not tried", () => {
  const providers: VisionProviderLike[] = [
    visionProvider({ id: "def", visionCapable: true, isDefaultVision: true }),
    visionProvider({ id: "fb", visionCapable: false, isFallback: true }),
  ];
  assert.deepEqual(ids(visionCandidates(providers)), ["def"]);
});

test("visionCandidates: vision-capable fallback carries the chain when no default-vision is set", () => {
  const providers: VisionProviderLike[] = [visionProvider({ id: "fb", visionCapable: true, isFallback: true })];
  const got: Candidate<VisionProviderLike>[] = visionCandidates(providers);
  assert.deepEqual(got, [{ provider: providers[0], modelId: "fb-m1", maxTokens: 0 }]);
});

test("visionCandidates: fails closed when only a non-vision fallback exists", () => {
  const providers: VisionProviderLike[] = [visionProvider({ id: "fb", isFallback: true })];
  assert.deepEqual(visionCandidates(providers), []);
});

// The negative cases the caller turns into "no vision provider assigned": each
// disqualifies the designated default-vision provider for a different reason.
const visionEmptyCases: { name: string; providers: VisionProviderLike[] }[] = [
  {
    name: "default-vision provider is not vision-capable",
    providers: [visionProvider({ id: "def", isDefaultVision: true })],
  },
  {
    name: "default-vision provider is disabled",
    providers: [visionProvider({ id: "def", enabled: false, visionCapable: true, isDefaultVision: true })],
  },
  {
    name: "nothing designated default-vision, however many vision-capable providers exist",
    providers: [
      visionProvider({ id: "a", visionCapable: true }),
      visionProvider({ id: "b", visionCapable: true }),
    ],
  },
];

for (const c of visionEmptyCases) {
  test(`visionCandidates: empty when ${c.name}`, () => {
    assert.deepEqual(visionCandidates(c.providers), []);
  });
}

test("visionCandidates: default-vision is also the fallback → deduped", () => {
  const providers: VisionProviderLike[] = [
    visionProvider({ id: "both", visionCapable: true, isDefaultVision: true, isFallback: true }),
  ];
  assert.deepEqual(ids(visionCandidates(providers)), ["both"]);
});

// ── embeddingCandidates ──────────────────────────────────────────────────────

function embedProvider(p: Partial<EmbeddingProviderLike> & { id: string }): EmbeddingProviderLike {
  return {
    enabled: true,
    api: "openai-completions",
    models: [{ id: `${p.id}-embed`, mode: "embedding" }],
    ...p,
  };
}

test("embeddingCandidates: default-provider tier wins over the first-usable tier", () => {
  const providers = [
    embedProvider({ id: "other" }),
    embedProvider({ id: "def" }),
  ];
  const got = embeddingCandidates(providers, { embedding: "def" });
  assert.deepEqual(
    got.map((c) => c.provider.id),
    ["def", "other"],
  );
});

test("embeddingCandidates: no default designated → falls to the first embedding-capable provider", () => {
  const providers = [embedProvider({ id: "a" }), embedProvider({ id: "b" })];
  const got = embeddingCandidates(providers, {});
  assert.deepEqual(got.map((c) => c.provider.id), ["a"]);
});

test("embeddingCandidates: a chat-only provider (no embedding-mode model) is never selected", () => {
  const providers = [
    embedProvider({ id: "chat-only", models: [{ id: "chat-only-m1", mode: "chat" }] }),
    embedProvider({ id: "embed-ok" }),
  ];
  const got = embeddingCandidates(providers, { embedding: "chat-only" });
  assert.deepEqual(got.map((c) => c.provider.id), ["embed-ok"]);
});

test("embeddingCandidates: a model with mode omitted (default 'chat') is not an embedding model", () => {
  const providers = [
    embedProvider({ id: "no-mode", models: [{ id: "no-mode-m1" }] }),
    embedProvider({ id: "embed-ok" }),
  ];
  const got = embeddingCandidates(providers, {});
  assert.deepEqual(got.map((c) => c.provider.id), ["embed-ok"]);
});

test("embeddingCandidates: anthropic-messages is never selected, even as the designated default", () => {
  const providers = [embedProvider({ id: "anthropic", api: "anthropic-messages" })];
  assert.deepEqual(embeddingCandidates(providers, { embedding: "anthropic" }), []);
});

test("embeddingCandidates: disabled provider is never selected", () => {
  const providers = [embedProvider({ id: "off", enabled: false }), embedProvider({ id: "on" })];
  const got = embeddingCandidates(providers, { embedding: "off" });
  assert.deepEqual(got.map((c) => c.provider.id), ["on"]);
});

test("embeddingCandidates: default is also the first-usable → deduped to one entry", () => {
  const providers = [embedProvider({ id: "only" })];
  const got = embeddingCandidates(providers, { embedding: "only" });
  assert.deepEqual(got.map((c) => c.provider.id), ["only"]);
});

test("embeddingCandidates: nothing usable → empty, no arbitrary chat-only fallback", () => {
  assert.deepEqual(embeddingCandidates([], {}), []);
  assert.deepEqual(embeddingCandidates([embedProvider({ id: "chat", models: [{ id: "m1", mode: "chat" }] })], {}), []);
});

test("embeddingCandidates: the candidate model is the provider's first embedding-mode model, not its first model overall", () => {
  const providers = [
    embedProvider({
      id: "mixed",
      models: [
        { id: "mixed-chat", mode: "chat" },
        { id: "mixed-embed-1", mode: "embedding" },
        { id: "mixed-embed-2", mode: "embedding" },
      ],
    }),
  ];
  const got = embeddingCandidates(providers, {});
  assert.deepEqual(got.map((c) => c.modelId), ["mixed-embed-1"]);
});

// ── shouldFailover ───────────────────────────────────────────────────────────
//
// The trigger set is a product decision, not an implementation detail: retry on
// provider-side failures (5xx, quota, credentials, transport), never on a
// malformed request the fallback would reject identically.

const failoverCases: { input: { status?: number; transportError?: boolean }; expect: boolean }[] = [
  { input: { transportError: true }, expect: true },
  { input: { status: 500 }, expect: true },
  { input: { status: 502 }, expect: true },
  { input: { status: 503 }, expect: true },
  { input: { status: 504 }, expect: true },
  { input: { status: 429 }, expect: true },
  { input: { status: 401 }, expect: true },
  { input: { status: 403 }, expect: true },
  { input: { status: 400 }, expect: false },
  { input: { status: 404 }, expect: false },
  { input: { status: 422 }, expect: false },
  { input: { status: 200 }, expect: false },
  { input: {}, expect: false },
];

for (const c of failoverCases) {
  test(`shouldFailover(${JSON.stringify(c.input)}) === ${c.expect}`, () => {
    assert.equal(shouldFailover(c.input), c.expect);
  });
}
