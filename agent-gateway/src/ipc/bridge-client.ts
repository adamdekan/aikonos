// RemoteBridgeClient — child-side IPC bridge client (CP3).
//
// WHY this exists: the Pi loop runs in a forked child process that holds no
// long-lived secrets. Every governed action (gate/execute/delegate) must go
// through the parent. This class sends IPC requests over a ChildLink and
// awaits the parent's *-result replies, acting as a structural drop-in for
// GovernanceBridge so src/pi/tools.ts needs no change.
//
// The parent binds identity per-run via setRunContext on the RemoteBridgeServer —
// none of gate/execute/delegate carries any identity field. That invariant is
// upheld here: we only forward the tool-call fields plus the active runId.
//
// runId threading: the child tags every request with the active runId so the
// parent's bridge-server can look up the per-run bridge. The active runId is
// set via withRun(runId, fn) for the duration of each prompt handling.
import { makeSeq, type ChildLink, type IpcMessage } from "./protocol.js";
import type {
  GateResult,
  ExecuteResult,
  DelegateResult,
  SaveWorkflowResult,
  RunWorkflowResult,
  ListWorkflowsResult,
  PublishWorkflowResult,
  ProposeWorkflowResult,
  AnalyzeImageResult,
  ScheduleWorkflowResult,
  ScheduleRecurrence,
  GetSkillBodyResult,
  GetSkillFileResult,
  SpawnSubagentsResult,
  SpawnSubagentsBranch,
  SpawnSubagentsBranchResult,
} from "./protocol.js";
import type { Approver } from "../broker/governance.js";

// The structural surface GovernanceBridge exposes publicly. Satisfying this
// interface makes RemoteBridgeClient a drop-in for makeTools(bridge) without
// modifying tools.ts (which is typed against GovernanceBridge directly).
export interface BridgeClientLike {
  gate(
    toolCallId: string,
    toolName: string,
    input: Record<string, unknown>,
    opts?: { readOnlyHint?: boolean },
  ): Promise<{ allow: boolean; reason?: string }>;
  execute(toolCallId: string): Promise<{ ok: boolean; output: unknown; error?: string }>;
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
  reason(
    instruction: string,
    outputSchema?: Record<string, unknown>,
  ): Promise<{ ok: boolean; output?: unknown; error?: string }>;
  // Personal-skills CP4: on-demand body fetch for a personal skill's
  // load_skill activation (bare directory name, no "personal:" prefix).
  // Optional — a bridge fake built before this feature existed satisfies the
  // structural type with no changes; makeLoadSkillTool's fetcher is itself
  // optional and simply cannot activate a personal entry without it.
  getSkillBody?(
    name: string,
  ): Promise<{ ok: boolean; body?: string; allowedTools?: string[]; filePaths?: string[]; error?: string }>;
  // Skill-full-tree CP3: on-demand single-file read backing read_skill_file.
  // contentB64 is base64-encoded (JSON-IPC-safe — see GetSkillFileResult).
  // Optional — a bridge fake built before this feature existed satisfies the
  // structural type with no changes; makeReadSkillFileTool's fetcher is itself
  // optional and simply cannot read a file without it.
  getSkillFile?(
    ref: string,
    path: string,
  ): Promise<{ ok: boolean; contentB64?: string; error?: string }>;
  // spawn_subagents: gate-then-bridge-direct
  // like analyzeImage — the model's tool_call is JIT-plan-gated (CheckFGA
  // skill:subagents) via gate(), then executed directly here, bypassing
  // InvokeTool (spawn_subagents has no Tool Proxy registration; every branch
  // tool call is separately gated on its own). Optional — mirrors
  // getSkillBody/getSkillFile: a bridge fake predating this feature still
  // satisfies BridgeClientLike unchanged.
  spawnSubagents?(
    branches: SpawnSubagentsBranch[],
    aggregatorInstruction: string,
  ): Promise<{ ok: boolean; branches?: SpawnSubagentsBranchResult[]; synthesis?: string; error?: string }>;
  // Parent-side concerns — the child stubs these so the structural type is satisfied.
  setApprover(a: Approver): void;
  setToken(token?: string): void;
  usageIdentity(): { tenantId: string; userId: string; agentId: string };
}

