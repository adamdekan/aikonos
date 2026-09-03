// load-skill.ts — load_skill built-in read-tool for CP7.
//
// The tool is registered once per session. When the model calls it:
//   1. Resolve the name → bundle (only non-disable_model_invocation bundles are accepted).
//   2. Return the bundle body (+ file manifest, CP3) to the model.
//   3. If context_fork=true, run inline and emit exactly one warn-level log.
//
// Activating a bundle grants no additional tool access — bundle.allowed_tools
// is dormant, unread here. The real
// authorization axis is FGA + the four governance gates (SubmitPlan → OPA →
// FGA → Biscuit) at each individual tool_call, unaffected by which skill(s)
// were activated for the turn.
//
// WHY not a bridge-routed tool: load_skill is a gateway-internal read-tool. It
// resolves a bundle that was already fetched at session-build time — no broker RPC
// needed. The body is returned directly; no governance gate is required because the
// bundle was already FGA-checked at ListUserAgentSkills time.
import type { SkillBundleEntry } from "../ipc/protocol.js";

// PERSONAL_SKILL_PREFIX: personal entries
// are unioned into the same SkillBundleEntry list as admin bundles under a
// qualified name/id ("personal:<name>"), so a bundle and a personal skill can
// never share a byName key — "exact name match resolves to the bundle" holds
// structurally, with no separate collision-detection branch needed. Exported
// so resolveSessionPlan (session-plan.ts), which builds these entries, and
// this module, which strips the prefix before the on-demand body fetch, share
// one literal.
export const PERSONAL_SKILL_PREFIX = "personal:";

export interface WarnLogger {
  warn(msg: string): void;
}

// textResult mirrors the shape every other tool in tools.ts returns so Pi can
// render it consistently.
function textResult(text: string) {
  return { content: [{ type: "text" as const, text }], details: {} };
}

// buildSkillCatalogText returns the catalog snippet injected into the system
// prompt — names + descriptions of non-disable_model_invocation bundles only.
// Returns "" when there is nothing to show (no injected section → no prompt bloat).
export function buildSkillCatalogText(bundles: SkillBundleEntry[]): string {
  const visible = bundles.filter((b) => !b.disableModelInvocation);
  if (visible.length === 0) return "";

  // Personal entries (origin:"personal") are visibly marked in the catalog —
  // their name already carries the qualified "personal:<name>" activation
  // key, but the explicit suffix keeps the distinction readable even if that
  // naming convention ever changes.
  const lines = visible.map((b) => `- ${b.name}: ${b.description}${b.origin === "personal" ? " (personal)" : ""}`);
  return `Available skills (call load_skill(name) to activate):\n${lines.join("\n")}`;
}

// buildSkillFilesManifest renders the "## Skill files" block appended to an
// activated skill's body — the second
// progressive-disclosure stage, telling the model which files exist and how
// to read one. Returns "" when there are no files (no injected section, no
// prompt bloat) — shared by both makeLoadSkillTool's activate() (covers both
// execute() and commandActivate(), since both call activate()) and the
// /command palette resolver in routes/agui.ts, so the block's exact wording
// lives in exactly one place.
export function buildSkillFilesManifest(catalogName: string, filePaths: string[]): string {
  if (filePaths.length === 0) return "";
  const list = filePaths.map((p) => `- ${p}`).join("\n");
  return `\n\n## Skill files\n${list}\nRead these with read_skill_file(skill="${catalogName}", path="<path>").`;
}

// The tool definition shape expected by the session builder. We do not import
// the full Pi SDK here to avoid coupling — the session builder passes the object
// directly to createAgentSession as a customTool entry after wrapping it with
// defineTool. To keep load-skill.ts testable without the SDK, we expose the
// raw shape and let session.ts wrap it.
//
// Error protocol: execute() never throws. On an unknown or rejected name it
// returns content[0].text starting with the literal prefix "ERROR:" — callers
// that need to distinguish success from failure (e.g. activateSkill in
// session-plan.ts) check for this prefix. The prefix is an internal contract
// between this module and its callers; it is never shown to the user verbatim
// (callers re-format or surface it as a structured result).
export interface LoadSkillToolDef {
  name: "load_skill";
  execute(toolCallId: string, params: { name: string }): Promise<{ content: { type: "text"; text: string }[]; details: unknown }>;
  // commandActivate is the /command pre-activation path (activateSkill in
  // session-plan.ts) — never wired into the model's tool set. It bypasses the
  // frozen, disable_model_invocation-filtered byName map so a personal skill
  // created after the child spawned, or an admin bundle marked
  // disable_model_invocation, still activates: the parent already authorized
  // the exact name via resolveCommandSkill. execute (the model's load_skill)
  // keeps using byName so DMI model-suppression stays intact. Returns the skill
  // body, or an "ERROR:"-prefixed string on an unresolvable name.
  commandActivate(name: string): Promise<string>;
}

