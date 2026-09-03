// Child entrypoint — the forked untrusted process's main (CP3).
//
// WHY this exists: the Pi loop must run in a process that holds no long-lived
// secrets (no OIDC bearer, no ownerGrant, no provider API key). All governed
// actions go through IPC → the parent's RemoteBridgeServer → the real
// GovernanceBridge. See  §CP3.
//
// PHASE-4 residual: a child process that achieves code execution can still open
// the SVID key file at /tls/gateway.key (bind-mounted in the container) and
// rebuild its own south gRPC client with the gateway's mTLS identity. That gives
// Phase-1-bounded impersonation (broker owner-grant still required). Closing this
// gap requires running the child under a distinct OS uid with no read access to
// /tls — a non-goal for Phase 2/3; see design §3b.
import {
  ChildLink,
  type Channel,
  type IpcMessage,
  type InitMessage,
  type PromptMessage,
  type AbortMessage,
  type ConvMessage,
  type SkillBundleEntry,
} from "./protocol.js";
import { EventQueue } from "../pi/event-queue.js";
import { appendUserInstructions } from "../pi/system-prompt.js";
import { RemoteBridgeClient } from "./bridge-client.js";
// SessionLike is defined in session-plan.ts (the session-factory module) and
// re-exported here so existing importers of child-entry.ts are unaffected.
import type { SessionLike } from "../pi/session-plan.js";
export type { SessionLike };

// ── Session factory seam ───────────────────────────────────────────────────────
//
// The real implementation (CP4) will be createSessionFromPlan from session.ts.
// CP3 accepts an injected factory so tests can drive the child with a fake
// AgentSession and no LLM call.
export interface SessionPlan extends InitMessage {}

export interface SessionFactory {
  // history is passed only when the thread is being lazily created (miss in the
  // thread store). It is never passed when an existing thread session is reused.
  (plan: SessionPlan, client: RemoteBridgeClient, history?: ConvMessage[]): Promise<SessionLike>;
}

// ── In-child thread store ──────────────────────────────────────────────────────
//
// Conversations multiplex inside the child: a per-threadId session map with its
// own idle TTL. Reuses ThreadStore from Phase 1 but stores AgentSession directly
// (no GovernanceBridge — the child holds none).
class ChildThreadStore {
  private readonly map = new Map<string, { session: SessionLike; lastUsedAt: number }>();

  get(threadId: string): SessionLike | undefined {
    const entry = this.map.get(threadId);
    if (entry) {
      entry.lastUsedAt = Date.now();
      return entry.session;
    }
    return undefined;
  }

  set(threadId: string, session: SessionLike): void {
    this.map.set(threadId, { session, lastUsedAt: Date.now() });
  }

  reapIdle(ttlMs: number): void {
    const now = Date.now();
    for (const [key, entry] of this.map) {
      if (now - entry.lastUsedAt >= ttlMs) {
        this.map.delete(key);
      }
    }
  }

  sessions(): SessionLike[] {
    return Array.from(this.map.values()).map((e) => e.session);
  }

  size(): number { return this.map.size; }
}

// ── Child main ────────────────────────────────────────────────────────────────

// The child receives AIKONOS_GATEWAY_THREAD_TTL_MS via the parent's scrubbed-env
// allowlist post-fork (see supervisor.ts CHILD_ENV_ALLOWLIST) — it cannot read
// the validated Config directly. A malformed value here must not wedge the
// reaper interval with a NaN TTL (every reapIdle check would be a no-op
// comparison against NaN); fall back to the current default instead.
export function resolveThreadTtlMs(raw: string | undefined): number {
  const n = raw !== undefined ? Number(raw) : NaN;
  return Number.isFinite(n) && n > 0 ? n : 30 * 60 * 1000;
}

const THREAD_TTL_MS = resolveThreadTtlMs(process.env["AIKONOS_GATEWAY_THREAD_TTL_MS"]);

// Cap on the tool description relayed over IPC/SSE — the UI shows a single
// clamped line, so the full description is never needed.
const DESCRIPTION_MAX = 200;

// skillActivationBlock wraps a successfully activated skill's body (already
// including any "## Skill files" manifest — see load-skill.ts's activate())
// in a clearly delimited block prepended to the turn's prompt text.
//
// WHY this exists: the
// /command and keyword-auto-load activation paths only ever inspected
// session.activateSkill's return value for an "ERROR:" prefix and discarded
// the body on success — the model never received the skill's instructions on
// those two flows. Only the model-invoked load_skill execute() path worked,
// because its return value becomes the tool result in conversation. "/command
// skills were authorization-only" until this fix.
function skillActivationBlock(name: string, body: string): string {
  return `[Skill activated: ${name}]\n${body}\n[End skill: ${name}]`;
}

