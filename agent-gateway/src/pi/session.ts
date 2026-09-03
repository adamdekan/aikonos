// Skill-filtering helpers shared by the resolve half (session-plan.ts) and the
// legacy resolveEnvModel fallback. The session-building logic itself
// (buildSession) was removed in F29 — resolveSessionPlan + createSessionFromPlan
// (session-plan.ts) are the single session-construction path now.
import type { Config } from "../config";
import type { SouthClient } from "../broker/south";
import type { LlmProvider } from "../../gen/ts/proto/broker";
import { log } from "../log";
import { failedPreconditionError } from "../http-errors.js";
import { chatCandidates, type Candidate } from "../llm/provider-fallback.js";

// Maps the broker's dotted tool ids to the Pi underscore tool names the LLM
// sees. Pi function names can't contain dots (OpenAI restriction), so the
// gateway translates on both ends. This is the inverse of the TOOLS map in
// broker/mapping.ts and must stay in sync with it.
export const BUILTIN_TOOL_NAME: Record<string, string> = {
  "web.fetch":      "web_fetch",
  "web.search":     "web_search",
  "workspace.read": "workspace_read",
  "doc.read":       "doc_read",
  "doc.write":      "doc_write",
  "email.draft":    "email_draft",
  "gdrive.read":    "gdrive_read",
  "gdrive.write":   "gdrive_write",
  "onedrive.read":  "onedrive_read",
  "onedrive.write": "onedrive_write",
  "docx.create":    "docx_create",
  "docx.edit":      "docx_edit",
  "docx.extract":   "docx_extract",
  "xlsx.create":    "xlsx_create",
  "xlsx.edit":      "xlsx_edit",
  "xlsx.extract":   "xlsx_extract",
  "xlsx.recalc":    "xlsx_recalc",
  "pptx.create":    "pptx_create",
  "pptx.edit":      "pptx_edit",
  "pptx.extract":   "pptx_extract",
  "pptx.thumbnail": "pptx_thumbnail",
  "pdf.create":     "pdf_create",
  "pdf.transform":  "pdf_transform",
  "pdf.extract":    "pdf_extract",
  "office.convert": "office_convert",
  "memory.read":    "memory_read",
  "memory.write":   "memory_write",
};

export interface ResolvedModel {
  providerId: string;
  modelId: string;
}

// resolveProviderModel picks the provider + model for a session: the head of
// the shared selection chain (llm/provider-fallback.ts — assigned → tenant
// default → tenant fallback). A model choice is convenience, never
// authorization — the gates are unchanged. Exported for unit tests. The caller
// handles the empty-providers env fallback.
export function resolveProviderModel(
  providers: LlmProvider[],
  spec?: AgentSpec,
): ResolvedModel {
  const [head] = chatCandidates(providers, spec);
  if (!head) throw new Error("no configured provider has a usable model");
  return { providerId: head.provider.id, modelId: head.modelId };
}

// ProviderTarget is one upstream the egress proxy may send a request to.
export interface ProviderTarget {
  upstreamBaseUrl: string;
  apiKey: string;
  modelId: string;
  api?: string;
  apiVersion?: string;
}

// ProviderCredentials is the secret-bearing resolution result the supervisor
// passes to proxy.register() at spawn time. Mirrors (but does not import, to
// avoid a session.ts <-> supervisor.ts import cycle) supervisor.ts's
// ProviderCredentials interface — keep the two shapes in sync.
export interface ProviderCredentials extends ProviderTarget {
  modelAllowlist: string[];
  // Remaining keyed candidates in selection order, primary excluded. The proxy
  // retries these on a failover-worthy upstream failure; [] when none.
  fallbacks: ProviderTarget[];
}

