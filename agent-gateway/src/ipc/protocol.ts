// IPC protocol module — discriminated-union message types + typed transport links.
//
// WHY this module exists: the IPC seam is the security boundary between the
// trusted parent (credential-holder: bridge, broker clients, OIDC bearers,
// provider keys) and the untrusted child (Pi loop). A typed codec here ensures
// the compiler rejects accidental identity fields in messages at the point they
// would be authored, not at runtime in the child.
//
// Identity (owner/grant/bearer/tenant/agentId) is NEVER part of any message.
// The parent binds identity from the child's spawn record. This is the
// load-bearing security property of the Phase-2 architecture.
import { EventEmitter } from "node:events";

// ── Message types ──────────────────────────────────────────────────────────────

// child→parent requests (each carries a monotonic seq for reply correlation)

export interface GateRequest {
  kind: "gate";
  seq: number;
  runId: string;
  toolCallId: string;
  toolName: string;
  input: Record<string, unknown>;
  opts?: { readOnlyHint?: boolean };
}

export interface ExecuteRequest {
  kind: "execute";
  seq: number;
  runId: string;
  toolCallId: string;
}

export interface DelegateRequest {
  kind: "delegate";
  seq: number;
  runId: string;
  to: string;
  intent: string;
  scopes?: string[];
}

export interface SaveWorkflowRequest {
  kind: "save-workflow";
  seq: number;
  runId: string;
  def: Record<string, unknown>;
}

export interface RunWorkflowRequest {
  kind: "run-workflow";
  seq: number;
  runId: string;
  lineageId: string;
  inputs: Record<string, string>;
}

export interface ListWorkflowsRequest {
  kind: "list-workflows";
  seq: number;
  runId: string;
}

export interface PublishWorkflowRequest {
  kind: "publish-workflow";
  seq: number;
  runId: string;
  lineageId: string;
  groupIds: string[];
  version: number;
}

export interface ProposeWorkflowRequest {
  kind: "propose-workflow";
  seq: number;
  runId: string;
  lineageId: string;
  def: Record<string, unknown>;
}

export interface AnalyzeImageRequest {
  kind: "analyze-image";
  seq: number;
  runId: string;
  path: string;
  prompt?: string;
}

export interface ScheduleRecurrence {
  kind: "cron" | "once";
  cronExpr?: string;
  runAt?: string; // ISO 8601, future
}

export interface ScheduleWorkflowRequest {
  kind: "schedule-workflow";
  seq: number;
  runId: string;
  lineageId: string;
  inputs: Record<string, string>;
  recurrence: ScheduleRecurrence;
}

// GetSkillBodyRequest: the child's
// load_skill execute() has no south access (no long-lived secrets in the
// child), so an on-demand personal-skill body fetch — unlike a bundle body,
// which already travels in the plan — must round-trip through the parent's
// GetPersonalSkillSouth call. name is the bare directory name (the
// "personal:" activation-key prefix is stripped by the caller before this
// request is built).
export interface GetSkillBodyRequest {
  kind: "get-skill-body";
  seq: number;
  runId: string;
  name: string;
}

// GetSkillFileRequest: the child's
// read_skill_file tool has no south access (same reason as get-skill-body), so
// reading one file's content round-trips through the parent. ref is either a
// bundle UUID or a "personal:<name>"-qualified id — the parent's
// GovernanceBridge.getSkillFile switches on the prefix.
export interface GetSkillFileRequest {
  kind: "get-skill-file";
  seq: number;
  runId: string;
  ref: string;
  path: string;
}

// SpawnSubagentsRequest/-Result: a
// gate-then-bridge-direct call — see gating-manifest.ts's spawn_subagents
// entry. The wire branch shape mirrors src/subagent/run.ts's SubagentBranch/
// BranchResult structurally (not imported — this module stays a
// self-contained wire-shape layer with no dependency on the runner, and the
// runner already imports FROM this module for its own event types).
export interface SpawnSubagentsBranch {
  task: string;
  role?: string;
}

export interface SpawnSubagentsBranchResult {
  index: number;
  task: string;
  role?: string;
  ok: boolean;
  output: string;
  error?: string;
  failure?: "error" | "timeout" | "denied" | "systemic";
  deniedTools?: string[];
}

export interface SpawnSubagentsRequest {
  kind: "spawn-subagents";
  seq: number;
  runId: string;
  branches: SpawnSubagentsBranch[];
  aggregatorInstruction: string;
}

// parent→child replies (each echoes the request seq)

