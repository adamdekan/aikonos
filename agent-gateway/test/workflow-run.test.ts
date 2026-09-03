// TDD tests for resolveInputs + runWorkflow (CP4 — run executor).
//
// WHY these tests exist: the run driver is the core of CP4. It must:
//   1. resolve ${inputs.<name>} tokens in step args from caller-supplied values,
//   2. gate+execute each step through the GovernanceBridge IN ORDER,
//   3. halt at the first denied step and surface the reason (no inherited authority —
//      the runner's own gate decided, not a stored token from authoring time).
// A fake bridge is sufficient because the governance contract (gate→execute) is
// the GovernanceBridge's own concern — tested in governance tests. Here we test
// that runWorkflow calls gate+execute for every step in order and honours halts.
import { test } from "node:test";
import assert from "node:assert/strict";
import { resolveInputs, resolveStepRefs, runWorkflow } from "../src/workflow/run.js";
import type { WorkflowDef } from "../src/workflow/author.js";
import type { BridgeClientLike } from "../src/ipc/bridge-client.js";
import { RemoteBridgeClient } from "../src/ipc/bridge-client.js";
import { ChildLink, type Channel, type IpcMessage } from "../src/ipc/protocol.js";

// ── Fake bridge ────────────────────────────────────────────────────────────────

interface BridgeCall {
  op: "gate" | "execute" | "reason";
  toolCallId?: string;
  toolName?: string;
  args?: Record<string, unknown>;
  opts?: { readOnlyHint?: boolean };
  instruction?: string;
  outputSchema?: Record<string, unknown>;
}

// FakeBridge is BridgeClientLike extended with the test-only helpers that let
// tests queue outputs and inspect call order — without re-stating the intersection
// at every call site.
type FakeBridge = BridgeClientLike & {
  queueResult(output: unknown): void;
  queueReasonResult(result: { ok: boolean; output?: unknown; error?: string }): void;
  calls: BridgeCall[];
};

// makeBridge returns a fake that satisfies BridgeClientLike structurally.
// gate/execute/reason are the only methods runWorkflow calls; the rest are
// stubs that exist only to satisfy the interface the type-checker enforces —
// proving the fake matches the exact contract CP5's workflow.run tool will
// pass at runtime.
function makeBridge(decisions: Record<string, { allow: boolean; reason?: string }> = {}): FakeBridge {
  const calls: BridgeCall[] = [];
  // resultsByOrder maps call-order index (0-based among execute calls) → output.
  // runWorkflow generates UUIDs for toolCallIds at run time, so we can't pre-wire
  // by ID — instead we resolve by the order in which execute() is called.
  const resultsByOrder: unknown[] = [];
  // reasonResultsByOrder maps call-order index (0-based among reason() calls) →
  // the result to return.
  const reasonResultsByOrder: { ok: boolean; output?: unknown; error?: string }[] = [];

  return {
    gate: async (
      toolCallId: string,
      toolName: string,
      args: Record<string, unknown>,
      opts?: { readOnlyHint?: boolean },
    ) => {
      calls.push({ op: "gate", toolCallId, toolName, args, opts });
      const d = decisions[toolName] ?? { allow: true };
      return d;
    },
    execute: async (toolCallId: string) => {
      const execIdx = calls.filter((c) => c.op === "execute").length;
      calls.push({ op: "execute", toolCallId });
      const output = execIdx < resultsByOrder.length ? resultsByOrder[execIdx] : null;
      return { ok: true, output };
    },
    reason: async (instruction: string, outputSchema?: Record<string, unknown>) => {
      const reasonIdx = calls.filter((c) => c.op === "reason").length;
      calls.push({ op: "reason", instruction, outputSchema });
      const result = reasonIdx < reasonResultsByOrder.length ? reasonResultsByOrder[reasonIdx] : { ok: true, output: "" };
      return result;
    },
    // Stubs — runWorkflow never calls these; present to satisfy BridgeClientLike.
    delegate: async () => ({ ok: false, error: "not implemented in fake" }),
    saveWorkflow: async () => ({ ok: false, error: "not implemented in fake" }),
    runWorkflow: async () => ({ ok: false, error: "not implemented in fake" }),
    listWorkflows: async () => ({ ok: false, error: "not implemented in fake" }),
    publishWorkflow: async () => ({ ok: false, error: "not implemented in fake" }),
    proposeWorkflow: async () => ({ ok: false, error: "not implemented in fake" }),
    analyzeImage: async () => ({ ok: false, error: "not implemented in fake" }),
    scheduleWorkflow: async () => ({ ok: false, error: "not implemented in fake" }),
    setApprover: () => {},
    setToken: () => {},
    usageIdentity: () => ({ tenantId: "", userId: "", agentId: "" }),
    // Queue an output to be returned by the nth execute() call (0-indexed).
    queueResult(output: unknown) {
      resultsByOrder.push(output);
    },
    // Queue a result to be returned by the nth reason() call (0-indexed).
    queueReasonResult(result: { ok: boolean; output?: unknown; error?: string }) {
      reasonResultsByOrder.push(result);
    },
    calls,
  };
}

