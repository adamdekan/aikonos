// run.ts — workflow run executor (CP4).
//
// Drives each step of a WorkflowDef through the GovernanceBridge gate→execute
// path under the runner's identity. Re-validation is not a separate concern
// here — it is the natural consequence of calling bridge.gate() for every step:
// the runner's own OPA + FGA check decides allow/deny for each tool call.
// No inherited authority from authoring time.
// The bridge parameter is typed as BridgeClientLike — the same structural
// interface makeTools() in pi/tools.ts receives. CP5's workflow.run Pi tool will
// pass the SAME bridge instance here, so the type must match exactly.
// GovernanceBridge and RemoteBridgeClient both satisfy BridgeClientLike
// structurally.
import { randomUUID } from "node:crypto";
import type { WorkflowDef } from "./author.js";
import type { BridgeClientLike } from "../ipc/bridge-client.js";

// ── Types ─────────────────────────────────────────────────────────────────────

export interface ResolvedStep {
  stepIndex: number;
  kind: "tool" | "reason";
  skill: string;
  args: Record<string, unknown>;
  instruction: string;
  outputSchema?: Record<string, unknown>;
}

export interface StepOutcome {
  stepIndex: number;
  kind: "tool" | "reason";
  skill: string;
  resolvedArgs: Record<string, unknown>;
  allowed: boolean;
  output: unknown;
  error?: string;
  // Present only when allowed is false.
  denyReason?: string;
}

// REASON_INSTRUCTION_ECHO_CAP caps the resolved instruction echoed into
// resolvedArgs for the session record. The full instruction still goes to the
// model — only the recorded echo is capped.
const REASON_INSTRUCTION_ECHO_CAP = 2000;

export interface RunResult {
  halted: boolean;
  haltedAtStep?: number;
  haltReason?: string;
  steps: StepOutcome[];
}

export interface RunLogger {
  info(obj: unknown, msg?: string): void;
  warn(obj: unknown, msg?: string): void;
}

// ── resolveInputs ─────────────────────────────────────────────────────────────

// TOKEN_RE matches ${inputs.<name>} placeholders in string arg values.
const TOKEN_RE = /\$\{inputs\.([^}]+)\}/g;

// resolveInputs replaces every ${inputs.<name>} token in step args with the
// provided runtime value. Falls back to the input's declared default when the
// caller omits it. Throws when a required input (no default) is missing.
// Non-string arg values and args without tokens pass through unchanged.
export function resolveInputs(
  def: WorkflowDef,
  values: Record<string, string>,
): ResolvedStep[] {
  // Build a lookup: input name → resolved value (caller value > default).
  const resolved: Record<string, string> = {};
  for (const input of def.inputs ?? []) {
    if (input.name in values) {
      resolved[input.name] = values[input.name];
    } else if ("default" in input && input.default !== undefined) {
      // "default" in input (not `!== ""`) so an explicit empty-string default is
      // honored — otherwise `default: ""` silently made the input required.
      resolved[input.name] = input.default;
    }
    // Required inputs with no default are left absent — detected below.
  }

  // replaceTokens replaces every ${inputs.<name>} token in a single string
  // value, shared by tool args and reason instructions.
  function replaceTokens(value: string, stepIndex: number, fieldLabel: string): string {
    let replaced = value;
    let match: RegExpExecArray | null;
    TOKEN_RE.lastIndex = 0;
    while ((match = TOKEN_RE.exec(value)) !== null) {
      const inputName = match[1];
      if (!(inputName in resolved)) {
        throw new Error(
          `workflow: input "${inputName}" is required but was not provided (step ${stepIndex}, ${fieldLabel})`,
        );
      }
      replaced = replaced.replace(match[0], resolved[inputName]);
    }
    return replaced;
  }

  return (def.steps ?? []).map((step, stepIndex) => {
    const kind: "tool" | "reason" = step.kind === "reason" ? "reason" : "tool";

    if (kind === "reason") {
      const instruction = replaceTokens(step.instruction ?? "", stepIndex, "instruction");
      return { stepIndex, kind, skill: step.skill, args: {}, instruction, outputSchema: step.output_schema };
    }

    const args: Record<string, unknown> = {};
    for (const [key, value] of Object.entries(step.args ?? {})) {
      if (typeof value !== "string") {
        args[key] = value;
        continue;
      }
      args[key] = replaceTokens(value, stepIndex, `arg "${key}"`);
    }
    return { stepIndex, kind, skill: step.skill, args, instruction: "" };
  });
}

// ── resolveStepRefs ─────────────────────────────────────────────────────────────

// STEP_REF_RE matches ${steps.<index>.output} and ${steps.<index>.output.<path>}.
// Group 1 = step index (0-based); group 2 = optional dotted drill path into the
// output object, leading dot included (e.g. ".content", ".a.b").
const STEP_REF_RE = /\$\{steps\.(\d+)\.output((?:\.[^}]+)?)\}/g;