const REQUEST_TIMEOUT_MS = 120_000;

/** Child-side IPC drop-in for GovernanceBridge: forwards gate/execute/delegate requests to the parent over a ChildLink, tagging each with the active runId so the parent routes to the correct per-run bridge. */
export class RemoteBridgeClient implements BridgeClientLike {
  private readonly seq = makeSeq();
  // The runId for the currently executing prompt. Set by withRun() in
  // child-entry.ts before any tool call can fire.
  private activeRunId: string | undefined;

  constructor(private readonly link: ChildLink) {}

  // withRun sets the active runId for the duration of fn(). All gate/execute/
  // delegate calls within fn() will tag their IPC requests with this runId so
  // the parent bridge-server routes them to the correct per-run bridge.
  async withRun<T>(runId: string, fn: () => Promise<T>): Promise<T> {
    this.activeRunId = runId;
    try {
      return await fn();
    } finally {
      this.activeRunId = undefined;
    }
  }

  private requireRunId(): string {
    if (!this.activeRunId) {
      throw new Error("RemoteBridgeClient: no active runId — gate/execute/delegate called outside withRun()");
    }
    return this.activeRunId;
  }

  // recv is the single generic mechanism collapsing the former 9 near-identical
  // methods' shared tail: it awaits the correlated reply via ChildLink.request
  // (the timeout constant lives here once) and strips the wire envelope
  // (kind/seq) back down to the plain result shape each method returns to its
  // caller. Each method below builds its own request object (kind + fields +
  // the active runId + a fresh seq — the per-op "table row"), then hands it to
  // recv with the matching replyKind.
  private async recv<R extends IpcMessage & { seq: number }>(
    msg: IpcMessage & { seq: number },
    replyKind: R["kind"],
  ): Promise<Omit<R, "kind" | "seq">> {
    const result = await this.link.request<R>(msg, replyKind, REQUEST_TIMEOUT_MS);
    const { kind: _k, seq: _s, ...rest } = result;
    return rest;
  }

  async gate(
    toolCallId: string,
    toolName: string,
    input: Record<string, unknown>,
    opts?: { readOnlyHint?: boolean },
  ): Promise<{ allow: boolean; reason?: string }> {
    const runId = this.requireRunId();
    return this.recv<GateResult>(
      { kind: "gate", seq: this.seq(), runId, toolCallId, toolName, input, opts },
      "gate-result",
    );
  }

  async execute(toolCallId: string): Promise<{ ok: boolean; output: unknown; error?: string }> {
    const runId = this.requireRunId();
    return this.recv<ExecuteResult>(
      { kind: "execute", seq: this.seq(), runId, toolCallId },
      "execute-result",
    );
  }

  async delegate(
    to: string,
    intent: string,
    scopes?: string[],
  ): Promise<{ ok: boolean; envelopeId?: string; error?: string }> {
    const runId = this.requireRunId();
    return this.recv<DelegateResult>(
      { kind: "delegate", seq: this.seq(), runId, to, intent, scopes },
      "delegate-result",
    );
  }

  async saveWorkflow(
    def: Record<string, unknown>,
  ): Promise<{ ok: boolean; workflowId?: string; lineageId?: string; version?: number; error?: string }> {
    const runId = this.requireRunId();
    return this.recv<SaveWorkflowResult>(
      { kind: "save-workflow", seq: this.seq(), runId, def },
      "save-workflow-result",
    );
  }

  async runWorkflow(
    lineageId: string,
    inputs: Record<string, string>,
  ): Promise<{ ok: boolean; result?: unknown; error?: string }> {
    const runId = this.requireRunId();
    return this.recv<RunWorkflowResult>(
      { kind: "run-workflow", seq: this.seq(), runId, lineageId, inputs },
      "run-workflow-result",
    );
  }

  async listWorkflows(): Promise<{ ok: boolean; items?: unknown[]; error?: string }> {
    const runId = this.requireRunId();
    return this.recv<ListWorkflowsResult>(
      { kind: "list-workflows", seq: this.seq(), runId },
      "list-workflows-result",
    );
  }

