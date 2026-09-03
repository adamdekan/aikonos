// Runtime failover (slice 3) for the two parent-side LLM call sites:
// GovernanceBridge.reason (workflow reason steps) and .analyzeImage (vision).
//
// WHY these tests exist: both used to take only the HEAD of the selection chain,
// so a broken tenant-default provider failed the whole call even with a healthy
// tenant fallback configured. These are plain non-streaming calls, so failover is
// a loop — but the loop must respect the SAME trigger set as the streaming proxy
// (shouldFailover), must not retry a request error, must keep the key out of every
// error message, and for vision must never reach a provider that cannot take an
// image.
import { test, mock } from "node:test";
import assert from "node:assert/strict";
import { GovernanceBridge } from "../src/broker/governance.js";

type FetchInput = Parameters<typeof globalThis.fetch>[0];

const urlOf = (input: FetchInput): string =>
  typeof input === "string" ? input : input instanceof URL ? input.toString() : input.url;

function makeNorth() {
  return {
    createTask: () => Promise.resolve({ taskId: "t-1" }),
    approveTask: () => Promise.resolve({ capabilityTokenIds: {} }),
    readWorkspaceFile: () =>
      Promise.resolve({ path: "references/apple.png", mimeType: "image/png", content: new Uint8Array([1, 2]), sizeBytes: 2 }),
  };
}

function makeSouth(providers: unknown[]) {
  const emitLlmUsageCalls: unknown[] = [];
  return {
    emitLlmUsageCalls,
    getLlmProviders: () => Promise.resolve({ providers }),
    emitLlmUsage: (req: unknown) => {
      emitLlmUsageCalls.push(req);
      return Promise.resolve();
    },
  };
}

const cfg = {
  workflowReasonMaxTokens: 2048, maxLlmCallsPerRun: 100, approvalTimeoutMs: 900000,
  egressTimeoutMs: 5000,
} as unknown as import("../src/config.js").Config;

const log = {
  info: () => {},
  warn: () => {},
  error: () => {},
  debug: () => {},
} as unknown as import("../src/log.js").Logger;

const identity = {
  token: "bearer-tok",
  tenantId: "11111111-1111-1111-1111-111111111111",
  userId: "alice@example.com",
  agentId: "alice-agent",
};

const PRIMARY_KEY = "sk-primary-secret";
const FALLBACK_KEY = "sk-fallback-secret";

// The tenant's chat chain: default provider, then the designated fallback.
const chatPrimary = {
  id: "primary",
  name: "primary",
  endpoint: "https://primary.example/v1",
  api: "openai-completions",
  apiKey: PRIMARY_KEY,
  enabled: true,
  isDefault: true,
  isFallback: false,
  models: [{ id: "primary-model" }],
};
const chatFallback = {
  id: "fallback",
  name: "fallback",
  endpoint: "https://fallback.example/v1",
  api: "openai-completions",
  apiKey: FALLBACK_KEY,
  enabled: true,
  isDefault: false,
  isFallback: true,
  models: [{ id: "fallback-model" }],
};

// The tenant's vision chain. The non-vision fallback must never be attempted.
const visionPrimary = { ...chatPrimary, visionCapable: true, isDefaultVision: true };
const visionFallback = { ...chatFallback, visionCapable: true, isDefaultVision: false };
const blindFallback = { ...chatFallback, visionCapable: false, isDefaultVision: false };

function chatBody(content: string, tokensIn = 3, tokensOut = 2): string {
  return JSON.stringify({
    choices: [{ message: { content } }],
    usage: { prompt_tokens: tokensIn, completion_tokens: tokensOut },
  });
}

// routeFetch answers per-host so a test can give each provider its own verdict,
// and records which hosts were actually contacted.
function routeFetch(verdicts: Record<string, () => Response>): { hosts: string[] } {
  const hosts: string[] = [];
  mock.method(globalThis, "fetch", async (input: FetchInput) => {
    const host = new URL(urlOf(input)).hostname;
    hosts.push(host);
    const verdict = verdicts[host];
    assert.ok(verdict, `unexpected provider host contacted: ${host}`);
    return verdict();
  });
  return { hosts };
}

const ok = (content: string): (() => Response) => () =>
  new Response(chatBody(content), { status: 200, headers: { "content-type": "application/json" } });
const status = (code: number, body = "nope"): (() => Response) => () => new Response(body, { status: code });
const boom = (): (() => Response) => () => {
  throw new TypeError("fetch failed");
};

function makeBridge(south: ReturnType<typeof makeSouth>) {
  const clients = { north: makeNorth(), south } as unknown as import("../src/broker/clients.js").BrokerClients;
  return new GovernanceBridge(cfg, clients, identity, async () => true, log, async () => {});
}

test.afterEach(() => {
  mock.restoreAll();
});

// ── reason ────────────────────────────────────────────────────────────────────

