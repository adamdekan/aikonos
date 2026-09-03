// Batch-2 run-driver tests:
//   Task 1 — a failed tool step's output must not flow downstream. A later
//     ${steps.N.output} reference to a step whose exec failed must halt the run
//     (same handling as a forward/out-of-range or missing-field reference),
//     while a failed step nothing references leaves the run untouched.
//   Task 7 — runWorkflow's onStep callback fires once per settled step in order
//     and is failure-isolated (a throwing callback never corrupts the run).
import { test } from "node:test";
import assert from "node:assert/strict";
import { runWorkflow, type StepOutcome } from "../src/workflow/run.js";
import type { WorkflowDef } from "../src/workflow/author.js";
import type { BridgeClientLike } from "../src/ipc/bridge-client.js";

interface Call {
  op: "gate" | "execute" | "reason";
  toolName?: string;
  args?: Record<string, unknown>;
}

type ExecOutcome = { ok: boolean; output: unknown; error?: string };

// makeBridge: execute() returns queued outcomes by call order (so a specific
// step can be made to fail), gate() honours per-tool denials.
function makeBridge(
  execOutcomes: ExecOutcome[],
  denials: Record<string, { allow: boolean; reason?: string }> = {},
): BridgeClientLike & { calls: Call[] } {
  const calls: Call[] = [];
  let execIdx = 0;
  return {
    calls,
    gate: async (_id: string, toolName: string, args: Record<string, unknown>) => {
      calls.push({ op: "gate", toolName, args });
      return denials[toolName] ?? { allow: true };
    },
    execute: async (_id: string) => {
      calls.push({ op: "execute" });
      return execOutcomes[execIdx++] ?? { ok: true, output: null };
    },
    reason: async (instruction: string) => {
      calls.push({ op: "reason" });
      return { ok: true, output: instruction };
    },
    delegate: async () => ({ ok: false, error: "stub" }),
    saveWorkflow: async () => ({ ok: false, error: "stub" }),
    runWorkflow: async () => ({ ok: false, error: "stub" }),
    listWorkflows: async () => ({ ok: false, error: "stub" }),
    publishWorkflow: async () => ({ ok: false, error: "stub" }),
    proposeWorkflow: async () => ({ ok: false, error: "stub" }),
    analyzeImage: async () => ({ ok: false, error: "stub" }),
    scheduleWorkflow: async () => ({ ok: false, error: "stub" }),
    setApprover: () => {},
    setToken: () => {},
    usageIdentity: () => ({ tenantId: "", userId: "", agentId: "" }),
  };
}

const silentLog = { info: () => {}, warn: () => {} };

function def(steps: WorkflowDef["steps"]): WorkflowDef {
  return {
    apiVersion: "aikonos.com/v1",
    kind: "Workflow",
    metadata: { name: "wf", visibility: { kind: "private" } },
    inputs: [],
    steps,
  };
}

test("Task1: a step referencing a FAILED step's output halts the run naming the failed step", async () => {
  const bridge = makeBridge([{ ok: false, output: { partial: "junk" }, error: "boom" }]);
  const wf = def([
    { skill: "doc.read", args: { path: "x" } },
    { skill: "doc.write", args: { content: "${steps.0.output}" } },
  ]);

  const result = await runWorkflow(bridge, wf, {}, silentLog);

  assert.equal(result.halted, true);
  assert.equal(result.haltedAtStep, 1);
  // Step 0 recorded (executed, failed); step 1 recorded unresolvable.
  assert.equal(result.steps.length, 2);
  assert.equal(result.steps[0]?.error, "boom");
  assert.equal(result.steps[1]?.allowed, false);
  assert.ok(
    result.steps[1]?.denyReason?.includes("step 0"),
    `halt reason must name the failed step; got: ${result.steps[1]?.denyReason}`,
  );
  assert.ok(result.steps[1]?.denyReason?.toLowerCase().includes("fail"));
  // The failure payload must NOT have flowed into step 1's args — gate for step 1
  // must never have been reached.
  assert.equal(bridge.calls.filter((c) => c.op === "execute").length, 1, "only step 0 executed");
  assert.equal(bridge.calls.filter((c) => c.op === "gate").length, 1, "step 1 was never gated");
});

test("Task1: a failed step that nothing references leaves the run running (unchanged behaviour)", async () => {
  const bridge = makeBridge([
    { ok: false, output: null, error: "boom" }, // step 0 fails
    { ok: true, output: "ok" }, // step 1 runs
  ]);
  const wf = def([
    { skill: "doc.read", args: { path: "x" } },
    { skill: "doc.write", args: { content: "static, no ref" } },
  ]);

  const result = await runWorkflow(bridge, wf, {}, silentLog);

  assert.equal(result.halted, false, "run continues past a failed step nothing references");
  assert.equal(result.steps.length, 2);
  assert.equal(result.steps[0]?.error, "boom");
  assert.equal(result.steps[1]?.allowed, true);
  assert.equal(bridge.calls.filter((c) => c.op === "execute").length, 2);
});

test("Task1: an instruction referencing a FAILED step's output halts the run", async () => {
  const bridge = makeBridge([{ ok: false, output: { x: 1 }, error: "boom" }]);
  const wf = def([
    { skill: "doc.read", args: { path: "x" } },
    { kind: "reason", skill: "", instruction: "summarise ${steps.0.output}" },
  ]);

  const result = await runWorkflow(bridge, wf, {}, silentLog);

  assert.equal(result.halted, true);
  assert.equal(result.haltedAtStep, 1);
  assert.ok(result.steps[1]?.denyReason?.includes("step 0"));
  // reason() must never be called for the unresolvable step.
  assert.equal(bridge.calls.filter((c) => c.op === "reason").length, 0);
});

test("Task7: onStep fires once per settled step, in order", async () => {
  const bridge = makeBridge([{ ok: true, output: "a" }, { ok: true, output: "b" }]);
  const wf = def([
    { skill: "doc.read", args: { path: "x" } },
    { skill: "doc.read", args: { path: "y" } },
  ]);

  const seen: number[] = [];
  const result = await runWorkflow(bridge, wf, {}, silentLog, undefined, (o: StepOutcome) => {
    seen.push(o.stepIndex);
  });

  assert.equal(result.halted, false);
  assert.deepEqual(seen, [0, 1], "onStep must fire once per step in order");
});

test("Task7: onStep also fires for the halting step", async () => {
  const bridge = makeBridge([{ ok: true, output: "a" }], { "doc.write": { allow: false, reason: "nope" } });
  const wf = def([
    { skill: "doc.read", args: { path: "x" } },
    { skill: "doc.write", args: { content: "c" } },
  ]);

  const seen: Array<{ i: number; allowed: boolean }> = [];
  await runWorkflow(bridge, wf, {}, silentLog, undefined, (o) => seen.push({ i: o.stepIndex, allowed: o.allowed }));

  assert.deepEqual(seen, [
    { i: 0, allowed: true },
    { i: 1, allowed: false },
  ]);
});

test("Task7: a throwing onStep is failure-isolated — the run completes normally", async () => {
  const bridge = makeBridge([{ ok: true, output: "a" }, { ok: true, output: "b" }]);
  const wf = def([
    { skill: "doc.read", args: { path: "x" } },
    { skill: "doc.read", args: { path: "y" } },
  ]);

  const result = await runWorkflow(bridge, wf, {}, silentLog, undefined, () => {
    throw new Error("callback blew up");
  });

  assert.equal(result.halted, false, "a throwing onStep must not corrupt the run");
  assert.equal(result.steps.length, 2);
});