// drill walks a dotted path into a step output object. Empty path returns the
// whole output. Throws when a segment is missing so a bad reference fails loudly
// instead of silently producing an empty or wrong value.
function drill(output: unknown, path: string): unknown {
  const parts = path.split(".").filter((p) => p.length > 0);
  let cur = output;
  for (const p of parts) {
    if (cur === null || typeof cur !== "object" || !(p in cur)) {
      throw new Error(`workflow: step output has no field "${p}" for reference "output${path}"`);
    }
    cur = (cur as Record<string, unknown>)[p];
  }
  return cur;
}

// resolveStepRefString replaces ${steps.<i>.output[.path]} tokens in a single
// string (a reason step's instruction) — same semantics as resolveStepRefs'
// per-arg replacement below, factored out so both paths share one
// implementation. `label` names the field in error messages. `failedSteps`
// carries the indices of steps that completed unsuccessfully (a failed tool
// exec): referencing one throws, same halt-on-bad-reference handling as a
// forward/out-of-range or missing-field reference — a failure payload must
// never flow downstream indistinguishably from real output.
function resolveStepRefString(
  value: string,
  priorOutputs: unknown[],
  label: string,
  failedSteps?: ReadonlySet<number>,
): string {
  let replaced = value;
  let match: RegExpExecArray | null;
  STEP_REF_RE.lastIndex = 0;
  while ((match = STEP_REF_RE.exec(value)) !== null) {
    const idx = Number(match[1]);
    if (idx < 0 || idx >= priorOutputs.length) {
      throw new Error(
        `workflow: reference ${match[0]} points to step ${idx}, which has not produced output yet (${label}); a step may only reference earlier steps`,
      );
    }
    if (failedSteps?.has(idx)) {
      throw new Error(
        `workflow: reference ${match[0]} points to step ${idx}, which failed; its output cannot be consumed (${label})`,
      );
    }
    const resolvedVal = drill(priorOutputs[idx], match[2]);
    const asString = typeof resolvedVal === "string" ? resolvedVal : JSON.stringify(resolvedVal);
    replaced = replaced.replace(match[0], asString);
  }
  return replaced;
}

// resolveInstructionRefs replaces ${steps.<i>.output[.path]} tokens in a
// reason step's resolved instruction — the instruction-string counterpart to
// resolveStepRefs' per-arg replacement for tool steps.
export function resolveInstructionRefs(
  instruction: string,
  priorOutputs: unknown[],
  failedSteps?: ReadonlySet<number>,
): string {
  return resolveStepRefString(instruction, priorOutputs, "instruction", failedSteps);
}

// resolveStepRefs replaces ${steps.<i>.output[.path]} tokens in a step's args
// with values from already-executed steps. Unlike resolveInputs (resolved once
// up front), this runs per-step at execution time because a step's output only
// exists after it runs. A resolved value that is a string is embedded as-is; any
// non-string (object/array/number) is JSON-stringified. Throws when a token
// references a step that has not produced output yet (forward/out-of-range) or a
// drill path absent from the output — a step may only consume EARLIER steps.
export function resolveStepRefs(
  args: Record<string, unknown>,
  priorOutputs: unknown[],
  failedSteps?: ReadonlySet<number>,
): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const [key, value] of Object.entries(args)) {
    if (typeof value !== "string") {
      out[key] = value;
      continue;
    }
    const replaced = resolveStepRefString(value, priorOutputs, `arg "${key}"`, failedSteps);
    out[key] = replaced;
  }
  return out;
}

// ── runWorkflow ───────────────────────────────────────────────────────────────