test("reason failover: default provider 500 → the tenant fallback answers, and usage is attributed to the fallback", async () => {
  const { hosts } = routeFetch({
    "primary.example": status(500, "primary exploded"),
    "fallback.example": ok("42"),
  });
  const south = makeSouth([chatPrimary, chatFallback]);

  const result = await makeBridge(south).reason("what is the answer?");

  assert.equal(result.ok, true, `expected ok, got error: ${result.error}`);
  assert.equal(result.output, "42");
  assert.deepEqual(hosts, ["primary.example", "fallback.example"], "both providers must have been attempted, in order");
  assert.equal(south.emitLlmUsageCalls.length, 1, "exactly one usage event — for the provider that actually served");
  assert.deepEqual(south.emitLlmUsageCalls[0], {
    tenantId: identity.tenantId,
    userId: identity.userId,
    agentId: identity.agentId,
    provider: "fallback",
    model: "fallback-model",
    tokensIn: 3,
    tokensOut: 2,
    cacheRead: 0,
    cacheWrite: 0,
    cost: 0,
    // No run context on this bridge (built without a usageRunId) — the emit
    // still records, attributed by source alone.
    runId: "",
    sessionId: "",
    source: "reason",
    quantity: 0,
    unit: "",
  });
});

for (const badStatus of [401, 403, 429, 500, 502]) {
  test(`reason failover: default provider ${badStatus} → retried on the tenant fallback`, async () => {
    const { hosts } = routeFetch({
      "primary.example": status(badStatus),
      "fallback.example": ok("42"),
    });
    const result = await makeBridge(makeSouth([chatPrimary, chatFallback])).reason("q");

    assert.equal(result.ok, true, `a ${badStatus} must fail over, got error: ${result.error}`);
    assert.deepEqual(hosts, ["primary.example", "fallback.example"]);
  });
}

for (const requestStatus of [400, 404, 422]) {
  test(`reason failover: default provider ${requestStatus} → NO failover, the fallback is never contacted`, async () => {
    // WHY: a request error means the fallback would reject the identical body
    // identically. Retrying only burns the fallback's quota and latency.
    const { hosts } = routeFetch({
      "primary.example": status(requestStatus, "bad request"),
      "fallback.example": ok("42"),
    });
    const result = await makeBridge(makeSouth([chatPrimary, chatFallback])).reason("q");

    assert.equal(result.ok, false);
    assert.match(result.error ?? "", new RegExp(String(requestStatus)));
    assert.deepEqual(hosts, ["primary.example"], "a request error must not reach the fallback");
  });
}

test("reason failover: a transport failure on the default provider is retried on the fallback", async () => {
  const { hosts } = routeFetch({
    "primary.example": boom(),
    "fallback.example": ok("42"),
  });
  const result = await makeBridge(makeSouth([chatPrimary, chatFallback])).reason("q");

  assert.equal(result.ok, true, `a transport failure must fail over, got error: ${result.error}`);
  assert.deepEqual(hosts, ["primary.example", "fallback.example"]);
});

test("reason failover: whole chain fails → the LAST error surfaces, and no key appears in it", async () => {
  // WHY: an exhausted chain must present exactly what a single failing provider
  // presented before (same error type, same ok:false shape) — and the key-leak
  // guard must survive: a transport failure never interpolates the caught error.
  routeFetch({
    "primary.example": status(500, "primary exploded"),
    "fallback.example": boom(),
  });
  const result = await makeBridge(makeSouth([chatPrimary, chatFallback])).reason("q");

  assert.equal(result.ok, false);
  assert.match(result.error ?? "", /fallback/, "the surfaced error must be the LAST provider's, not the first's");
  assert.match(result.error ?? "", /connection failed/);
  for (const secret of [PRIMARY_KEY, FALLBACK_KEY]) {
    assert.ok(!(result.error ?? "").includes(secret), "no api key may appear in a surfaced error");
  }
});

test("reason failover: a bad output_schema parse is never retried on the fallback", async () => {
  // WHY: the model answered — the provider works fine. A different provider is not
  // a fix for output that does not match the schema, and retrying would double the
  // cost of every malformed reason step.
  const { hosts } = routeFetch({
    "primary.example": ok("not json"),
    "fallback.example": ok('{"email":"a@b.com"}'),
  });
  const result = await makeBridge(makeSouth([chatPrimary, chatFallback])).reason("extract", { type: "object" });

  assert.equal(result.ok, false);
  assert.match(result.error ?? "", /did not match output_schema/i);
  assert.deepEqual(hosts, ["primary.example"], "a schema-parse failure must not reach the fallback");
});

