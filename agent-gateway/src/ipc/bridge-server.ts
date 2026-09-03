// RemoteBridgeServer — parent-side IPC bridge server (CP2, security core).
//
// WHY this exists: when the Pi loop runs in a forked child process, it cannot
// call the GovernanceBridge directly (the bridge holds all long-lived secrets:
// OIDC bearer, ownerGrant, BrokerClients/SVID). Instead the child sends IPC
// requests; this class services those requests by calling the bridge bound to
// the specific run that originated the request.
//
// SECURITY INVARIANT (CP2): identity is bound PER-RUN by the parent calling
// setRunContext(runId, identity, approver) BEFORE dispatching the prompt.
// Each incoming message carries a runId; the server looks up the run's bridge
// and calls it. Any identity-shaped field in a message body (owner, userId,
// tenantId, agentId) is structurally ignored — we destructure only the fields
// the bridge method needs. A child cannot change who it acts as by injecting
// fields.
//
// RESIDUAL (single-keying): under "single" keying all users share one child and
// therefore one bridge-server. Per-run context prevents cross-run clobber at the
// IPC level, but a compromised shared child can reference a concurrent run's
// context by guessing or sniffing its runId. Phase 3 per-user keying is the
// OS-level boundary. Identity is NEVER taken from the child message body.
import type { ParentLink } from "./protocol.js";
import type { IpcMessage, GateResult, ExecuteResult, DelegateResult, SaveWorkflowResult, RunWorkflowResult, ListWorkflowsResult, PublishWorkflowResult, ProposeWorkflowResult, AnalyzeImageResult, ScheduleWorkflowResult, ScheduleRecurrence, GetSkillBodyResult, GetSkillFileResult, SpawnSubagentsResult, SpawnSubagentsBranch, SpawnSubagentsBranchResult } from "./protocol.js";
import type { Approver, Identity } from "../broker/governance.js";
import type { SubagentEventSink } from "../subagent/run.js";

// okFalseError is the shared failure-reply shape for the 7 ops whose result is
// a plain {ok, error} pair on failure. Only gate (allow/reason) and execute
// (output:null) need a bespoke failureReply — see the registrations below.
function okFalseError(error: string): { ok: false; error: string } {
  return { ok: false, error };
}

// RunIdentity is the per-run identity the parent binds when calling setRunContext.
// It is the same shape as Identity — re-exported here so call sites import one
// type rather than two modules.
export type RunIdentity = Identity;

// The minimal bridge surface this server needs — matches the methods on
// GovernanceBridge that the child may invoke. Defined as an interface so tests
// can inject a spy without importing the full GovernanceBridge class.
export interface BridgeLike {
  gate(
    toolCallId: string,
    toolName: string,
    input: Record<string, unknown>,
    opts?: { readOnlyHint?: boolean },
  ): Promise<{ allow: boolean; reason?: string }>;

  execute(
    toolCallId: string,
  ): Promise<{ ok: boolean; output: unknown; error?: string }>;

  delegate(
    to: string,
    intent: string,
    scopes?: string[],
  ): Promise<{ ok: boolean; envelopeId?: string; error?: string }>;

  saveWorkflow(
    def: Record<string, unknown>,
  ): Promise<{ ok: boolean; workflowId?: string; lineageId?: string; version?: number; error?: string }>;

  runWorkflow(
    lineageId: string,
    inputs: Record<string, string>,
  ): Promise<{ ok: boolean; result?: unknown; error?: string }>;

  listWorkflows(): Promise<{ ok: boolean; items?: unknown[]; error?: string }>;

  publishWorkflow(
    lineageId: string,
    groupIds: string[],
    version?: number,
  ): Promise<{ ok: boolean; visibilityKind?: string; groups?: string[]; error?: string }>;

  proposeWorkflow(
    lineageId: string,
    def: Record<string, unknown>,
  ): Promise<{ ok: boolean; version?: number; error?: string }>;

  analyzeImage(
    path: string,
    prompt?: string,
  ): Promise<{ ok: boolean; text?: string; error?: string }>;

  scheduleWorkflow(
    lineageId: string,
    inputs: Record<string, string>,
    recurrence: ScheduleRecurrence,
  ): Promise<{ ok: boolean; scheduleId?: string; missingInputs?: string[]; error?: string }>;

  // Personal-skills CP4: on-demand body fetch (bare directory name). Optional
  // so a bridge fake predating this feature (workflow/analyze-image tests)
  // still satisfies BridgeLike unchanged.
  getSkillBody?(
    name: string,
  ): Promise<{ ok: boolean; body?: string; allowedTools?: string[]; filePaths?: string[]; error?: string }>;

