// Maps a Pi tool call (toolName + input) to a aikonos tool_id + effect_class.
// The aikonos Tool Proxy only implements a fixed set of tools; the gateway
// exposes exactly those to the LLM as custom tools (see pi/tools.ts), so the
// mapping is 1:1 by name. Effect classes mirror the aikonos toolregistry +
// skill contracts and drive the OPA tool_invocation policy.
import { EffectClass } from "../../gen/ts/proto/plan";
import { resolveMcpAlias } from "../pi/mcp-alias.js";

// Read-verb prefixes that mark an MCP tool as side-effect-free. Matches a bare
// verb ("get"), snake_case ("get_article"), or camelCase ("getArticle"): the
// char after the verb must be end-of-string, "_", or an uppercase letter.
const READ_VERBS = ["get", "list", "search", "read", "fetch", "describe", "lookup", "query", "find"];

function isReadOnlyToolName(toolName: string): boolean {
  for (const verb of READ_VERBS) {
    if (toolName.length < verb.length) continue;
    if (toolName.slice(0, verb.length).toLowerCase() !== verb) continue;
    const next = toolName[verb.length];
    if (next === undefined || next === "_" || (next >= "A" && next <= "Z")) return true;
  }
  return false;
}

/** Resolved aikonos tool identity: the dotted broker tool_id and its OPA effect_class used to drive approval routing. */
export interface ToolMapping {
  toolId: string;
  effectClass: EffectClass;
}

// The aikonos-native tools the gateway surfaces. Keyed by the PI tool name
// (LLM/OpenAI function names can't contain dots, so we use underscores) and
// mapped to the aikonos tool_id (dotted) the broker registry + Tool Proxy know.
const TOOLS: Record<string, ToolMapping> = {
  web_fetch: { toolId: "web.fetch", effectClass: EffectClass.READ_ONLY },
  web_search: { toolId: "web.search", effectClass: EffectClass.READ_ONLY },
  doc_read: { toolId: "doc.read", effectClass: EffectClass.READ_ONLY },
  workspace_read: { toolId: "workspace.read", effectClass: EffectClass.READ_ONLY },
  doc_write: { toolId: "doc.write", effectClass: EffectClass.WRITE_LOCAL },
  // email.draft is declared WRITE_EXTERNAL so the OPA policy routes it to human
  // approval (the HITL path) — the broker Tool Proxy still composes the draft.
  email_draft: { toolId: "email.draft", effectClass: EffectClass.WRITE_EXTERNAL },
  // Cloud-storage connectors. Reads auto-approve; writes are WRITE_EXTERNAL so
  // OPA routes them to human approval before anything lands in the user's Drive.
  gdrive_read: { toolId: "gdrive.read", effectClass: EffectClass.READ_ONLY },
  gdrive_write: { toolId: "gdrive.write", effectClass: EffectClass.WRITE_EXTERNAL },
  onedrive_read: { toolId: "onedrive.read", effectClass: EffectClass.READ_ONLY },
  onedrive_write: { toolId: "onedrive.write", effectClass: EffectClass.WRITE_EXTERNAL },
  // analyze_image is gated by the "vision" capability skill (CP2), not a Tool
  // Proxy invocation — its toolId is the bare skill name so the broker's
  // per-step FGA check (skill:<toolID>) becomes "skill:vision". Read-only: it
  // reads a workspace file and calls a vision provider, no side effects.
  analyze_image: { toolId: "vision", effectClass: EffectClass.READ_ONLY },
  // spawn_subagents is gated by the
  // "subagents" capability skill, not a Tool Proxy invocation — its toolId is
  // the bare skill name so the broker's per-step FGA check (skill:<toolID>)
  // becomes "skill:subagents", mirroring analyze_image's "vision" entry above.
  // READ_ONLY: the spawn call itself has no side effect of its own — it forks
  // children and calls the parent-side aggregator; every branch's own tool
  // call is separately gated through the ordinary path.
  spawn_subagents: { toolId: "subagents", effectClass: EffectClass.READ_ONLY },
  // Office document tools (office-worker backed). Effect classes mirror
  // broker/internal/toolregistry/authoritative.go: the four *.extract tools are
  // READ_ONLY, everything else writes to the workspace (WRITE_LOCAL). They
  // route through the ordinary gate → SubmitPlan → InvokeTool path.
  docx_create: { toolId: "docx.create", effectClass: EffectClass.WRITE_LOCAL },
  docx_edit: { toolId: "docx.edit", effectClass: EffectClass.WRITE_LOCAL },
  docx_extract: { toolId: "docx.extract", effectClass: EffectClass.READ_ONLY },
  xlsx_create: { toolId: "xlsx.create", effectClass: EffectClass.WRITE_LOCAL },
  xlsx_edit: { toolId: "xlsx.edit", effectClass: EffectClass.WRITE_LOCAL },
  xlsx_extract: { toolId: "xlsx.extract", effectClass: EffectClass.READ_ONLY },
  xlsx_recalc: { toolId: "xlsx.recalc", effectClass: EffectClass.WRITE_LOCAL },
  pptx_create: { toolId: "pptx.create", effectClass: EffectClass.WRITE_LOCAL },
  pptx_edit: { toolId: "pptx.edit", effectClass: EffectClass.WRITE_LOCAL },
  pptx_extract: { toolId: "pptx.extract", effectClass: EffectClass.READ_ONLY },
  pptx_thumbnail: { toolId: "pptx.thumbnail", effectClass: EffectClass.WRITE_LOCAL },
  pdf_create: { toolId: "pdf.create", effectClass: EffectClass.WRITE_LOCAL },
  pdf_transform: { toolId: "pdf.transform", effectClass: EffectClass.WRITE_LOCAL },
  pdf_extract: { toolId: "pdf.extract", effectClass: EffectClass.READ_ONLY },
  office_convert: { toolId: "office.convert", effectClass: EffectClass.WRITE_LOCAL },
  // Agent memory. Per-tool skills — no umbrella
  // "memory" skill — so each dotted id is its own FGA grant, and read-only
  // memory is a valid posture. Ordinary gated path, like the office tools.
  memory_read: { toolId: "memory.read", effectClass: EffectClass.READ_ONLY },
  memory_write: { toolId: "memory.write", effectClass: EffectClass.WRITE_LOCAL },
};