test("reason failover: no fallback configured → a 500 surfaces unchanged (single-provider behavior preserved)", async () => {
  const { hosts } = routeFetch({ "primary.example": status(500, "primary exploded") });
  const result = await makeBridge(makeSouth([chatPrimary])).reason("q");

  assert.equal(result.ok, false);
  assert.match(result.error ?? "", /500/);
  assert.deepEqual(hosts, ["primary.example"]);
});

// ── analyzeImage ──────────────────────────────────────────────────────────────

test("vision failover: default-vision provider 500 → a vision-capable fallback answers", async () => {
  const { hosts } = routeFetch({
    "primary.example": status(500, "primary exploded"),
    "fallback.example": ok("a red apple"),
  });
  const south = makeSouth([visionPrimary, visionFallback]);

  const result = await makeBridge(south).analyzeImage("references/apple.png", "what fruit?");

  assert.equal(result.ok, true, `expected ok, got error: ${result.error}`);
  assert.equal(result.text, "a red apple");
  assert.deepEqual(hosts, ["primary.example", "fallback.example"]);
  assert.equal(south.emitLlmUsageCalls.length, 1);
  assert.deepEqual(south.emitLlmUsageCalls[0], {
    tenantId: identity.tenantId,
    userId: identity.userId,
    agentId: identity.agentId,
    provider: "fallback",
    model: "fallback-model",
    tokensIn: 3,
    tokensOut: 2,
    cacheRead: 0,
    cacheWrite: 0,
    cost: 0,
    // No run context on this bridge (built without a usageRunId) — the emit
    // still records, attributed by source alone.
    runId: "",
    sessionId: "",
    source: "vision",
    quantity: 0,
    unit: "",
  });
});

test("vision failover: a NON-vision-capable fallback is never attempted — vision stays fail-closed", async () => {
  // WHY: the load-bearing vision invariant. A provider that cannot process an
  // image would fail by construction, so visionCandidates filters it out of the
  // chain entirely rather than "best-efforting" an image at it. The default-vision
  // provider's 500 must surface, NOT be papered over by a blind fallback.
  const { hosts } = routeFetch({
    "primary.example": status(500, "primary exploded"),
    "fallback.example": ok("should never be reached"),
  });
  const result = await makeBridge(makeSouth([visionPrimary, blindFallback])).analyzeImage("references/apple.png");

  assert.equal(result.ok, false);
  assert.match(result.error ?? "", /500/);
  assert.deepEqual(hosts, ["primary.example"], "a non-vision-capable provider must never receive an image");
});

test("vision failover: default-vision provider 400 → NO failover, the fallback is never contacted", async () => {
  const { hosts } = routeFetch({
    "primary.example": status(400, "bad request"),
    "fallback.example": ok("a red apple"),
  });
  const result = await makeBridge(makeSouth([visionPrimary, visionFallback])).analyzeImage("references/apple.png");

  assert.equal(result.ok, false);
  assert.match(result.error ?? "", /400/);
  assert.deepEqual(hosts, ["primary.example"]);
});

test("vision failover: whole chain fails → the LAST error surfaces, and no key appears in it", async () => {
  routeFetch({
    "primary.example": boom(),
    "fallback.example": status(503, "fallback down"),
  });
  const result = await makeBridge(makeSouth([visionPrimary, visionFallback])).analyzeImage("references/apple.png");

  assert.equal(result.ok, false);
  assert.match(result.error ?? "", /fallback/, "the surfaced error must be the LAST provider's");
  assert.match(result.error ?? "", /503/);
  for (const secret of [PRIMARY_KEY, FALLBACK_KEY]) {
    assert.ok(!(result.error ?? "").includes(secret), "no api key may appear in a surfaced error");
  }
});

test("vision failover: the second candidate gets its OWN rate-limit pre-check before it is attempted", async () => {
  // WHY: each provider is its own rate-limit and spend-cap subject (same property
  // the egress proxy enforces per attempt) — a failover must not spend the
  // fallback's budget under the head's clearance.
  routeFetch({
    "primary.example": status(500, "primary exploded"),
    "fallback.example": ok("a red apple"),
  });
  const seen: Array<[string, string, string, string | undefined]> = [];
  const clients = { north: makeNorth(), south: makeSouth([visionPrimary, visionFallback]) } as unknown as
    import("../src/broker/clients.js").BrokerClients;
  const bridge = new GovernanceBridge(
    cfg,
    clients,
    identity,
    async () => true,
    log,
    async (tenantId, agentId, provider, userId) => {
      seen.push([tenantId, agentId, provider, userId]);
    },
  );

  const result = await bridge.analyzeImage("references/apple.png");

  assert.equal(result.ok, true, `expected ok, got error: ${result.error}`);
  assert.deepEqual(
    seen,
    [
      [identity.tenantId, identity.agentId, "primary.example", identity.userId],
      [identity.tenantId, identity.agentId, "fallback.example", identity.userId],
    ],
    "each attempted candidate must be pre-gated on its own hostname, exactly once",
  );
});