  // Skill-full-tree CP3: on-demand single-file read backing read_skill_file.
  // ref is a bundle UUID or a "personal:<name>"-qualified id. contentB64 is
  // base64-encoded (JSON-IPC-safe — see GetSkillFileResult). Optional so a
  // bridge fake predating this feature still satisfies BridgeLike unchanged.
  getSkillFile?(
    ref: string,
    path: string,
  ): Promise<{ ok: boolean; contentB64?: string; error?: string }>;

  // spawn_subagents: gate-then-bridge-direct
  // like analyzeImage. Optional so a bridge fake predating this feature still
  // satisfies BridgeLike unchanged (same precedent as getSkillBody/getSkillFile
  // above).
  spawnSubagents?(
    branches: SpawnSubagentsBranch[],
    aggregatorInstruction: string,
  ): Promise<{ ok: boolean; branches?: SpawnSubagentsBranchResult[]; synthesis?: string; error?: string }>;

  // Per-run mutable state — the approver and OIDC bearer change each run while
  // the rest of the identity (tenant/user/agent) stays fixed for the session.
  // Optional so that test stubs that don't need per-run state can omit them.
  setToken?(token?: string): void;
  setApprover?(approver: Approver): void;
}

// Factory type injected via constructor so tests can supply spy bridges without
// importing GovernanceBridge.
//
// consumeLlmBudget books one parent-side LLM call (reason/analyzeImage) against
// the child's per-run egress budget, returning false when it is spent. The
// supervisor supplies it per child so bridge-direct calls and proxied child
// calls share one counter. Optional: a bridge built with no child behind it
// (scheduler runViaWorkflow) has no budget to share.
//
// runId attributes the bridge's own parent-side LLM calls (reason/vision) to the
// run that caused them. Deliberately NOT
// folded into RunIdentity.runId — that field doubles as CreateGatewayTask's
// idempotency key and is set on the scheduler path only.
//
// sessionId attributes those same calls to the chat session, so the webui's
// per-session usage strip counts them. Separate from runId because one session
// spans many runs.
// onSubagentEvent: notified once per
// subagent branch at spawn and once at resolution, for the chat timeline.
// Added as a trailing optional param exactly as consumeLlmBudget/runId/
// sessionId arrived — every existing factory keeps compiling unchanged.
export type BridgeFactory = (
  identity: RunIdentity,
  approver: Approver,
  consumeLlmBudget?: () => boolean,
  runId?: string,
  sessionId?: string,
  onSubagentEvent?: SubagentEventSink,
) => BridgeLike;

export class RemoteBridgeServer {
  // Per-run bridges: keyed by runId. Built lazily when setRunContext is called,
  // torn down when clearRunContext is called or dispose() fires.
  private readonly runBridges = new Map<string, BridgeLike>();

  // pending tracks in-flight requests (by seq) so dispose() can reject them.
  private readonly pending = new Map<number, (err: Error) => void>();

  private disposed = false;