// SkillBodyFetcher resolves a personal skill's body on demand at load_skill
// activation — a personal entry's body
// is deliberately never included in bundles[] (the
// south list RPC is frontmatter-only, to stay clear of the gRPC list-size
// ceiling and to pick up a same-session edit to Skills/<name>/ without a
// respawn). Structurally satisfied by GovernanceBridge.getSkillBody and
// RemoteBridgeClient.getSkillBody — bind() the method before passing it in
// so `this` resolves correctly.
export interface SkillBodyFetcher {
  // allowedTools: dormant, not consumed —
  // kept on the return shape only because GovernanceBridge.getSkillBody's wire
  // contract still carries it.
  (name: string): Promise<{ ok: boolean; body?: string; allowedTools?: string[]; filePaths?: string[]; error?: string }>;
}

// makeLoadSkillTool builds the load_skill tool definition bound to the given
// bundle list. The bundles list must already be filtered to the session's
// FGA-granted set (as returned by ListUserAgentSkills), unioned with the
// caller's own personal entries (origin:"personal", resolveSessionPlan).
// fetchPersonalBody is required only to activate a personal entry — omit it
// (or pass no personal entries) and every bundle activation is unaffected.
export function makeLoadSkillTool(
  bundles: SkillBundleEntry[],
  logger: WarnLogger,
  fetchPersonalBody?: SkillBodyFetcher,
): LoadSkillToolDef {
  // byName — the model's load_skill accepted set: non-disable_model_invocation
  // bundles only. Bundles with disable_model_invocation are excluded here (CP8
  // /command can still activate them — via byNameAll below, not this map).
  const byName = new Map<string, SkillBundleEntry>();
  // byNameAll — every bundle, DMI included. Used only by commandActivate: the
  // /command path the parent already authorized (resolveCommandSkill), which is
  // documented to honor disable_model_invocation bundles.
  const byNameAll = new Map<string, SkillBundleEntry>();
  for (const b of bundles) {
    byNameAll.set(b.name, b);
    if (!b.disableModelInvocation) {
      byName.set(b.name, b);
    }
  }

  const warnContextFork = (name: string) => {
    logger.warn(
      `load_skill: skill "${name}" has context_fork=true but ChildSupervisor wiring is deferred — running inline (context_fork ignored)`,
    );
  };

  // activate resolves an entry to its body (+ file manifest). For a personal
  // entry it fetches body fresh (frontmatter-only south list; picks up a
  // same-session Skills/<name>/ edit), so callers may pass a bare
  // { name, origin:"personal" } with no body. Returns the body string, or an
  // "ERROR:"-prefixed string on an unresolvable personal fetch — the same
  // "ERROR:" contract execute() and commandActivate() surface. Activation has
  // no side effect on tool authorization (D5) — allowed_tools is never read.
  async function activate(entry: {
    name: string;
    origin?: "personal";
    body?: string;
    filePaths?: string[];
  }): Promise<string> {
    let body = entry.body;
    let filePaths = entry.filePaths ?? [];
    if (entry.origin === "personal") {
      if (!fetchPersonalBody) {
        return `ERROR: unknown or unavailable skill "${entry.name}"`;
      }
      const bareName = entry.name.slice(PERSONAL_SKILL_PREFIX.length);
      const fetched = await fetchPersonalBody(bareName);
      if (!fetched.ok) {
        return `ERROR: unknown or unavailable skill "${entry.name}"`;
      }
      body = fetched.body ?? "";
      filePaths = fetched.filePaths ?? entry.filePaths ?? [];
    }

    return (body ?? "") + buildSkillFilesManifest(entry.name, filePaths);
  }

  return {
    name: "load_skill",
    async execute(_toolCallId: string, params: { name: string }) {
      const b = byName.get(params.name);
      if (!b) {
        return textResult(`ERROR: unknown or unavailable skill "${params.name}"`);
      }
      // context_fork=true: run inline (v1 limitation), emit exactly one warn log.
      if (b.contextFork) warnContextFork(b.name);
      return textResult(await activate(b));
    },
    async commandActivate(name: string) {
      // Personal names bypass byName* entirely so a DMI or after-spawn-created
      // personal skill still resolves via activate's fresh fetch.
      if (name.startsWith(PERSONAL_SKILL_PREFIX)) {
        return activate({ name, origin: "personal" });
      }
      // Admin /command honors DMI bundles (byNameAll), per resolveCommandSkill's
      // documented contract — unlike the model's execute path (byName).
      const b = byNameAll.get(name);
      if (!b) {
        return `ERROR: unknown or unavailable skill "${name}"`;
      }
      if (b.contextFork) warnContextFork(b.name);
      return activate(b);
    },
  };
}
