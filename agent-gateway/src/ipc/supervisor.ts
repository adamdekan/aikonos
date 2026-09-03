// ChildSupervisor — the child process pool (CP6).
//
// WHY this exists: the parent (credential-holder) must be the only process that
// holds provider API keys, OIDC bearers, ownerGrants, and BrokerClients/SVID.
// The supervisor owns the child lifecycle — spawn, reuse, evict, crash-recovery —
// so all call sites get a handle without ever exposing a secret.
//
// Identity is bound from the spawn record at construction time. Nothing in the
// IPC message body can change who a child acts as.
//
// Keying flag (AIKONOS_GATEWAY_CHILD_KEYING):
//   "single"   — legacy opt-in. One shared child for all identities; key=constant.
//   "per-user" — default (F28). Key = "<tenantId> <userId> <agentId>".
import { fork, type ChildProcess } from "node:child_process";
import { fileURLToPath } from "node:url";
import { join, dirname } from "node:path";
import { ParentLink } from "./protocol.js";
import type { Channel, IpcMessage, InitMessage, TextDeltaEvent, ToolStartEvent, ToolEndEvent, UsageEvent, DoneEvent, ErrorEvent, PromptMessage, ConvMessage } from "./protocol.js";
import { RemoteBridgeServer } from "./bridge-server.js";
import type { BridgeLike, BridgeFactory, RunIdentity } from "./bridge-server.js";
import type { SubagentEventSink } from "../subagent/run.js";
import type { Identity, Approver } from "../broker/governance.js";
import type { EgressProxy, ProviderTarget, RegisterResult } from "../llm/egress-proxy.js";
import { resolveSessionPlan } from "../pi/session-plan.js";
import type { ResolveIdentity, ResolveSouth, ResolveCfg } from "../pi/session-plan.js";
import type { AgentSpec } from "../pi/session.js";
import { log } from "../log.js";
import { failedPreconditionError } from "../http-errors.js";

// ── Env allowlist ─────────────────────────────────────────────────────────────
//
// The child env is built from an EXPLICIT allowlist — nothing else crosses.
// Fail-closed: a future secret added to the parent's env is excluded by default
// without any code change. A denylist would pass it through silently.
//
// Allowlist rationale (each var justified by what the child actually reads):
//   PATH                         — needed if Node or any subprocess resolves a
//                                  binary by name (e.g. Pi SDK internal tooling)
//   HOME                         — Pi SDK AuthStorage/SettingsManager use home
//                                  dir as a fallback; all storage here is in-
//                                  memory, but the SDK may still read HOME
//   TMPDIR / TMP / TEMP          — os.tmpdir() reads these; createSessionFromPlan
//                                  calls mkdtempSync(join(tmpdir(), …))
//   NODE_ENV                     — Pi SDK and its deps read this for behaviour
//                                  switches (dev vs production logging, etc.)
//   AIKONOS_GATEWAY_THREAD_TTL_MS — child-entry.ts line 77 reads this directly
//   AIKONOS_CHILD_ENTRY           — injected below; child-entry.ts entry guard

const CHILD_ENV_ALLOWLIST: ReadonlySet<string> = new Set([
  "PATH",
  "HOME",
  "TMPDIR",
  "TMP",
  "TEMP",
  "NODE_ENV",
  "AIKONOS_GATEWAY_THREAD_TTL_MS",
]);

/** Returns an allowlist-only copy of env for the forked child (secrets absent from the allowlist are excluded by default, fail-closed) plus the injected AIKONOS_CHILD_ENTRY=1 marker. */
export function scrubEnv(env: NodeJS.ProcessEnv): NodeJS.ProcessEnv {
  const result: NodeJS.ProcessEnv = {};
  for (const k of CHILD_ENV_ALLOWLIST) {
    if (Object.prototype.hasOwnProperty.call(env, k) && env[k] !== undefined) {
      result[k] = env[k];
    }
  }
  result["AIKONOS_CHILD_ENTRY"] = "1";
  return result;
}

// Minimum interval between plan re-resolves for a reused child. Bounds the extra
// south RPCs under bursty reuse (e.g. rapid interactive messages) while keeping
// FGA grant changes near-live for reused children (Approach A).
const PLAN_RECHECK_MS = 10_000;

// Order-independent equality for tool-name lists. resolveSessionPlan builds the
// list in a stable order today, but comparing as sets keeps the change-detection
// correct regardless of ordering.
function sameToolSet(a: string[], b: string[]): boolean {
  if (a.length !== b.length) return false;
  const set = new Set(a);
  return b.every((n) => set.has(n));
}

// ── ChildHandle ────────────────────────────────────────────────────────────────

