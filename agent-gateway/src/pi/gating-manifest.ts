// Declarative per-tool gating manifest (F4).
//
// WHY: two gating models were tribal knowledge spread across governance.ts
// (bridge-direct skip list, ~L110-121), gate-tool-call.ts (the load_skill
// exemption, ~L27-30), and mapping.ts (which names resolve via mapTool at
// all). This manifest is the single declared source; CP2 wires
// gateSkippedTools()/ungatedBuiltins() into those two call sites so the
// declared model and the actual behavior cannot drift. Placed in src/pi/
// (alongside tools.ts, whose TOOL_NAMES it imports) rather than src/broker/:
// governance.ts's existing imports of GovernanceBridge from src/pi/ files
// (session.ts, mcp-tools.ts) are all `import type`, erased
// at compile time, so broker/governance.ts importing this runtime module from
// src/pi/ does not close a require cycle — tools.ts itself imports nothing
// from src/broker.
import { TOOL_NAMES } from "./tools.js";

export type GatingModel = "gated" | "bridge-direct" | "gate-then-bridge-direct" | "ungated-builtin";

export interface GatingEntry {
  model: GatingModel;
  authz: string;
}

// Structural rule for all mcp__* tool names: they flow gate→execute exactly
// like static "gated" tools (GovernanceBridge.gate() → mapTool resolves the
// mcp__<conn>__<tool> name via mapMcpTool → JIT plan → InvokeTool routes to
// Proxy.invokeMCP, which itself calls CheckFGA per-call for mcp: ids).
export const MCP_GATING: GatingModel = "gated";

export const GATING_MANIFEST: Readonly<Record<string, GatingEntry>> = {
  web_fetch: {
    model: "gated",
    authz: "JIT plan: SubmitPlan → CheckFGA(user, can_invoke, skill:web.fetch) → Biscuit → InvokeTool",
  },
  web_search: {
    model: "gated",
    authz: "JIT plan: SubmitPlan → CheckFGA(user, can_invoke, skill:web.search) → Biscuit → InvokeTool",
  },
  workspace_read: {
    model: "gated",
    authz: "JIT plan: SubmitPlan → CheckFGA(user, can_invoke, skill:workspace.read) → Biscuit → InvokeTool",
  },
  doc_read: {
    model: "gated",
    authz: "JIT plan: SubmitPlan → CheckFGA(user, can_invoke, skill:doc.read) → Biscuit → InvokeTool",
  },
  doc_write: {
    model: "gated",
    authz: "JIT plan: SubmitPlan → CheckFGA(user, can_invoke, skill:doc.write) → Biscuit → InvokeTool",
  },
  email_draft: {
    model: "gated",
    authz: "JIT plan: SubmitPlan → CheckFGA(user, can_invoke, skill:email.draft) → Biscuit → InvokeTool",
  },
  gdrive_read: {
    model: "gated",
    authz: "JIT plan: SubmitPlan → CheckFGA(user, can_invoke, skill:gdrive.read) → Biscuit → InvokeTool",
  },
  gdrive_write: {
    model: "gated",
    authz: "JIT plan: SubmitPlan → CheckFGA(user, can_invoke, skill:gdrive.write) → Biscuit → InvokeTool",
  },
  onedrive_read: {
    model: "gated",
    authz: "JIT plan: SubmitPlan → CheckFGA(user, can_invoke, skill:onedrive.read) → Biscuit → InvokeTool",
  },
  onedrive_write: {
    model: "gated",
    authz: "JIT plan: SubmitPlan → CheckFGA(user, can_invoke, skill:onedrive.write) → Biscuit → InvokeTool",
  },
  delegate: {
    model: "bridge-direct",
    authz: "broker SendEnvelope: OPA envelope_send + FGA re-check",
  },
  workflow_save: {
    model: "bridge-direct",
    authz: "broker RPC boundary: CheckFGA(user, can_invoke, skill:workflows)",
  },
  workflow_run: {
    model: "bridge-direct",
    authz:
      "broker RPC boundary: CheckFGA(user, can_invoke, skill:workflows) to fetch/run; each step re-gates individually at run time through GovernanceBridge.gate() under the runner's own grants",
  },
  workflow_list: {
    model: "bridge-direct",
    authz: "broker RPC boundary: CheckFGA(user, can_invoke, skill:workflows)",
  },
  workflow_publish: {
    model: "bridge-direct",
    authz: "broker RPC boundary: CheckFGA(user, can_invoke, skill:workflows)",
  },
  workflow_propose: {
    model: "bridge-direct",
    authz: "broker RPC boundary: CheckFGA(user, can_invoke, skill:workflows)",
  },
  workflow_schedule: {
    model: "bridge-direct",
    authz:
      "broker RPC boundary: CheckFGA(user, can_invoke, skill:scheduler) at create + workflow visibility re-check (skill:workflows) via GetWorkflow",
  },
  analyze_image: {
    model: "gate-then-bridge-direct",
    authz: "JIT plan CheckFGA(skill:vision); execution bridge-direct, bypasses InvokeTool",
  },
  // spawn_subagents mirrors analyze_image:
  // JIT-plan-gated (CheckFGA skill:subagents) via gate(), then executed
  // bridge-direct — it has no Tool Proxy registration, and every branch's own
  // tool call is separately gated through the ordinary path.
  spawn_subagents: {
    model: "gate-then-bridge-direct",
    authz: "JIT plan CheckFGA(skill:subagents); execution bridge-direct, spawns branch children directly, bypasses InvokeTool",
  },
  // Office document tools — ordinary gated path (SubmitPlan → CheckFGA(skill:<id>)
  // → Biscuit → InvokeTool → office-worker), same as web.fetch/doc.write.
  docx_create: { model: "gated", authz: "JIT plan: SubmitPlan → CheckFGA(user, can_invoke, skill:docx.create) → Biscuit → InvokeTool" },
  docx_edit: { model: "gated", authz: "JIT plan: SubmitPlan → CheckFGA(user, can_invoke, skill:docx.edit) → Biscuit → InvokeTool" },
  docx_extract: { model: "gated", authz: "JIT plan: SubmitPlan → CheckFGA(user, can_invoke, skill:docx.extract) → Biscuit → InvokeTool" },
  xlsx_create: { model: "gated", authz: "JIT plan: SubmitPlan → CheckFGA(user, can_invoke, skill:xlsx.create) → Biscuit → InvokeTool" },
  xlsx_edit: { model: "gated", authz: "JIT plan: SubmitPlan → CheckFGA(user, can_invoke, skill:xlsx.edit) → Biscuit → InvokeTool" },
  xlsx_extract: { model: "gated", authz: "JIT plan: SubmitPlan → CheckFGA(user, can_invoke, skill:xlsx.extract) → Biscuit → InvokeTool" },
  xlsx_recalc: { model: "gated", authz: "JIT plan: SubmitPlan → CheckFGA(user, can_invoke, skill:xlsx.recalc) → Biscuit → InvokeTool" },
  pptx_create: { model: "gated", authz: "JIT plan: SubmitPlan → CheckFGA(user, can_invoke, skill:pptx.create) → Biscuit → InvokeTool" },
  pptx_edit: { model: "gated", authz: "JIT plan: SubmitPlan → CheckFGA(user, can_invoke, skill:pptx.edit) → Biscuit → InvokeTool" },
  pptx_extract: { model: "gated", authz: "JIT plan: SubmitPlan → CheckFGA(user, can_invoke, skill:pptx.extract) → Biscuit → InvokeTool" },
  pptx_thumbnail: { model: "gated", authz: "JIT plan: SubmitPlan → CheckFGA(user, can_invoke, skill:pptx.thumbnail) → Biscuit → InvokeTool" },
  pdf_create: { model: "gated", authz: "JIT plan: SubmitPlan → CheckFGA(user, can_invoke, skill:pdf.create) → Biscuit → InvokeTool" },
  pdf_transform: { model: "gated", authz: "JIT plan: SubmitPlan → CheckFGA(user, can_invoke, skill:pdf.transform) → Biscuit → InvokeTool" },
  pdf_extract: { model: "gated", authz: "JIT plan: SubmitPlan → CheckFGA(user, can_invoke, skill:pdf.extract) → Biscuit → InvokeTool" },
  office_convert: { model: "gated", authz: "JIT plan: SubmitPlan → CheckFGA(user, can_invoke, skill:office.convert) → Biscuit → InvokeTool" },
  // Agent memory — ordinary gated path. The group scope adds a second check
  // inside InvokeTool (CheckFGA(user, member, group:<id>)) on top of the
  // per-tool skill grant below.
  memory_read: { model: "gated", authz: "JIT plan: SubmitPlan → CheckFGA(user, can_invoke, skill:memory.read) → Biscuit → InvokeTool" },
  memory_write: { model: "gated", authz: "JIT plan: SubmitPlan → CheckFGA(user, can_invoke, skill:memory.write) → Biscuit → InvokeTool" },
  load_skill: {
    model: "ungated-builtin",
    authz: "FGA checked upstream at skill-list resolution",
  },
  // read_skill_file: same posture as
  // load_skill — read-only, no side effects, authority already established
  // upstream (FGA can_use for a bundle / workspace ownership for a personal
  // skill). No SubmitPlan round-trip for reading tenant/self-owned content.
  read_skill_file: {
    model: "ungated-builtin",
    authz: "FGA checked upstream at skill-list resolution (bundles) / workspace ownership (personal)",
  },
};

