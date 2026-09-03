// author.ts — deterministic session→Workflow extraction (CP3).
//
// WHY a separate module: authorWorkflow is pure data transformation — no I/O,
// no RPC calls. This keeps it testable without any broker/process involvement.
import type { SessionToolEntry } from "../scheduler/session-record.js";

// ── Types ─────────────────────────────────────────────────────────────────────

export interface WorkflowVisibility {
  kind: "private" | "shared";
  groups?: string[];
}

export interface WorkflowMetadata {
  name: string;
  description?: string;
  version?: number;
  owner?: string;
  visibility: WorkflowVisibility;
}

export interface WorkflowInput {
  name: string;
  schema?: Record<string, unknown>;
  default?: string;
}

// kind absent ⇒ "tool" (back-compat with every definition stored before the
// reason step existed). "reason" steps carry no authority — see
// .
export interface WorkflowStep {
  kind?: "tool" | "reason";
  // WHY skill stays required (not optional) even though a reason step has none:
  // the run driver's ResolvedStep (src/workflow/run.ts, CP-R4) still expects a
  // plain string for every step — a reason step carries "" here, and the driver
  // dispatches on `kind` before ever consulting `skill`.
  skill: string;
  args?: Record<string, unknown>;
  instruction?: string;
  output_schema?: Record<string, unknown>;
}