// resolveProviderCredentials is the extracted, directly-testable body of the
// gateway's provider credential resolver.
//
// Returns the whole ordered chain, not a single pinned provider: every keyed
// candidate of chatCandidates (assigned → tenant default → tenant fallback),
// head as the primary and the rest as `fallbacks` for the proxy to retry.
//
// Fail-loud contract — no silent degrade into a broken config:
//   Case A: the tenant has >=1 enabled DB provider and NO candidate in the
//     chain has an apiKey (the on-prem failure mode: Vault wiped, has_key still
//     true in Postgres) → throws naming every provider tried and the
//     remediation. NO env fallback — a broken DB provider is a broken
//     deployment state, not a fallback case. A keyless provider that a keyed
//     candidate rescues is warned about (never silently dropped) and excluded
//     from the chain.
//   Case B: the tenant has zero enabled DB providers → env OpenRouter
//     fallback; if that fallback's key (cfg.openrouterApiKey) is ALSO empty,
//     throws naming AIKONOS_OPENROUTER_API_KEY instead of returning an unusable
//     credential set.
//   Case D: the getLlmProviders RPC itself fails (broker unreachable/transient
//     transport error) — if the env fallback key is present, fail OPEN to it
//     (deliberate: a transient broker blip shouldn't take down chat when a
//     usable fallback key exists) and log a warning naming that choice; if the
//     env fallback key is ALSO empty, throw loud naming both facts. No path
//     may return credentials with an empty apiKey.
export async function resolveProviderCredentials(
  cfg: Pick<Config, "llmModel" | "openrouterApiKey">,
  south: Pick<SouthClient, "getLlmProviders">,
  tenantId: string,
  agentSpec?: AgentSpec,
): Promise<ProviderCredentials> {
  const envFallback: ProviderCredentials = {
    upstreamBaseUrl: "https://openrouter.ai/api/v1",
    apiKey: cfg.openrouterApiKey,
    modelId: cfg.llmModel,
    modelAllowlist: [cfg.llmModel],
    fallbacks: [],
    api: "openai-completions",
    apiVersion: "",
  };

  let providers: LlmProvider[];
  try {
    const resp = await south.getLlmProviders({ tenantId });
    providers = (resp.providers ?? []).filter((p) => p.enabled);
  } catch (err) {
    if (cfg.openrouterApiKey) {
      log.warn({ err, tenantId }, "broker unreachable — falling back to env provider");
      return envFallback;
    }
    throw failedPreconditionError(
      `llm credentials unavailable: broker unreachable resolving LLM providers and the env fallback (AIKONOS_OPENROUTER_API_KEY) is empty`,
    );
  }

  if (providers.length === 0) {
    if (!cfg.openrouterApiKey) {
      throw failedPreconditionError(
        "llm credentials unavailable: no LLM providers configured and the env fallback (AIKONOS_OPENROUTER_API_KEY) is empty",
      );
    }
    return envFallback;
  }

  const candidates = chatCandidates(providers, agentSpec);
  if (candidates.length === 0) {
    throw failedPreconditionError(
      "llm credentials unavailable: no enabled LLM provider has a usable model",
    );
  }

  const keyed = candidates.filter((c) => c.provider.apiKey !== "");
  if (keyed.length === 0) {
    const tried = candidates.map((c) => `"${c.provider.id}"`).join(", ");
    throw failedPreconditionError(
      `llm credentials unavailable: no API key available for provider(s) ${tried} — re-enter in Admin → LLM Providers`,
    );
  }
  // A keyless provider excluded from an otherwise-usable chain must still reach
  // the operator: this is the Vault-wipe failure mode degrading gracefully, and
  // silence would let it sit unnoticed until the fallback breaks too.
  for (const { provider } of candidates) {
    if (!provider.apiKey) {
      log.warn(
        { tenantId, provider: provider.id },
        `provider "${provider.id}" has no API key available — excluded from the provider chain; re-enter it in Admin → LLM Providers`,
      );
    }
  }

  // Allow every model of every candidate — not just the primary's. The proxy
  // rewrites the model when it fails over, and session-plan.ts resolves its own
  // model from the same chain without knowing which providers hold a key; a
  // narrower allowlist would trip either. The gates, not the allowlist, are
  // authorization.
  const allowlist: string[] = [];
  for (const { provider, modelId } of candidates) {
    for (const m of provider.models) if (!allowlist.includes(m.id)) allowlist.push(m.id);
    if (!allowlist.includes(modelId)) allowlist.push(modelId);
  }

  const [primary, ...rest] = keyed;
  return {
    ...providerTarget(primary),
    modelAllowlist: allowlist,
    fallbacks: rest.map(providerTarget),
  };
}

function providerTarget({ provider, modelId }: Candidate<LlmProvider>): ProviderTarget {
  return {
    upstreamBaseUrl: provider.endpoint,
    apiKey: provider.apiKey,
    modelId,
    api: provider.api,
    apiVersion: provider.apiVersion,
  };
}

// resolveEnvModel preserves the legacy single-OpenRouter resolution for the
// env-fallback path (no DB providers / broker unreachable):
//   agent model → tenant platform-config model → gateway env default.
// Exported for unit tests.
export async function resolveEnvModel(
  cfg: Pick<Config, "llmModel">,
  south: Pick<SouthClient, "getTenantModel">,
  tenantId: string,
  agentModel?: string,
): Promise<string> {
  if (agentModel) return agentModel;
  try {
    const resp = await south.getTenantModel({ tenantId });
    if (resp.model) return resp.model;
  } catch (err) {
    log.warn({ err, tenantId }, "getTenantModel RPC failed — falling back to env default");
  }
  return cfg.llmModel;
}