export interface ChildHandle {
  readonly key: string;
  // The tenant/user this child was spawned to serve. Spend-caps CP3: carried
  // alongside agentId so a child usage relay can be forwarded to
  // south.emitLlmUsage with the full spawn-bound identity without threading
  // it through every run() call site.
  readonly tenantId: string;
  readonly userId: string;
  // The agent identity this child was spawned to serve. Used by
  // evictIdleForAgent (F28) to find every idle child bound to a given agent
  // regardless of keying mode — under "single" keying one shared child still
  // records whichever agentId spawned it, so a soul edit for that agent evicts it.
  readonly agentId: string;
  // Recorded at fork time — true only for a
  // child spawned via withEphemeralChild's subagent-branch path. forwardUsage
  // reads this to tag usage source "subagent" vs "chat". Deliberately a fact on
  // the handle, not something derived by matching the pool key's "subagent:"
  // prefix in forwardUsage: that prefix is a CP6 sanctioned exception scoped to
  // this file's OWN key-format functions (branchKeyPrefix/ephemeralKey), not a
  // license for every method in the file to re-parse it — CP5 forbade a
  // distant module keying behaviour off that prefix, and forwardUsage keying
  // off it would be exactly that.
  readonly isSubagentBranch: boolean;
  readonly link: ParentLink;
  readonly bridgeServer: RemoteBridgeServer;
  readonly proxyToken: string;
  // timestamp of last prompt sent; supervisor reaper uses this.
  lastUsedAt: number;
  // true when a prompt is currently being processed (for LRU-eviction logic)
  busy: boolean;
  // The FGA-derived tool allowlist the child was spawned with. A reused child is
  // compared against a fresh resolve so a grant added after spawn triggers a
  // respawn (Approach A) — without this a long-lived child serves a stale tool set.
  allowedToolNames: string[];
  // The system prompt the child was spawned with. Compared on each idle reuse
  // alongside allowedToolNames — a persona edit takes effect on the next idle
  // reuse after PLAN_RECHECK_MS (same timing semantics as tool-allowlist refresh).
  systemPrompt: string;
  // Last time the plan was re-resolved for this handle. Throttles the extra south
  // RPCs to at most one per PLAN_RECHECK_MS across bursty reuse.
  lastPlanCheckAt: number;
  // Per-run context delegation — callers use these instead of touching bridgeServer directly.
  setRunContext(
    runId: string,
    identity: RunIdentity,
    approver: Approver,
    sessionId?: string,
    onSubagentEvent?: SubagentEventSink,
  ): void;
  clearRunContext(runId: string): void;
  // Send an abort directive to the child for a specific run. No-op if the child
  // does not support abort or the run is already terminal.
  abortRun(runId: string): void;
}

// ── Provider credential resolver ───────────────────────────────────────────────
//
// The supervisor needs the real api key to pass to proxy.register().
// The default impl reads process.env.OPENROUTER_API_KEY (parent has it).
// Tests inject a fake that returns a controllable key — no env read.
//
// ProviderTarget is re-exported from the egress proxy — the proxy is where a
// target is actually consumed, and it is already imported here, so one
// definition serves both. ProviderCredentials still mirrors pi/session.ts's
// shape (not imported, to avoid a session.ts <-> supervisor.ts cycle).

export type { ProviderTarget };

export interface ProviderCredentials extends ProviderTarget {
  modelAllowlist: string[];
  // Remaining keyed candidates in tenant selection order, primary excluded.
  fallbacks: ProviderTarget[];
}

export type ProviderCredentialResolver = (
  identity: ResolveIdentity,
  agentSpec?: AgentSpec,
) => Promise<ProviderCredentials>;

// ── SpawnOptions / SpawnFn seam ────────────────────────────────────────────────
//
// The child factory is injected so unit tests supply a fake ParentLink without
// starting a real process. Production passes `defaultSpawnChild`.

export interface SpawnChildOptions {
  env: NodeJS.ProcessEnv;
}

export type SpawnChildFn = (opts: SpawnChildOptions) => ParentLink;

// ── GatewayOverloadError ───────────────────────────────────────────────────────
//
// Thrown when all children are busy and past the concurrency cap. CP7/CP8
// map this to HTTP 503 {error:"gateway_overloaded"}.

export class GatewayOverloadError extends Error {
  constructor() {
    super("gateway overloaded: all children busy and concurrency cap reached");
    this.name = "GatewayOverloadError";
  }
}

// ── Ephemeral (subagent) pool keys ─────────────────────────────────────────────
//
// Subagent branch children are keyed synthetically rather than by identity, so
// they can never collide with — or be reused by — an interactive pooled child
// for the same (tenant, user, agent). The shape is pinned here because the graph
// runner and the run-teardown path both construct it.

/**
 * Prefix shared by every ephemeralKey belonging to `runId`'s fan-out. Exported
 * so run-teardown (CP6) can find every branch of a run without re-hardcoding
 * the "subagent:" literal a second place — the two sites would otherwise be
 * free to drift out of the shape ephemeralKey itself defines.
 */
export function branchKeyPrefix(runId: string): string {
  return `subagent:${runId}:`;
}

/** Pool key for branch `index` of the subagent fan-out belonging to `runId`. */
export function ephemeralKey(runId: string, index: number): string {
  return `${branchKeyPrefix(runId)}${index}`;
}

// ── Circuit-breaker state ──────────────────────────────────────────────────────

interface CrashRecord {
  key: string;
  timestamps: number[];
  blockedUntil: number;
}

// ── Supervisor config ──────────────────────────────────────────────────────────

export interface SupervisorConfig {
  // "single" | "per-user"
  keying: "single" | "per-user";
  maxChildren: number;
  childTtlMs: number;
  // Circuit-breaker: N exits within windowMs → block for blockMs
  cbMaxCrashes: number;
  cbWindowMs: number;
  cbBlockMs: number;
}