// runWorkflow drives each resolved step through bridge.gate() → bridge.execute()
// in order. A denied gate halts the run immediately: subsequent steps are not
// attempted and the denial reason is returned. The bridge is the same
// GovernanceBridge instance bound to the runner's identity — re-validation is
// enforced by the broker via OPA + FGA on every gate() call, not by this driver.
export async function runWorkflow(
  bridge: BridgeClientLike,
  def: WorkflowDef,
  values: Record<string, string>,
  log: RunLogger,
  // Per-skill read-only hints for mcp: steps, resolved from the MCP server's
  // advertised annotations (see GovernanceBridge.resolveMcpReadOnlyHints). The
  // interactive Pi path passes the same hint per call; without it an mcp: step
  // falls back to mapping.ts's tool-name heuristic, which misclassifies tools
  // whose read verb is not a name prefix (e.g. "pokeapi_get_pokemon") as
  // write-external and needlessly routes them to HITL. A missing key = fall back
  // to the heuristic (undefined opts).
  readOnlyHints?: ReadonlyMap<string, boolean>,
  // Fired after each StepOutcome is recorded, so a caller (the SSE run route)
  // can stream live per-step progress. Failure-isolated: a throwing callback is
  // swallowed so it can never corrupt the run.
  onStep?: (outcome: StepOutcome) => void,
): Promise<RunResult> {
  const resolved = resolveInputs(def, values);
  const outcomes: StepOutcome[] = [];
  // Outputs of executed steps, indexed by stepIndex, for ${steps.N.output} refs.
  const priorOutputs: unknown[] = [];
  // Indices of steps whose tool exec failed (exec.ok === false): a later
  // ${steps.N.output} reference to one of these throws instead of consuming the
  // failure payload as if it were real output.
  const failedSteps = new Set<number>();

  // record pushes an outcome and notifies onStep, isolating callback failures.
  const record = (outcome: StepOutcome): void => {
    outcomes.push(outcome);
    if (onStep) {
      try {
        onStep(outcome);
      } catch {
        /* failure-isolated — a throwing progress callback must not break the run */
      }
    }
  };

  for (const step of resolved) {
    if (step.kind === "reason") {
      // Resolve cross-step output references in the instruction — same
      // halt-on-bad-reference behavior as the tool args path below.
      let instruction: string;
      try {
        instruction = resolveInstructionRefs(step.instruction, priorOutputs, failedSteps);
      } catch (err) {
        const reason = err instanceof Error ? err.message : String(err);
        log.warn({ stepIndex: step.stepIndex, kind: "reason", reason }, "workflow step reference could not be resolved");
        record({
          stepIndex: step.stepIndex,
          kind: "reason",
          skill: step.skill,
          resolvedArgs: { instruction: step.instruction.slice(0, REASON_INSTRUCTION_ECHO_CAP) },
          allowed: false,
          output: null,
          denyReason: reason,
        });
        return { halted: true, haltedAtStep: step.stepIndex, haltReason: reason, steps: outcomes };
      }

      const resolvedArgs = { instruction: instruction.slice(0, REASON_INSTRUCTION_ECHO_CAP) };
      const reasonResult = await bridge.reason(instruction, step.outputSchema);
      if (!reasonResult.ok) {
        const reason = reasonResult.error ?? "reason step failed";
        log.warn({ stepIndex: step.stepIndex, kind: "reason", reason }, "workflow reason step failed");
        record({
          stepIndex: step.stepIndex,
          kind: "reason",
          skill: step.skill,
          resolvedArgs,
          allowed: false,
          output: null,
          denyReason: reason,
        });
        return { halted: true, haltedAtStep: step.stepIndex, haltReason: reason, steps: outcomes };
      }

      priorOutputs[step.stepIndex] = reasonResult.output;
      record({
        stepIndex: step.stepIndex,
        kind: "reason",
        skill: step.skill,
        resolvedArgs,
        allowed: true,
        output: reasonResult.output,
      });
      continue;
    }

    // Resolve cross-step output references against steps already executed. A bad
    // reference (forward/out-of-range or missing field) halts the run with the
    // reason recorded on the step — same shape as a governance denial.
    let args: Record<string, unknown>;
    try {
      args = resolveStepRefs(step.args, priorOutputs, failedSteps);
    } catch (err) {
      const reason = err instanceof Error ? err.message : String(err);
      log.warn({ stepIndex: step.stepIndex, skill: step.skill, reason }, "workflow step reference could not be resolved");
      record({
        stepIndex: step.stepIndex,
        kind: "tool",
        skill: step.skill,
        resolvedArgs: step.args,
        allowed: false,
        output: null,
        denyReason: reason,
      });
      return { halted: true, haltedAtStep: step.stepIndex, haltReason: reason, steps: outcomes };
    }

    const toolCallId = randomUUID();

    // Pass the resolved read-only hint only when we actually have one, so a
    // missing entry leaves opts undefined and the bridge falls back to the
    // name heuristic (rather than forcing an incorrect write classification).
    const opts = readOnlyHints?.has(step.skill)
      ? { readOnlyHint: readOnlyHints.get(step.skill) }
      : undefined;
    const decision = await bridge.gate(toolCallId, step.skill, args, opts);
    if (!decision.allow) {
      const denyReason = decision.reason ?? "denied by policy";
      log.warn(
        { stepIndex: step.stepIndex, skill: step.skill, reason: denyReason },
        "workflow step denied by governance bridge",
      );
      // Record the denied step so the caller has a full, inspectable run trace
      // with no silently-dropped rows (CP18).
      record({
        stepIndex: step.stepIndex,
        kind: "tool",
        skill: step.skill,
        resolvedArgs: args,
        allowed: false,
        output: null,
        denyReason,
      });
      return {
        halted: true,
        haltedAtStep: step.stepIndex,
        haltReason: denyReason,
        steps: outcomes,
      };
    }

    const exec = await bridge.execute(toolCallId);
    priorOutputs[step.stepIndex] = exec.output;
    // A failed exec is recorded, but its output must not be consumable
    // downstream — mark it so a later ${steps.N.output} reference halts.
    if (!exec.ok) failedSteps.add(step.stepIndex);
    record({
      stepIndex: step.stepIndex,
      kind: "tool",
      skill: step.skill,
      resolvedArgs: args,
      allowed: true,
      output: exec.output,
      error: exec.ok ? undefined : exec.error,
    });

    if (!exec.ok) {
      log.warn(
        { stepIndex: step.stepIndex, skill: step.skill, error: exec.error },
        "workflow step execution failed",
      );
    }
  }

  return { halted: false, steps: outcomes };
}