  constructor(
    private readonly link: ParentLink,
    private readonly makeBridge: BridgeFactory,
    // The session plan's allowedToolNames, stored at fork time (supervisor.ts's
    // forkChild). Checked by dispatch for EVERY op that corresponds to a Pi tool
    // — see toolAllowed below for the fail-open rationale.
    private readonly allowedToolNames: readonly string[] = [],
  ) {
    // Per-op table: each registration supplies (kind, field extraction,
    // bridge-method call, success/error reply shape) to the single generic
    // dispatch() method below, which owns the shared bridge-lookup / fail-closed
    // / withPending / reply-send logic that used to be duplicated 9 times.
    this.link.on("gate", (msg) => {
      const { seq, runId, toolCallId, toolName, input, opts } = msg;
      this.dispatch<GateResult>(seq, runId, "gate-result",
        (bridge) => bridge.gate(toolCallId, toolName, input, opts),
        (reason) => ({ allow: false, reason }),
        toolName,
      );
    });
    this.link.on("execute", (msg) => {
      const { seq, runId, toolCallId } = msg;
      this.dispatch<ExecuteResult>(seq, runId, "execute-result",
        (bridge) => bridge.execute(toolCallId),
        (error) => ({ ok: false, output: null, error }),
      );
    });
    this.link.on("delegate", (msg) => {
      const { seq, runId, to, intent, scopes } = msg;
      this.dispatch<DelegateResult>(seq, runId, "delegate-result",
        (bridge) => bridge.delegate(to, intent, scopes),
        okFalseError,
        "delegate",
      );
    });
    this.link.on("save-workflow", (msg) => {
      const { seq, runId, def } = msg;
      this.dispatch<SaveWorkflowResult>(seq, runId, "save-workflow-result",
        (bridge) => bridge.saveWorkflow(def),
        okFalseError,
        "workflow_save",
      );
    });
    this.link.on("run-workflow", (msg) => {
      const { seq, runId, lineageId, inputs } = msg;
      this.dispatch<RunWorkflowResult>(seq, runId, "run-workflow-result",
        (bridge) => bridge.runWorkflow(lineageId, inputs),
        okFalseError,
        "workflow_run",
      );
    });
    this.link.on("list-workflows", (msg) => {
      const { seq, runId } = msg;
      this.dispatch<ListWorkflowsResult>(seq, runId, "list-workflows-result",
        (bridge) => bridge.listWorkflows(),
        okFalseError,
        "workflow_list",
      );
    });
    this.link.on("publish-workflow", (msg) => {
      const { seq, runId, lineageId, groupIds, version } = msg;
      this.dispatch<PublishWorkflowResult>(seq, runId, "publish-workflow-result",
        (bridge) => bridge.publishWorkflow(lineageId, groupIds, version),
        okFalseError,
        "workflow_publish",
      );
    });
    this.link.on("propose-workflow", (msg) => {
      const { seq, runId, lineageId, def } = msg;
      this.dispatch<ProposeWorkflowResult>(seq, runId, "propose-workflow-result",
        (bridge) => bridge.proposeWorkflow(lineageId, def),
        okFalseError,
        "workflow_propose",
      );
    });
    this.link.on("analyze-image", (msg) => {
      const { seq, runId, path, prompt } = msg;
      this.dispatch<AnalyzeImageResult>(seq, runId, "analyze-image-result",
        (bridge) => bridge.analyzeImage(path, prompt),
        okFalseError,
        // Carries no toolName on the wire, and analyzeImage verifies no prior
        // gate decision: it reads a workspace file under the user's own bearer
        // and then makes a vision call with a child-chosen prompt. Without this
        // literal a child holding neither skill:vision nor a workspace-read
        // grant has an FGA-free read primitive plus an unbounded spend channel.
        "analyze_image",
      );
    });
    this.link.on("schedule-workflow", (msg) => {
      const { seq, runId, lineageId, inputs, recurrence } = msg;
      this.dispatch<ScheduleWorkflowResult>(seq, runId, "schedule-workflow-result",
        (bridge) => bridge.scheduleWorkflow(lineageId, inputs, recurrence),
        okFalseError,
        "workflow_schedule",
      );
    });
    this.link.on("get-skill-body", (msg) => {
      const { seq, runId, name } = msg;
      // No toolName literal: load_skill / read_skill_file live outside allowedToolNames' namespace
      // (session-plan.ts filters only makeTools/mcpTools), so a literal here would deny every call.
      this.dispatch<GetSkillBodyResult>(seq, runId, "get-skill-body-result",
        (bridge) => bridge.getSkillBody
          ? bridge.getSkillBody(name)
          : Promise.resolve(okFalseError("getSkillBody not supported by this bridge")),
        okFalseError,
      );
    });
    this.link.on("get-skill-file", (msg) => {
      const { seq, runId, ref, path } = msg;
      this.dispatch<GetSkillFileResult>(seq, runId, "get-skill-file-result",
        (bridge) => bridge.getSkillFile
          ? bridge.getSkillFile(ref, path)
          : Promise.resolve(okFalseError("getSkillFile not supported by this bridge")),
        okFalseError,
      );
    });
    this.link.on("spawn-subagents", (msg) => {
      const { seq, runId, branches, aggregatorInstruction } = msg;
      this.dispatch<SpawnSubagentsResult>(seq, runId, "spawn-subagents-result",
        (bridge) => bridge.spawnSubagents
          ? bridge.spawnSubagents(branches, aggregatorInstruction)
          : Promise.resolve(okFalseError("spawnSubagents not supported by this bridge")),
        okFalseError,
        // This op carries no toolName on the wire, but IS the spawn_subagents
        // tool executing bridge-direct. Naming it here is what stops a child
        // from skipping the gate round-trip to evade the membership check.
        "spawn_subagents",
      );
    });
  }