// maxChildren/childTtlMs are no longer read from process.env here (F26) — they
// flow from the validated Config (config.ts:buildConfig) via the Partial
// override the ChildSupervisor constructor already merges over this default.
// AIKONOS_GATEWAY_CHILD_KEYING stays a direct env read verbatim — it is not in
// scope for this batch (F26 non-goal: "child keying").
export function defaultSupervisorConfig(): SupervisorConfig {
  const rawKeying = process.env["AIKONOS_GATEWAY_CHILD_KEYING"];
  if (rawKeying !== undefined && rawKeying !== "" && rawKeying !== "single" && rawKeying !== "per-user") {
    throw new Error(`invalid AIKONOS_GATEWAY_CHILD_KEYING="${rawKeying}": expected "single" or "per-user"`);
  }
  const keying: "single" | "per-user" = (rawKeying === "single") ? "single" : "per-user";
  return {
    keying,
    maxChildren: 32,
    childTtlMs: 1_800_000,
    cbMaxCrashes: 3,
    cbWindowMs: 10_000,
    cbBlockMs: 30_000,
  };
}

// ── South/cfg dependencies for resolveSessionPlan ─────────────────────────────

export interface SupervisorDeps {
  south: ResolveSouth;
  cfg: ResolveCfg;
  // Spend-caps CP3: forwards a child's usage relay to the broker. A separate,
  // optional field (not folded into ResolveSouth) so resolveSessionPlan's
  // south dependency — and every existing test fake typed against it — is
  // unaffected by this metering-only addition; absent in a test that doesn't
  // exercise usage forwarding simply means usage events aren't emitted.
  emitLlmUsage?: (req: {
    tenantId: string;
    userId: string;
    agentId: string;
    provider: string;
    model: string;
    tokensIn: number;
    tokensOut: number;
    cacheRead: number;
    cacheWrite: number;
    cost: number;
    runId: string;
    sessionId: string;
    source: string;
    quantity: number;
    unit: string;
  }) => Promise<unknown>;
}

// ── ChildSupervisor ────────────────────────────────────────────────────────────

/** Child process pool: spawns, reuses, LRU-evicts, and crash-guards forked Pi-loop children; identity is bound at spawn time and never flows through IPC message bodies. */
export class ChildSupervisor {
  private readonly children = new Map<string, ChildHandle>();
  // Keys granted a cap slot by enforceCapBefore whose spawn has not yet landed in
  // children. Deliberately NOT a placeholder entry in children: every iterator
  // over that map (LRU pass, TTL reaper, evictIdleForAgent, dispose) would then
  // have to defend against a handle-less member.
  private readonly reserved = new Set<string>();
  private readonly crashes = new Map<string, CrashRecord>();
  private readonly config: SupervisorConfig;
  private readonly reaper: ReturnType<typeof setInterval>;

  constructor(
    private readonly proxy: EgressProxy,
    private readonly deps: SupervisorDeps,
    private readonly makeBridge: BridgeFactory,
    private readonly spawnChild: SpawnChildFn,
    private readonly resolveCredentials: ProviderCredentialResolver,
    config?: Partial<SupervisorConfig>,
  ) {
    this.config = { ...defaultSupervisorConfig(), ...config };

    this.reaper = setInterval(() => this.reapIdle(), 60_000);
    this.reaper.unref();
  }

  // keyFor derives the pool key from an identity.
  // "single" → one shared child; "per-user" → one child per (tenantId, userId, agentId).
  //
  // WHY space separator: UUIDs contain only hex digits and hyphens — a space is
  // the one character that cannot appear in any of the three fields, so joining
  // with " " is collision-safe. A "/" separator would collide for e.g.
  // (userId="a/b", agentId="c") vs (userId="a", agentId="b/c").
  keyFor(identity: Identity): string {
    if (this.config.keying === "per-user") {
      return `${identity.tenantId} ${identity.userId} ${identity.agentId}`;
    }
    return "__single__";
  }

  // getOrSpawn returns an existing child for the key or forks a new one.
  // Throws GatewayOverloadError when the cap is exceeded and no idle child
  // can be evicted.
  // agentSpec is optional — present for agent-bound runs (ticker, external) so
  // the session plan can honour the agent's preferred provider/model. Absent for
  // personal /agui runs (tenant-default path).
  async getOrSpawn(key: string, identity: Identity, agentSpec?: AgentSpec): Promise<ChildHandle> {
    const existing = this.children.get(key);
    if (existing) {
      existing.lastUsedAt = Date.now();
      // Approach A: a long-lived child caches the FGA-derived tool allowlist from
      // spawn time, so a grant added since then would not take effect. Re-resolve
      // the plan (throttled, idle-only) and respawn the child when its tool set
      // changed. A busy child is never killed mid-run — it refreshes on the next
      // idle reuse. A transient resolve failure keeps the existing child (fail to
      // the last-known-good plan rather than dropping a working session).
      if (!existing.busy && Date.now() - existing.lastPlanCheckAt >= PLAN_RECHECK_MS) {
        existing.lastPlanCheckAt = Date.now();
        try {
          const fresh = await this.resolveAllowedToolNames(identity, agentSpec);
          if (
            !existing.busy &&
            (!sameToolSet(fresh.allowedToolNames, existing.allowedToolNames) ||
              fresh.systemPrompt !== existing.systemPrompt)
          ) {
            log.info({ key }, "tool allowlist or persona changed since spawn — respawning child");
            // Hold the slot this evict frees: the respawn below only registers
            // several awaits later, so an unreserved gap hands the freed slot to
            // a concurrent new-key caller and the pool ends over cap. spawn()'s
            // finally releases it on both outcomes.
            //
            // Order matters: reserve only once the evict has landed. Both
            // statements are synchronous, so the transient count is unobservable
            // either way — but a throwing evict is caught below and returns
            // `existing` without ever calling spawn(), so a reservation taken
            // first would never be released and the pool would be permanently
            // one slot smaller.
            this.evict(key, "tool allowlist or persona changed");
            this.reserved.add(key);
            return this.spawn(key, identity, agentSpec);
          }
        } catch (err) {
          log.warn({ key, err: String(err) }, "plan re-resolve failed — keeping existing child");
        }
      }
      return existing;
    }

    this.checkCircuitBreaker(key);
    await this.enforceCapBefore(key);

    return this.spawn(key, identity, agentSpec);
  }

