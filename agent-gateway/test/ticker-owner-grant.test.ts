// CP3/CP8 ticker test: ownerGrant from claimDueScheduledRuns is threaded into
// the supervisor run identity.
//
// WHY: the scheduler path claims runs from the broker, each carrying a
// broker-minted ownerGrant. After CP8 the ticker calls the supervisor with an
// identity that includes the grant — the grant flows into the child's spawn
// record, NOT any IPC message body. If the ownerGrant threading line were
// deleted from ticker.ts the captured identity would have ownerGrant=undefined
// and the test would fail.
import { test } from "node:test";
import assert from "node:assert/strict";
import { tick } from "../src/scheduler/ticker.js";
import type { Config } from "../src/config.js";
import type { Logger } from "../src/log.js";
import type { BrokerClients } from "../src/broker/clients.js";
import type { Identity, Approver } from "../src/broker/governance.js";
import type { RunIdentity } from "../src/ipc/bridge-server.js";
import {
  makePairedChannel,
  ParentLink,
  ChildLink,
} from "../src/ipc/protocol.js";
import type { DoneEvent, PromptMessage } from "../src/ipc/protocol.js";
import {
  ChildSupervisor,
  type SupervisorDeps,
  type ProviderCredentialResolver,
  type SpawnChildFn,
} from "../src/ipc/supervisor.js";
import type { EgressProxy, RegisterResult, RegisterOptions } from "../src/llm/egress-proxy.js";
import type { ResolveIdentity } from "../src/pi/session-plan.js";

// ── Stubs ─────────────────────────────────────────────────────────────────────

const TENANT = "11111111-1111-1111-1111-111111111111";

const baseCfg = {
  defaultTenantId: TENANT,
  schedulerEnabled: true,
  schedulerTickMs: 30000,
  schedulerClaimLimit: 5,
  schedulerRunTimeoutMs: 2000,
  gatewaySpiffeId: "spiffe://aikonos.com/agent-gateway",
  agentForUserOverrides: {},
  openrouterApiKey: "",
  llmModel: "",
  brokerNorthAddr: "",
  brokerSouthAddr: "",
  brokerServerName: "",
  tlsCert: "",
  tlsKey: "",
  tlsCa: "",
  port: 8080,
  oidcIssuer: "",
  oidcJwksUrl: "",
  oidcAudience: "",
} as unknown as Config;

const log: Logger = {
  info: () => {},
  warn: () => {},
  error: () => {},
  debug: () => {},
} as unknown as Logger;

function makeFakeProxy(): EgressProxy {
  return {
    register: (_opts: RegisterOptions): RegisterResult => ({
      childToken: "tok",
      childBaseUrl: "http://127.0.0.1:9999/tok/v1",
    }),
    unregister: () => {},
    resetRunBudget: () => {},
    listen: async () => 9999,
    close: async () => {},
  } as unknown as EgressProxy;
}

function makeFakeDeps(): SupervisorDeps {
  return {
    south: {
      getLlmProviders: async () => ({ providers: [] }),
      getTenantModel: async () => ({ model: "" }),
      listUserSkills: async () => ({ skills: [] }),
      listUserAgentSkills: async () => ({ bundles: [] }),
      listAccessibleMcpServersForAgent: async () => ({ connections: [] }),
      listMcpServerToolsSouth: async () => ({ tools: [] }),
      getAgentSpec: async () => ({ found: false }),
    },
    // llmModel must match makeFakeCreds()'s modelAllowlist below — the
    // supervisor's CP1 coherence guard
    // rejects a spawn whose session-plan model isn't in the resolved
    // credentials' allowlist.
    cfg: { llmModel: "m1", defaultTenantId: TENANT },
  };
}

function makeFakeCreds(): ProviderCredentialResolver {
  return async (_: ResolveIdentity) => ({
    upstreamBaseUrl: "https://openrouter.ai/api/v1",
    apiKey: "dummy",
    modelId: "m1",
    modelAllowlist: ["m1"],
    fallbacks: [],
  });
}