  // setRunContext binds a GovernanceBridge for the given runId. Called by the
  // parent (supervisor via ChildHandle) before dispatching the prompt so the
  // child's gate/execute/delegate requests can be serviced with the correct
  // identity and approval surface for THIS run's HTTP connection.
  setRunContext(
    runId: string,
    identity: RunIdentity,
    approver: Approver,
    sessionId?: string,
    onSubagentEvent?: SubagentEventSink,
  ): void {
    const bridge = this.makeBridge(identity, approver, undefined, runId, sessionId, onSubagentEvent);
    this.runBridges.set(runId, bridge);
  }

  // clearRunContext removes the per-run bridge when the run completes or the
  // connection closes. The bridge GC's; any pending IPC requests for this runId
  // will fail closed (reply not-found path in handlers below).
  clearRunContext(runId: string): void {
    this.runBridges.delete(runId);
  }

  // dispose() is called when the child is evicted or has crashed. It rejects
  // any in-flight gate/execute/delegate promises immediately so callers get a
  // clean error instead of waiting for the 30 s IPC timeout.
  dispose(): void {
    this.disposed = true;
    const err = new Error("child evicted: IPC bridge closed");
    for (const reject of this.pending.values()) {
      reject(err);
    }
    this.pending.clear();
    this.runBridges.clear();
  }

  // toolAllowed is the IPC-boundary membership backstop. Omitting a tool from the schema handed to the model
  // constrains a well-behaved child only; a compromised one can send whatever
  // IPC message it likes. Nothing downstream catches it either — a subagent
  // branch runs under the caller's own real identity, so CheckFGA(user,
  // can_invoke, skill:subagents) passes at depth 1 exactly as at depth 0, and
  // unbounded recursive fan-out follows.
  //
  // FAIL-OPEN on an empty set, deliberately: this is defence-in-depth against a
  // compromised child, NOT the primary authorization gate — that remains
  // FGA/OPA/Biscuit at the broker, which still runs for every call that gets
  // through here. Sessions that legitimately build no full plan (test harnesses,
  // any spawn path constructing a bridge server without one) must keep working;
  // failing closed on an absent set would break them for no security gain.
  private toolAllowed(toolName: string): boolean {
    if (this.allowedToolNames.length === 0) return true;
    return this.allowedToolNames.includes(toolName);
  }

  // withPending wraps a bridge call in a cancellation race tracked by seq.
  // If dispose() fires before the bridge resolves, the pending reject wins and
  // the result is discarded (the child is gone; no reply can reach it anyway).
  private withPending<T>(seq: number, work: Promise<T>): Promise<T> {
    return new Promise<T>((resolve, reject) => {
      this.pending.set(seq, reject);
      work.then(
        (v) => {
          this.pending.delete(seq);
          resolve(v);
        },
        (err: unknown) => {
          this.pending.delete(seq);
          reject(err);
        },
      );
    });
  }

  // dispatch is the single generic mechanism collapsing the former 9 handle*
  // methods: bridge lookup (fail-closed on missing run context), withPending
  // race tracking, and success/error reply construction. Each registration in
  // the constructor above supplies the per-op row (replyKind, bridge-method
  // call over the fields it destructured from the request, failure shape).
  //
  // SECURITY: call() only ever receives the bound bridge — never the raw msg —
  // so a bridge-method invocation can only use the fields the caller's closure
  // explicitly destructured above. This preserves the "no identity field is
  // ever forwarded" invariant exactly as the former per-op methods did.
  private dispatch<R extends IpcMessage>(
    seq: number,
    runId: string,
    replyKind: R["kind"],
    call: (bridge: BridgeLike) => Promise<Omit<R, "kind" | "seq">>,
    failureReply: (message: string) => Omit<R, "kind" | "seq">,
    toolName?: string,
  ): void {
    if (this.disposed) return;

    if (toolName !== undefined && !this.toolAllowed(toolName)) {
      this.link.send({
        kind: replyKind,
        seq,
        ...failureReply(`tool "${toolName}" is not in this session's tool set`),
      });
      return;
    }

    const bridge = this.runBridges.get(runId);
    if (!bridge) {
      // No run context for this runId — fail closed. This prevents a child from
      // sending requests with a fabricated runId to reach another run's bridge.
      this.link.send({ kind: replyKind, seq, ...failureReply(`no run context for runId=${runId}`) });
      return;
    }

    this.withPending(seq, call(bridge)).then(
      (result) => {
        this.link.send({ kind: replyKind, seq, ...result });
      },
      (err: unknown) => {
        if (this.disposed) return; // child gone; nothing to reply to
        this.link.send({ kind: replyKind, seq, ...failureReply(String(err)) });
      },
    );
  }
}