const silentLog = {
  info: (_obj: unknown, _msg?: string) => {},
  warn: (_obj: unknown, _msg?: string) => {},
  error: (_obj: unknown, _msg?: string) => {},
};

// ── Fixtures ───────────────────────────────────────────────────────────────────

function makeDef(overrides: Partial<WorkflowDef> = {}): WorkflowDef {
  return {
    apiVersion: "aikonos.com/v1",
    kind: "Workflow",
    metadata: { name: "test-wf", visibility: { kind: "private" } },
    inputs: [
      { name: "since", default: "2024-01-01" },
      { name: "query", default: "default query" },
    ],
    steps: [
      { skill: "doc.read",  args: { path: "${inputs.since}" } },
      { skill: "web.fetch", args: { url: "https://example.com", q: "${inputs.query}" } },
    ],
    ...overrides,
  };
}

// ── resolveInputs tests ────────────────────────────────────────────────────────

test("resolveInputs: substitutes ${inputs.<name>} tokens from provided values", () => {
  // WHY: the core contract — template tokens in step args must be replaced
  // with the caller's runtime values before the steps are executed.
  const def = makeDef();
  const resolved = resolveInputs(def, { since: "2025-03-01", query: "hello" });

  assert.equal(resolved.length, 2);
  assert.deepEqual(resolved[0]?.args, { path: "2025-03-01" });
  assert.deepEqual(resolved[1]?.args, { url: "https://example.com", q: "hello" });
});

test("resolveInputs: uses input default when a value is not supplied", () => {
  // WHY: inputs with defaults must not require the caller to supply them —
  // omitting a non-required input must silently fall back to the declared default.
  const def = makeDef();
  const resolved = resolveInputs(def, { since: "2025-06-01" }); // query omitted

  assert.deepEqual(resolved[1]?.args, { url: "https://example.com", q: "default query" });
});

test("resolveInputs: throws when a required input (no default) is missing", () => {
  // WHY: a missing required input must error immediately, not produce a broken
  // step with a raw "${inputs.x}" string that confuses the tool proxy.
  const def = makeDef({
    inputs: [{ name: "required_thing" }], // no default
    steps: [{ skill: "doc.read", args: { path: "${inputs.required_thing}" } }],
  });

  assert.throws(
    () => resolveInputs(def, {}),
    /required_thing/,
    "must throw naming the missing required input",
  );
});

test("resolveInputs: non-template args pass through unchanged", () => {
  // WHY: static arg values (not ${inputs.*}) must survive resolution untouched —
  // the resolver must only touch template tokens, nothing else.
  const def = makeDef({
    inputs: [],
    steps: [{ skill: "web.fetch", args: { url: "https://static.example.com", limit: 10 } }],
  });
  const resolved = resolveInputs(def, {});

  assert.deepEqual(resolved[0]?.args, { url: "https://static.example.com", limit: 10 });
});

test("resolveInputs: step with no args produces an empty args object", () => {
  const def = makeDef({
    inputs: [],
    steps: [{ skill: "doc.list" }],
  });
  const resolved = resolveInputs(def, {});
  assert.deepEqual(resolved[0]?.args, {});
});

// ── resolveStepRefs tests (cross-step output chaining) ──────────────────────────

test("resolveStepRefs: substitutes ${steps.N.output} with a prior step's output", () => {
  // WHY: a later step must be able to consume an earlier step's result. Without
  // this the token writes through literally (the live bug: a file contained the
  // raw string "${steps.1.output}").
  const out = resolveStepRefs({ content: "${steps.0.output}" }, ["hello world"]);
  assert.deepEqual(out, { content: "hello world" });
});

