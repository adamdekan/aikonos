// CP4: parent-side session plan resolution + child-side session creation.
//
// WHY this split exists: buildSession today does two things in the trusted parent
// process: (a) resolve providers/model/skills/MCP tools via south RPCs — needs
// secrets (api keys, bearer tokens); and (b) create the Pi AgentSession and
// register tools. Phase 2 forks the Pi loop into an untrusted child that must
// hold no long-lived secret. The split lets the parent resolve everything secret-
// bearing and pass only a secret-free SessionPlan (= the InitMessage) to the
// child, which creates the session against the parent's egress proxy with a
// dummy key.
//
// The existing buildSession in session.ts is NOT deleted — it composes these two
// halves in-process so all current callers (server.ts, ticker.ts, external/core.ts)
// continue to work until CP7/CP8 rewire them to the supervisor.
import { mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import {
  AuthStorage,
  ModelRegistry,
  SessionManager,
  SettingsManager,
  DefaultResourceLoader,
  createAgentSession as piCreateAgentSession,
  defineTool,
  type AgentSession,
  type ExtensionAPI,
  type CreateAgentSessionOptions,
} from "@earendil-works/pi-coding-agent";
import { Type } from "typebox";
import type { InitMessage, McpTool, ConvMessage, SkillBundleEntry } from "../ipc/protocol.js";
import type { BridgeClientLike } from "../ipc/bridge-client.js";
import { makeTools, TOOL_NAMES } from "./tools.js";
import { allowedPiToolNames, computeActiveToolNames, resolveEnvModel, type AgentSpec } from "./session.js";
import { chatCandidates, type ChatProviderLike } from "../llm/provider-fallback.js";
import { buildSkillCatalogText, makeLoadSkillTool, PERSONAL_SKILL_PREFIX } from "./load-skill.js";
import { makeReadSkillFileTool } from "./read-skill-file.js";
import { piMcpToolName, fitsPiToolName, MAX_PI_TOOL_NAME_LEN } from "./mcp-alias.js";
import { gateToolCall } from "./gate-tool-call.js";
import { buildSystemPrompt } from "./system-prompt.js";
import { log } from "../log.js";

// Re-exported for existing callers/tests importing buildSystemPrompt from this
// module — the implementation itself lives in system-prompt.js (see WHY there).
export { buildSystemPrompt };

// isRecord narrows unknown to Record<string, unknown> without an `as` cast.
// Used to validate JSON.parse results before assigning to typed schema fields.
function isRecord(v: unknown): v is Record<string, unknown> {
  return v !== null && typeof v === "object" && !Array.isArray(v);
}

// ── Public types ───────────────────────────────────────────────────────────────

// SessionPlan is the secret-free handshake sent from parent to child as an
// InitMessage. No api-key, bearer, grant, or tenant secret is permitted here.
export type SessionPlan = InitMessage;

// The subset of AgentSession that child-entry and createSessionFromPlan actually
// use. Lives here (the session-factory module) so child-entry can import UP from
// the session layer rather than the reverse. AgentSession satisfies this structurally.
export interface SessionLike {
  prompt(text: string): Promise<void>;
  subscribe(listener: (event: import("@earendil-works/pi-coding-agent").AgentSessionEvent) => void): () => void;
  dispose(): void;
  // Optional: the real AgentSession supports abort(); fakes may omit it.
  abort?(): Promise<void>;
  // CP8: activateSkill pre-activates a skill bundle by name before session.prompt()
  // is called, mirroring what load_skill does when the model calls it. Returns the
  // activated skill's body (plus any appended "## Skill files" manifest) on
  // success, or an "ERROR:"-prefixed message string if the name is unknown —
  // callers (child-entry.ts) branch on that prefix, never on null (bugfix:
  // the body used to be discarded on success, so /command and keyword
  // auto-load activation was authorization-only — the model never saw the
  // skill's instructions). Optional because sessions built without skill
  // bundles (no bundles granted) have no activation path.
  activateSkill?(name: string): Promise<string>;
  // Live-visibility: Pi tool name → tool description, built from the exact tool
  // definitions registered with this session. Read child-side when relaying
  // tool_start so the UI can label each call by description rather than name.
  // Optional — fakes and the raw AgentSession omit it.
  toolDescriptions?: Record<string, string>;
}

// The subset of identity the resolve half needs. Does NOT include bearer or
// ownerGrant — those stay in the parent's spawn record.
export interface ResolveIdentity {
  tenantId: string;
  userId: string;
  agentId: string;
}

// ProviderLike covers the fields resolveSessionPlan actually reads from a
// provider response. Intentionally narrower than the proto LlmProvider so
// test stubs don't need the full generated type. Defined by the selection
// module — the ordering and the shape it reads belong together.
export type { ProviderModelLike } from "../llm/provider-fallback.js";
export type ProviderLike = ChatProviderLike;

// PersonalSkillListLike is the frontmatter-only catalog row shape
// resolveSessionPlan reads from listPersonalSkillsSouth. Narrower than the generated PersonalSkillEntry so stubs
// don't need the full proto type; warning is read by the Skills management
// view, not chat, so it's omitted here.
export interface PersonalSkillListLike {
  name: string;
  description: string;
  keywords: string[];
  allowedTools: string[];
  disableModelInvocation: boolean;
  valid: boolean;
}

// PersonalSkillSouth is the minimal surface personalSkillBundleEntries needs —
// shared by resolveSessionPlan and, since CP5, the /agui route's /command
// resolution and keyword auto-load matching, so all three catalogs union the caller's own
// Skills/<name>/ entries through one implementation.
export interface PersonalSkillSouth {
  listPersonalSkillsSouth?(req: { tenantId: string; userId: string }): Promise<{ skills: PersonalSkillListLike[] }>;
}

// personalSkillBundleEntries fetches the caller's own personal skills and maps
// them into the shared SkillBundleEntry shape, tagged origin:"personal" with a
// qualified "personal:<name>" id/name so they can never collide with a bundle
// (see PERSONAL_SKILL_PREFIX in load-skill.ts). OIDC personal sessions only —
// svc-<agentId>/group-<id> (the broker's synthetic south principals) have no
// Skills/ folder of their own and get []. Fail-open: a missing RPC (older
// south stub) or any south failure returns [] rather than throwing — every
// caller's session/turn build must survive a broken personal-skills fetch.
export async function personalSkillBundleEntries(
  south: PersonalSkillSouth,
  tenantId: string,
  userId: string,
): Promise<SkillBundleEntry[]> {
  const isServiceOrGroupPrincipal = userId.startsWith("svc-") || userId.startsWith("group-");
  if (isServiceOrGroupPrincipal || !south.listPersonalSkillsSouth) return [];
  try {
    const resp = await south.listPersonalSkillsSouth({ tenantId, userId });
    // Invalid entries (unparseable/oversize/missing description) are excluded
    // from every catalog — the per-skill warning is surfaced by the Skills
    // management view (ListPersonalSkills), not chat/command/auto-load.
    return (resp.skills ?? [])
      .filter((s) => s.valid)
      .map((s) => ({
        id: PERSONAL_SKILL_PREFIX + s.name,
        name: PERSONAL_SKILL_PREFIX + s.name,
        description: s.description,
        body: "", // fetched on demand at load_skill activation — see load-skill.ts
        allowedTools: s.allowedTools,
        contextFork: false,
        disableModelInvocation: s.disableModelInvocation,
        keywords: s.keywords,
        // [] at plan-build time — the list RPC is frontmatter-only; refreshed at load_skill activation from the
        // on-demand fetch (see load-skill.ts's activate()).
        filePaths: [],
        origin: "personal" as const,
      }));
  } catch (err) {
    log.warn({ err: String(err), userId }, "listPersonalSkillsSouth RPC failed — no personal skills");
    return [];
  }
}

// Minimal south-client surface resolveSessionPlan needs. Keeps the dependency
// narrow so tests stub only what is used.
export interface ResolveSouth {
  getLlmProviders(req: { tenantId: string }): Promise<{ providers: ProviderLike[] }>;
  getTenantModel(req: { tenantId: string }): Promise<{ model: string }>;
  listUserSkills(req: { tenantId: string; userId: string }): Promise<{ skills: string[] }>;
  listUserAgentSkills(req: { tenantId: string; userId: string }): Promise<{ bundles: SkillBundleEntry[] }>;
  listAccessibleMcpServersForAgent(req: { tenantId: string; agentId: string }): Promise<{ connections?: { id: string; name: string }[] }>;
  listMcpServerToolsSouth(req: { tenantId: string; connectorId: string; userId: string }): Promise<{
    tools?: { name: string; description: string; inputSchemaJson?: string; readOnlyHint?: boolean }[];
  }>;
  getAgentSpec(req: { tenantId: string; agentId: string }): Promise<{ found: boolean; soul?: string }>;
  // Org governance settings (A-series). Optional so existing stubs need not
  // implement it; absent/failed resolution simply means no org preamble.
  getOrgSettings?(req: { tenantId: string }): Promise<{ settings?: { instructionPreamble?: string } }>;
  // Personal skills. Optional so every
  // existing ResolveSouth stub (workflow/analyze-image/etc. tests) is
  // unaffected; absent/failed resolution simply means no personal entries.
  listPersonalSkillsSouth?(req: { tenantId: string; userId: string }): Promise<{ skills: PersonalSkillListLike[] }>;
}

// pickModelId picks the head of the shared selection chain (assigned → tenant
// default → tenant fallback; llm/provider-fallback.ts). undefined = the tenant
// has nothing usable, which sends the caller to the env-fallback model.
// Agent-bound runs (ticker, external) must not silently lose their preferred
// provider — that was the CP7/CP8 TODO. Personal runs pass no agentSpec and get
// the tenant-default path.
function pickModelId(
  providers: ProviderLike[],
  agentSpec?: AgentSpec,
): { providerId: string; modelId: string } | undefined {
  const [head] = chatCandidates(providers, agentSpec);
  return head ? { providerId: head.provider.id, modelId: head.modelId } : undefined;
}

// ResolveCfg is the minimal config shape that resolveSessionPlan reads.
// It intentionally excludes any secret (openrouterApiKey) — the resolve path
// structurally cannot see a key. The supervisor/CP6 reads the key and passes
// it to proxy.register; resolve never touches it.
export interface ResolveCfg {
  llmModel: string;
  defaultTenantId: string;
}

export interface ResolveSessionDeps {
  south: ResolveSouth;
  cfg: ResolveCfg;
  // proxyBaseUrl: the parent-local egress proxy URL the child registers its
  // provider against. CP5 will supply the real loopback URL; for now callers
  // pass a config value or placeholder. The child never uses the real api key.
  proxyBaseUrl?: string;
  // agentSpec: present for agent-bound runs (ticker, external). When set,
  // pickModelId honours the agent's preferred provider/model before falling
  // back to tenant-default or any. Absent for personal /agui runs.
  agentSpec?: AgentSpec;
  // Depth-1 cap for the subagent-handoff feature: true only when this plan is being built for a subagent BRANCH child
  // (ChildSupervisor.withEphemeralChild), never for the ordinary pooled/
  // interactive path. Threaded explicitly here — upstream of buildSystemPrompt
  // below — rather than inferred from the ephemeral pool key's "subagent:"
  // prefix: an authorization-shaped decision must not depend on string
  // formatting. Filtering allowedToolNames after the plan is built would leave
  // the system prompt still advertising spawn_subagents to a child that will
  // be refused the moment it tries to call it.
  isSubagentBranch?: boolean;
}

// Narrower createAgentSession seam: returns { session: SessionLike } so test
// fakes (which implement only prompt/subscribe/dispose) satisfy the type without
// constructing a full AgentSession class instance. The real Pi SDK result
// satisfies this structurally because AgentSession has those methods.
export type CreateAgentSessionFn = (
  options?: CreateAgentSessionOptions,
) => Promise<{ session: SessionLike }>;

// CreateSessionDeps is the seam that createSessionFromPlan injects for tests.
// The real Pi createAgentSession is the default; tests inject a fake.
export interface CreateSessionDeps {
  // Spy: called when a provider is registered into the model registry.
  // Injected so tests can assert baseUrl + apiKey without touching the
  // opaque ModelRegistry.
  onRegisterProvider?: (providerId: string, baseUrl: string, apiKey: string) => void;
  // Override the Pi createAgentSession (default: the real SDK function).
  createAgentSession?: CreateAgentSessionFn;
  // When useProxy:false the create half uses this key for the real provider.
  // Ignored when useProxy:true (dummy key used instead).
  realApiKey?: string;
}

// ── resolveSessionPlan ─────────────────────────────────────────────────────────

// Resolve the MCP tool list from south discovery RPCs. Returns McpTool[] — the
// same shape that InitMessage.mcpTools expects. No bridge, no api key.
async function resolveMcpTools(
  south: ResolveSouth,
  tenantId: string,
  agentId: string,
): Promise<McpTool[]> {
  let servers: { id: string; name: string }[];
  try {
    const resp = await south.listAccessibleMcpServersForAgent({ tenantId, agentId });
    servers = resp.connections ?? [];
  } catch (err) {
    log.warn({ err: String(err), agentId }, "listAccessibleMcpServersForAgent failed — no MCP tools in plan");
    return [];
  }

  const tools: McpTool[] = [];

  for (const server of servers) {
    try {
      const resp = await south.listMcpServerToolsSouth({
        tenantId,
        connectorId: server.id,
        userId: agentId,
      });
      for (const t of resp.tools ?? []) {
        const name = piMcpToolName(server.id, t.name);
        if (!fitsPiToolName(name)) {
          log.warn(
            { tool: t.name, server: server.id, len: name.length, max: MAX_PI_TOOL_NAME_LEN },
            "mcp tool name exceeds the provider function-name limit — tool omitted from plan",
          );
          continue;
        }
        let schema: Record<string, unknown> = {};
        if (t.inputSchemaJson) {
          try {
            const parsed: unknown = JSON.parse(t.inputSchemaJson);
            if (isRecord(parsed)) {
              schema = parsed;
            }
            // non-object parse result (array, string, etc.) — use open object
          } catch {
            // unparseable schema — use open object
          }
        }
        tools.push({ name, schema, toolId: name });
      }
    } catch (err) {
      log.warn({ err: String(err), server: server.id }, "listMcpServerToolsSouth failed — skipping server in plan");
    }
  }

  return tools;
}

// resolveSessionPlan runs in the trusted parent. It fires the south RPCs, picks
// the model, applies the user-skill filter, and returns a secret-free
// SessionPlan (= InitMessage) that can be sent to the untrusted child without
// exposing any api-key, bearer, or grant.
/** Runs south RPCs in the trusted parent to produce a secret-free SessionPlan (model, tool list, MCP tools, proxy URL) safe to send to the untrusted child; no api-key, bearer, or grant is included. */
export async function resolveSessionPlan(
  identity: ResolveIdentity,
  deps: ResolveSessionDeps,
): Promise<SessionPlan> {
  const { south, cfg, agentSpec } = deps;
  const proxyBaseUrl = deps.proxyBaseUrl ?? "http://127.0.0.1:0";
  // Defense-in-depth: a credentialed URL would smuggle an api-key into the plan
  // (the InitMessage sent to the untrusted child). CP5 builds this URL internally;
  // this guard catches misconfiguration or a supply-chain tamper at plan-resolve time.
  const parsedProxy = new URL(proxyBaseUrl);
  if (parsedProxy.username || parsedProxy.password) {
    throw new Error(`resolveSessionPlan: proxyBaseUrl must not contain credentials — key must never travel in the plan`);
  }

  // Resolve user skills from south (FGA-backed). Failure → warn + all tools.
  let userSkills: string[] | undefined;
  try {
    const resp = await south.listUserSkills({ tenantId: identity.tenantId, userId: identity.userId });
    userSkills = resp.skills;
  } catch (err) {
    log.warn({ err: String(err), userId: identity.userId }, "listUserSkills RPC failed — all tools visible");
  }

  // Resolve granted skill bundles for the session (FGA-backed via ListUserAgentSkills).
  // Failure → warn + empty list (no bundles in session, SubmitPlan remains the enforcement point).
  let skillBundles: SkillBundleEntry[] = [];
  try {
    const resp = await south.listUserAgentSkills({ tenantId: identity.tenantId, userId: identity.userId });
    skillBundles = resp.bundles ?? [];
  } catch (err) {
    log.warn({ err: String(err), userId: identity.userId }, "listUserAgentSkills RPC failed — no skill bundles in session");
  }

  // Personal skills: union the caller's
  // own Skills/<name>/ catalog into the same skillBundles list via the shared
  // helper (see personalSkillBundleEntries above) — keyed on identity.userId,
  // not on agentSpec presence, so an OIDC user with an agentSpec (agent-bound
  // chat) still gets their own personal skills.
  skillBundles.push(...(await personalSkillBundleEntries(south, identity.tenantId, identity.userId)));

  // Resolve providers from south. Failure → env fallback.
  let providers: ProviderLike[] = [];
  try {
    const resp = await south.getLlmProviders({ tenantId: identity.tenantId });
    providers = resp.providers.filter((p) => p.enabled);
  } catch (err) {
    log.warn({ err: String(err), tenantId: identity.tenantId }, "getLlmProviders RPC failed — env fallback");
  }

  let modelId: string;
  // providerId is the real DB provider id backing modelId — carried in the
  // plan so the child's usage relay can attribute spend to the correct
  // provider (see InitMessage.providerId). Empty when the env fallback path
  // is taken (no DB providers resolved) — no provider row to attribute to;
  // the broker's cost fallback records tokens with cost 0 in that case.
  let providerId = "";
  if (providers.length > 0) {
    const picked = pickModelId(providers, agentSpec);
    if (picked) {
      modelId = picked.modelId;
      providerId = picked.providerId;
    } else {
      modelId = await resolveEnvModel(cfg, south, identity.tenantId, agentSpec?.model);
    }
  } else {
    modelId = await resolveEnvModel(cfg, south, identity.tenantId, agentSpec?.model);
  }

  // Resolve MCP tools from south discovery.
  const mcpTools = await resolveMcpTools(south, identity.tenantId, identity.agentId);

  const mcpToolNames = mcpTools.map((t) => t.name);
  // Agent-bound runs (external invoke, and any future path that threads a spec)
  // are gated by the agent's own configured skills — agent.Skills is authoritative,
  // matching the broker's SubmitPlan enforcement. Personal runs pass no agentSpec
  // and fall through to the user's FGA-derived skill list. MCP tools discovered
  // for the agent survive either branch (they are agent-scoped by discovery, not
  // in the FGA skill registry).
  let activeToolNames = computeActiveToolNames(TOOL_NAMES, mcpToolNames, agentSpec, userSkills);
  if (deps.isSubagentBranch) {
    // Depth-1 cap: see the isSubagentBranch doc on ResolveSessionDeps above.
    // Runs BEFORE buildSystemPrompt below, so the prompt built from
    // activeToolNames never advertises a tool this branch will be refused.
    activeToolNames = activeToolNames.filter((name) => name !== "spawn_subagents");
  }

  // Resolve soul for the system prompt. Never throws out of resolveSessionPlan.
  let soul = "";
  if (agentSpec?.soul && agentSpec.soul.trim()) {
    soul = agentSpec.soul;
  } else if (identity.agentId.startsWith("agent:")) {
    const bareId = identity.agentId.slice("agent:".length);
    try {
      const r = await south.getAgentSpec({ tenantId: identity.tenantId, agentId: bareId });
      if (r.found) soul = r.soul ?? "";
    } catch (err) {
      log.warn({ err: String(err), agentId: bareId }, "getAgentSpec for soul failed — continuing without soul");
    }
  }

  // A1 — org-global instruction preamble. Read parent-side and injected as a
  // governance-level block ahead of the agent soul. Never throws out of
  // resolveSessionPlan; an unavailable RPC just means no preamble.
  let orgPreamble = "";
  if (south.getOrgSettings) {
    try {
      const r = await south.getOrgSettings({ tenantId: identity.tenantId });
      orgPreamble = r.settings?.instructionPreamble ?? "";
    } catch (err) {
      log.warn({ err: String(err), tenantId: identity.tenantId }, "getOrgSettings failed — continuing without org preamble");
    }
  }

  // Inject skill catalog into system prompt (names+descriptions only, no bodies).
  // Only non-disable_model_invocation bundles appear — bodies are fetched lazily
  // by the model via load_skill(name).
  const skillCatalog = buildSkillCatalogText(skillBundles);
  const hasPersonalSkills = skillBundles.some((b) => b.origin === "personal");
  // Mirrors createSessionFromPlan's own read_skill_file registration gate
  // (skillBundles.length > 0) so the prompt's teaching line always matches
  // whether the tool is actually registered.
  const hasSkillBundles = skillBundles.length > 0;
  const systemPrompt = buildSystemPrompt(activeToolNames, soul, skillCatalog, orgPreamble, hasPersonalSkills, hasSkillBundles);

  return {
    kind: "init",
    modelId,
    providerId,
    systemPrompt,
    allowedToolNames: activeToolNames,
    mcpTools,
    proxyBaseUrl,
    proxyModelAllowlist: [modelId],
    skillBundles,
  };
}

// ── createSessionFromPlan ──────────────────────────────────────────────────────

// DUMMY_KEY is a placeholder credential used when the child registers its
// provider against the parent's egress proxy (useProxy:true). The proxy injects
// the real key before forwarding to the upstream — the child never sees it.
const DUMMY_KEY = "aikonos-proxy-child-key";

// Number of times the LLM SDK retries a transient provider failure (connection
// error, 408/409/429, or >=500) before the run fails. Bounds a transient blip
// from discarding a whole run's already-gathered tool results. Each retry is a
// fresh request through the egress proxy, capped by its own idle timeout.
//
// 1, not 2: the egress proxy now owns provider-level failover (a pre-stream
// failure switches provider, up to a ≤3-target chain), so this SDK retry stacks
// multiplicatively on top of it. What the proxy CANNOT cover is a mid-stream
// failure (bytes already on the wire — no rewind) and the proxy itself being
// unreachable, which is the whole remaining job of an SDK retry. 3×2=6 upstream
// requests worst case, each pre-gated by its own rate-limit/spend-cap check.
const LLM_PROVIDER_MAX_RETRIES = 1;

// createSessionFromPlan runs in the child (or in-process for the legacy path).
// It registers the provider, tools, and MCP tools from the plan and creates the
// Pi AgentSession. No south RPC, no api-key read from env.
//
// useProxy:true  → register provider at plan.proxyBaseUrl with DUMMY_KEY
//                  (forked-child path; real key stays in the parent proxy).
// useProxy:false → register provider at the real endpoint with realApiKey
//                  (legacy in-process buildSession path; real key is in the parent).
/** Creates a Pi AgentSession from a secret-free SessionPlan; useProxy:true registers the provider against the parent's egress proxy with a dummy key (child path); useProxy:false uses the real api key (legacy in-process path). When history is provided and non-empty, the fresh SessionManager is seeded with those prior turns so the LLM sees the full conversation context on resume. */
export async function createSessionFromPlan(
  plan: SessionPlan,
  bridge: BridgeClientLike,
  deps: CreateSessionDeps = {},
  opts: { useProxy: boolean },
  history?: ConvMessage[],
): Promise<{ session: SessionLike; modelId: string }> {
  const doCreateAgentSession = deps.createAgentSession ?? piCreateAgentSession;

  const authStorage = AuthStorage.inMemory();
  const modelRegistry = ModelRegistry.inMemory(authStorage);

  // Determine provider registration params.
  const baseUrl = opts.useProxy ? plan.proxyBaseUrl : "https://openrouter.ai/api/v1";
  if (!opts.useProxy && !deps.realApiKey) {
    throw new Error("createSessionFromPlan: realApiKey is required when useProxy is false — no key supplied and falling back to DUMMY_KEY would cause an opaque LLM auth failure");
  }
  const apiKey = opts.useProxy ? DUMMY_KEY : (deps.realApiKey ?? DUMMY_KEY);

  modelRegistry.registerProvider("openrouter", {
    name: "OpenRouter",
    baseUrl,
    apiKey,
    api: "openai-completions",
    authHeader: true,
    headers: { "HTTP-Referer": "https://aikonos.com", "X-Title": "Aikonos Agent Gateway" },
    models: [
      {
        id: plan.modelId,
        name: `OpenRouter ${plan.modelId}`,
        reasoning: false,
        input: ["text"] as ("text" | "image")[],
        cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
        contextWindow: 200000,
        maxTokens: 8192,
      },
    ],
  });

  deps.onRegisterProvider?.("openrouter", baseUrl, apiKey);

  modelRegistry.refresh();

  const model = modelRegistry.find("openrouter", plan.modelId);
  if (!model) throw new Error(`createSessionFromPlan: could not resolve model openrouter/${plan.modelId}`);

  // Static tools filtered to allowedToolNames.
  const allowedSet = new Set(plan.allowedToolNames);
  const allStaticTools = makeTools(bridge);
  const activeStaticTools = allStaticTools.filter((t) => allowedSet.has(t.name));

  // MCP tools from the plan — each routes through the bridge client exactly like
  // static tools. Schema comes from the plan (serialised by resolveSessionPlan).
  const mcpToolDefs = plan.mcpTools
    .filter((t) => allowedSet.has(t.name))
    .map((t) => {
      const capturedName = t.name;
      return defineTool({
        name: t.name,
        label: `MCP: ${t.name}`,
        description: `MCP tool ${t.name}`,
        parameters: Type.Object({}, { additionalProperties: true }),
        execute: async (toolCallId, _params) => {
          const decision = await bridge.gate(
            toolCallId,
            capturedName,
            (_params as Record<string, unknown>) ?? {},
          );
          if (!decision.allow) {
            return { content: [{ type: "text" as const, text: `aikonos: ${decision.reason ?? "denied"}` }], details: {} };
          }
          const r = await bridge.execute(toolCallId);
          const body = r.ok
            ? typeof r.output === "string" ? r.output : JSON.stringify(r.output, null, 2)
            : `ERROR: ${r.error ?? "tool failed"}`;
          return { content: [{ type: "text" as const, text: body }], details: r.output };
        },
      });
    });

  // Register load_skill built-in when bundles are present. Activating a bundle
  // has no effect on tool authorization —
  // the tool-call gate (gate-tool-call.ts) checks only bridge.gate(), unrelated
  // to which skill(s) were activated for the turn.
  const skillBundles = plan.skillBundles ?? [];
  // Personal entries' on-demand body fetch goes through the bridge (the
  // child holds no south client — see load-skill.ts's SkillBodyFetcher doc).
  // bind(bridge) preserves `this` without a non-null assertion on the
  // optional method (bridge.getSkillBody?.bind(bridge) short-circuits to
  // undefined when the bridge predates this feature).
  const fetchPersonalBody = bridge.getSkillBody?.bind(bridge);
  const loadSkillToolDef = skillBundles.length > 0
    ? makeLoadSkillTool(skillBundles, { warn: (msg) => log.warn(msg) }, fetchPersonalBody)
    : null;

  const loadSkillPiTool = loadSkillToolDef
    ? defineTool({
        name: loadSkillToolDef.name,
        label: "Load skill",
        description: "Activate a skill bundle by name. Call this when you want to use a listed skill. Returns the skill body and activates its tool allowlist for this turn.",
        parameters: Type.Object({
          name: Type.String({ description: "skill bundle name to activate" }),
        }),
        execute: async (toolCallId, params) => loadSkillToolDef.execute(toolCallId, params as { name: string }),
      })
    : null;

  // Register read_skill_file whenever load_skill is — same condition/site: the second progressive-disclosure stage only
  // makes sense when there is a catalog to read files from. Resolution runs
  // against the FULL skillBundles list (bundles ∪ personal, DMI included),
  // unlike load_skill's model-facing byName — see read-skill-file.ts.
  // fetchSkillFile is undefined only for a bridge fake predating this feature
  // (same bind-optional precedent as fetchPersonalBody above); in production
  // bridge.getSkillFile is always present, so this collapses to skillBundles.length > 0.
  const fetchSkillFile = bridge.getSkillFile?.bind(bridge);
  const readSkillFileToolDef = skillBundles.length > 0 && fetchSkillFile
    ? makeReadSkillFileTool(skillBundles, fetchSkillFile)
    : null;

  const readSkillFilePiTool = readSkillFileToolDef
    ? defineTool({
        name: readSkillFileToolDef.name,
        label: "Read skill file",
        description: "Read one file from an activated skill's file tree by its listed path. Use after load_skill when its response lists files under \"## Skill files\".",
        parameters: Type.Object({
          skill: Type.String({ description: "the catalog name of the skill (as listed/activated)" }),
          path: Type.String({ description: "the file's relative path, exactly as listed" }),
        }),
        execute: async (toolCallId, params) =>
          readSkillFileToolDef.execute(toolCallId, params as { skill: string; path: string }),
      })
    : null;

  const customTools = [
    ...activeStaticTools,
    ...mcpToolDefs,
    ...(loadSkillPiTool ? [loadSkillPiTool] : []),
    ...(readSkillFilePiTool ? [readSkillFilePiTool] : []),
  ];

  // Name → description map for live tool-trace labelling in the UI. Built from
  // the exact definitions registered below so it covers static + MCP + load_skill.
  const toolDescriptions: Record<string, string> = {};
  for (const t of customTools) toolDescriptions[t.name] = t.description;

  // Extend the allowed tool name list to include load_skill/read_skill_file
  // when bundles are present.
  const activeToolNames = [
    ...plan.allowedToolNames,
    ...(loadSkillPiTool ? ["load_skill"] : []),
    ...(readSkillFilePiTool ? ["read_skill_file"] : []),
  ];

  // Build the SessionManager. When history is present and non-empty, seed it with
  // prior turns so the LLM sees the full conversation context on resume after
  // thread eviction. A simple incrementing counter supplies ordering; the LLM
  // needs only relative order, not wall-clock timestamps.
  const sessionManager = SessionManager.inMemory(process.cwd());
  if (history && history.length > 0) {
    let ts = 1;
    for (const turn of history) {
      if (turn.role === "user") {
        sessionManager.appendMessage({
          role: "user",
          content: turn.content,
          timestamp: ts++,
        });
      } else {
        sessionManager.appendMessage({
          role: "assistant",
          content: [{ type: "text", text: turn.content }],
          api: "openai-completions",
          provider: "openrouter",
          model: plan.modelId,
          usage: {
            input: 0,
            output: 0,
            cacheRead: 0,
            cacheWrite: 0,
            totalTokens: 0,
            cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 },
          },
          stopReason: "stop",
          timestamp: ts++,
        });
      }
    }
  }

  const agentDir = mkdtempSync(join(tmpdir(), "aikonos-pi-"));

  const loader = new DefaultResourceLoader({
    cwd: process.cwd(),
    agentDir,
    systemPrompt: plan.systemPrompt,
    noContextFiles: true,
    noSkills: true,
    noPromptTemplates: true,
    noThemes: true,
    extensionFactories: [
      (pi: ExtensionAPI) => {
        pi.on("tool_call", (event) => gateToolCall(bridge, event, log));
      },
    ],
  });
  await loader.reload();

  const { session: piSession } = await doCreateAgentSession({
    cwd: process.cwd(),
    model,
    thinkingLevel: "off",
    authStorage,
    modelRegistry,
    customTools,
    tools: activeToolNames,
    resourceLoader: loader,
    sessionManager,
    // provider.maxRetries flows to the underlying LLM SDK (e.g. the OpenAI
    // client's built-in retry), which retries connection errors + 408/409/429/
    // >=500 with backoff. The Pi default is 0 (retries off), so a single
    // transient provider 5xx would kill a whole run and discard the tool
    // results already gathered this turn. Retrying re-sends the same completion
    // — the conversation context (with web_search/web_fetch results) is intact
    // — so the chain continues instead of failing on one blip.
    // ponytail: constant, not an env knob; make it AIKONOS_GATEWAY_LLM_MAX_RETRIES if operators need to tune it.
    settingsManager: SettingsManager.inMemory({
      compaction: { enabled: false },
      retry: { provider: { maxRetries: LLM_PROVIDER_MAX_RETRIES } },
    }),
  });

  // CP8: wrap the Pi session to add activateSkill. The wrapper satisfies
  // SessionLike structurally — callers that only need prompt/subscribe/dispose/abort
  // are unaffected. activateSkill is the /command pre-activation path: it uses
  // loadSkillToolDef.commandActivate (byName-independent for personal skills,
  // disable_model_invocation-inclusive for admin bundles) rather than the model's
  // execute path, so a DMI or after-spawn-created skill the parent already
  // authorized still activates. commandActivate's return value (body
  // on success, "ERROR:"-prefixed message on failure) is forwarded verbatim —
  // child-entry.ts prepends a successful activation's body to the turn's prompt.
  const session: SessionLike = {
    prompt: (text) => piSession.prompt(text),
    subscribe: (listener) => piSession.subscribe(listener),
    dispose: () => piSession.dispose(),
    abort: piSession.abort ? () => piSession.abort!() : undefined,
    toolDescriptions,
    activateSkill: loadSkillToolDef
      ? (name: string) => loadSkillToolDef.commandActivate(name)
      : undefined,
  };

  return { session, modelId: plan.modelId };
}