export interface GateResult {
  kind: "gate-result";
  seq: number;
  allow: boolean;
  reason?: string;
}

export interface ExecuteResult {
  kind: "execute-result";
  seq: number;
  ok: boolean;
  output: unknown;
  error?: string;
}

export interface DelegateResult {
  kind: "delegate-result";
  seq: number;
  ok: boolean;
  envelopeId?: string;
  error?: string;
}

export interface SaveWorkflowResult {
  kind: "save-workflow-result";
  seq: number;
  ok: boolean;
  workflowId?: string;
  lineageId?: string;
  version?: number;
  error?: string;
}

export interface RunWorkflowResult {
  kind: "run-workflow-result";
  seq: number;
  ok: boolean;
  result?: unknown;
  error?: string;
}

export interface ListWorkflowsResult {
  kind: "list-workflows-result";
  seq: number;
  ok: boolean;
  items?: unknown[];
  error?: string;
}

export interface PublishWorkflowResult {
  kind: "publish-workflow-result";
  seq: number;
  ok: boolean;
  visibilityKind?: string;
  groups?: string[];
  error?: string;
}

export interface ProposeWorkflowResult {
  kind: "propose-workflow-result";
  seq: number;
  ok: boolean;
  version?: number;
  error?: string;
}

export interface AnalyzeImageResult {
  kind: "analyze-image-result";
  seq: number;
  ok: boolean;
  text?: string;
  error?: string;
}

export interface ScheduleWorkflowResult {
  kind: "schedule-workflow-result";
  seq: number;
  ok: boolean;
  scheduleId?: string;
  // Names of the workflow's required (no-default) inputs missing from the
  // supplied values. Non-empty ⇒ the create still succeeded (never a hard
  // failure) — the tool result surfaces this as a warning.
  missingInputs?: string[];
  error?: string;
}

export interface GetSkillBodyResult {
  kind: "get-skill-body-result";
  seq: number;
  ok: boolean;
  body?: string;
  allowedTools?: string[];
  // full-tree file paths for this personal skill.
  // Optional so a parent predating this feature still round-trips.
  filePaths?: string[];
  error?: string;
}

// GetSkillFileResult — contentB64 is base64-encoded raw bytes (a skill file
// may be binary), never a raw Uint8Array. WHY: the real child is forked with
// `serialization: "json"` (supervisor.ts's defaultSpawnChild), which flattens
// a Uint8Array into a plain numeric-keyed object — silently corrupting the
// content (the fake in-memory test channel doesn't serialize, so this bug
// hid behind green tests until the base64 wire shape closed it). Encoding to
// a plain string at the source (GovernanceBridge.getSkillFile) makes the wire
// shape JSON-safe by construction. The UTF-8/size content gate runs in
// read-skill-file.ts on the decoded bytes, not here — the same gate must
// apply whether this travels over real child IPC or the in-process legacy
// bridge path.
export interface GetSkillFileResult {
  kind: "get-skill-file-result";
  seq: number;
  ok: boolean;
  contentB64?: string;
  error?: string;
}

export interface SpawnSubagentsResult {
  kind: "spawn-subagents-result";
  seq: number;
  ok: boolean;
  branches?: SpawnSubagentsBranchResult[];
  synthesis?: string;
  error?: string;
}

// parent→child directives

// A single conversation turn carried from client → gateway → child.
// WHY history on PromptMessage and not elsewhere: conversation turns are CONTENT,
// not identity. The load-bearing "no identity field in any message" rule still
// holds — no owner/grant/bearer/tenant/agentId is added here. The child uses
// history only to seed a fresh SessionManager on lazy-create; it is never used
// to derive who the child acts as.
export interface ConvMessage {
  role: "user" | "assistant";
  content: string;
}

export interface PromptMessage {
  kind: "prompt";
  runId: string;
  threadId: string;
  text: string;
  // Prior conversation turns sent by the client on resume. Present only when the
  // client has prior context to seed; absent on the very first message of a session.
  // The child passes this to the SessionFactory only in the lazy-create branch —
  // never when an existing thread session is being reused.
  history?: ConvMessage[];
  // CP8: when the user submitted a /command, the gateway resolves the skill name
  // server-side (FGA-checked) and sets this field so the child pre-activates the
  // bundle's allowlist before session.prompt() runs — same effect as the model
  // calling load_skill(name), but triggered by explicit user /command.
  activateSkillName?: string;
  // auto-skill-loading CP2: when the gateway's keyword matcher (skill-match.ts)
  // finds bundle(s) whose keywords appear in the user's message, it sets this
  // field so the child pre-activates every matched bundle's allowlist before
  // session.prompt() runs — same mechanism as activateSkillName, extended to a
  // union of bundles. Only set when body.skillName (/command) was NOT supplied —
  // explicit /command takes precedence over auto-matching for that turn.
  activateSkillNames?: string[];
  // Per-user chat instructions from the webui settings modal. CONTENT, not
  // identity (same rule as history) — no owner/grant/bearer travels here.
  // Applied only in the lazy-create branch: the thread's system prompt is
  // fixed at session creation, so a mid-thread change takes effect on the
  // next new chat (same freeze semantics as an agent soul edit).
  userInstructions?: string;
}