export interface WorkflowDef {
  apiVersion: "aikonos.com/v1";
  kind: "Workflow";
  metadata: WorkflowMetadata;
  inputs?: WorkflowInput[];
  steps: WorkflowStep[];
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// titleFrom mirrors the webui/session-record.ts helper: first 6 words, ≤40 chars.
function titleFrom(text: string): string {
  const words = text.trim().split(/\s+/);
  const sixWords = words.slice(0, 6).join(" ");
  return sixWords.length > 40 ? sixWords.slice(0, 40) : sixWords;
}

// ISO-8601 date pattern — a standalone date in an arg strongly suggests it is
// variable (the user will want to change it for future runs).
const ISO_DATE_RE = /^\d{4}-\d{2}-\d{2}$/;

// looksLikeISODate returns true for strings that are exactly YYYY-MM-DD.
function looksLikeISODate(value: string): boolean {
  return ISO_DATE_RE.test(value);
}

// ── workflowDefFromToolParams ─────────────────────────────────────────────────

// isStringRecord narrows an unknown value to Record<string, unknown>.
function isStringRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

// workflowDefFromToolParams wraps the flat workflow_save Pi tool params into a
// canonical WorkflowDef that the broker's SaveWorkflow RPC accepts.
//
// WHY this exists: the broker validates that definitionJson contains apiVersion,
// kind, and metadata — the flat tool params carry none of those. The webui path
// works because it sends a pre-built canonical document; the Pi tool path must
// construct the envelope here instead of passing raw params through.
//
// Visibility is always "private" — publishing is a separate workflow_publish
// operation with its own FGA gate.
export function workflowDefFromToolParams(
  params: Record<string, unknown>,
): WorkflowDef {
  const name = typeof params.name === "string" ? params.name : "";
  const description =
    typeof params.description === "string" && params.description.length > 0
      ? params.description
      : undefined;

  const rawSteps = Array.isArray(params.steps) ? params.steps : [];
  const steps: WorkflowStep[] = rawSteps.map((s, i) => {
    const step = isStringRecord(s) ? s : {};
    const rawKind = typeof step.kind === "string" ? step.kind : "";
    if (rawKind !== "" && rawKind !== "tool" && rawKind !== "reason") {
      throw new Error(`step ${i}: invalid kind "${rawKind}" — must be "tool" or "reason"`);
    }
    const kind: "tool" | "reason" = rawKind === "reason" ? "reason" : "tool";

    const skill = typeof step.skill === "string" ? step.skill : "";
    if (step.args !== undefined && !isStringRecord(step.args)) {
      throw new Error(`step ${i}: "args" must be a plain object`);
    }
    const args =
      isStringRecord(step.args) ? (step.args as Record<string, unknown>) : {};
    const instruction = typeof step.instruction === "string" ? step.instruction : "";
    if (step.output_schema !== undefined && !isStringRecord(step.output_schema)) {
      throw new Error(`step ${i}: "output_schema" must be a plain object`);
    }
    const outputSchema = isStringRecord(step.output_schema)
      ? (step.output_schema as Record<string, unknown>)
      : undefined;

    if (kind === "tool") {
      if (skill.length === 0) {
        throw new Error(`step ${i}: "skill" is required for a tool step`);
      }
      if (instruction.length > 0) {
        throw new Error(`step ${i}: "instruction" must be absent for a tool step`);
      }
      if (outputSchema !== undefined) {
        throw new Error(`step ${i}: "output_schema" must be absent for a tool step`);
      }
      return { kind: "tool", skill, args };
    }

    // kind === "reason"
    if (instruction.length === 0) {
      throw new Error(`step ${i}: "instruction" is required for a reason step`);
    }
    if (skill.length > 0) {
      throw new Error(`step ${i}: "skill" must be absent for a reason step`);
    }
    if (step.args !== undefined) {
      throw new Error(`step ${i}: "args" must be absent for a reason step`);
    }
    return {
      kind: "reason",
      skill: "",
      instruction,
      ...(outputSchema !== undefined ? { output_schema: outputSchema } : {}),
    };
  });

  const rawInputs = Array.isArray(params.inputs) ? params.inputs : [];
  const inputs: WorkflowInput[] = rawInputs.map((inp) => {
    const entry = isStringRecord(inp) ? inp : {};
    const inputName = typeof entry.name === "string" ? entry.name : "";
    const result: WorkflowInput = { name: inputName };
    if (typeof entry.default === "string") {
      result.default = entry.default;
    }
    if (isStringRecord(entry.schema)) {
      result.schema = entry.schema as Record<string, unknown>;
    }
    return result;
  });

  return {
    apiVersion: "aikonos.com/v1",
    kind: "Workflow",
    metadata: {
      name,
      ...(description !== undefined ? { description } : {}),
      visibility: { kind: "private" },
    },
    inputs,
    steps,
  };
}

// ── authorWorkflow ────────────────────────────────────────────────────────────

// authorWorkflow extracts a structurally valid WorkflowDef from a session
// transcript. It is deterministic: the same prompt + tools always produces the
// same output. No I/O, no side effects.
//
// Input detection rule: a string arg value is templated as ${inputs.<name>}
// when it also appears verbatim as a substring of the user prompt OR when it
// looks like an ISO date (YYYY-MM-DD). This catches the most common variable
// patterns (referenced entities, dates) without heuristics that could
// over-template stable config values like URLs.
export function authorWorkflow(
  prompt: string,
  tools: SessionToolEntry[],
): WorkflowDef {
  const inputs: WorkflowInput[] = [];
  // Track which input names have been allocated to avoid duplicates.
  const inputNames = new Set<string>();

  const steps: WorkflowStep[] = tools.map((tool) => {
    let rawArgs: Record<string, unknown> = {};
    try {
      rawArgs = JSON.parse(tool.argsJson) as Record<string, unknown>;
    } catch {
      // Unparseable args — treat as empty; the step still records the skill.
    }

    const args: Record<string, unknown> = {};
    for (const [key, value] of Object.entries(rawArgs)) {
      if (
        typeof value === "string" &&
        (prompt.includes(value) || looksLikeISODate(value))
      ) {
        // Pick an input name: prefer the arg key; append a counter if taken.
        let inputName = key;
        if (inputNames.has(inputName)) {
          let counter = 2;
          while (inputNames.has(`${key}_${counter}`)) {
            counter++;
          }
          inputName = `${key}_${counter}`;
        }
        inputNames.add(inputName);
        inputs.push({ name: inputName, default: value });
        args[key] = `\${inputs.${inputName}}`;
      } else {
        args[key] = value;
      }
    }

    return { skill: tool.name, args };
  });

  return {
    apiVersion: "aikonos.com/v1",
    kind: "Workflow",
    metadata: {
      name: titleFrom(prompt),
      visibility: { kind: "private" },
    },
    inputs: inputs.length > 0 ? inputs : [],
    steps,
  };
}