// Reverse lookup keyed by the dotted broker toolId. Callers that already hold
// the canonical broker skill id (not the Pi underscore name) resolve through
// this — notably the workflow run driver: stored WorkflowDef steps carry
// `skill: "web.fetch"`, because workflow_save/propose document the field as a
// "broker skill id, e.g. web.fetch". Without this, every workflow step would be
// denied ("tool 'web.fetch' is not permitted by aikonos policy") and runs would
// always halt at step 0. Keys (dotted) never collide with TOOLS keys (underscore).
const TOOLS_BY_ID: Record<string, ToolMapping> = Object.fromEntries(
  Object.values(TOOLS).map((m) => [m.toolId, m]),
);

// MCP tools use a Pi name of the form mcp__<connectorId>__<toolName>; the Pi
// name cannot contain colons (OpenAI function name restrictions), so we use
// double-underscores as the separator and reconstruct the broker toolId here.
// Effect class: READ_ONLY when the server advertises readOnlyHint or the tool
// name starts with a read verb; WRITE_EXTERNAL otherwise (HITL-eligible).
function mapMcpTool(piToolName: string, readOnlyHint?: boolean): ToolMapping | undefined {
  if (!piToolName.startsWith("mcp__")) return undefined;
  // mcp__<alias>__<toolName...> → toolId: mcp:<connectorId>:<toolName...>
  // The first segment is a short alias, not the connector id — see
  // pi/mcp-alias.ts for why the full UUID cannot fit in a 64-char function name.
  const rest = piToolName.slice("mcp__".length);
  const sep = rest.indexOf("__");
  if (sep === -1) return undefined;
  // An alias no session registered is an unknown tool, not a connector id to
  // pass through: this toolId is what gets FGA-checked and Biscuit-scoped, so an
  // unattributable name must fail closed.
  const connectorId = resolveMcpAlias(rest.slice(0, sep));
  if (connectorId === undefined) return undefined;
  return mcpMapping(connectorId, rest.slice(sep + 2), readOnlyHint);
}

// mapMcpBrokerId resolves the dotted broker-id form of an MCP tool
// (mcp:<connectorId>:<toolName>) — the form workflow steps store — mirroring
// mapMcpTool's Pi double-underscore form.
function mapMcpBrokerId(brokerId: string, readOnlyHint?: boolean): ToolMapping | undefined {
  if (!brokerId.startsWith("mcp:")) return undefined;
  const rest = brokerId.slice("mcp:".length);
  const sep = rest.indexOf(":");
  if (sep === -1) return undefined;
  return mcpMapping(rest.slice(0, sep), rest.slice(sep + 1), readOnlyHint);
}