// resolveToolDescription picks the human-readable label relayed on tool_start.
// For load_skill it prefers the activated bundle's own description (matched by
// input.name) over load_skill's generic tool description, so the UI line reads
// as the skill rather than "activate a skill bundle…". Returns undefined when
// nothing is known — the UI then falls back to the tool name.
export function resolveToolDescription(
  toolName: string,
  input: Record<string, unknown>,
  toolDescriptions: Record<string, string> | undefined,
  skillBundles: SkillBundleEntry[] | undefined,
): string | undefined {
  let desc: string | undefined;
  if (toolName === "load_skill") {
    const name = typeof input["name"] === "string" ? input["name"] : undefined;
    desc = name ? skillBundles?.find((b) => b.name === name)?.description : undefined;
  }
  desc ??= toolDescriptions?.[toolName];
  if (desc === undefined) return undefined;
  return desc.length > DESCRIPTION_MAX ? desc.slice(0, DESCRIPTION_MAX) : desc;
}

// InflightEntry tracks the active run so an abort message can cancel it.
interface InflightEntry {
  session: SessionLike;
  queue: EventQueue<QueueItem>;
}

// QueueItem must be declared at module scope so InflightEntry can reference it.
// child-entry's internal queue item type (also used inside handlePrompt).
type QueueItem =
  | { done: true }
  | { error: Error }
  | { type: "text_delta"; delta: string }
  | { type: "tool_start"; toolCallId: string; toolName: string; input: Record<string, unknown>; description?: string }
  | { type: "tool_end"; toolCallId: string; ok: boolean; result: unknown }
  | {
      type: "usage";
      inputTokens: number;
      outputTokens: number;
      cost: number;
      cacheRead: number;
      cacheWrite: number;
      provider: string;
      model: string;
    };

