// CP5: GovernanceBridge.analyzeImage — parent-side vision handler.
//
// WHY these tests exist: analyzeImage is the only place the real provider API
// key and the raw image bytes ever meet. It must (a) resolve the tenant-default
// vision provider via south.getLlmProviders + visionCandidates, failing closed
// with no chat-provider fallback when none is assigned; (b) read the file via
// north.readWorkspaceFile using the session's own bearer token — no new identity
// path; (c) refuse non-image mime types before ever calling the vision provider;
// (d) call callVisionProvider (CP4) only once both checks pass.
//
// callVisionProvider does a real fetch() internally (CP4, already unit-tested
// against its own dialect-building logic in vision.test.ts) — here we stub
// globalThis.fetch so no real HTTP occurs, and assert on call counts to prove
// the ordering/fail-closed invariants rather than re-testing dialect shaping.
import { test, mock } from "node:test";
import assert from "node:assert/strict";
import { GovernanceBridge } from "../src/broker/governance.js";

function makeNorth(opts: { readWorkspaceFile?: (req: Record<string, unknown>, token?: string) => Promise<unknown> } = {}) {
  const readCalls: Array<{ req: Record<string, unknown>; token?: string }> = [];
  return {
    readCalls,
    createTask: () => Promise.resolve({ taskId: "t-1" }),
    approveTask: () => Promise.resolve({ capabilityTokenIds: {} }),
    sendEnvelope: () => Promise.resolve({ envelopeId: "env-1" }),
    getWorkflow: () => Promise.resolve({ definitionJson: "{}" }),
    listWorkflows: () => Promise.resolve({ items: [] }),
    publishWorkflow: () => Promise.resolve({ visibilityKind: "private", groups: [] }),
    saveWorkflow: () => Promise.resolve({ workflowId: "wf-1", lineageId: "lin-1", version: 1 }),
    proposeWorkflowVersion: () => Promise.resolve({ version: 2 }),
    readWorkspaceFile: (req: Record<string, unknown>, token?: string) => {
      readCalls.push({ req, token });
      return (opts.readWorkspaceFile ?? (() => Promise.resolve({ path: "x", mimeType: "image/png", content: new Uint8Array([1, 2, 3]), sizeBytes: 3 })))(req, token);
    },
  };
}

function makeSouth(opts: { providers?: unknown[] } = {}) {
  const getLlmProvidersCalls: unknown[] = [];
  const emitLlmUsageCalls: unknown[] = [];
  return {
    getLlmProvidersCalls,
    emitLlmUsageCalls,
    createGatewayTask: () => Promise.resolve({ taskId: "t-1" }),
    approveGatewayTask: () => Promise.resolve({ capabilityTokenIds: {} }),
    submitPlan: () => Promise.resolve({ outcome: 1, capabilityTokenIds: { 1: "tok" }, violations: [], steps: [] }),
    invokeTool: () => Promise.resolve({ success: true, result: null, error: "", costUnitsConsumed: 0 }),
    emitStatus: () => Promise.resolve(),
    getWorkflow: () => Promise.resolve({ definitionJson: "{}" }),
    listWorkflows: () => Promise.resolve({ items: [] }),
    publishWorkflow: () => Promise.resolve({ visibilityKind: "private", groups: [] }),
    saveWorkflow: () => Promise.resolve({ workflowId: "wf-1", lineageId: "lin-1", version: 1 }),
    proposeWorkflowVersion: () => Promise.resolve({ version: 2 }),
    getLlmProviders: (req: unknown) => {
      getLlmProvidersCalls.push(req);
      return Promise.resolve({ providers: opts.providers ?? [] });
    },
    emitLlmUsage: (req: unknown) => {
      emitLlmUsageCalls.push(req);
      return Promise.resolve();
    },
  };
}

