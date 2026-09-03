// CP2: gateway ticker fire branch for workflow-mode scheduled runs
//.
//
// Unlike a prompt-mode run, a workflow-mode DueRun never touches the
// ChildSupervisor — the fire branch builds a GovernanceBridge directly from
// the claim-minted ownerGrant and drives the workflow through
// bridge.runWorkflow(). These tests fake clients.south end-to-end (getWorkflow,
// submitPlan, createGatewayTask, approveGatewayTask, invokeTool, emitStatus) so
// a real GovernanceBridge + run.ts driver executes, proving the wiring rather
// than a stubbed bridge.
import { test } from "node:test";
import assert from "node:assert/strict";
import { tick } from "../src/scheduler/ticker.js";
import type { Config } from "../src/config.js";
import type { Logger } from "../src/log.js";
import type { BrokerClients } from "../src/broker/clients.js";
import type { ChildSupervisor } from "../src/ipc/supervisor.js";
import { ValidationOutcome } from "../gen/ts/proto/plan";
import type { WorkflowDef } from "../src/workflow/author.js";

const TENANT = "11111111-1111-1111-1111-111111111111";

const baseCfg = {
  defaultTenantId: TENANT,
  schedulerEnabled: true,
  schedulerTickMs: 30000,
  schedulerClaimLimit: 5,
  schedulerRunTimeoutMs: 2000,
  gatewaySpiffeId: "spiffe://aikonos.com/agent-gateway",
  agentForUserOverrides: {},
} as unknown as Config;

const log: Logger = {
  info: () => {},
  warn: () => {},
  error: () => {},
  debug: () => {},
} as unknown as Logger;

// supervisorThatMustNotSpawn proves the workflow fire branch never touches the
// ChildSupervisor — any call to getOrSpawn (the only seam runViaChild uses)
// fails the test loudly instead of silently falling back to the prompt path.
function supervisorThatMustNotSpawn(): ChildSupervisor {
  return {
    getOrSpawn: () => {
      throw new Error("workflow-mode run must not spawn a Pi child");
    },
  } as unknown as ChildSupervisor;
}

function workflowDef(steps: { skill: string }[]): WorkflowDef {
  return {
    apiVersion: "aikonos.com/v1",
    kind: "Workflow",
    metadata: { name: "weekly report", visibility: { kind: "private" } },
    inputs: [],
    steps: steps.map((s) => ({ kind: "tool" as const, skill: s.skill, args: {} })),
  };
}

// FakeSouthOpts lets each test control the per-toolId governance outcome
// (APPROVED / NEEDS_HUMAN / DENIED) exactly the way the broker's OPA +
// FGA gates would, without standing up the real broker.
interface FakeSouthOpts {
  def: WorkflowDef;
  outcomeFor: Record<string, ValidationOutcome>;
  hang?: boolean; // never resolve getWorkflow — used by the timeout test
  providers?: unknown[]; // resolved by GovernanceBridge.reason() for a kind:"reason" step
}

function makeFakeSouth(opts: FakeSouthOpts, calls: { approveGatewayTask: number; reportedResult?: { success: boolean; summary: string } }) {
  return {
    getWorkflow: () => {
      if (opts.hang) return new Promise(() => {});
      return Promise.resolve({ definitionJson: JSON.stringify(opts.def), version: 1, boundAgentId: "" });
    },
    createGatewayTask: () => Promise.resolve({ taskId: "task-1" }),
    submitPlan: (req: { plan: { steps: { toolId: string }[] } }) => {
      const toolId = req.plan.steps[0]?.toolId ?? "";
      const outcome = opts.outcomeFor[toolId] ?? ValidationOutcome.DENIED;
      if (outcome === ValidationOutcome.DENIED) {
        return Promise.resolve({ outcome, capabilityTokenIds: [], violations: [`denied: ${toolId}`] });
      }
      return Promise.resolve({ outcome, capabilityTokenIds: ["tok0", "tok1"], violations: [] });
    },
    approveGatewayTask: () => {
      calls.approveGatewayTask += 1;
      return Promise.resolve({ capabilityTokenIds: ["tok0", "tok1"] });
    },
    invokeTool: () => Promise.resolve({ success: true, result: { ok: true }, costUnitsConsumed: 1 }),
    emitStatus: () => Promise.resolve(),
    reportScheduledRunResult: (req: { success: boolean; summary: string }) => {
      calls.reportedResult = { success: req.success, summary: req.summary };
      return Promise.resolve();
    },
    getLlmProviders: () => Promise.resolve({ providers: opts.providers ?? [] }),
    emitLlmUsage: () => Promise.resolve(),
  };
}