export function startChild(
  channel: Channel,
  factory: SessionFactory,
): void {
  const link = new ChildLink(channel);
  const client = new RemoteBridgeClient(link);
  const threads = new ChildThreadStore();

  // Registry of currently in-flight runs. Populated when a prompt starts,
  // deleted when the run reaches a terminal (done/error). Used by the abort
  // handler to cancel a run the parent has signalled as abandoned.
  const inflight = new Map<string, InflightEntry>();

  // Reap idle sessions on the same cadence as the Phase-1 parent-side reaper.
  const reaper = setInterval(() => threads.reapIdle(THREAD_TTL_MS), 60_000);
  reaper.unref();

  // ── init ─────────────────────────────────────────────────────────────────
  //
  // The parent sends exactly one init before any prompt. We store the plan so
  // new threads created after init can call the factory with it.
  let plan: InitMessage | undefined;

  link.on("init", (msg) => {
    plan = msg;
  });

  // ── abort ─────────────────────────────────────────────────────────────────
  //
  // The parent sends abort when the SSE client disconnects mid-run. We call
  // session.abort() (if the SDK supports it) and close the queue so the drain
  // loop exits and stops relaying events — preventing a 180s token burn.

  link.on("abort", (msg: AbortMessage) => {
    const entry = inflight.get(msg.runId);
    if (!entry) return;
    void entry.session.abort?.();
    entry.queue.close();
  });

  // ── prompt ────────────────────────────────────────────────────────────────
  //
  // Each prompt message drives one LLM turn. The session is looked up by
  // threadId (reused across turns) or created fresh. Session events are relayed
  // over IPC tagged with the prompt's runId. The stream is terminated by a
  // `done` or `error` event — the parent must not emit further events for this
  // runId after receiving either terminal.

  link.on("prompt", (msg) => {
    void handlePrompt(msg);
  });

  async function handlePrompt(msg: PromptMessage): Promise<void> {
    const { runId, threadId, text } = msg;

    // Lazy-create session on first prompt for this thread.
    let session = threads.get(threadId);
    if (!session) {
      if (!plan) {
        link.send({ kind: "error", runId, message: "child received prompt before init" });
        return;
      }
      try {
        // Per-user chat instructions ride the prompt message (content, not
        // identity) and are folded into the system prompt only here, at lazy
        // session creation — an existing thread keeps its prompt, mirroring
        // history seeding and soul-freeze semantics.
        const sessionPlan = msg.userInstructions
          ? { ...plan, systemPrompt: appendUserInstructions(plan.systemPrompt, msg.userInstructions) }
          : plan;
        session = await factory(sessionPlan, client, msg.history);
      } catch (err) {
        link.send({ kind: "error", runId, message: `session creation failed: ${String(err)}` });
        return;
      }
      threads.set(threadId, session);
    }

    // Bounded queue bridges the synchronous subscribe callback → async IPC relay.
    // WHY bounded: a stalled parent (slow or disconnected IPC reader) must not
    // cause the child's backlog to grow without limit. Overflow is fail-closed:
    // the drain loop throws, we send an error IPC message and stop relaying.
    const queue = new EventQueue<QueueItem>();

    // Register in the inflight map so the abort handler can cancel this run.
    inflight.set(runId, { session, queue });

    const unsub = session.subscribe((event) => {
      if (event.type === "message_update" && event.assistantMessageEvent?.type === "text_delta") {
        queue.push({ type: "text_delta", delta: event.assistantMessageEvent.delta });
      } else if (event.type === "tool_execution_start") {
        // event.args is the tool call arguments from the SDK event.
        const input = (event.args ?? {}) as Record<string, unknown>;
        queue.push({
          type: "tool_start",
          toolCallId: event.toolCallId,
          toolName: event.toolName,
          input,
          description: resolveToolDescription(event.toolName, input, session.toolDescriptions, plan?.skillBundles),
        });
      } else if (event.type === "tool_execution_end") {
        // event.result is the tool output content from the SDK event.
        queue.push({
          type: "tool_end",
          toolCallId: event.toolCallId,
          ok: !event.isError,
          result: event.result,
        });
      } else if (event.type === "message_end") {
        // Relay usage so the parent can forward it via south emitLlmUsage.
        // The child has no south client — usage can only leave via IPC.
        // WHY provider comes from the plan, not message.provider: the child
        // always registers its model under a fixed local registry name
        // ("openrouter") regardless of the real dialect — message.provider
        // would carry that placeholder, not the real provider id the broker
        // needs to look up a per-provider cost-fallback rate. plan.providerId
        // is resolved parent-side (resolveSessionPlan) from the real DB
        // provider config. message.model is used as-is — it does reflect the
        // real configured model id (plan.modelId).
        const { message } = event;
        if (message.role === "assistant") {
          queue.push({
            type: "usage",
            inputTokens: message.usage.input,
            outputTokens: message.usage.output,
            cost: message.usage.cost?.total ?? 0,
            cacheRead: message.usage.cacheRead ?? 0,
            cacheWrite: message.usage.cacheWrite ?? 0,
            provider: plan?.providerId ?? "",
            model: message.model ?? plan?.modelId ?? "",
          });
          // A failed LLM turn ends with stopReason "error" (Pi resolves
          // prompt() normally, it does not reject) — surface it as a run
          // error instead of letting the run finish silently empty.
          if (message.stopReason === "error") {
            queue.push({ error: new Error(message.errorMessage || "LLM call failed") });
          }
        }
      }
    });

    // Successfully activated skills' bodies. Collected below and
    // prepended to the turn's prompt text so the model actually receives the
    // skill's instructions, not just an authorization side effect. Injected
    // once per turn (this array is scoped to this single handlePrompt call);
    // the child session's own conversation history persists it thereafter.
    const activationBlocks: string[] = [];

    // CP8: if the parent resolved a /command skill, pre-activate it before the
    // LLM turn runs — same effect as the model calling load_skill(name).
    // An unknown name (returns an "ERROR:"-prefixed string) is surfaced as a
    // user-facing text delta and the run terminates without calling the LLM.
    // A successful activation's body is stashed in activationBlocks below.
    if (msg.activateSkillName) {
      // Wrap in withRun: activating a personal skill calls getSkillBody, which
      // requires an active runId to reach the parent's per-run bridge. Runs
      // before session.prompt()'s own withRun below — sequential, never nested.
      const activateName = msg.activateSkillName;
      const activateResult = session.activateSkill
        ? await client.withRun(runId, () => session.activateSkill!(activateName))
        : undefined;
      if (activateResult === undefined || activateResult.startsWith("ERROR:")) {
        const message = activateResult ?? `unknown skill "${activateName}"`;
        link.send({ kind: "text_delta", runId, delta: message });
        link.send({ kind: "done", runId });
        inflight.delete(runId);
        unsub();
        return;
      }
      activationBlocks.push(skillActivationBlock(activateName, activateResult));
    }

    // auto-skill-loading: the parent's keyword matcher (skill-match.ts) may
    // have resolved several bundles for this turn. Activate each in turn —
    // each activation only injects that bundle's body/file-manifest into the
    // prompt; it grants no additional tool access. Best-effort: unlike
    // the explicit /command path, a per-bundle failure here does not abort the
    // turn (and is not added to activationBlocks) — auto-loading is a
    // convenience, not a user request that must succeed or be reported as an
    // error.
    if (msg.activateSkillNames && msg.activateSkillNames.length > 0 && session.activateSkill) {
      const autoNames = msg.activateSkillNames;
      // Wrap in withRun: auto-loading a personal skill calls getSkillBody, which
      // requires an active runId to reach the parent's per-run bridge (same
      // reason as the /command site above).
      await client.withRun(runId, async () => {
        for (const name of autoNames) {
          const result = await session.activateSkill!(name);
          if (!result.startsWith("ERROR:")) {
            activationBlocks.push(skillActivationBlock(name, result));
          }
        }
      });
    }

    // Prepend any activated skill bodies ahead of the user's own prompt text.
    const promptText = activationBlocks.length > 0
      ? `${activationBlocks.join("\n\n")}\n\n${text}`
      : text;

    // withRun sets the active runId on the bridge client for the duration of
    // the prompt so any tool call (gate/execute/delegate) tags its IPC request
    // with this runId. The parent's bridge-server uses it to look up the
    // per-run bridge that holds the correct identity and approver.
    void client.withRun(runId, () => Promise.resolve(session.prompt(promptText))).then(
      () => queue.push({ done: true }),
      (err: unknown) => queue.push({ error: err instanceof Error ? err : new Error(String(err)) }),
    ).finally(() => unsub());

    try {
      for await (const item of queue.drain()) {
        if ("done" in item) {
          inflight.delete(runId);
          link.send({ kind: "done", runId });
          return;
        }
        if ("error" in item) {
          inflight.delete(runId);
          link.send({ kind: "error", runId, message: item.error.message });
          return;
        }

        switch (item.type) {
          case "text_delta":
            link.send({ kind: "text_delta", runId, delta: item.delta });
            break;
          case "tool_start":
            link.send({ kind: "tool_start", runId, toolCallId: item.toolCallId, toolName: item.toolName, input: item.input, description: item.description });
            break;
          case "tool_end":
            link.send({ kind: "tool_end", runId, toolCallId: item.toolCallId, ok: item.ok, result: item.result });
            break;
          case "usage":
            link.send({
              kind: "usage",
              runId,
              inputTokens: item.inputTokens,
              outputTokens: item.outputTokens,
              cost: item.cost,
              cacheRead: item.cacheRead,
              cacheWrite: item.cacheWrite,
              provider: item.provider,
              model: item.model,
            });
            break;
        }
      }
    } catch (err) {
      // Queue overflowed or aborted — stop relaying and signal the parent.
      inflight.delete(runId);
      const message = err instanceof Error ? err.message : String(err);
      link.send({ kind: "error", runId, message });
    }
  }

  // ── shutdown ──────────────────────────────────────────────────────────────

  link.on("shutdown", () => {
    clearInterval(reaper);
    // dispose() unsubscribes the event bus and signals abort — not a no-op.
    for (const session of threads.sessions()) {
      session.dispose();
    }
    process.exit(0);
  });
}

// ── Entry guard: only run as a real forked child ──────────────────────────────
//
// When this module is imported in tests, startChild is not called automatically.
// Tests inject their own channel + factory via startChild(channel, factory).
if (
  typeof process !== "undefined" &&
  process.send !== undefined &&
  // Avoid running during test import (module resolution evaluates top-level).
  process.env["AIKONOS_CHILD_ENTRY"] === "1"
) {
  // Real fork: wrap process as the channel.
  // process.send is defined — this is genuinely a child_process.fork'd process.
  const processChannel: Channel = {
    send(msg: unknown): void {
      process.send!(msg);
    },
    on(_event: "message", handler: (msg: IpcMessage) => void): void {
      process.on("message", handler as (msg: unknown) => void);
    },
  };

  // CP4: wire the real createSessionFromPlan factory. The child registers its
  // provider at plan.proxyBaseUrl with a dummy key (useProxy:true) — the real
  // api key never enters the child process.
  const { createSessionFromPlan } = await import("../pi/session-plan.js");
  const realFactory: SessionFactory = async (plan, client, history) => {
    const { session } = await createSessionFromPlan(plan, client, {}, { useProxy: true }, history);
    return session;
  };

  startChild(processChannel, realFactory);
}