test("resolveStepRefs: drills into an object output via a dotted path", () => {
  // doc.read returns { path, content, content_length } — the useful value is
  // .content, so ${steps.N.output.content} must reach into the object.
  const out = resolveStepRefs({ content: "${steps.1.output.content}" }, [{}, { path: "x", content: "EMAIL BODY" }]);
  assert.deepEqual(out, { content: "EMAIL BODY" });
});

test("resolveStepRefs: a whole-object reference is JSON-stringified", () => {
  const out = resolveStepRefs({ x: "${steps.0.output}" }, [{ a: 1 }]);
  assert.deepEqual(out, { x: '{"a":1}' });
});

test("resolveStepRefs: token embedded in surrounding text is replaced in place", () => {
  const out = resolveStepRefs({ msg: "body=${steps.0.output.content} end" }, [{ content: "Z" }]);
  assert.deepEqual(out, { msg: "body=Z end" });
});

test("resolveStepRefs: throws on a forward/out-of-range step reference", () => {
  // A step may only reference EARLIER steps; referencing itself or a later step
  // (no output yet) is a hard error, not a silent empty string.
  assert.throws(() => resolveStepRefs({ x: "${steps.2.output}" }, ["only-one"]), /step 2/);
});

test("resolveStepRefs: throws when the drill path is absent from the output", () => {
  assert.throws(() => resolveStepRefs({ x: "${steps.0.output.missing}" }, [{ content: "y" }]), /missing/);
});

test("resolveStepRefs: non-string args and no-token strings pass through unchanged", () => {
  const out = resolveStepRefs({ n: 5, s: "static" }, ["ignored"]);
  assert.deepEqual(out, { n: 5, s: "static" });
});

// ── runWorkflow tests ──────────────────────────────────────────────────────────

test("runWorkflow: a later step resolves ${steps.N.output} from an earlier step's output", async () => {
  // WHY: end-to-end chaining. Step 0 produces output; step 1 references it. The
  // gate() call AND the recorded resolvedArgs for step 1 must carry the resolved
  // value, not the literal token — this is the reported bug fixed end to end.
  const bridge = makeBridge();
  bridge.queueResult({ content: "EMAIL BODY", path: "t.txt" }); // step 0 output
  bridge.queueResult(null); // step 1 output
  const def = makeDef({
    inputs: [],
    steps: [
      { skill: "doc.read", args: { path: "t.txt" } },
      { skill: "doc.write", args: { path: "out.txt", content: "${steps.0.output.content}" } },
    ],
  });

  const result = await runWorkflow(bridge, def, {}, silentLog);

  assert.equal(result.halted, false);
  const gateCalls = bridge.calls.filter((c) => c.op === "gate");
  assert.deepEqual(gateCalls[1]?.args, { path: "out.txt", content: "EMAIL BODY" }, "step 1 gate must see resolved args");
  assert.deepEqual(result.steps[1]?.resolvedArgs, { path: "out.txt", content: "EMAIL BODY" }, "recorded resolvedArgs must reflect resolution");
});

test("runWorkflow: an unresolvable step reference halts with a clear reason", async () => {
  const bridge = makeBridge();
  bridge.queueResult({ content: "x" });
  const def = makeDef({
    inputs: [],
    steps: [
      { skill: "doc.read", args: { path: "t.txt" } },
      { skill: "doc.write", args: { content: "${steps.5.output}" } },
    ],
  });

  const result = await runWorkflow(bridge, def, {}, silentLog);

  assert.equal(result.halted, true);
  assert.equal(result.haltedAtStep, 1);
  assert.equal(result.steps[1]?.allowed, false);
  assert.ok(result.steps[1]?.denyReason?.includes("step 5"), `halt reason must name the bad reference; got: ${result.steps[1]?.denyReason}`);
});

test("runWorkflow: gates and executes each step in order", async () => {
  // WHY: execution order is the contract. Step 0 must gate+execute before step 1
  // is even gated — parallel or out-of-order execution would break sequential
  // tool chains (e.g. read file → write summary).
  const def = makeDef();
  const bridge = makeBridge();

  const result = await runWorkflow(bridge, def, { since: "2025-01-01", query: "hi" }, silentLog);

  assert.equal(result.halted, false);
  assert.equal(result.steps.length, 2);

  // gate(step0) → execute(step0) → gate(step1) → execute(step1)
  assert.equal(bridge.calls[0]?.op, "gate",    "first call must be gate for step 0");
  assert.equal(bridge.calls[1]?.op, "execute", "second call must be execute for step 0");
  assert.equal(bridge.calls[2]?.op, "gate",    "third call must be gate for step 1");
  assert.equal(bridge.calls[3]?.op, "execute", "fourth call must be execute for step 1");
});

