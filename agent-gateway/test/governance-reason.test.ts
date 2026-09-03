// CP-R3: GovernanceBridge.reason — parent-side bounded LLM call for workflow
// `reason` steps. Mirrors governance-analyze-image.test.ts (CP5 precedent):
// resolves the tenant-default chat provider via south.getLlmProviders, fails
// closed with no provider assigned, never touches the forked child.
import { test, mock } from "node:test";
import assert from "node:assert/strict";
import { GovernanceBridge } from "../src/broker/governance.js";

function makeNorth() {
  return {
    createTask: () => Promise.resolve({ taskId: "t-1" }),
    approveTask: () => Promise.resolve({ capabilityTokenIds: {} }),
    sendEnvelope: () => Promise.resolve({ envelopeId: "env-1" }),
    getWorkflow: () => Promise.resolve({ definitionJson: "{}" }),
    listWorkflows: () => Promise.resolve({ items: [] }),
    publishWorkflow: () => Promise.resolve({ visibilityKind: "private", groups: [] }),
    saveWorkflow: () => Promise.resolve({ workflowId: "wf-1", lineageId: "lin-1", version: 1 }),
    proposeWorkflowVersion: () => Promise.resolve({ version: 2 }),
    readWorkspaceFile: () =>
      Promise.resolve({ path: "x", mimeType: "image/png", content: new Uint8Array([1]), sizeBytes: 1 }),
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
  workflowReasonMaxTokens: 2048, maxLlmCallsPerRun: 100, approvalTimeoutMs: 900000,
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

const chatProvider = {
  id: "openai",
  name: "openai",
  endpoint: "https://api.openai.com/v1",
  api: "openai-completions",
  apiKey: "sk-test",
  enabled: true,
  isDefault: true,
  models: [{ id: "gpt-4o" }],
};

test.afterEach(() => {
  mock.restoreAll();
});

test("reason: happy path returns parsed output for a raw (schema-less) instruction", async () => {
  mock.method(globalThis, "fetch", async () =>
    new Response(JSON.stringify({ choices: [{ message: { content: "the answer is 42" } }] }), {
      status: 200,
      headers: { "content-type": "application/json" },
    }),
  );

  const north = makeNorth();
  const south = makeSouth({ providers: [chatProvider] });
  const clients = { north, south } as unknown as import("../src/broker/clients.js").BrokerClients;
  const bridge = new GovernanceBridge(cfg, clients, identity, async () => true, log);

  const result = await bridge.reason("what is the answer?");

  assert.equal(result.ok, true, `expected ok, got error: ${result.error}`);
  assert.equal(result.output, "the answer is 42");
});

test("reason: happy path parses structured output when outputSchema is present", async () => {
  mock.method(globalThis, "fetch", async () =>
    new Response(JSON.stringify({ choices: [{ message: { content: '{"email":"a@b.com"}' } }] }), {
      status: 200,
      headers: { "content-type": "application/json" },
    }),
  );

  const north = makeNorth();
  const south = makeSouth({ providers: [chatProvider] });
  const clients = { north, south } as unknown as import("../src/broker/clients.js").BrokerClients;
  const bridge = new GovernanceBridge(cfg, clients, identity, async () => true, log);

  const result = await bridge.reason("extract the email", { type: "object", properties: { email: { type: "string" } } });

  assert.equal(result.ok, true, `expected ok, got error: ${result.error}`);
  assert.deepEqual(result.output, { email: "a@b.com" });
});

// The token budget a reason step gets is the model's own when the operator set
// one, and cfg.workflowReasonMaxTokens only when they did not. This is what makes
// the ceiling tunable from the Providers admin panel: workflowReasonMaxTokens is
// read from env but never compose-substituted, so on a running deployment it
// cannot be changed without a rebuild. A step that halts on a truncated response
// (the on-prem workflow failure) is then an edit, not a redeploy.
async function reasonRequestBody(models: { id: string; maxTokens?: number }[]) {
  const fetchMock = mock.method(globalThis, "fetch", async () =>
    new Response(JSON.stringify({ choices: [{ message: { content: "ok" } }] }), {
      status: 200,
      headers: { "content-type": "application/json" },
    }),
  );

  const clients = {
    north: makeNorth(),
    south: makeSouth({ providers: [{ ...chatProvider, models }] }),
  } as unknown as import("../src/broker/clients.js").BrokerClients;
  const bridge = new GovernanceBridge(cfg, clients, identity, async () => true, log);

  const result = await bridge.reason("summarise this");
  assert.equal(result.ok, true, `expected ok, got error: ${result.error}`);

  const [, init] = fetchMock.mock.calls[0].arguments;
  return JSON.parse(String(init?.body));
}

test("reason: a model's own maxTokens overrides the configured default", async () => {
  const body = await reasonRequestBody([{ id: "gpt-5.6-terra", maxTokens: 32000 }]);
  assert.equal(body.max_completion_tokens, 32000);
});

test("reason: an unset model maxTokens falls back to the configured default", async () => {
  assert.equal(cfg.workflowReasonMaxTokens, 2048, "guards the fixture this test reads meaning from");

  const unset = await reasonRequestBody([{ id: "gpt-4o" }]);
  assert.equal(unset.max_completion_tokens, 2048);

  // 0 is how the provider record spells "unset" — it must not reach the wire as
  // a real budget, which would make every reason step fail on an empty response.
  const zero = await reasonRequestBody([{ id: "gpt-4o", maxTokens: 0 }]);
  assert.equal(zero.max_completion_tokens, 2048);
});

test("reason: no default chat provider fails closed, never calls fetch", async () => {
  const fetchMock = mock.method(globalThis, "fetch", async () => {
    throw new Error("must not be called");
  });

  const north = makeNorth();
  const south = makeSouth({ providers: [] });
  const clients = { north, south } as unknown as import("../src/broker/clients.js").BrokerClients;
  const bridge = new GovernanceBridge(cfg, clients, identity, async () => true, log);

  const result = await bridge.reason("do something");

  assert.equal(result.ok, false);
  assert.match(result.error ?? "", /no default llm provider configured/i);
  assert.equal(fetchMock.mock.calls.length, 0);
});

test("reason: provider call failure propagates as ok:false", async () => {
  mock.method(globalThis, "fetch", async () => new Response("unauthorized", { status: 401 }));

  const north = makeNorth();
  const south = makeSouth({ providers: [chatProvider] });
  const clients = { north, south } as unknown as import("../src/broker/clients.js").BrokerClients;
  const bridge = new GovernanceBridge(cfg, clients, identity, async () => true, log);

  const result = await bridge.reason("do something");

  assert.equal(result.ok, false);
  assert.match(result.error ?? "", /401/);
});

test("reason: unparseable schema output fails closed with a named reason", async () => {
  mock.method(globalThis, "fetch", async () =>
    new Response(JSON.stringify({ choices: [{ message: { content: "not json" } }] }), {
      status: 200,
      headers: { "content-type": "application/json" },
    }),
  );

  const north = makeNorth();
  const south = makeSouth({ providers: [chatProvider] });
  const clients = { north, south } as unknown as import("../src/broker/clients.js").BrokerClients;
  const bridge = new GovernanceBridge(cfg, clients, identity, async () => true, log);

  const result = await bridge.reason("extract the email", { type: "object" });

  assert.equal(result.ok, false);
  assert.match(result.error ?? "", /reason step output did not match output_schema/i);
});

// ── Spend-caps CP3: rate-limit pre-gate + usage emission ───────────────────────

test("reason: a rate-limit pre-gate denial fails the step and never calls the provider", async () => {
  const fetchMock = mock.method(globalThis, "fetch", async () => {
    throw new Error("must not be called");
  });

  const north = makeNorth();
  const south = makeSouth({ providers: [chatProvider] });
  const clients = { north, south } as unknown as import("../src/broker/clients.js").BrokerClients;
  const rateLimitCalls: unknown[] = [];
  const denyChecker = async (tenantId: string, agentId: string, provider: string) => {
    rateLimitCalls.push([tenantId, agentId, provider]);
    throw new Error("rate limit exceeded: spend_agent");
  };
  const bridge = new GovernanceBridge(cfg, clients, identity, async () => true, log, denyChecker);

  const result = await bridge.reason("do something");

  assert.equal(result.ok, false);
  assert.match(result.error ?? "", /rate limit exceeded: spend_agent/);
  assert.equal(fetchMock.mock.calls.length, 0, "the provider must never be called after a pre-gate denial");
  // Keyed by hostname (not provider.id) — matches the egress proxy's
  // convention (new URL(upstreamBaseUrl).hostname), so a per-provider
  // rate-limit policy matches regardless of pre-gate call site.
  assert.deepEqual(rateLimitCalls, [[identity.tenantId, identity.agentId, "api.openai.com"]]);
  assert.equal(south.emitLlmUsageCalls.length, 0, "a denied call must not emit usage");
});

test("reason: a successful call emits EmitLlmUsage with identity, provider/model, and tokens; cost is 0", async () => {
  mock.method(globalThis, "fetch", async () =>
    new Response(
      JSON.stringify({ choices: [{ message: { content: "42" } }], usage: { prompt_tokens: 11, completion_tokens: 4 } }),
      { status: 200, headers: { "content-type": "application/json" } },
    ),
  );

  const north = makeNorth();
  const south = makeSouth({ providers: [chatProvider] });
  const clients = { north, south } as unknown as import("../src/broker/clients.js").BrokerClients;
  const allowChecker = async () => {};
  // usageRunId ("run-r1") is the last ctor arg — attribution for the run whose
  // reason step this is.
  const bridge = new GovernanceBridge(cfg, clients, identity, async () => true, log, allowChecker, undefined, "run-r1");

  const result = await bridge.reason("what is the answer?");

  assert.equal(result.ok, true, `expected ok, got error: ${result.error}`);
  assert.equal(south.emitLlmUsageCalls.length, 1);
  assert.deepEqual(south.emitLlmUsageCalls[0], {
    tenantId: identity.tenantId,
    userId: identity.userId,
    agentId: identity.agentId,
    provider: chatProvider.id,
    model: "gpt-4o",
    tokensIn: 11,
    tokensOut: 4,
    cacheRead: 0,
    cacheWrite: 0,
    cost: 0,
    runId: "run-r1",
    // A parent-side call has no webui chat session of its own.
    sessionId: "",
    source: "reason",
    quantity: 0,
    unit: "",
  });
});

test("Spend-caps CP4 reason: the pre-gate checker's 4th argument is the run identity's userId, including the external-invoke svc-<agentId> shape", async () => {
  // WHY: CheckRateLimit needs userId to enforce per-user spend caps. External
  // agent invokes and scheduled runs set identity.userId = "svc-<agentId>"
  // (external/core.ts) — this must reach the pre-gate call unchanged, proving
  // those paths inherit userId threading automatically via the run's identity
  // rather than needing a separate wiring path.
  mock.method(globalThis, "fetch", async () =>
    new Response(JSON.stringify({ choices: [{ message: { content: "42" } }] }), {
      status: 200,
      headers: { "content-type": "application/json" },
    }),
  );

  const svcIdentity = { ...identity, userId: "svc-alice-agent-uuid" };
  const north = makeNorth();
  const south = makeSouth({ providers: [chatProvider] });
  const clients = { north, south } as unknown as import("../src/broker/clients.js").BrokerClients;
  const rateLimitCalls: unknown[] = [];
  const allowChecker = async (tenantId: string, agentId: string, provider: string, userId?: string) => {
    rateLimitCalls.push([tenantId, agentId, provider, userId]);
  };
  const bridge = new GovernanceBridge(cfg, clients, svcIdentity, async () => true, log, allowChecker);

  const result = await bridge.reason("do something");

  assert.equal(result.ok, true, `expected ok, got error: ${result.error}`);
  assert.deepEqual(rateLimitCalls, [[svcIdentity.tenantId, svcIdentity.agentId, "api.openai.com", "svc-alice-agent-uuid"]]);
});

test("reason: an emit failure never fails an otherwise-successful reason step", async () => {
  mock.method(globalThis, "fetch", async () =>
    new Response(JSON.stringify({ choices: [{ message: { content: "42" } }] }), {
      status: 200,
      headers: { "content-type": "application/json" },
    }),
  );

  const north = makeNorth();
  const south = makeSouth({ providers: [chatProvider] });
  south.emitLlmUsage = () => Promise.reject(new Error("broker down"));
  const clients = { north, south } as unknown as import("../src/broker/clients.js").BrokerClients;
  const bridge = new GovernanceBridge(cfg, clients, identity, async () => true, log, async () => {});

  const result = await bridge.reason("what is the answer?");

  assert.equal(result.ok, true, `emit failure must not fail the step, got error: ${result.error}`);
});

// ── Per-run LLM-call budget seam (EgressProxy property 9) ──────────────────────

test("reason: a spent per-run LLM-call budget denies the step before any RPC or fetch", async () => {
  // WHY: reason() bypasses the egress proxy entirely, so without this seam a
  // workflow reason step would be an unbounded side channel around the per-run
  // call cap. Denial must cost nothing: no provider fetch, no usage emit.
  const fetchMock = mock.method(globalThis, "fetch", async () => {
    throw new Error("must not be called");
  });

  const north = makeNorth();
  const south = makeSouth({ providers: [chatProvider] });
  const clients = { north, south } as unknown as import("../src/broker/clients.js").BrokerClients;
  const bridge = new GovernanceBridge(cfg, clients, identity, async () => true, log, async () => {}, () => false);

  const result = await bridge.reason("do something");

  assert.equal(result.ok, false);
  assert.match(result.error ?? "", /llm call budget exceeded for this run/);
  assert.equal(fetchMock.mock.calls.length, 0, "an over-budget step must never reach the provider");
  assert.equal(south.emitLlmUsageCalls.length, 0, "a denied step must not emit usage");
});

test("reason: the budget is charged ONCE per call, not once per failover candidate", async () => {
  // WHY: mirrors the egress proxy's rule — charging per candidate would silently
  // divide the configured budget by the chain length.
  mock.method(globalThis, "fetch", async () =>
    new Response(JSON.stringify({ choices: [{ message: { content: "42" } }] }), {
      status: 200,
      headers: { "content-type": "application/json" },
    }),
  );

  const north = makeNorth();
  const south = makeSouth({ providers: [chatProvider] });
  const clients = { north, south } as unknown as import("../src/broker/clients.js").BrokerClients;
  let bookings = 0;
  const bridge = new GovernanceBridge(cfg, clients, identity, async () => true, log, async () => {}, () => {
    bookings++;
    return true;
  });

  assert.equal((await bridge.reason("q1")).ok, true);
  assert.equal((await bridge.reason("q2")).ok, true);
  assert.equal(bookings, 2, "one booking per reason() call");
});

test("reason: no budget seam injected (scheduler runViaWorkflow) → unchanged behaviour", async () => {
  // WHY: that path builds a bridge with no child behind it and is already bounded
  // by a workflow's finite step count plus schedulerRunTimeoutMs — there is no
  // unbounded loop to cap, so an absent seam must not fail closed.
  mock.method(globalThis, "fetch", async () =>
    new Response(JSON.stringify({ choices: [{ message: { content: "42" } }] }), {
      status: 200,
      headers: { "content-type": "application/json" },
    }),
  );

  const north = makeNorth();
  const south = makeSouth({ providers: [chatProvider] });
  const clients = { north, south } as unknown as import("../src/broker/clients.js").BrokerClients;
  const bridge = new GovernanceBridge(cfg, clients, identity, async () => true, log, async () => {});

  assert.equal((await bridge.reason("q")).ok, true);
});