// AbortMessage asks the child to cancel a specific in-flight run identified by
// runId. The child calls session.abort() (if supported) and closes the run's
// EventQueue so the drain loop ends without emitting further events.
export interface AbortMessage {
  kind: "abort";
  runId: string;
}

export interface ShutdownMessage {
  kind: "shutdown";
}

export interface McpTool {
  name: string;
  schema: Record<string, unknown>;
  toolId: string;
}

// SkillBundleEntry is the secret-free wire shape for a granted skill bundle.
// No api-key, bearer, or tenant secret is permitted here — bodies are admin-
// authored SKILL.md content, not credentials.
export interface SkillBundleEntry {
  id: string;
  name: string;
  description: string;
  body: string;
  allowedTools: string[];
  contextFork: boolean;
  disableModelInvocation: boolean;
  // admin-editable auto-load match terms (auto-skill-loading); normalized
  // (lowercase/trim/dedup) server-side. Empty list = bundle never auto-loads.
  keywords: string[];
  // origin: additive optional field, wire-
  // compatible with every existing bundle entry (absent = ordinary admin-
  // authored bundle). Set to "personal" for the caller's own Skills/<name>/
  // entries unioned into this same list by resolveSessionPlan. name/id already
  // carry the "personal:" qualifier for these entries — origin is the explicit
  // semantic marker the catalog and system-prompt renderers key off of.
  origin?: "personal";
  // full-tree file paths (relative, sorted ascending), no content. Bundle entries get this from the south list RPC at
  // plan-build time; personal entries start [] (the list RPC is frontmatter-
  // only) and are refreshed at load_skill activation from the on-demand fetch.
  filePaths: string[];
}

// init carries the secret-free session plan from resolveSessionPlan (CP4).
// No api-key, bearer, or grant is permitted here.
export interface InitMessage {
  kind: "init";
  modelId: string;
  // The real provider id (e.g. "openai", "anthropic") resolved by the parent
  // at session-plan build time. Not a secret — the child registers its model
  // against the parent's egress proxy under a fixed local name regardless of
  // the true dialect, so the child's own AssistantMessage.provider field is
  // never the real provider id. Threaded here so the usage relay can carry
  // the real id for the broker's per-provider cost-fallback lookup. Optional
  // for backward/forward plan compatibility — an absent value relays "".
  providerId?: string;
  systemPrompt: string;
  allowedToolNames: string[];
  mcpTools: McpTool[];
  proxyBaseUrl: string;
  proxyModelAllowlist: string[];
  // CP7: granted skill bundles (FGA-checked at resolve time). Used by
  // createSessionFromPlan to register the load_skill built-in tool and inject
  // the catalog into the system prompt. Empty list when no bundles are granted.
  skillBundles?: SkillBundleEntry[];
}

// child→parent events (each carries the runId that ties it to its prompt)

export interface TextDeltaEvent {
  kind: "text_delta";
  runId: string;
  delta: string;
}

export interface ToolStartEvent {
  kind: "tool_start";
  runId: string;
  toolCallId: string;
  toolName: string;
  // The tool arguments passed to the tool call, forwarded for AG-UI toolCall frames.
  input: Record<string, unknown>;
  // Human-readable description of the tool (or, for load_skill, of the skill
  // bundle being activated). Resolved child-side from the registered tool
  // definitions; truncated before send. Absent when no description is known.
  description?: string;
}

export interface ToolEndEvent {
  kind: "tool_end";
  runId: string;
  toolCallId: string;
  ok: boolean;
  // The tool result content, forwarded for AG-UI toolResult frames.
  result: unknown;
}

export interface UsageEvent {
  kind: "usage";
  runId: string;
  inputTokens: number;
  outputTokens: number;
  // Additive metering fields (spend-caps CP3). Optional so a version-skewed
  // child (old build, new parent or vice versa) still round-trips a usage
  // event — the parent treats an absent field as 0/"" rather than failing.
  cost?: number;
  cacheRead?: number;
  cacheWrite?: number;
  provider?: string;
  model?: string;
}