test("runWorkflow: halts at a denied step and surfaces the reason", async () => {
  // WHY: this is the re-validation proof. The runner's own gate decided deny —
  // the run must stop immediately (not proceed to the next step) and the denial
  // reason must be returned so the caller can surface it. No inherited authority.
  const bridge = makeBridge({ "web.fetch": { allow: false, reason: "runner lacks web.fetch" } });
  const def = makeDef();

  const result = await runWorkflow(bridge, def, { since: "2025-01-01", query: "hi" }, silentLog);

  assert.equal(result.halted, true, "run must be halted");
  assert.equal(result.haltedAtStep, 1, "must halt at step index 1 (web.fetch)");
  assert.ok(
    result.haltReason?.includes("runner lacks web.fetch"),
    `haltReason must carry the gate's reason; got: ${result.haltReason}`,
  );
  // Step index 0 (doc.read) executed; step index 1 (web.fetch) gate fired but was
  // denied — execute must NOT have been called for the denied step.
  const executeCallIds = bridge.calls.filter((c) => c.op === "execute").map((c) => c.toolCallId);
  assert.equal(executeCallIds.length, 1, "execute must be called exactly once (only step 0)");
});

test("runWorkflow: collect step results for all allowed steps", async () => {
  // WHY: the caller needs the per-step output to build a run summary or pass
  // results to a downstream consumer. An empty result set is a bug, and silently
  // returning null when the bridge returned a real value is also a bug.
  const bridge = makeBridge();
  const def = makeDef({
    inputs: [],
    steps: [{ skill: "doc.read", args: { path: "/x" } }],
  });
  // Queue the output for the first (and only) execute() call.
  // queueResult uses call order, not toolCallId, because runWorkflow generates
  // UUIDs at run time — we can't know them ahead of time.
  bridge.queueResult("file contents here");

  const result = await runWorkflow(bridge, def, {}, silentLog);

  assert.equal(result.halted, false);
  assert.equal(result.steps.length, 1);
  assert.equal(result.steps[0]?.stepIndex, 0);
  assert.ok(result.steps[0]?.allowed, "step must be marked allowed");
  // Output forwarding: the value the bridge returned must reach the step outcome.
  assert.equal(result.steps[0]?.output, "file contents here", "step output must be forwarded from bridge.execute()");
});

test("runWorkflow: a denied first step produces halted=true with haltedAtStep=0", async () => {
  // WHY: the first step may itself be denied — the halt index is 0-based and must
  // correctly identify the step even when it is the very first.
  const bridge = makeBridge({ "doc.read": { allow: false, reason: "no doc access" } });
  const def = makeDef({
    inputs: [],
    steps: [{ skill: "doc.read", args: { path: "/sensitive" } }],
  });

  const result = await runWorkflow(bridge, def, {}, silentLog);

  assert.equal(result.halted, true);
  assert.equal(result.haltedAtStep, 0);
  // CP18: denied step IS recorded so the caller can render it (no silently-dropped row).
  assert.equal(result.steps.length, 1, "denied step must be recorded");
  assert.equal(result.steps[0]?.allowed, false);
  assert.ok(result.steps[0]?.denyReason?.includes("no doc access"), "denyReason must carry the gate reason");
});

// ── CP18: per-step skill + resolvedArgs + denyReason ─────────────────────────

test("runWorkflow: each allowed step records its skill and resolvedArgs (CP18)", async () => {
  // WHY: CP18 success criterion — the run result carries, per step, the skill and
  // the resolved args. Without these fields a run summary has no inspectable record
  // of what was actually executed (skill name, concrete argument values after
  // ${inputs.*} substitution).
  const bridge = makeBridge();
  bridge.queueResult("step0-out");
  bridge.queueResult("step1-out");

  const result = await runWorkflow(
    bridge,
    makeDef(),
    { since: "2025-03-01", query: "hello" },
    silentLog,
  );

  assert.equal(result.halted, false);
  assert.equal(result.steps.length, 2);

  const s0 = result.steps[0];
  assert.equal(s0?.skill, "doc.read", "step 0 must carry its skill");
  assert.deepEqual(s0?.resolvedArgs, { path: "2025-03-01" }, "step 0 must carry resolved args");
  assert.equal(s0?.output, "step0-out", "step 0 output preserved");

  const s1 = result.steps[1];
  assert.equal(s1?.skill, "web.fetch", "step 1 must carry its skill");
  assert.deepEqual(s1?.resolvedArgs, { url: "https://example.com", q: "hello" }, "step 1 must carry resolved args");
  assert.equal(s1?.output, "step1-out", "step 1 output preserved");
});