// AgentSpec carries the resolved agent config from the broker. When present,
// the session is narrowed to the agent's model and skill set.
export interface AgentSpec {
  model: string;
  approvalMode: string;
  skills: string[];
  preferredProvider?: string;
  allowedProviders?: string[];
  soul?: string;
  gatewayEnabled?: boolean;
}

// allowedPiToolNames converts a skill list to the set of Pi tool names the
// session may expose. Built-in skills use BUILTIN_TOOL_NAME; MCP skills
// ("mcp:<connectorId>:<toolName>") map to "mcp__<connectorId>__<toolName>".
// The delegate tool is always included: it is a governance action, not a Tool
// Proxy invocation, and must remain available regardless of skill restrictions.
// The workflows skill maps to three Pi tools (save/run/list): it is gated
// server-side by FGA in the broker workflow RPCs (CP5a), so all three surface
// together when the user holds the skill.
export function allowedPiToolNames(skills: string[]): Set<string> {
  const allowed = new Set<string>();
  const skillSet = new Set(skills);
  for (const skill of skills) {
    if (skill.startsWith("mcp:")) {
      // mcp:<connectorId>:<toolName> → mcp__<connectorId>__<toolName>
      //
      // Dead for the live plan path: computeActiveToolNames below passes MCP
      // names through unfiltered, so nothing this branch emits is ever matched.
      // It also predates pi/mcp-alias.ts and still writes the full connector id,
      // which mapMcpTool no longer accepts. If this branch is ever made live,
      // build the name with piMcpToolName from that module or the tool resolves
      // to nothing.
      const withoutPrefix = skill.slice("mcp:".length);
      const firstColon = withoutPrefix.indexOf(":");
      if (firstColon !== -1) {
        allowed.add("mcp__" + withoutPrefix.slice(0, firstColon) + "__" + withoutPrefix.slice(firstColon + 1));
      }
    } else if (skill === "workflows") {
      // workflows is a multi-tool skill: one FGA grant surfaces all five Pi tools.
      allowed.add("workflow_save");
      allowed.add("workflow_run");
      allowed.add("workflow_list");
      allowed.add("workflow_publish");
      allowed.add("workflow_propose");
    } else if (skill === "vision") {
      // vision is a capability skill (CP2), not a Tool Proxy tool_id — surfaces
      // the analyze_image Pi tool, which is then gated per-call via mapTool's
      // "vision" toolId (skill:vision) like any other tool.
      allowed.add("analyze_image");
    } else if (skill === "subagents") {
      // subagents is a capability skill,
      // same posture as vision — surfaces the spawn_subagents Pi tool, gated
      // per-call via mapTool's "subagents" toolId (skill:subagents). The
      // depth-1 cap (a subagent branch child must never itself hold this tool)
      // is enforced upstream, in resolveSessionPlan — see its isSubagentBranch
      // dep — not here, since this function has no notion of "am I a branch".
      allowed.add("spawn_subagents");
    } else {
      const piName = BUILTIN_TOOL_NAME[skill];
      if (piName) allowed.add(piName);
    }
  }
  // workflow_schedule requires BOTH
  // capability skills — "workflows" (visibility/save/run) and "scheduler"
  // (the broker's create-time skill:scheduler gate) — neither alone suffices.
  if (skillSet.has("workflows") && skillSet.has("scheduler")) {
    allowed.add("workflow_schedule");
  }
  allowed.add("delegate");
  return allowed;
}

// computeActiveToolNames returns the tool name list that a session should
// declare, applying skill filtering when either an agent spec or a user skill
// list is provided. MCP tool names come from the live session and are passed
// in. Exported for unit tests.
//
// Priority: spec.skills (named-agent path) > userSkills (personal-session path)
// > all tools (no restriction). MCP tools are left unfiltered in the
// userSkills path — ListUserSkills covers registry tools only.
export function computeActiveToolNames(
  allStaticNames: string[],
  mcpNames: string[],
  spec?: AgentSpec,
  userSkills?: string[],
): string[] {
  if (spec) {
    const allowed = allowedPiToolNames(spec.skills);
    // MCP tools are agent-scoped by discovery (listAccessibleMcpServersForAgent)
    // and are not in the FGA skill registry, so allowedPiToolNames can never
    // admit them — leave them unfiltered, mirroring the userSkills path below.
    // Without this, an agent's discovered MCP tools would all be dropped.
    return [...allStaticNames.filter((n) => allowed.has(n)), ...mcpNames];
  }
  if (userSkills !== undefined) {
    const allowed = allowedPiToolNames(userSkills);
    // MCP tools are not in the FGA skill registry — leave them unfiltered.
    return [...allStaticNames.filter((n) => allowed.has(n)), ...mcpNames];
  }
  return [...allStaticNames, ...mcpNames];
}