function mcpMapping(connectorId: string, toolName: string, readOnlyHint?: boolean): ToolMapping | undefined {
  if (!connectorId || !toolName) return undefined;
  const effectClass =
    readOnlyHint === true || isReadOnlyToolName(toolName)
      ? EffectClass.READ_ONLY
      : EffectClass.WRITE_EXTERNAL;
  return {
    toolId: `mcp:${connectorId}:${toolName}`,
    effectClass,
  };
}

/**
 * Maps a tool name to a ToolMapping. Accepts both forms a caller may hold:
 * the Pi underscore name the LLM sees (web_fetch, mcp__conn__tool) and the
 * canonical dotted broker skill id workflow steps store (web.fetch, mcp:conn:tool).
 * Handles built-in aikonos tools and MCP tools; returns undefined for unknown tools.
 */
export function mapTool(toolName: string, readOnlyHint?: boolean): ToolMapping | undefined {
  return (
    TOOLS[toolName] ??
    TOOLS_BY_ID[toolName] ??
    mapMcpTool(toolName, readOnlyHint) ??
    mapMcpBrokerId(toolName, readOnlyHint)
  );
}

export function knownToolNames(): string[] {
  return Object.keys(TOOLS);
}

// knownToolIds returns the built-in aikonos tool ids in the canonical dotted form
// (web.fetch, doc.read, …). Used to tell the model which skills a workflow may
// reference and to build the error message when it invents one.
export function knownToolIds(): string[] {
  return Object.values(TOOLS).map((m) => m.toolId);
}

// Skills that resolve via mapTool (so their own tool_call gets real per-call
// FGA gating) but are NOT valid workflow-step targets, because they have no
// Tool Proxy registration to route to via InvokeTool at run time. The five
// workflow_* tool names and "delegate" need no entry here — they're already
// excluded by simply never appearing in TOOLS/TOOLS_BY_ID. "vision" is
// different: analyze_image's toolId ("vision") deliberately lives in TOOLS so
// mapTool resolves it (see the analyze_image entry above), which would
// otherwise let a workflow step reference `skill: "vision"` and pass
// authoring-time validation, only to fail at run time instead of being
// cleanly rejected up front.
// "subagents" is the same shape as "vision":
// spawn_subagents' toolId resolves via mapTool for its own tool_call gating,
// but workflow-level fan-out is an explicit spec non-goal, so a workflow step
// must not be able to reference it either.
const WORKFLOW_UNRESOLVABLE_SKILLS = new Set(["vision", "subagents"]);

// workflowResolvableToolIds returns the built-in aikonos tool ids a workflow
// step's `skill` field may legally reference — knownToolIds() minus the
// skills that resolve via mapTool but aren't Tool-Proxy-routable.
function workflowResolvableToolIds(): string[] {
  return knownToolIds().filter((id) => !WORKFLOW_UNRESOLVABLE_SKILLS.has(id));
}

// unknownSkills returns the subset of skill ids that do NOT resolve to a
// workflow-routable aikonos tool. Used to reject workflow definitions
// referencing invented (or run-time-unroutable, e.g. "vision") tools at
// authoring time — the same resolver that gates them at run time, so a saved
// workflow can never contain a step that would be denied for being unknown.
// mcp:<connector>:<tool> ids resolve structurally (access is enforced per-run
// by FGA), so they are accepted here; only genuinely unresolvable ids are
// flagged.
export function unknownSkills(skills: string[]): string[] {
  const resolvable = new Set(workflowResolvableToolIds());
  return skills.filter((s) => {
    const mapping = mapTool(s);
    if (mapping === undefined) return true;
    // mcp: tools always pass (structural match); built-in tools must also be
    // workflow-resolvable (not in WORKFLOW_UNRESOLVABLE_SKILLS).
    if (mapping.toolId.startsWith("mcp:")) return false;
    return !resolvable.has(mapping.toolId);
  });
}

// knownWorkflowStepSkills returns the tool ids to suggest in the "invented
// skill" error message shown to a workflow author — the same universe
// unknownSkills validates against, so the guidance never lists an id that
// would itself be rejected.
export function knownWorkflowStepSkills(): string[] {
  return workflowResolvableToolIds();
}