  // withEphemeralChild spawns a child under a caller-supplied synthetic key
  // (see ephemeralKey), runs fn against it exactly once, and evicts it — pass,
  // throw, abort, or crash. Unlike getOrSpawn it never consults the pool: a
  // subagent branch must start on fresh context, and leaving the child pooled
  // afterwards would let a later getOrSpawn land on a child built for a subtask
  // that has already finished.
  //
  // The child counts against maxChildren while it lives, so a saturated pool
  // rejects with GatewayOverloadError from enforceCapBefore — reject-don't-queue,
  // reusing the pooled path's existing guarantee rather than adding a second one.
  // It is marked busy before fn starts so the LRU pass cannot take it in the
  // window before the branch's prompt is in flight. That flag does NOT hold for
  // the whole of fn: run()'s settle calls markIdle, so between turns the child
  // is LRU-evictable again. Branch runners therefore issue exactly ONE run() per
  // ephemeral child (src/subagent/run.ts) — a second sequential turn could be
  // taken out from under the branch by a sibling spawning at cap.
  // onAdmitted: fired once this key has
  // won a pool slot — i.e. AFTER enforceCapBefore succeeds, BEFORE spawn() is
  // attempted — never on a GatewayOverloadError rejection. This is the one
  // point that is both "committed to a real spawn attempt" and "early enough
  // that a systemic spawn() failure (credential-resolve error, etc.) still
  // fires it". The subagent runner uses this to emit its "spawned" timeline
  // event: firing any earlier would announce a spawn for a branch that never
  // won a slot (the pool-overload case); firing any later (inside fn) would
  // miss that same event for a branch whose spawn() itself throws. Optional so
  // every existing caller (getOrSpawn's own callers never touch this method)
  // keeps compiling unchanged.
  async withEphemeralChild<T>(
    key: string,
    identity: Identity,
    fn: (handle: ChildHandle) => Promise<T>,
    agentSpec?: AgentSpec,
    onAdmitted?: () => void,
  ): Promise<T> {
    // spawn() does children.set(key, …) unconditionally, so a key already live in
    // the pool would be overwritten — orphaning that child with no kill and no
    // proxy unregister. Refuse instead of leaking.
    if (this.children.has(key)) {
      throw new Error(`ephemeral child key collision: "${key}" is already in the pool`);
    }
    // No checkCircuitBreaker here: a synthetic key is single-use, so a breaker on
    // it could never trip on a second attempt. For the same reason the crash
    // record onChildExit writes for this key is unreadable dead weight — dropped
    // below, or this.crashes would grow by one entry per branch ever spawned.
    await this.enforceCapBefore(key);
    onAdmitted?.();

    // isSubagentBranch:true unconditionally — withEphemeralChild is the sole
    // production entry point for subagent branch spawns; the ordinary pooled path (getOrSpawn) never
    // calls spawn() with this flag set.
    const handle = await this.spawn(key, identity, agentSpec, true);
    this.markBusy(handle);
    try {
      return await fn(handle);
    } finally {
      // Idempotent when the child already crashed (onChildExit removed it) — the
      // one teardown implementation, so proxy-token release cannot drift.
      this.evict(key, "ephemeral branch settled");
      this.crashes.delete(key);
    }
  }

  // resolveAllowedToolNames re-resolves the plan for change detection on a
  // reused child. resolveSessionPlan is side-effect-free (proxy.register happens
  // separately in spawn), so the default throwaway proxy URL is safe here —
  // only allowedToolNames and systemPrompt are read from the result.
  // Returns both so the caller can detect either tool-allowlist drift OR persona
  // drift in a single RPC round (no extra calls).
  protected async resolveAllowedToolNames(
    identity: Identity,
    agentSpec?: AgentSpec,
  ): Promise<{ allowedToolNames: string[]; systemPrompt: string }> {
    const resolveIdentity: ResolveIdentity = {
      tenantId: identity.tenantId,
      userId: identity.userId,
      agentId: identity.agentId,
    };
    const plan = await resolveSessionPlan(resolveIdentity, {
      south: this.deps.south,
      cfg: this.deps.cfg,
      agentSpec,
    });
    return { allowedToolNames: plan.allowedToolNames, systemPrompt: plan.systemPrompt };
  }

  // markBusy / markIdle let call sites tell the supervisor whether a child is
  // actively processing so the LRU-eviction logic can skip busy children.
  markBusy(handle: ChildHandle): void {
    handle.busy = true;
    handle.lastUsedAt = Date.now();
  }

