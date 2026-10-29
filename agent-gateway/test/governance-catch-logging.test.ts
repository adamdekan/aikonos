// Task 3 — GovernanceBridge catch blocks must log server-side (mirroring
// finish()'s existing this.log.warn) while keeping the { ok:false, error }
// return shape identical. saveWorkflow is the representative method: a throwing
// broker client must produce exactly one log.warn and the unchanged return.
import { test } from "node:test";
import assert from "node:assert/strict";
import { GovernanceBridge } from "../src/broker/governance.js";

const cfg = {
  gatewaySpiffeId: "spiffe://aikonos.com/agent-gateway",
  defaultTenantId: "11111111-1111-1111-1111-111111111111",
  workflowReasonMaxTokens: 2048, maxLlmCallsPerRun: 100, approvalTimeoutMs: 900000,
  egressTimeoutMs: 120000,
} as unknown as import("../src/config.js").Config;

const identity = {
  token: "bearer-tok",
  tenantId: "11111111-1111-1111-1111-111111111111",
  userId: "alice@example.com",
  agentId: "alice-agent",
  ownerGrant: "grant-alice", // F171: forwarded on every south read
};

test("saveWorkflow: a throwing broker client logs one warn and returns { ok:false, error } unchanged", async () => {
  const warnCalls: unknown[] = [];
  const log = {
    info: () => {},
    warn: (obj: unknown) => warnCalls.push(obj),
    error: () => {},
    debug: () => {},
  } as unknown as import("../src/log.js").Logger;

  const north = {
    saveWorkflow: () => Promise.reject(new Error("broker unavailable")),
  };
  const south = {};
  const clients = { north, south } as unknown as import("../src/broker/clients.js").BrokerClients;

  const bridge = new GovernanceBridge(cfg, clients, identity, async () => true, log);

  // A valid canonical def (doc.read is a known skill so invalidSkillError passes
  // and the throwing north.saveWorkflow is actually reached).
  const result = await bridge.saveWorkflow({
    name: "wf",
    steps: [{ skill: "doc.read", args: {} }],
  });

  assert.equal(result.ok, false);
  assert.match(result.error ?? "", /broker unavailable/);
  assert.equal(warnCalls.length, 1, "exactly one log.warn at the catch site");
});

test("getSkillBody: a throwing south client logs one warn and returns { ok:false, error } unchanged", async () => {
  const warnCalls: unknown[] = [];
  const log = {
    info: () => {},
    warn: (obj: unknown) => warnCalls.push(obj),
    error: () => {},
    debug: () => {},
  } as unknown as import("../src/log.js").Logger;

  const north = {};
  const south = {
    getPersonalSkillSouth: () => Promise.reject(new Error("south unreachable")),
  };
  const clients = { north, south } as unknown as import("../src/broker/clients.js").BrokerClients;

  const bridge = new GovernanceBridge(cfg, clients, identity, async () => true, log);

  const result = await bridge.getSkillBody("my-notes");

  assert.equal(result.ok, false);
  assert.match(result.error ?? "", /south unreachable/);
  assert.equal(warnCalls.length, 1, "exactly one log.warn at the catch site");
});

// ── getSkillFile routing ───────────────────

test("getSkillFile: a 'personal:'-prefixed ref routes to getPersonalSkillFileSouth with the bare name", async () => {
  const log = { info: () => {}, warn: () => {}, error: () => {}, debug: () => {} } as unknown as import("../src/log.js").Logger;

  const calls: unknown[] = [];
  const north = {};
  const south = {
    getPersonalSkillFileSouth: (req: unknown) => {
      calls.push(req);
      return Promise.resolve({ content: new TextEncoder().encode("notes body") });
    },
    getAgentSkillFileSouth: () => {
      throw new Error("must not be called for a personal ref");
    },
  };
  const clients = { north, south } as unknown as import("../src/broker/clients.js").BrokerClients;

  const bridge = new GovernanceBridge(cfg, clients, identity, async () => true, log);
  const result = await bridge.getSkillFile("personal:my-notes", "notes.md");

  assert.equal(result.ok, true);
  // contentB64, not raw bytes — the JSON-IPC-safe wire shape (bug 1, CP3 review):
  // a raw Uint8Array is flattened by the real child's serialization:"json" fork.
  assert.equal(result.contentB64, Buffer.from("notes body", "utf8").toString("base64"));
  assert.deepEqual(calls, [{
    tenantId: identity.tenantId, userId: identity.userId, name: "my-notes", path: "notes.md", ownerGrant: "grant-alice",
  }]);
});