// done terminates a run stream normally.
export interface DoneEvent {
  kind: "done";
  runId: string;
}

// error terminates a run stream with failure; the parent must not emit further
// events for this runId after receiving it.
export interface ErrorEvent {
  kind: "error";
  runId: string;
  message: string;
}

export type IpcMessage =
  | GateRequest
  | ExecuteRequest
  | DelegateRequest
  | SaveWorkflowRequest
  | RunWorkflowRequest
  | ListWorkflowsRequest
  | PublishWorkflowRequest
  | ProposeWorkflowRequest
  | AnalyzeImageRequest
  | ScheduleWorkflowRequest
  | GetSkillBodyRequest
  | GetSkillFileRequest
  | SpawnSubagentsRequest
  | GateResult
  | ExecuteResult
  | DelegateResult
  | SaveWorkflowResult
  | RunWorkflowResult
  | ListWorkflowsResult
  | PublishWorkflowResult
  | ProposeWorkflowResult
  | AnalyzeImageResult
  | ScheduleWorkflowResult
  | GetSkillBodyResult
  | GetSkillFileResult
  | SpawnSubagentsResult
  | PromptMessage
  | AbortMessage
  | ShutdownMessage
  | InitMessage
  | TextDeltaEvent
  | ToolStartEvent
  | ToolEndEvent
  | UsageEvent
  | DoneEvent
  | ErrorEvent;

// ── Monotonic seq generator ────────────────────────────────────────────────────

export function makeSeq(): () => number {
  let n = 0;
  return () => ++n;
}

// ── Channel abstraction ────────────────────────────────────────────────────────
//
// A Channel is the minimal surface the links need from their underlying
// transport. Both the real ChildProcess/process and the fake paired channel
// satisfy this shape.

export interface Channel {
  // Accepting unknown lets the test seam inject extra fields to verify the
  // server ignores them, without requiring a cast at the call site.
  send(msg: unknown): void;
  on(event: "message", handler: (msg: IpcMessage) => void): void;
}

// ── Fake in-memory paired channel (test seam) ──────────────────────────────────
//
// Returns [sideA, sideB]: a message sent on A arrives on B's "message" event
// and vice versa. No real process involved.

class FakeChannel implements Channel {
  private readonly emitter = new EventEmitter();
  private peer: FakeChannel | undefined;

  setPeer(other: FakeChannel): void {
    this.peer = other;
  }

  send(msg: unknown): void {
    // Deliver asynchronously (setImmediate) to match real IPC semantics.
    setImmediate(() => this.peer?.emitter.emit("message", msg));
  }

  on(event: "message", handler: (msg: IpcMessage) => void): void {
    this.emitter.on(event, handler);
  }
}

export function makePairedChannel(): [Channel, Channel] {
  const a = new FakeChannel();
  const b = new FakeChannel();
  a.setPeer(b);
  b.setPeer(a);
  return [a, b];
}

// ── Pending-request map ────────────────────────────────────────────────────────

interface PendingEntry<R extends IpcMessage> {
  resolve: (msg: R) => void;
  reject: (err: Error) => void;
  timer: ReturnType<typeof setTimeout>;
}

// ── Base link ─────────────────────────────────────────────────────────────────
//
// Shared logic for ParentLink and ChildLink: kind-based dispatch, request()
// correlation, and unknown-kind handling.

const DEFAULT_REQUEST_TIMEOUT_MS = 30_000;

class BaseLink {
  protected readonly channel: Channel;
  private readonly handlers = new Map<string, Array<(msg: IpcMessage) => void>>();
  // pending is keyed by `${kind}:${seq}` so concurrent requests for different
  // reply kinds don't collide (in practice each link only uses one reply kind
  // per request, but being precise here future-proofs against CP2+ extensions).
  private readonly pending = new Map<string, PendingEntry<IpcMessage>>();
  private unknownKindHandler: ((msg: unknown) => void) | undefined;

  constructor(channel: Channel) {
    this.channel = channel;
    channel.on("message", (msg) => this.dispatch(msg));
  }

  send(msg: unknown): void {
    this.channel.send(msg);
  }

  on<K extends IpcMessage["kind"]>(
    kind: K,
    handler: (msg: Extract<IpcMessage, { kind: K }>) => void,
  ): void {
    const list = this.handlers.get(kind) ?? [];
    list.push(handler as (msg: IpcMessage) => void);
    this.handlers.set(kind, list);
  }