  markIdle(handle: ChildHandle): void {
    handle.busy = false;
    handle.lastUsedAt = Date.now();
  }

  // run sends a prompt to the child, marks it busy for the duration, and routes
  // all IPC events tagged with that runId to onEvent. Resolves when done arrives;
  // rejects when error arrives or when the child exits mid-run.
  //
  // WHY runId-scoped routing: a child multiplexes threads, so events from a
  // concurrent run on the same child must not bleed into another run's SSE stream.
  run(
    handle: ChildHandle,
    prompt: { runId: string; threadId: string; text: string; sessionId?: string; history?: ConvMessage[]; activateSkillName?: string; activateSkillNames?: string[]; userInstructions?: string },
    onEvent: (evt: TextDeltaEvent | ToolStartEvent | ToolEndEvent | UsageEvent | DoneEvent | ErrorEvent) => void,
  ): Promise<void> {
    this.markBusy(handle);
    // The LLM-call budget is per RUN, but children are pooled and reused across
    // runs — without this reset the second run on a reused child would start
    // already spent (see EgressProxy property 9).
    this.proxy.resetRunBudget(handle.proxyToken);

    return new Promise<void>((resolve, reject) => {
      const { runId } = prompt;

      type RunEvent = TextDeltaEvent | ToolStartEvent | ToolEndEvent | UsageEvent | DoneEvent | ErrorEvent;
      const eventKinds: Array<RunEvent["kind"]> = ["text_delta", "tool_start", "tool_end", "usage", "done", "error"];

      // Capture each per-run handler by kind so we can remove them in cleanup.
      // WHY: BaseLink accumulates handlers per kind; without off(), a shared child
      // that serves many runs grows a handler list of 6N entries, one set per run.
      const perKindHandlers = {} as Record<RunEvent["kind"], (msg: RunEvent) => void>;

      // Declared before settle so the closure in settle can reference it without
      // a temporal dead zone issue (settle is called after exitHandler is assigned,
      // but declaring it here makes the ordering explicit for readers).
      let exitHandler: (code: number | null) => void;

      let settled = false;
      const settle = (fn: () => void) => {
        if (settled) return;
        settled = true;
        // Remove the per-run event handlers so they don't accumulate on a
        // long-lived shared child. Without off(), each run adds N handlers
        // (one per kind) that never deregister — 6N growth per run.
        for (const kind of eventKinds) {
          handle.link.off(kind, perKindHandlers[kind]);
        }
        // Remove the exit handler registered for this run. Without offExit, a
        // long-lived shared child accumulates one dead exit handler per run,
        // triggering Node's MaxListenersExceededWarning after ~10 concurrent runs.
        handle.link.offExit(exitHandler);
        this.markIdle(handle);
        fn();
      };

      // Install per-kind handlers. Each checks runId before forwarding so
      // events from other concurrent runs on the same child are ignored.
      //
      // done/error are terminal: settle and resolve/reject exactly once.
      for (const kind of eventKinds) {
        const handler = (msg: RunEvent) => {
          if (msg.runId !== runId) return;
          onEvent(msg);
          if (msg.kind === "usage") {
            this.forwardUsage(handle, msg, prompt.sessionId ?? "");
          } else if (msg.kind === "done") {
            settle(() => resolve());
          } else if (msg.kind === "error") {
            settle(() => reject(new Error(msg.message)));
          }
        };
        perKindHandlers[kind] = handler;
        handle.link.on(kind, handler);
      }

      // If the child exits while this run is in flight, reject so the caller
      // can surface an error frame rather than hanging forever. Capture the
      // reference so settle() can call offExit — otherwise a long-lived shared
      // child accumulates one dead exit handler per run (MaxListenersExceeded).
      exitHandler = () => {
        settle(() => reject(new Error("child process exited mid-run")));
      };
      handle.link.onExit(exitHandler);

      const msg: PromptMessage = {
        kind: "prompt",
        runId,
        threadId: prompt.threadId,
        text: prompt.text,
        ...(prompt.history !== undefined ? { history: prompt.history } : {}),
        ...(prompt.activateSkillName !== undefined ? { activateSkillName: prompt.activateSkillName } : {}),
        ...(prompt.activateSkillNames !== undefined ? { activateSkillNames: prompt.activateSkillNames } : {}),
        ...(prompt.userInstructions !== undefined ? { userInstructions: prompt.userInstructions } : {}),
      };
      handle.link.send(msg);
    });
  }

  dispose(): void {
    clearInterval(this.reaper);
    for (const [key] of this.children) {
      this.evict(key, "supervisor dispose");
    }
  }

  // evictIdleForAgent (F28) — push invalidation for persona/soul edits. Evicts
  // every IDLE child bound to agentId so the next getOrSpawn re-resolves the
  // plan and picks up the new soul immediately, instead of waiting for the
  // PLAN_RECHECK_MS idle-reuse backstop. Busy children are left alone — a
  // mid-run persona stays frozen until the run ends, same as today.
  //
  // Callers (e.g. PUT /agents/:id/soul) pass the bare agent id, but
  // src/routes/agui.ts's sessionAgentId prefixes named agents as
  // "agent:<id>" when spawning — the synthetic personal-agent ids stay bare.
  // Match both forms so eviction actually finds named-agent children.
  evictIdleForAgent(agentId: string, reason: string): void {
    for (const [key, handle] of this.children) {
      if ((handle.agentId === agentId || handle.agentId === `agent:${agentId}`) && !handle.busy) {
        this.evict(key, reason);
      }
    }
  }