test("runWorkflow: a denied step is recorded with allowed:false + denyReason, run halts (CP18)", async () => {
  // WHY: CP18 — a halted step must appear in steps[] so the caller can render it.
  // Previously the denied step was dropped (return early without push). This means
  // a run summary would show step 0 executed and nothing for step 1 — the user
  // can't tell what was denied or why.
  const bridge = makeBridge({ "web.fetch": { allow: false, reason: "runner lacks web.fetch scope" } });
  bridge.queueResult("doc-out");

  const result = await runWorkflow(bridge, makeDef(), { since: "2025-01-01", query: "hi" }, silentLog);

  assert.equal(result.halted, true);
  assert.equal(result.haltedAtStep, 1);
  // Two entries: step 0 allowed + step 1 denied.
  assert.equal(result.steps.length, 2, "both steps must be in steps[] (allowed + denied)");

  const denied = result.steps[1];
  assert.equal(denied?.allowed, false, "denied step must have allowed:false");
  assert.equal(denied?.skill, "web.fetch", "denied step must carry its skill");
  assert.deepEqual(denied?.resolvedArgs, { url: "https://example.com", q: "hi" }, "denied step must carry resolved args");
  assert.ok(
    denied?.denyReason?.includes("runner lacks web.fetch scope"),
    `denyReason must carry the gate reason; got: ${denied?.denyReason}`,
  );
  // execute must NOT have been called for the denied step.
  assert.equal(
    bridge.calls.filter((c) => c.op === "execute").length,
    1,
    "execute called only for the allowed step, not the denied one",
  );
});

test("runWorkflow: bridge.gate() and bridge.execute() are each called once per executed step (CP18 audit parity)", async () => {
  // WHY: audit parity — every executed step must pass through both gate() and
  // execute() on the bridge. Skipping gate() would bypass OPA+FGA re-validation;
  // skipping execute() would bypass the audit record the broker writes on execution.
  const bridge = makeBridge();
  bridge.queueResult(null);
  bridge.queueResult(null);

  await runWorkflow(bridge, makeDef(), { since: "2025-01-01", query: "q" }, silentLog);

  const gates = bridge.calls.filter((c) => c.op === "gate");
  const executes = bridge.calls.filter((c) => c.op === "execute");
  assert.equal(gates.length, 2, "gate must be called once per step");
  assert.equal(executes.length, 2, "execute must be called once per step");
  // gate(step0), execute(step0), gate(step1), execute(step1) — strict ordering.
  assert.equal(bridge.calls[0]?.op, "gate");
  assert.equal(bridge.calls[1]?.op, "execute");
  assert.equal(bridge.calls[2]?.op, "gate");
  assert.equal(bridge.calls[3]?.op, "execute");
});

// ── MCP read-only hint threading ─────────────────────────────────────────────

test("runWorkflow: passes the resolved readOnlyHint to gate() for an mcp: step", async () => {
  // WHY: an MCP tool the server advertises as read-only (e.g. pokeapi_get_pokemon,
  // whose read verb is not a name prefix) must gate with { readOnlyHint: true } so
  // it is classified READ_ONLY and does not get routed to HITL. Without the hint
  // the driver would leave opts undefined and the name heuristic would misclassify
  // it as write-external.
  const bridge = makeBridge();
  bridge.queueResult({ ok: true });
  const skill = "mcp:conn-1:pokeapi_get_pokemon";
  const def = makeDef({
    inputs: [],
    steps: [{ skill, args: { pokemon_name: "eevee" } }],
  });

  const result = await runWorkflow(bridge, def, {}, silentLog, new Map([[skill, true]]));

  assert.equal(result.halted, false);
  const gateCall = bridge.calls.find((c) => c.op === "gate");
  assert.deepEqual(gateCall?.opts, { readOnlyHint: true }, "mcp: step must gate with the resolved read-only hint");
});

