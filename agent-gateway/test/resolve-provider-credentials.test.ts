// CP1: the gateway's provider
// credential resolver must fail loud instead of silently falling back to a
// broken env config.
//
// WHY this exists: before this fix, resolveCredentials (previously an inline
// closure in server.ts, not independently testable) silently returned the env
// OpenRouter fallback whenever a resolved DB provider had no key in Vault —
// even when that env fallback's own key was also empty. The result was a
// spawned child whose every LLM call 401/400s with nothing surfaced on the
// /agui stream (on-prem incident). resolveProviderCredentials is the extracted,
// directly-testable version of that resolver.
import { test, mock } from "node:test";
import assert from "node:assert/strict";
import { resolveProviderCredentials, type ProviderCredentials } from "../src/pi/session.js";
import { log } from "../src/log.js";
import type { LlmProvider } from "../gen/ts/proto/broker.js";

function mkModel(id: string) {
  return { id, mode: "", priceIn: 0, priceOut: 0, priceCacheRead: 0, priceCacheWrite: 0, contextWindow: 0, maxTokens: 0 };
}

function mkProvider(p: Partial<LlmProvider> & { id: string }): LlmProvider {
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
    models: [mkModel("m1")],
    visionCapable: false,
    isDefaultVision: false,
    isFallback: false,
    priceInMicrosPerMtok: 0,
    priceOutMicrosPerMtok: 0,
    config: {},
    ...p,
  };
}

function fakeSouth(providers: LlmProvider[]) {
  return { getLlmProviders: async () => ({ providers, defaults: {} }) };
}

function failingSouth(err: Error) {
  return { getLlmProviders: async () => { throw err; } };
}

// ── Case A: resolved provider has no key → throw, no env fallback ─────────────

test("resolveProviderCredentials Case A: resolved provider apiKey empty → throws naming provider id + remediation, no env fallback", async () => {
  const providers = [mkProvider({ id: "openai-prod", isDefault: true, apiKey: "" })];
  const south = fakeSouth(providers);
  const cfg = { llmModel: "anthropic/claude-sonnet-4.6", openrouterApiKey: "should-not-be-used" };

  await assert.rejects(
    () => resolveProviderCredentials(cfg, south, "tenant-1"),
    (err: unknown) => {
      assert.ok(err instanceof Error);
      assert.match(err.message, /openai-prod/);
      assert.match(err.message, /re-enter/i);
      assert.match(err.message, /Admin.*LLM Providers/);
      return true;
    },
  );
});

test("resolveProviderCredentials Case A (regression): resolved provider WITH a key resolves normally", async () => {
  const providers = [mkProvider({ id: "openai-prod", isDefault: true, apiKey: "real-key", models: [mkModel("gpt-5.4-nano")] })];
  const south = fakeSouth(providers);
  const cfg = { llmModel: "anthropic/claude-sonnet-4.6", openrouterApiKey: "" };

  const creds = await resolveProviderCredentials(cfg, south, "tenant-1");
  assert.equal(creds.apiKey, "real-key");
  assert.equal(creds.modelId, "gpt-5.4-nano");
  assert.ok(creds.modelAllowlist.includes("gpt-5.4-nano"));
});

// ── Case B: zero enabled providers → env fallback; empty env key → throw ──────

test("resolveProviderCredentials Case B: zero enabled providers + empty AIKONOS_OPENROUTER_API_KEY → throws naming the env var", async () => {
  const south = fakeSouth([]);
  const cfg = { llmModel: "anthropic/claude-sonnet-4.6", openrouterApiKey: "" };

  await assert.rejects(
    () => resolveProviderCredentials(cfg, south, "tenant-1"),
    (err: unknown) => {
      assert.ok(err instanceof Error);
      assert.match(err.message, /AIKONOS_OPENROUTER_API_KEY/);
      return true;
    },
  );
});