// ── Tests ─────────────────────────────────────────────────────────────────────

test("CP8 ticker: ownerGrant from DueRun flows into supervisor run identity (not IPC body)", async () => {
  // WHY: if ticker.ts's ownerGrant threading is removed (identity has no
  // ownerGrant), this assertion fails. The grant must reach governance via the
  // parent-side spawn identity record — never an IPC message body that the
  // untrusted child could forge.
  const grant = "v1.tenant.owner.expires.sig";

  // Build a fake child that auto-replies done so supervisor.run() resolves.
  const [parentSide, childSide] = makePairedChannel();
  const capturedLink = new ParentLink(parentSide);
  capturedLink.onExit = () => {};
  capturedLink.kill = () => {};
  const childLink = new ChildLink(childSide);
  childLink.on("prompt", (msg: PromptMessage) => {
    setImmediate(() => childLink.send({ kind: "done", runId: msg.runId } as DoneEvent));
  });

  const capturedIdentities: Identity[] = [];
  const supervisor = new ChildSupervisor(
    makeFakeProxy(),
    makeFakeDeps(),
    (identity: RunIdentity, _approver: Approver) => {
      capturedIdentities.push(identity);
      return {
        gate: async () => ({ allow: true }),
        execute: async () => ({ ok: true, output: "ok" }),
        delegate: async () => ({ ok: true }),
        saveWorkflow: async () => ({ ok: true }),
        runWorkflow: async () => ({ ok: true, result: null }),
        listWorkflows: async () => ({ ok: true, items: [] }),
        publishWorkflow: async () => ({ ok: true }),
        proposeWorkflow: async () => ({ ok: true }),
        analyzeImage: async () => ({ ok: true, text: "a red apple" }),
        scheduleWorkflow: async () => ({ ok: true }),
        setToken: () => {},
        setApprover: () => {},
      };
    },
    (() => capturedLink) as SpawnChildFn,
    makeFakeCreds(),
  );

  const south = {
    claimDueScheduledRuns: () =>
      Promise.resolve({
        runs: [
          {
            id: "run-1",
            ownerUserId: "alice@example.com",
            prompt: "list files",
            approvedTools: ["doc.read"],
            ownerGrant: grant,
          },
        ],
      }),
    reportScheduledRunResult: () => Promise.resolve(),
  };
  const clients = { south } as unknown as BrokerClients;

  await tick(baseCfg, clients, log, supervisor);

  assert.ok(capturedIdentities.length > 0, "bridge factory must have been called");
  assert.equal(
    capturedIdentities[0].ownerGrant,
    grant,
    "ticker must thread DueRun.ownerGrant into the child's spawn identity",
  );

  supervisor.dispose();
});

test("CP3 auth: resolveAgentApiKey response ownerGrant is exposed in ResolvedPrincipal", async () => {
  // WHY: the external API path calls resolveAgentApiKey and must thread the
  // returned ownerGrant into the runAgentInvocation call. This test proves the
  // authenticateApiKey wrapper exposes ownerGrant from the south response so the
  // rest.ts handler can forward it.
  const { authenticateApiKey } = await import("../src/external/auth.js");

  const expectedGrant = "v1.tenant.svc-agent.expires.sig";
  const south = {
    resolveAgentApiKey: async () => ({
      valid: true,
      agentId: "agent-uuid",
      tenantId: TENANT,
      principal: "svc-agent-uuid",
      ownerGrant: expectedGrant,
    }),
  } as unknown as import("../src/broker/south.js").SouthClient;

  const result = await authenticateApiKey("Bearer tk_validkey123", TENANT, south, log);

  assert.equal(result.ok, true, "authentication must succeed");
  if (!result.ok) return;

  assert.equal(
    result.principal.ownerGrant,
    expectedGrant,
    "ownerGrant from resolveAgentApiKey response must be present in ResolvedPrincipal",
  );
});
