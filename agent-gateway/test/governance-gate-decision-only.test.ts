// GovernanceBridge.gate() for the gate-then-bridge-direct model
//.
//
// WHY these tests exist: `spawn_subagents` and `analyze_image` map to a bare
// capability-skill toolId ("subagents"/"vision") that is deliberately absent
// from broker/internal/toolregistry — so the broker's mintStepTokens skips them
// and SubmitPlan returns APPROVED with an EMPTY capabilityTokenIds map. Both
// tools execute via a direct parent-side bridge call and never reach InvokeTool,
// so there is no Biscuit for them to redeem; demanding one blocked every call
// 100% of the time in production ("internal: no capability token minted").
//
// Every pre-existing gate test mocks capabilityTokenIds as populated, which is
// exactly why the defect shipped — these tests pin the EMPTY case, on both the
// straight-APPROVED and the post-HITL-approval paths, and pin that a genuinely
// InvokeTool-routed tool still fails closed without a token.
import { test } from "node:test";
import assert from "node:assert/strict";
import { GovernanceBridge } from "../src/broker/governance.js";
import { ValidationOutcome } from "../gen/ts/proto/plan.js";
import type { BrokerClients } from "../src/broker/clients.js";
import type { Config } from "../src/config.js";
import type { Logger } from "../src/log.js";

const TENANT = "11111111-1111-1111-1111-111111111111";

const cfg = { gatewaySpiffeId: "spiffe://aikonos.com/agent-gateway", defaultTenantId: TENANT } as unknown as Config;

const log = { info: () => {}, warn: () => {}, error: () => {}, debug: () => {} } as unknown as Logger;

const identity = {
  token: "bearer-tok",
  tenantId: TENANT,
  userId: "alice@example.com",
  agentId: "alice-agent",
};

/** A broker that approves every plan but mints NO capability token — the real
 *  broker's behavior for a toolId with no toolregistry scope. */
function makeClients(outcome: ValidationOutcome): BrokerClients {
  const invokeCalls: unknown[] = [];
  return {
    north: {
      createTask: () => Promise.resolve({ taskId: "t-1" }),
      approveTask: () => Promise.resolve({ capabilityTokenIds: {} }),
    },
    south: {
      createGatewayTask: () => Promise.resolve({ taskId: "t-1" }),
      approveGatewayTask: () => Promise.resolve({ capabilityTokenIds: {} }),
      submitPlan: () => Promise.resolve({ outcome, capabilityTokenIds: {}, violations: [], steps: [] }),
      invokeTool: (req: unknown) => {
        invokeCalls.push(req);
        return Promise.resolve({ success: true, result: null, error: "", costUnitsConsumed: 0 });
      },
      emitStatus: () => Promise.resolve(),
      invokeCalls,
    },
  } as unknown as BrokerClients;
}

function makeBridge(outcome = ValidationOutcome.APPROVED) {
  const clients = makeClients(outcome);
  return new GovernanceBridge(cfg, clients, { ...identity }, async () => true, log);
}

test("gate: spawn_subagents is allowed on APPROVED with no capability token minted", async () => {
  const decision = await makeBridge().gate("call-1", "spawn_subagents", { branches: [] });

  assert.deepEqual(decision, { allow: true });
});

test("gate: analyze_image is allowed on APPROVED with no capability token minted (same root cause)", async () => {
  const decision = await makeBridge().gate("call-2", "analyze_image", { path: "references/x.png" });

  assert.deepEqual(decision, { allow: true });
});

test("gate: spawn_subagents is allowed after a human approval that mints no token", async () => {
  const decision = await makeBridge(ValidationOutcome.NEEDS_HUMAN).gate("call-3", "spawn_subagents", {});

  assert.deepEqual(decision, { allow: true });
});

test("gate: a genuinely InvokeTool-routed tool still fails closed when no token is minted", async () => {
  const decision = await makeBridge().gate("call-4", "doc_write", { path: "a.md", content: "x" });

  assert.equal(decision.allow, false);
  assert.match(decision.reason ?? "", /no capability token minted/);
});

test("execute() after a gate-then-bridge-direct gate fails loudly instead of invoking with a blank token", async () => {
  // No pending entry is registered for these tools, so execute() — which only
  // ever runs for InvokeTool-routed tools — cannot reach the broker with an
  // empty capabilityToken.
  const bridge = makeBridge();
  await bridge.gate("call-5", "spawn_subagents", {});

  const res = await bridge.execute("call-5");

  assert.equal(res.ok, false);
  assert.match(res.error ?? "", /not authorized/);
});