  async publishWorkflow(
    lineageId: string,
    groupIds: string[],
    version = 0,
  ): Promise<{ ok: boolean; visibilityKind?: string; groups?: string[]; error?: string }> {
    const runId = this.requireRunId();
    return this.recv<PublishWorkflowResult>(
      { kind: "publish-workflow", seq: this.seq(), runId, lineageId, groupIds, version },
      "publish-workflow-result",
    );
  }

  async proposeWorkflow(
    lineageId: string,
    def: Record<string, unknown>,
  ): Promise<{ ok: boolean; version?: number; error?: string }> {
    const runId = this.requireRunId();
    return this.recv<ProposeWorkflowResult>(
      { kind: "propose-workflow", seq: this.seq(), runId, lineageId, def },
      "propose-workflow-result",
    );
  }

  async analyzeImage(
    path: string,
    prompt?: string,
  ): Promise<{ ok: boolean; text?: string; error?: string }> {
    const runId = this.requireRunId();
    return this.recv<AnalyzeImageResult>(
      { kind: "analyze-image", seq: this.seq(), runId, path, prompt },
      "analyze-image-result",
    );
  }

  async scheduleWorkflow(
    lineageId: string,
    inputs: Record<string, string>,
    recurrence: ScheduleRecurrence,
  ): Promise<{ ok: boolean; scheduleId?: string; missingInputs?: string[]; error?: string }> {
    const runId = this.requireRunId();
    return this.recv<ScheduleWorkflowResult>(
      { kind: "schedule-workflow", seq: this.seq(), runId, lineageId, inputs, recurrence },
      "schedule-workflow-result",
    );
  }

  async getSkillBody(
    name: string,
  ): Promise<{ ok: boolean; body?: string; allowedTools?: string[]; filePaths?: string[]; error?: string }> {
    const runId = this.requireRunId();
    return this.recv<GetSkillBodyResult>(
      { kind: "get-skill-body", seq: this.seq(), runId, name },
      "get-skill-body-result",
    );
  }

  async getSkillFile(
    ref: string,
    path: string,
  ): Promise<{ ok: boolean; contentB64?: string; error?: string }> {
    const runId = this.requireRunId();
    return this.recv<GetSkillFileResult>(
      { kind: "get-skill-file", seq: this.seq(), runId, ref, path },
      "get-skill-file-result",
    );
  }

  async spawnSubagents(
    branches: SpawnSubagentsBranch[],
    aggregatorInstruction: string,
  ): Promise<{ ok: boolean; branches?: SpawnSubagentsBranchResult[]; synthesis?: string; error?: string }> {
    const runId = this.requireRunId();
    return this.recv<SpawnSubagentsResult>(
      { kind: "spawn-subagents", seq: this.seq(), runId, branches, aggregatorInstruction },
      "spawn-subagents-result",
    );
  }

  // reason steps execute exclusively inside the parent's runWorkflow driver
  // (runWorkflowDriver is always invoked with the parent GovernanceBridge as
  // `this` — see governance.ts:runWorkflow). The child's RemoteBridgeClient
  // only ever forwards the single "run-workflow" IPC call; it never drives
  // individual steps, so this method is unreachable in practice. It exists
  // solely to satisfy the BridgeClientLike structural type.
  async reason(
    _instruction: string,
    _outputSchema?: Record<string, unknown>,
  ): Promise<{ ok: boolean; output?: unknown; error?: string }> {
    return { ok: false, error: "reason steps execute parent-side only" };
  }

  // PHASE-4 residual: these are parent-side concerns only. The child calls them
  // never — they exist solely to satisfy the GovernanceBridge structural type so
  // makeTools(client) type-checks without modifying tools.ts.
  setApprover(_a: Approver): void { /* no-op: parent owns the approver */ }
  setToken(_token?: string): void { /* no-op: parent owns the token */ }
  usageIdentity(): { tenantId: string; userId: string; agentId: string } {
    // The child has no identity context. Usage is relayed via UsageEvent IPC
    // (child-entry.ts), not via a south call from the child.
    return { tenantId: "", userId: "", agentId: "" };
  }
}