// Assert every TOOL_NAMES entry has a manifest entry, at module load — a new
// Pi tool added to tools.ts without a manifest entry fails fast instead of
// silently defaulting to some other tool's gating model.
for (const name of TOOL_NAMES) {
  if (!(name in GATING_MANIFEST)) {
    throw new Error(`gating-manifest.ts: missing entry for tool '${name}'`);
  }
}

/** Tool names whose gate() call skips the JIT plan (bridge-direct model). */
export function gateSkippedTools(): ReadonlySet<string> {
  return new Set(
    Object.entries(GATING_MANIFEST)
      .filter(([, entry]) => entry.model === "bridge-direct")
      .map(([name]) => name),
  );
}

/** Tool names whose gate() call is for the policy decision only — no capability
 *  token is minted or required (gate-then-bridge-direct model). Their toolId is
 *  a bare capability-skill name ("vision"/"subagents") with no toolregistry
 *  scope, so the broker mints nothing, and they execute via a direct parent-side
 *  bridge call that never reaches InvokeTool — there is no Biscuit to redeem. */
export function decisionOnlyGatedTools(): ReadonlySet<string> {
  return new Set(
    Object.entries(GATING_MANIFEST)
      .filter(([, entry]) => entry.model === "gate-then-bridge-direct")
      .map(([name]) => name),
  );
}

/** Tool names unconditionally allowed without calling bridge.gate (ungated-builtin model). */
export function ungatedBuiltins(): ReadonlySet<string> {
  return new Set(
    Object.entries(GATING_MANIFEST)
      .filter(([, entry]) => entry.model === "ungated-builtin")
      .map(([name]) => name),
  );
}