test("runWorkflow: leaves gate opts undefined when no hint is known (name-heuristic fallback)", async () => {
  // WHY: a missing hint must NOT force a classification — opts stays undefined so
  // mapping.ts's tool-name heuristic decides, preserving the pre-fix behaviour for
  // unknown tools rather than silently marking them read-only.
  const bridge = makeBridge();
  bridge.queueResult(null);
  const def = makeDef({
    inputs: [],
    steps: [{ skill: "mcp:conn-1:some_tool", args: {} }],
  });

  await runWorkflow(bridge, def, {}, silentLog); // no hints map

  const gateCall = bridge.calls.find((c) => c.op === "gate");
  assert.equal(gateCall?.opts, undefined, "no hint → opts undefined → heuristic fallback");
});

// ── CP-R4: reason step dispatch ────────────────────────────────────────────────

test("runWorkflow: mixed tool → reason → tool run executes in order, reason bypasses gate/execute", async () => {
  const bridge = makeBridge();
  bridge.queueResult({ content: "row data" }); // step 0 (tool) output
  bridge.queueReasonResult({ ok: true, output: { email: "a@b.com" } }); // step 1 (reason) output
  bridge.queueResult(null); // step 2 (tool) output

  const def = makeDef({
    inputs: [],
    steps: [
      { skill: "doc.read", args: { path: "registry.csv" } },
      { kind: "reason", skill: "", instruction: "find the row for ${steps.0.output.content}" },
      { skill: "doc.write", args: { path: "out.txt", content: "${steps.1.output.email}" } },
    ],
  });

  const result = await runWorkflow(bridge, def, {}, silentLog);

  assert.equal(result.halted, false);
  assert.equal(result.steps.length, 3);
  assert.equal(result.steps[1]?.kind, "reason");
  assert.deepEqual(result.steps[1]?.output, { email: "a@b.com" });

  // Ordering: gate/execute for step 0, reason for step 1, gate/execute for step 2.
  assert.deepEqual(
    bridge.calls.map((c) => c.op),
    ["gate", "execute", "reason", "gate", "execute"],
  );
  // No gate/execute call for the reason step itself.
  assert.equal(bridge.calls[2]?.op, "reason");

  // Step 2's tool args reference the reason step's structured output field.
  const step2Gate = bridge.calls[3];
  assert.deepEqual(step2Gate?.args, { path: "out.txt", content: "a@b.com" });
});

test("runWorkflow: reason step's raw-text output is referenced by a later tool step", async () => {
  const bridge = makeBridge();
  bridge.queueReasonResult({ ok: true, output: "plain text answer" });
  bridge.queueResult(null);

  const def = makeDef({
    inputs: [],
    steps: [
      { kind: "reason", skill: "", instruction: "answer the question" },
      { skill: "doc.write", args: { content: "${steps.0.output}" } },
    ],
  });

  const result = await runWorkflow(bridge, def, {}, silentLog);

  assert.equal(result.halted, false);
  const gateCall = bridge.calls.find((c) => c.op === "gate");
  assert.deepEqual(gateCall?.args, { content: "plain text answer" });
});

test("runWorkflow: reason step failure halts the run with a named reason; subsequent steps not executed", async () => {
  const bridge = makeBridge();
  bridge.queueReasonResult({ ok: false, error: "no default LLM provider configured" });

  const def = makeDef({
    inputs: [],
    steps: [
      { kind: "reason", skill: "", instruction: "do something" },
      { skill: "doc.write", args: { content: "should not run" } },
    ],
  });

  const result = await runWorkflow(bridge, def, {}, silentLog);

  assert.equal(result.halted, true);
  assert.equal(result.haltedAtStep, 0);
  assert.ok(result.haltReason?.includes("no default LLM provider configured"));
  assert.equal(result.steps.length, 1, "the tool step after the failed reason step must not be recorded");
  assert.equal(bridge.calls.filter((c) => c.op === "gate" || c.op === "execute").length, 0, "no gate/execute for the reason step nor the never-reached tool step");
});

test("runWorkflow: reason step calls bridge.reason exactly once, never gate/execute", async () => {
  const bridge = makeBridge();
  bridge.queueReasonResult({ ok: true, output: "x" });

  const def = makeDef({
    inputs: [],
    steps: [{ kind: "reason", skill: "", instruction: "think" }],
  });

  const result = await runWorkflow(bridge, def, {}, silentLog);

  assert.equal(result.halted, false);
  assert.equal(bridge.calls.length, 1);
  assert.equal(bridge.calls[0]?.op, "reason");
});