  // evictBranchesForRun — run-teardown
  // sweep. An `/agui` SSE close or user stop must abort every in-flight
  // subagent branch child of that run and release its egress-proxy token, so
  // no branch child or proxy registration outlives the run that spawned it.
  // Mirrors evictIdleForAgent's shape: walk `children`, match by key prefix
  // (every branch of runId shares branchKeyPrefix(runId), never a different
  // run's — sibling runs are untouched by construction of the key match).
  //
  // abortRun BEFORE evict, same order as a branch's own finally
  // (src/subagent/run.ts): tells the child to stop the LLM call immediately,
  // ahead of evict's "shutdown" + kill(500) actually tearing the process down.
  // evict() is the sole teardown implementation and is idempotent (early-
  // returns once the key is gone), so the branch's own withEphemeralChild
  // finally later evicting the same key again is a clean no-op — the proxy
  // token is still released exactly once.
  //
  // Does NOT wait for the branch's own supervisor.run() promise to settle:
  // that promise resolves independently once the evicted child's process
  // actually exits (the "shutdown" message's process.exit(0), or kill(500)'s
  // forced fallback), via the same onExit path onChildExit and run()'s own
  // exitHandler already use for a crash. Blocking here on that would tie the
  // SSE close handler's own completion to a process-exit round trip it does
  // not need to wait for.
  evictBranchesForRun(runId: string, reason: string): void {
    const prefix = branchKeyPrefix(runId);
    for (const [key, handle] of this.children) {
      if (!key.startsWith(prefix)) continue;
      handle.abortRun(runId);
      this.evict(key, reason);
    }
  }

  // Spend-caps CP3: forwards one child usage relay to south.emitLlmUsage under
  // the child's spawn-bound identity (tenantId/userId/agentId — never the
  // per-run caller, since the child holds no identity of its own). Mirrors
  // the deleted pi/usage.ts's posture: skip a fully-zero turn (nothing
  // happened), fire-and-forget with warn-on-error — an emit failure must
  // never break the run whose events are still streaming via onEvent above.
  //
  // Attribution: runId comes off the IPC
  // event, sessionId from the parent-side run() caller (only the webui chat path
  // knows one — scheduler/external send ""). source distinguishes a subagent
  // branch child from an ordinary pooled one via handle.isSubagentBranch — a fact recorded at fork time, never
  // derived here from the pool key's shape.
  private forwardUsage(handle: ChildHandle, msg: UsageEvent, sessionId: string): void {
    const cacheRead = msg.cacheRead ?? 0;
    const cacheWrite = msg.cacheWrite ?? 0;
    if (msg.inputTokens === 0 && msg.outputTokens === 0 && cacheRead === 0 && cacheWrite === 0) {
      return;
    }
    if (!this.deps.emitLlmUsage) return;
    void this.deps
      .emitLlmUsage({
        tenantId: handle.tenantId,
        userId: handle.userId,
        agentId: handle.agentId,
        provider: msg.provider ?? "",
        model: msg.model ?? "",
        tokensIn: msg.inputTokens,
        tokensOut: msg.outputTokens,
        cacheRead,
        cacheWrite,
        cost: msg.cost ?? 0,
        runId: msg.runId,
        sessionId,
        source: handle.isSubagentBranch ? "subagent" : "chat",
        quantity: 0,
        unit: "",
      })
      .catch((err: unknown) => {
        log.warn({ err: String(err) }, "emitLlmUsage failed");
      });
  }

  // ── Private ──────────────────────────────────────────────────────────────────

  // spawn holds the cap reservation enforceCapBefore took for this key until the
  // child is registered in children — or the attempt fails. Releasing in a
  // finally rather than at children.set covers every failure path (credential
  // resolve, proxy.register, session plan, allowlist divergence, fork, handle
  // wiring); a release on the success path only would leak a permanent phantom
  // slot per failed spawn until the pool admitted nothing.
  private async spawn(
    key: string,
    identity: Identity,
    agentSpec?: AgentSpec,
    isSubagentBranch = false,
  ): Promise<ChildHandle> {
    try {
      return await this.forkChild(key, identity, agentSpec, isSubagentBranch);
    } finally {
      this.reserved.delete(key);
    }
  }