test("resolveProviderCredentials Case B: zero enabled providers + valid env key → resolves the env fallback", async () => {
  const south = fakeSouth([]);
  const cfg = { llmModel: "anthropic/claude-sonnet-4.6", openrouterApiKey: "sk-real" };

  const creds = await resolveProviderCredentials(cfg, south, "tenant-1");
  assert.equal(creds.apiKey, "sk-real");
  assert.equal(creds.modelId, "anthropic/claude-sonnet-4.6");
  assert.deepEqual(creds.modelAllowlist, ["anthropic/claude-sonnet-4.6"]);
});

// ── Case D: getLlmProviders RPC transport failure ──────────────────────────
//
// Distinct from Case B (zero *enabled* providers, a real broker answer): here
// the RPC itself never returned. Correction: fail OPEN to the env
// fallback only when that fallback key is present (deliberate, logged
// fail-open for a transient broker blip); otherwise throw loud naming BOTH
// facts — broker unreachable AND the env var being empty — so an operator
// doesn't mistake this for Case B's "no providers configured" message.

test("resolveProviderCredentials Case D: RPC transport failure + empty env key → throws naming broker-unreachable AND the empty env var", async () => {
  const south = failingSouth(new Error("ECONNREFUSED"));

  await assert.rejects(
    () => resolveProviderCredentials({ llmModel: "m", openrouterApiKey: "" }, south, "tenant-1"),
    (err: unknown) => {
      assert.ok(err instanceof Error);
      assert.match(err.message, /llm credentials unavailable/);
      assert.match(err.message, /broker unreachable/i);
      assert.match(err.message, /AIKONOS_OPENROUTER_API_KEY/);
      return true;
    },
  );
});

test("resolveProviderCredentials Case D: RPC transport failure + valid env key → fails OPEN to the env fallback (deliberate)", async () => {
  const south = failingSouth(new Error("ECONNREFUSED"));

  const creds = await resolveProviderCredentials({ llmModel: "m", openrouterApiKey: "sk-real" }, south, "tenant-1");
  assert.equal(creds.apiKey, "sk-real");
  assert.equal(creds.modelId, "m");
  assert.deepEqual(creds.modelAllowlist, ["m"]);
});

// ── Invariant: no path may return an empty apiKey ──────────────────────────

// ── Fallback chain (tenant is_fallback provider) ───────────────────────────
//
// The resolver returns the whole ordered chain, not one pinned provider: the
// egress proxy retries the tail on a failover-worthy upstream failure. A
// keyless provider drops out of the chain but must still reach the operator's
// log — that is the on-prem Vault-wipe mode degrading gracefully, not silently.

test("resolveProviderCredentials: keyless primary falls through to the keyed fallback, with a warning", async () => {
  const warn = mock.method(log, "warn", () => {});
  try {
    const providers = [
      mkProvider({ id: "openai-prod", isDefault: true, apiKey: "", models: [mkModel("gpt-5")] }),
      mkProvider({ id: "anthropic-fb", isFallback: true, apiKey: "fb-key", models: [mkModel("sonnet")] }),
    ];
    const creds: ProviderCredentials = await resolveProviderCredentials(
      { llmModel: "m", openrouterApiKey: "" },
      fakeSouth(providers),
      "tenant-1",
    );

    assert.equal(creds.apiKey, "fb-key");
    assert.equal(creds.modelId, "sonnet");
    assert.deepEqual(creds.fallbacks, []);

    const warned = warn.mock.calls.map((c) => String(c.arguments[1]));
    assert.equal(warned.length, 1);
    assert.match(warned[0], /openai-prod/);
    assert.match(warned[0], /re-enter it in Admin → LLM Providers/);
  } finally {
    warn.mock.restore();
  }
});

test("resolveProviderCredentials: every candidate keyless → still throws the grep-contract string, naming all tried", async () => {
  const providers = [
    mkProvider({ id: "openai-prod", isDefault: true, apiKey: "" }),
    mkProvider({ id: "anthropic-fb", isFallback: true, apiKey: "" }),
  ];

  await assert.rejects(
    () => resolveProviderCredentials({ llmModel: "m", openrouterApiKey: "should-not-be-used" }, fakeSouth(providers), "tenant-1"),
    (err: unknown) => {
      assert.ok(err instanceof Error);
      // Pinned by contracts.json + scripts/compose-verify.sh.
      assert.match(err.message, /llm credentials unavailable/);
      assert.match(err.message, /openai-prod/);
      assert.match(err.message, /anthropic-fb/);
      assert.match(err.message, /re-enter/i);
      assert.match(err.message, /Admin.*LLM Providers/);
      return true;
    },
  );
});