test("runWorkflow: reason instruction resolves ${inputs.*} and ${steps.N.output} tokens", async () => {
  const bridge = makeBridge();
  bridge.queueResult("row-output");
  bridge.queueReasonResult({ ok: true, output: "answer" });

  const def = makeDef({
    inputs: [{ name: "ip", default: "1.2.3.4" }],
    steps: [
      { skill: "doc.read", args: { path: "registry.csv" } },
      { kind: "reason", skill: "", instruction: "find ${inputs.ip} in ${steps.0.output}" },
    ],
  });

  const result = await runWorkflow(bridge, def, {}, silentLog);

  assert.equal(result.halted, false);
  const reasonCall = bridge.calls.find((c) => c.op === "reason");
  assert.equal(reasonCall?.instruction, "find 1.2.3.4 in row-output");
});

test("runWorkflow: reason instruction with a forward/self step reference halts the run", async () => {
  const bridge = makeBridge();

  const def = makeDef({
    inputs: [],
    steps: [{ kind: "reason", skill: "", instruction: "see ${steps.0.output}" }],
  });

  const result = await runWorkflow(bridge, def, {}, silentLog);

  assert.equal(result.halted, true);
  assert.equal(result.haltedAtStep, 0);
  assert.ok(result.haltReason?.includes("step 0"));
  assert.equal(bridge.calls.length, 0, "bridge.reason must not be called when the reference cannot resolve");
});

test("runWorkflow: resolvedArgs.instruction is capped at 2000 chars for the session record", async () => {
  const bridge = makeBridge();
  const longInstruction = "x".repeat(3000);
  bridge.queueReasonResult({ ok: true, output: "y" });

  const def = makeDef({
    inputs: [],
    steps: [{ kind: "reason", skill: "", instruction: longInstruction }],
  });

  const result = await runWorkflow(bridge, def, {}, silentLog);

  assert.equal(result.halted, false);
  const echoed = result.steps[0]?.resolvedArgs.instruction;
  assert.equal(typeof echoed, "string");
  assert.equal((echoed as string).length, 2000, "echoed instruction must be capped at 2000 chars");
  // The full instruction (uncapped) must still be what reaches the model.
  const reasonCall = bridge.calls.find((c) => c.op === "reason");
  assert.equal(reasonCall?.instruction?.length, 3000, "the full instruction must still be sent to the model");
});

// ── CP-R4: reason steps execute parent-side only (pinning test) ────────────────
//
// runWorkflow always drives against whatever BridgeClientLike instance is
// passed in. In production there are exactly two call sites:
//   - governance.ts:GovernanceBridge.runWorkflow calls runWorkflowDriver(this, ...)
//     — `this` is the parent's own GovernanceBridge, so bridge.reason() resolves
//     to GovernanceBridge.reason(), which calls the LLM provider directly from
//     the parent process.
//   - pi/tools.ts's workflow_run tool forwards ONE "run-workflow" IPC request to
//     the parent (bridge-server.ts), which then runs the SAME runWorkflowDriver
//     against the parent's own GovernanceBridge — never against the child's
//     RemoteBridgeClient. The child's RemoteBridgeClient.reason() is therefore
//     unreachable in practice; it exists only to satisfy BridgeClientLike.
// This test pins that unreachability directly: RemoteBridgeClient.reason()
// must resolve without ever touching the IPC link (no "reason" message kind
// exists in the protocol — see src/ipc/protocol.ts's IpcMessage union — so a
// per-step reason call could never be forwarded to the parent even if it tried).
test("RemoteBridgeClient.reason() never forwards over IPC — the child cannot drive reason steps", async () => {
  let sent = false;
  const fakeChannel: Channel = {
    send: (_msg: unknown) => {
      sent = true;
    },
    on: (_event: "message", _handler: (msg: IpcMessage) => void) => {},
  };
  const client = new RemoteBridgeClient(new ChildLink(fakeChannel));

  const result = await client.reason("do something", undefined);

  assert.equal(sent, false, "RemoteBridgeClient.reason() must never send an IPC message to the parent");
  assert.equal(result.ok, false);
  assert.equal(result.error, "reason steps execute parent-side only");
});