const cfg = {
  gatewaySpiffeId: "spiffe://aikonos.com/agent-gateway",
  llmModel: "",
  brokerNorthAddr: "",
  brokerSouthAddr: "",
  brokerServerName: "",
  tlsCert: "",
  tlsKey: "",
  tlsCa: "",
  port: 8080,
  defaultTenantId: "11111111-1111-1111-1111-111111111111",
  keycloakUrl: "",
  keycloakRealm: "",
  keycloakClient: "",
  schedulerEnabled: false,
  schedulerTickMs: 30000,
  schedulerClaimLimit: 10,
  schedulerRunTimeoutMs: 180000,
  agentForUserOverrides: {},
  openrouterApiKey: "",
  oidcIssuer: "",
  oidcJwksUrl: "",
  oidcAudience: "",
  egressTimeoutMs: 120000,
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

const visionProvider = {
  id: "openai",
  name: "openai",
  endpoint: "https://api.openai.com/v1",
  api: "openai-completions",
  apiKey: "sk-test",
  enabled: true,
  visionCapable: true,
  isDefaultVision: true,
  models: [{ id: "gpt-4o" }],
};

test.afterEach(() => {
  mock.restoreAll();
});

test("analyzeImage: happy path returns the vision text", async () => {
  mock.method(globalThis, "fetch", async () =>
    new Response(JSON.stringify({ choices: [{ message: { content: "a red apple" } }] }), {
      status: 200,
      headers: { "content-type": "application/json" },
    }),
  );

  const north = makeNorth();
  const south = makeSouth({ providers: [visionProvider] });
  const clients = { north, south } as unknown as import("../src/broker/clients.js").BrokerClients;
  const bridge = new GovernanceBridge(cfg, clients, identity, async () => true, log);

  const result = await bridge.analyzeImage("references/apple.png", "what fruit is this?");

  assert.equal(result.ok, true, `expected ok, got error: ${result.error}`);
  assert.equal(result.text, "a red apple");
  assert.equal(north.readCalls.length, 1);
  assert.equal(north.readCalls[0].req.path, "references/apple.png");
  assert.equal(north.readCalls[0].token, "bearer-tok", "must reuse the session's own bearer token");
});

test("analyzeImage: no vision provider assigned returns typed error, never calls readWorkspaceFile", async () => {
  const fetchMock = mock.method(globalThis, "fetch", async () => {
    throw new Error("must not be called");
  });

  const north = makeNorth();
  const south = makeSouth({ providers: [] }); // no default-vision provider
  const clients = { north, south } as unknown as import("../src/broker/clients.js").BrokerClients;
  const bridge = new GovernanceBridge(cfg, clients, identity, async () => true, log);

  const result = await bridge.analyzeImage("references/apple.png");

  assert.equal(result.ok, false);
  assert.match(result.error ?? "", /no vision provider assigned/i);
  assert.equal(north.readCalls.length, 0, "readWorkspaceFile must not be called when no vision provider is assigned");
  assert.equal(fetchMock.mock.calls.length, 0, "the vision provider must never be called");
});

test("analyzeImage: non-image mime type returns typed error, never calls the vision provider", async () => {
  const fetchMock = mock.method(globalThis, "fetch", async () => {
    throw new Error("must not be called");
  });

  const north = makeNorth({
    readWorkspaceFile: () =>
      Promise.resolve({ path: "notes.txt", mimeType: "text/plain", content: new Uint8Array([1]), sizeBytes: 1 }),
  });
  const south = makeSouth({ providers: [visionProvider] });
  const clients = { north, south } as unknown as import("../src/broker/clients.js").BrokerClients;
  const bridge = new GovernanceBridge(cfg, clients, identity, async () => true, log);

  const result = await bridge.analyzeImage("notes.txt");

  assert.equal(result.ok, false);
  assert.match(result.error ?? "", /not an image/i);
  assert.equal(north.readCalls.length, 1, "the file must still be read to inspect its mime type");
  assert.equal(fetchMock.mock.calls.length, 0, "the vision provider must never be called for non-image bytes");
});

// ── Spend-caps CP3: rate-limit pre-gate + usage emission ───────────────────────

test("analyzeImage: a rate-limit pre-gate denial fails the call and never reads the file or calls the provider", async () => {
  const fetchMock = mock.method(globalThis, "fetch", async () => {
    throw new Error("must not be called");
  });

  const north = makeNorth();
  const south = makeSouth({ providers: [visionProvider] });
  const clients = { north, south } as unknown as import("../src/broker/clients.js").BrokerClients;
  const rateLimitCalls: unknown[] = [];
  const denyChecker = async (tenantId: string, agentId: string, provider: string) => {
    rateLimitCalls.push([tenantId, agentId, provider]);
    throw new Error("rate limit exceeded: spend_agent");
  };
  const bridge = new GovernanceBridge(cfg, clients, identity, async () => true, log, denyChecker);

  const result = await bridge.analyzeImage("references/apple.png");

  assert.equal(result.ok, false);
  assert.match(result.error ?? "", /rate limit exceeded: spend_agent/);
  assert.equal(north.readCalls.length, 0, "a pre-gate denial must not read the file");
  assert.equal(fetchMock.mock.calls.length, 0, "the provider must never be called after a pre-gate denial");
  // Keyed by hostname (not provider.id) — matches the egress proxy's
  // convention (new URL(upstreamBaseUrl).hostname), so a per-provider
  // rate-limit policy matches regardless of pre-gate call site.
  assert.deepEqual(rateLimitCalls, [[identity.tenantId, identity.agentId, "api.openai.com"]]);
  assert.equal(south.emitLlmUsageCalls.length, 0, "a denied call must not emit usage");
});

test("analyzeImage: a successful call emits EmitLlmUsage with identity, provider/model, and tokens; cost is 0", async () => {
  mock.method(globalThis, "fetch", async () =>
    new Response(
      JSON.stringify({ choices: [{ message: { content: "a red apple" } }], usage: { prompt_tokens: 30, completion_tokens: 8 } }),
      { status: 200, headers: { "content-type": "application/json" } },
    ),
  );

  const north = makeNorth();
  const south = makeSouth({ providers: [visionProvider] });
  const clients = { north, south } as unknown as import("../src/broker/clients.js").BrokerClients;
  // usageRunId ("run-v1") is the last ctor arg — attribution for the run whose
  // vision call this is.
  const bridge = new GovernanceBridge(cfg, clients, identity, async () => true, log, async () => {}, undefined, "run-v1");

  const result = await bridge.analyzeImage("references/apple.png", "what fruit is this?");

  assert.equal(result.ok, true, `expected ok, got error: ${result.error}`);
  assert.equal(south.emitLlmUsageCalls.length, 1);
  assert.deepEqual(south.emitLlmUsageCalls[0], {
    tenantId: identity.tenantId,
    userId: identity.userId,
    agentId: identity.agentId,
    provider: visionProvider.id,
    model: "gpt-4o",
    tokensIn: 30,
    tokensOut: 8,
    cacheRead: 0,
    cacheWrite: 0,
    cost: 0,
    runId: "run-v1",
    // A parent-side call has no webui chat session of its own.
    sessionId: "",
    source: "vision",
    quantity: 0,
    unit: "",
  });
});

test("Spend-caps CP4 analyzeImage: the pre-gate checker's 4th argument is the run identity's userId, including the external-invoke svc-<agentId> shape", async () => {
  // WHY: mirrors the reason-step test — external/scheduled identities carry
  // userId = "svc-<agentId>" (external/core.ts); the pre-gate call must forward
  // it unchanged so a per-user (here, per-svc-identity) spend cap can match.
  mock.method(globalThis, "fetch", async () =>
    new Response(JSON.stringify({ choices: [{ message: { content: "a red apple" } }] }), {
      status: 200,
      headers: { "content-type": "application/json" },
    }),
  );

  const svcIdentity = { ...identity, userId: "svc-alice-agent-uuid" };
  const north = makeNorth();
  const south = makeSouth({ providers: [visionProvider] });
  const clients = { north, south } as unknown as import("../src/broker/clients.js").BrokerClients;
  const rateLimitCalls: unknown[] = [];
  const allowChecker = async (tenantId: string, agentId: string, provider: string, userId?: string) => {
    rateLimitCalls.push([tenantId, agentId, provider, userId]);
  };
  const bridge = new GovernanceBridge(cfg, clients, svcIdentity, async () => true, log, allowChecker);

  const result = await bridge.analyzeImage("references/apple.png");

  assert.equal(result.ok, true, `expected ok, got error: ${result.error}`);
  assert.deepEqual(rateLimitCalls, [[svcIdentity.tenantId, svcIdentity.agentId, "api.openai.com", "svc-alice-agent-uuid"]]);
});

test("analyzeImage: an emit failure never fails an otherwise-successful call", async () => {
  mock.method(globalThis, "fetch", async () =>
    new Response(JSON.stringify({ choices: [{ message: { content: "a red apple" } }] }), {
      status: 200,
      headers: { "content-type": "application/json" },
    }),
  );

  const north = makeNorth();
  const south = makeSouth({ providers: [visionProvider] });
  south.emitLlmUsage = () => Promise.reject(new Error("broker down"));
  const clients = { north, south } as unknown as import("../src/broker/clients.js").BrokerClients;
  const bridge = new GovernanceBridge(cfg, clients, identity, async () => true, log, async () => {});

  const result = await bridge.analyzeImage("references/apple.png");

  assert.equal(result.ok, true, `emit failure must not fail the call, got error: ${result.error}`);
});

// ── Upstream timeout wiring ───────────────────────────────────────────────────

test("analyzeImage: a hung vision provider is aborted by cfg.egressTimeoutMs, not waited on forever", async () => {
  // WHY: vision.ts owning an AbortController is only half the fix — the call site
  // has to actually pass a timeout, otherwise a hung provider still blocks the run
  // (and the child waiting on the IPC reply) indefinitely.
  mock.method(globalThis, "fetch", (_url: string, init?: RequestInit) =>
    new Promise((_resolve, reject) => {
      init?.signal?.addEventListener("abort", () => reject(new DOMException("aborted", "AbortError")));
    }),
  );

  const north = makeNorth();
  const south = makeSouth({ providers: [visionProvider] });
  const clients = { north, south } as unknown as import("../src/broker/clients.js").BrokerClients;
  const fastCfg = { ...cfg, egressTimeoutMs: 30 };
  const bridge = new GovernanceBridge(fastCfg, clients, identity, async () => true, log);

  // vision.ts unref()s its abort timer (a pending LLM call must never hold the
  // process open), so hold the loop open here or node exits before it fires.
  const keepAlive = setTimeout(() => {}, 5000);
  const result = await bridge
    .analyzeImage("references/apple.png")
    .finally(() => clearTimeout(keepAlive));

  assert.equal(result.ok, false);
  assert.match(result.error ?? "", /timed out after 30ms/);
  assert.ok(!(result.error ?? "").includes("sk-test"), "the error must never carry the key");
});

// ── Per-run LLM-call budget seam (EgressProxy property 9) ─────────────────────

test("analyzeImage: a spent per-run LLM-call budget denies before any RPC, read, or fetch", async () => {
  // WHY: analyzeImage bypasses the egress proxy, so without the shared counter it
  // would be an unbounded side channel around the per-run cap. Denial must cost
  // nothing — not even the workspace read.
  const fetchMock = mock.method(globalThis, "fetch", async () => {
    throw new Error("must not be called");
  });

  const north = makeNorth();
  const south = makeSouth({ providers: [visionProvider] });
  const clients = { north, south } as unknown as import("../src/broker/clients.js").BrokerClients;
  const bridge = new GovernanceBridge(cfg, clients, identity, async () => true, log, async () => {}, () => false);

  const result = await bridge.analyzeImage("references/apple.png");

  assert.equal(result.ok, false);
  assert.match(result.error ?? "", /llm call budget exceeded for this run/);
  assert.equal(south.getLlmProvidersCalls.length, 0, "no provider resolution for an over-budget call");
  assert.equal(north.readCalls.length, 0, "no workspace read for an over-budget call");
  assert.equal(fetchMock.mock.calls.length, 0, "no provider fetch for an over-budget call");
  assert.equal(south.emitLlmUsageCalls.length, 0, "a denied call must not emit usage");
});

test("analyzeImage: the budget is charged ONCE per call, not once per vision candidate", async () => {
  mock.method(globalThis, "fetch", async () =>
    new Response(JSON.stringify({ choices: [{ message: { content: "a red apple" } }] }), {
      status: 200,
      headers: { "content-type": "application/json" },
    }),
  );

  const north = makeNorth();
  const south = makeSouth({ providers: [visionProvider] });
  const clients = { north, south } as unknown as import("../src/broker/clients.js").BrokerClients;
  let bookings = 0;
  const bridge = new GovernanceBridge(cfg, clients, identity, async () => true, log, async () => {}, () => {
    bookings++;
    return true;
  });

  assert.equal((await bridge.analyzeImage("references/apple.png")).ok, true);
  assert.equal(bookings, 1);
});