function makeDueRun(id: string, def: WorkflowDef) {
  return {
    id,
    ownerUserId: "alice@example.com",
    prompt: "",
    approvedTools: [],
    ownerGrant: "v1.tenant.owner.expires.sig",
    workflowLineageId: "lineage-1",
    workflowInputs: {},
  };
}

test("workflow schedule fires via GovernanceBridge, never spawns a child", async () => {
  const def = workflowDef([{ skill: "web.fetch" }]);
  const calls = { approveGatewayTask: 0 } as { approveGatewayTask: number; reportedResult?: { success: boolean; summary: string } };
  const south = makeFakeSouth(
    { def, outcomeFor: { "web.fetch": ValidationOutcome.APPROVED } },
    calls,
  );
  const clients = {
    south,
    north: {},
  } as unknown as BrokerClients;
  (clients.south as unknown as { claimDueScheduledRuns: () => Promise<unknown> }).claimDueScheduledRuns = () =>
    Promise.resolve({ runs: [makeDueRun("sched-1", def)] });

  await tick(baseCfg, clients, log, supervisorThatMustNotSpawn());

  assert.ok(calls.reportedResult, "reportScheduledRunResult must be called");
  assert.equal(calls.reportedResult?.success, true);
  assert.equal(calls.reportedResult?.summary, "1/1 steps ok");
});

test("fire-time approver pre-authorizes exactly the current version's own tool steps (not a stored/baseline list)", async () => {
  // email.draft is WRITE_EXTERNAL in mapping.ts and would need HITL — the only
  // reason it can be pre-authorized here is that it is one of THIS workflow's
  // own tool steps. If the derivation instead used the scheduler's baseline
  // tools (web.fetch/doc.read/doc.write) or an empty list, this step would be
  // denied and the run would halt.
  const def = workflowDef([{ skill: "web.fetch" }, { skill: "email.draft" }]);
  const calls = { approveGatewayTask: 0 } as { approveGatewayTask: number; reportedResult?: { success: boolean; summary: string } };
  const south = makeFakeSouth(
    {
      def,
      outcomeFor: {
        "web.fetch": ValidationOutcome.APPROVED,
        "email.draft": ValidationOutcome.NEEDS_HUMAN,
      },
    },
    calls,
  );
  const clients = { south, north: {} } as unknown as BrokerClients;
  (clients.south as unknown as { claimDueScheduledRuns: () => Promise<unknown> }).claimDueScheduledRuns = () =>
    Promise.resolve({ runs: [makeDueRun("sched-2", def)] });

  await tick(baseCfg, clients, log, supervisorThatMustNotSpawn());

  assert.equal(calls.approveGatewayTask, 1, "the NEEDS_HUMAN step must have gone through the approve path");
  assert.equal(calls.reportedResult?.success, true);
  assert.equal(calls.reportedResult?.summary, "2/2 steps ok");
});

test("a step outside the workflow's own tool steps is never pre-authorized (halts, never overriding an OPA denial)", async () => {
  // A step the broker itself denies (OPA DENY) must still halt the run even
  // though it is part of the workflow — the approver only ever answers the
  // NEEDS_HUMAN/STEP_UP question and must never override a DENIED outcome.
  const def = workflowDef([{ skill: "web.fetch" }, { skill: "email.draft" }]);
  const calls = { approveGatewayTask: 0 } as { approveGatewayTask: number; reportedResult?: { success: boolean; summary: string } };
  const south = makeFakeSouth(
    {
      def,
      outcomeFor: {
        "web.fetch": ValidationOutcome.APPROVED,
        "email.draft": ValidationOutcome.DENIED,
      },
    },
    calls,
  );
  const clients = { south, north: {} } as unknown as BrokerClients;
  (clients.south as unknown as { claimDueScheduledRuns: () => Promise<unknown> }).claimDueScheduledRuns = () =>
    Promise.resolve({ runs: [makeDueRun("sched-3", def)] });

  await tick(baseCfg, clients, log, supervisorThatMustNotSpawn());

  assert.equal(calls.approveGatewayTask, 0, "a DENIED step must never reach the approver");
  assert.equal(calls.reportedResult?.success, false);
  assert.equal(
    calls.reportedResult?.summary,
    "1/2 steps ok; halted at step 1: denied: email.draft",
  );
});