  private async forkChild(
    key: string,
    identity: Identity,
    agentSpec?: AgentSpec,
    isSubagentBranch = false,
  ): Promise<ChildHandle> {
    // Resolve credentials in the parent — the child never sees them.
    const creds = await this.resolveCredentials(identity, agentSpec);

    // Register with the proxy — key+baseUrl stay in the parent's memory.
    const regResult: RegisterResult = this.proxy.register({
      upstreamBaseUrl: creds.upstreamBaseUrl,
      realApiKey: creds.apiKey,
      modelAllowlist: creds.modelAllowlist,
      // The proxy retries these in order on a pre-stream upstream failure.
      fallbacks: creds.fallbacks,
      tenantId: identity.tenantId,
      agentId: identity.agentId,
      userId: identity.userId,
      ...(creds.api !== undefined ? { api: creds.api } : {}),
      ...(creds.apiVersion !== undefined ? { apiVersion: creds.apiVersion } : {}),
    });

    // Resolve the secret-free session plan, fork the child, and wire it up.
    // Everything from here through the init send is wrapped so ANY throw
    // (sync or async) unregisters the proxy token registered above —
    // otherwise a real upstream key leaks into EgressProxy.children with no
    // child ever spawned to use it.
    try {
      // Pass agentSpec so the plan can honour the agent's preferred
      // provider/model (ticker + external paths).
      const resolveIdentity: ResolveIdentity = {
        tenantId: identity.tenantId,
        userId: identity.userId,
        agentId: identity.agentId,
      };
      const plan = await resolveSessionPlan(resolveIdentity, {
        south: this.deps.south,
        cfg: this.deps.cfg,
        proxyBaseUrl: regResult.childBaseUrl,
        agentSpec,
        isSubagentBranch,
      });

      // CP1 coherence guard:
      // resolveCredentials and resolveSessionPlan run two independent south
      // resolutions and could in principle diverge (e.g. a transient RPC failure
      // on one call but not the other) — a child told to use a model the proxy
      // never allowlisted would 400 on every LLM call with nothing surfaced on
      // /agui. Fail loud here, before the child is forked, instead of letting
      // that divergence reach the proxy at request time.
      if (!creds.modelAllowlist.includes(plan.modelId)) {
        throw failedPreconditionError(
          `llm credentials unavailable: session plan model "${plan.modelId}" is not in the resolved credentials' allowlist [${creds.modelAllowlist.join(", ")}] — credential and session-plan resolution diverged`,
        );
      }

      // Fork with scrubbed env — no secrets cross the process boundary.
      const scrubbedEnv = scrubEnv(process.env);
      const link: ParentLink = this.spawnChild({ env: scrubbedEnv });

      // Bridge-direct parent-side LLM calls (reason/analyzeImage) never touch the
      // egress proxy, but they bill the same run — so hand every bridge built for
      // THIS child a consumer of THIS child's budget counter.
      const bridgeServer = new RemoteBridgeServer(
        link,
        (bridgeIdentity, approver, _budget, runId, _sessionId, onSubagentEvent) =>
          this.makeBridge(
            bridgeIdentity,
            approver,
            () => this.proxy.consumeLlmBudget(regResult.childToken),
            runId,
            undefined,
            onSubagentEvent,
          ),
        // The IPC-boundary tool-membership backstop. Stored with the child here rather than threaded through
        // setRunContext's five call sites: the set is a property of the CHILD
        // (fixed by the plan it was forked with), not of a run. A plan change
        // respawns the child (getOrSpawn's Approach A re-resolve), so the stored
        // set can never go stale against the schema the child actually holds.
        // Copied, not aliased: the same array is also assigned to the mutable
        // public handle.allowedToolNames below, and a security-bearing set must
        // not be reachable for mutation through it.
        [...plan.allowedToolNames],
      );

      const handle: ChildHandle = {
        key,
        tenantId: identity.tenantId,
        userId: identity.userId,
        agentId: identity.agentId,
        isSubagentBranch,
        link,
        bridgeServer,
        proxyToken: regResult.childToken,
        lastUsedAt: Date.now(),
        busy: false,
        allowedToolNames: plan.allowedToolNames,
        systemPrompt: plan.systemPrompt,
        lastPlanCheckAt: Date.now(),
        setRunContext(
          runId: string,
          runIdentity: RunIdentity,
          approver: Approver,
          sessionId?: string,
          onSubagentEvent?: SubagentEventSink,
        ): void {
          bridgeServer.setRunContext(runId, runIdentity, approver, sessionId, onSubagentEvent);
        },
        clearRunContext(runId: string): void {
          bridgeServer.clearRunContext(runId);
        },
        abortRun(runId: string): void {
          link.send({ kind: "abort", runId });
        },
      };

      this.children.set(key, handle);
      // Release the reservation the instant the handle is countable. A window
      // where the key sits in BOTH maps inflates the count into the LRU branch,
      // and the arriving caller's victim would be this very child — killed
      // before its spawner ever runs against it. spawn()'s finally stays as the
      // failure-path net; Set.delete is idempotent.
      this.reserved.delete(key);

      // Wire the exit handler through the first-class onExit method — no duck-type probe.
      link.onExit(() => this.onChildExit(key, handle));

      // Send the secret-free init plan to the child.
      link.send(plan);

      log.info({ key, modelId: plan.modelId }, "child spawned");
      return handle;
    } catch (err) {
      this.proxy.unregister(regResult.childToken);
      throw err;
    }
  }

  private onChildExit(key: string, handle: ChildHandle): void {
    if (!this.children.has(key)) return;

    log.warn({ key }, "child exited — removing from pool");
    this.children.delete(key);
    handle.bridgeServer.dispose();
    this.proxy.unregister(handle.proxyToken);

    // Record crash for circuit-breaker.
    const now = Date.now();
    const rec = this.crashes.get(key) ?? { key, timestamps: [], blockedUntil: 0 };
    rec.timestamps.push(now);
    // Trim timestamps outside the window.
    rec.timestamps = rec.timestamps.filter((t) => now - t <= this.config.cbWindowMs);

    if (rec.timestamps.length >= this.config.cbMaxCrashes) {
      rec.blockedUntil = now + this.config.cbBlockMs;
      log.warn({ key, crashes: rec.timestamps.length }, "circuit breaker tripped — respawn blocked");
    }
    this.crashes.set(key, rec);
  }