test("getSkillFile: a bare ref (bundle UUID) routes to getAgentSkillFileSouth with id=ref", async () => {
  const log = { info: () => {}, warn: () => {}, error: () => {}, debug: () => {} } as unknown as import("../src/log.js").Logger;

  const calls: unknown[] = [];
  const north = {};
  const south = {
    getAgentSkillFileSouth: (req: unknown) => {
      calls.push(req);
      return Promise.resolve({ content: new TextEncoder().encode("bundle file body") });
    },
    getPersonalSkillFileSouth: () => {
      throw new Error("must not be called for a bundle ref");
    },
  };
  const clients = { north, south } as unknown as import("../src/broker/clients.js").BrokerClients;

  const bridge = new GovernanceBridge(cfg, clients, identity, async () => true, log);
  const result = await bridge.getSkillFile("bundle-uuid-1", "references/guide.md");

  assert.equal(result.ok, true);
  assert.equal(result.contentB64, Buffer.from("bundle file body", "utf8").toString("base64"));
  assert.deepEqual(calls, [{
    tenantId: identity.tenantId, userId: identity.userId, id: "bundle-uuid-1", path: "references/guide.md", ownerGrant: "grant-alice",
  }]);
});

test("getSkillFile: a throwing south client logs one warn and returns { ok:false, error } unchanged", async () => {
  const warnCalls: unknown[] = [];
  const log = {
    info: () => {},
    warn: (obj: unknown) => warnCalls.push(obj),
    error: () => {},
    debug: () => {},
  } as unknown as import("../src/log.js").Logger;

  const north = {};
  const south = {
    getAgentSkillFileSouth: () => Promise.reject(new Error("south unreachable")),
  };
  const clients = { north, south } as unknown as import("../src/broker/clients.js").BrokerClients;

  const bridge = new GovernanceBridge(cfg, clients, identity, async () => true, log);
  const result = await bridge.getSkillFile("bundle-uuid-1", "x.txt");

  assert.equal(result.ok, false);
  assert.match(result.error ?? "", /south unreachable/);
  assert.equal(warnCalls.length, 1, "exactly one log.warn at the catch site");
});

// F171: the south MCP listing needs an owner grant; a personal token-path run
// (the HTTP workflow route) mints its session's own once, on first need.
test("resolveMcpReadOnlyHints: a token-only run mints one session grant for its south listings", async () => {
  const log = { info: () => {}, warn: () => {}, error: () => {}, debug: () => {} } as unknown as import("../src/log.js").Logger;
  const mints: unknown[] = [];
  const grants: string[] = [];
  const north = {
    mintSessionGrant: async (req: unknown, token?: string) => {
      mints.push({ req, token });
      return { ownerGrant: "minted" };
    },
  };
  const south = {
    listMcpServerToolsSouth: async (req: { ownerGrant: string }) => {
      grants.push(req.ownerGrant);
      return { tools: [{ name: "read", readOnlyHint: true }] };
    },
  };
  const clients = { north, south } as unknown as import("../src/broker/clients.js").BrokerClients;
  const tokenOnly = { token: "bearer-tok", tenantId: identity.tenantId, userId: identity.userId, agentId: "" };
  const bridge = new GovernanceBridge(cfg, clients, tokenOnly, async () => true, log);

  const hints = await (bridge as unknown as {
    resolveMcpReadOnlyHints(def: unknown): Promise<Map<string, boolean>>;
  }).resolveMcpReadOnlyHints({ steps: [{ skill: "mcp:c1:read", args: {} }, { skill: "mcp:c2:read", args: {} }] });

  assert.deepEqual(mints, [{ req: { tenantId: identity.tenantId, userId: identity.userId }, token: "bearer-tok" }]);
  assert.deepEqual(grants, ["minted", "minted"]);
  assert.equal(hints.get("mcp:c1:read"), true);
});