test("a workflow run that exceeds schedulerRunTimeoutMs reports failure and frees the pool slot", async () => {
  const def = workflowDef([{ skill: "web.fetch" }]);
  const calls = { approveGatewayTask: 0 } as { approveGatewayTask: number; reportedResult?: { success: boolean; summary: string } };
  const south = makeFakeSouth(
    { def, outcomeFor: { "web.fetch": ValidationOutcome.APPROVED }, hang: true },
    calls,
  );
  const clients = { south, north: {} } as unknown as BrokerClients;
  (clients.south as unknown as { claimDueScheduledRuns: () => Promise<unknown> }).claimDueScheduledRuns = () =>
    Promise.resolve({ runs: [makeDueRun("sched-4", def)] });

  const cfg = { ...baseCfg, schedulerRunTimeoutMs: 30 } as unknown as Config;

  const start = Date.now();
  await tick(cfg, clients, log, supervisorThatMustNotSpawn());
  const elapsed = Date.now() - start;

  assert.ok(elapsed < 2000, "tick() must resolve promptly on timeout, not hang for the real getWorkflow call");
  assert.equal(calls.reportedResult?.success, false);
  assert.match(calls.reportedResult?.summary ?? "", /run timed out/);
});

test("prompt-mode runs (no workflowLineageId) are unaffected — still route through the child supervisor path", async () => {
  // Regression guard: a plain prompt DueRun must still hit the pre-existing
  // child-supervisor branch, not the new workflow branch. We assert this by
  // making the fake supervisor's getOrSpawn a marker call instead of throwing.
  let spawnCalled = false;
  const supervisor = {
    getOrSpawn: () => {
      spawnCalled = true;
      // Return a handle whose run() resolves immediately with nothing — enough
      // for runViaChild to complete without a real child process.
      return Promise.resolve({
        setRunContext: () => {},
        clearRunContext: () => {},
        abortRun: () => {},
      });
    },
    run: () => Promise.resolve(),
    keyFor: () => "k",
  } as unknown as ChildSupervisor;

  const calls = { approveGatewayTask: 0 } as { approveGatewayTask: number; reportedResult?: { success: boolean; summary: string } };
  const south = {
    claimDueScheduledRuns: () =>
      Promise.resolve({
        runs: [
          {
            id: "sched-prompt",
            ownerUserId: "alice@example.com",
            prompt: "say hi",
            approvedTools: [],
            ownerGrant: "v1.tenant.owner.expires.sig",
            workflowLineageId: "",
            workflowInputs: {},
          },
        ],
      }),
    reportScheduledRunResult: (req: { success: boolean; summary: string }) => {
      calls.reportedResult = { success: req.success, summary: req.summary };
      return Promise.resolve();
    },
  };
  const clients = { south } as unknown as BrokerClients;

  // supervisor.run signature used by runViaChild is (handle, {runId, threadId, text}, onEvent).
  // Our fake supervisor.run above resolves immediately; runViaChild awaits it
  // inside a race against a timer that never fires within schedulerRunTimeoutMs.
  await tick(baseCfg, clients, log, supervisor);

  assert.equal(spawnCalled, true, "prompt-mode run must still call supervisor.getOrSpawn");
});

// ── Spend-caps CP3 finding 1: the ticker-built GovernanceBridge must receive
// the rate-limit checker, or a scheduled workflow's reason steps are never
// pre-gated ──────────────────────────────────────────────────────────────────

test("a rateLimitChecker passed to tick() reaches the runViaWorkflow-built bridge and denies a reason step", async () => {
  const def: WorkflowDef = {
    apiVersion: "aikonos.com/v1",
    kind: "Workflow",
    metadata: { name: "reasoning report", visibility: { kind: "private" } },
    inputs: [],
    steps: [{ kind: "reason", skill: "", args: {}, instruction: "summarize" }],
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
  const calls = { approveGatewayTask: 0 } as { approveGatewayTask: number; reportedResult?: { success: boolean; summary: string } };
  const south = makeFakeSouth({ def, outcomeFor: {}, providers: [chatProvider] }, calls);
  const clients = { south, north: {} } as unknown as BrokerClients;
  (clients.south as unknown as { claimDueScheduledRuns: () => Promise<unknown> }).claimDueScheduledRuns = () =>
    Promise.resolve({ runs: [makeDueRun("sched-reason", def)] });

  const rateLimitCalls: unknown[] = [];
  const denyChecker = async (tenantId: string, agentId: string, provider: string) => {
    rateLimitCalls.push([tenantId, agentId, provider]);
    throw new Error("rate limit exceeded: spend_agent");
  };

  await tick(baseCfg, clients, log, supervisorThatMustNotSpawn(), denyChecker);

  assert.equal(rateLimitCalls.length, 1, "the ticker-built bridge must invoke the injected rate-limit checker");
  assert.equal(calls.reportedResult?.success, false);
  assert.match(
    calls.reportedResult?.summary ?? "",
    /rate limit exceeded: spend_agent/,
    "the reason step's pre-gate denial must halt the run",
  );
});