test("resolveProviderCredentials: fallbacks carry the chain tail in selection order", async () => {
  const providers = [
    mkProvider({ id: "assigned", apiKey: "a-key", models: [mkModel("assigned-m")] }),
    mkProvider({ id: "def", isDefault: true, apiKey: "d-key", models: [mkModel("def-m")] }),
    mkProvider({ id: "fb", isFallback: true, apiKey: "f-key", models: [mkModel("fb-m")] }),
  ];
  const creds: ProviderCredentials = await resolveProviderCredentials(
    { llmModel: "m", openrouterApiKey: "" },
    fakeSouth(providers),
    "tenant-1",
    { model: "assigned-m", approvalMode: "", skills: [], preferredProvider: "assigned" },
  );

  assert.equal(creds.modelId, "assigned-m");
  assert.equal(creds.apiKey, "a-key");
  assert.deepEqual(
    creds.fallbacks.map((f) => [f.modelId, f.apiKey]),
    [
      ["def-m", "d-key"],
      ["fb-m", "f-key"],
    ],
  );
});

test("resolveProviderCredentials: modelAllowlist is the union over the whole chain, keyless candidates included", async () => {
  const warn = mock.method(log, "warn", () => {});
  try {
    const providers = [
      // Keyless, but session-plan.ts resolves its model from the same chain
      // without seeing keys — its models must be allowlisted or the spawn-time
      // coherence guard would reject the plan.
      mkProvider({ id: "def", isDefault: true, apiKey: "", models: [mkModel("def-m1"), mkModel("def-m2")] }),
      mkProvider({ id: "fb", isFallback: true, apiKey: "f-key", models: [mkModel("fb-m1")] }),
    ];
    const creds: ProviderCredentials = await resolveProviderCredentials(
      { llmModel: "m", openrouterApiKey: "" },
      fakeSouth(providers),
      "tenant-1",
    );
    assert.deepEqual(creds.modelAllowlist, ["def-m1", "def-m2", "fb-m1"]);
  } finally {
    warn.mock.restore();
  }
});

test("resolveProviderCredentials: enabled providers with no usable model throw instead of resolving", async () => {
  const providers = [mkProvider({ id: "def", isDefault: true, models: [] })];

  await assert.rejects(
    () => resolveProviderCredentials({ llmModel: "m", openrouterApiKey: "sk-real" }, fakeSouth(providers), "tenant-1"),
    (err: unknown) => {
      assert.ok(err instanceof Error);
      assert.match(err.message, /llm credentials unavailable/);
      assert.match(err.message, /usable model/);
      return true;
    },
  );
});

test("resolveProviderCredentials invariant: every non-throwing path returns a non-empty apiKey", async () => {
  // Case C (enabled provider with a key).
  const withProvider = await resolveProviderCredentials(
    { llmModel: "m", openrouterApiKey: "" },
    fakeSouth([mkProvider({ id: "p1", isDefault: true, apiKey: "real-key" })]),
    "tenant-1",
  );
  assert.notEqual(withProvider.apiKey, "");

  // Case B (zero providers, env fallback).
  const envFallback = await resolveProviderCredentials(
    { llmModel: "m", openrouterApiKey: "sk-real" },
    fakeSouth([]),
    "tenant-1",
  );
  assert.notEqual(envFallback.apiKey, "");

  // Case D (RPC failure, env fallback).
  const rpcFailureFallback = await resolveProviderCredentials(
    { llmModel: "m", openrouterApiKey: "sk-real" },
    failingSouth(new Error("ECONNREFUSED")),
    "tenant-1",
  );
  assert.notEqual(rpcFailureFallback.apiKey, "");
});