  private checkCircuitBreaker(key: string): void {
    const rec = this.crashes.get(key);
    if (!rec) return;
    if (Date.now() < rec.blockedUntil) {
      throw new Error(`child for key=${key} circuit-breaker open — too many crashes`);
    }
  }

  // enforceCapBefore grants newKey a slot, or throws GatewayOverloadError.
  //
  // WHY in-flight reservations count: children.set(key, handle) lands two awaits
  // into spawn(), so a caller that fans out with Promise.allSettled
  // (src/subagent/run.ts) would have every branch read children.size in the same
  // microtask batch, before any of them registered — all clearing a cap with room
  // for one. Counting reservations closes that window. A reservation has no
  // handle, so the LRU pass below cannot free one: only a real idle child is
  // evictable, and each admission evicts at most one.
  private async enforceCapBefore(newKey: string): Promise<void> {
    if (this.children.size + this.reserved.size < this.config.maxChildren) {
      this.reserved.add(newKey);
      return;
    }

    // Try to LRU-evict an idle child.
    const idleByAge = [...this.children.entries()]
      .filter(([, h]) => !h.busy)
      .sort(([, a], [, b]) => a.lastUsedAt - b.lastUsedAt);

    const victim = idleByAge[0];
    if (victim) {
      this.evict(victim[0], "LRU cap eviction");
      this.reserved.add(newKey);
      return;
    }

    // All children are busy and we are at cap.
    throw new GatewayOverloadError();
  }

  private evict(key: string, reason: string): void {
    const handle = this.children.get(key);
    if (!handle) return;

    this.children.delete(key);
    handle.bridgeServer.dispose();
    this.proxy.unregister(handle.proxyToken);

    // Ask the child to shut down gracefully; the link's kill method (if
    // provided by the spawn wrapper) forcibly terminates after a drain grace.
    handle.link.send({ kind: "shutdown" });
    handle.link.kill(500);

    log.info({ key, reason }, "child evicted");
  }

  private reapIdle(): void {
    const now = Date.now();
    for (const [key, handle] of this.children) {
      if (!handle.busy && now - handle.lastUsedAt >= this.config.childTtlMs) {
        this.evict(key, "idle TTL");
      }
    }
  }
}

// ── Default production spawn ───────────────────────────────────────────────────
//
// Wraps child_process.fork into a ParentLink. The forked process runs
// child-entry.ts with the scrubbed env.

// The gateway runs via tsx (no compile step — see agent-gateway/Dockerfile), so
// the fork target is the .ts source and the child must load the tsx ESM hook via
// execArgv, otherwise a plain node child cannot parse TypeScript.
const CHILD_ENTRY_PATH = join(
  dirname(fileURLToPath(import.meta.url)),
  "child-entry.ts",
);

// ProcessChannel wraps a real ChildProcess as a Channel.
export class ProcessChannel {
  constructor(private readonly proc: ChildProcess) {}

  send(msg: unknown): void {
    // A child whose process has already exited has a torn-down IPC channel.
    // Calling proc.send() in that state does not throw synchronously — it
    // returns false AND schedules an unlistened 'error' event
    // (ERR_IPC_CHANNEL_CLOSED) on a later tick (verified empirically: Node's
    // internal child_process send() defers the emit, so a try/catch around
    // this call cannot contain it), which crashes the WHOLE gateway process
    // since nothing here (or in defaultSpawnChild) listens for 'error' on the
    // ChildProcess. This is reachable in production the moment CP6's run-
    // teardown evicts a genuinely in-flight
    // branch child: the branch's own `finally` (src/subagent/run.ts) still
    // calls handle.abortRun() — i.e. link.send() — after the evicted child
    // has already exited. Checking `connected` first is the fix: every
    // caller of link.send() on this handle (evict's own "shutdown", that
    // same post-exit abortRun) is a best-effort directive to a process that
    // may already be gone, so dropping it silently is correct.
    if (!this.proc.connected) return;
    this.proc.send(msg as object);
  }

  on(event: "message", handler: (msg: IpcMessage) => void): void {
    this.proc.on(event, handler as (msg: unknown) => void);
  }
}

/** Production child factory: forks child-entry.ts with the scrubbed env via tsx and wraps the process as a ParentLink with removable per-run exit handlers. */
export function defaultSpawnChild(opts: SpawnChildOptions): ParentLink {
  const proc = fork(CHILD_ENTRY_PATH, [], {
    env: opts.env,
    serialization: "json",
    execArgv: ["--import", "tsx"],
  });
  const channel = new ProcessChannel(proc);
  const link = new ParentLink(channel);

  // One real proc listener fans out to a removable handler list so offExit can
  // detach individual run-scoped handlers without removing the shared listener.
  const exitHandlers: Array<(code: number | null) => void> = [];
  proc.on("exit", (code) => {
    for (const h of exitHandlers) h(code);
  });

  link.onExit = (handler: (code: number | null) => void) => {
    exitHandlers.push(handler);
  };

  link.offExit = (handler: (code: number | null) => void) => {
    const idx = exitHandlers.indexOf(handler);
    if (idx !== -1) exitHandlers.splice(idx, 1);
  };

  // Wire kill through the first-class kill method.
  link.kill = (delayMs: number) => {
    setTimeout(() => proc.kill(), delayMs).unref();
  };

  return link;
}