  // Remove a previously registered handler for the given kind. No-op if the
  // handler was not registered. Mirrors the semantics of EventEmitter.off() so
  // callers can deregister run-scoped listeners after a run completes, preventing
  // the handler list from growing by O(N) per run on a long-lived shared child.
  off<K extends IpcMessage["kind"]>(
    kind: K,
    handler: (msg: Extract<IpcMessage, { kind: K }>) => void,
  ): void {
    const list = this.handlers.get(kind);
    if (!list) return;
    const idx = list.indexOf(handler as (msg: IpcMessage) => void);
    if (idx !== -1) list.splice(idx, 1);
  }

  // Register a handler for messages whose kind is not in the union. Called
  // instead of throwing so the process survives a malformed or version-skewed
  // child message.
  onUnknownKind(handler: (msg: unknown) => void): void {
    this.unknownKindHandler = handler;
  }

  // Send a request and await the reply whose kind === replyKind and seq matches.
  // replyTimeoutMs defaults to DEFAULT_REQUEST_TIMEOUT_MS.
  request<R extends IpcMessage>(
    msg: IpcMessage & { seq: number },
    replyKind: R["kind"],
    replyTimeoutMs = DEFAULT_REQUEST_TIMEOUT_MS,
  ): Promise<R> {
    return new Promise<R>((resolve, reject) => {
      const key = `${replyKind}:${msg.seq}`;

      const timer = setTimeout(() => {
        this.pending.delete(key);
        reject(new Error(`IPC timeout: no ${replyKind} reply for seq=${msg.seq}`));
      }, replyTimeoutMs);
      // Don't let this timer hold the event loop open.
      timer.unref?.();

      this.pending.set(key, {
        resolve: resolve as (m: IpcMessage) => void,
        reject,
        timer,
      });

      this.channel.send(msg);
    });
  }

  private dispatch(msg: IpcMessage): void {
    // Try to resolve a pending request first (reply messages carry seq).
    if ("seq" in msg) {
      const key = `${msg.kind}:${msg.seq}`;
      const entry = this.pending.get(key);
      if (entry) {
        clearTimeout(entry.timer);
        this.pending.delete(key);
        entry.resolve(msg);
        // Fall through — also fire any registered kind handlers (e.g. CP2
        // bridge-server also listens to gate-result for logging).
      }
    }

    const list = this.handlers.get(msg.kind);
    if (list) {
      for (const h of list) h(msg);
      return;
    }

    // Not a pending reply and no registered handler — unknown or unhandled kind.
    if (this.unknownKindHandler) {
      this.unknownKindHandler(msg);
    }
  }
}

// ── ParentLink ────────────────────────────────────────────────────────────────
//
// Wraps a Channel representing the parent side (wrapping a forked ChildProcess
// in production, or a FakeChannel in tests). The parent sends directives
// (prompt/shutdown/init/gate-result/…) and receives requests + events.

export class ParentLink extends BaseLink {
  constructor(channel: Channel) {
    super(channel);
  }

  // onExit registers a handler called when the underlying child process exits.
  // Production: the real fork wrapper overrides this on the returned instance.
  // Test fakes override it and call the handler from simulateExit().
  onExit(handler: (code: number | null) => void): void {
    // Default no-op: overridden by the spawn wrapper.
    void handler;
  }

  // offExit removes a previously registered exit handler (reference-equality).
  // Symmetric with onExit: callers that register a handler in a run-scoped
  // closure must call offExit in their cleanup path so the handler list does not
  // grow by O(N) on a long-lived shared child serving N sequential runs.
  // Production: the real fork wrapper overrides this on the returned instance.
  // Test fakes must override it to remove from the same list onExit appends to.
  offExit(handler: (code: number | null) => void): void {
    // Default no-op: overridden by the spawn wrapper.
    void handler;
  }

  // kill schedules a forcible termination of the child after delayMs.
  // Production: the real fork wrapper overrides this on the returned instance.
  // Test fakes may leave it as a no-op.
  kill(delayMs: number): void {
    // Default no-op: overridden by the spawn wrapper.
    void delayMs;
  }
}

// ── ChildLink ─────────────────────────────────────────────────────────────────
//
// Wraps a Channel representing the child side (wrapping process in production,
// or a FakeChannel in tests). The child sends requests + events and receives
// directives + results.

export class ChildLink extends BaseLink {
  constructor(channel: Channel) {
    super(channel);
  }
}
