// Pure builder: accumulated IPC events → session-record JSON (CP2).
//
// WHY a separate module: runViaChild is IPC-wiring; the record-building is pure
// data transformation. Splitting them lets tests drive a fixed event sequence
// without spawning a child process, and keeps ticker.ts thin.

// ── Input types ───────────────────────────────────────────────────────────────

export interface ScheduledRunInput {
  runId: string;
  scheduleId: string;
  prompt: string;
  runAt: string;       // ISO-8601, stamped once at run start
  finishedAt: string;  // ISO-8601, stamped at completion
}

// Discriminated union of the IPC event kinds we accumulate. Only the fields
// needed by the builder are declared — callers narrow to the right variant.
export type SessionEvent =
  | { kind: "text_delta"; delta: string }
  | { kind: "tool_start"; toolCallId: string; toolName: string; input: Record<string, unknown> }
  | { kind: "tool_end"; toolCallId: string; ok: boolean; result: unknown }
  | { kind: "error"; message: string }
  | { kind: "done" };

// ── Output types ──────────────────────────────────────────────────────────────

export interface SessionToolEntry {
  id: string;
  name: string;
  argsJson: string;
  result: unknown;
  isError: boolean;
  done: boolean;
}

export interface AssistantMessage {
  role: "assistant";
  text: string;
  tools: SessionToolEntry[];
  error: string | null;
}

export interface UserMessage {
  role: "user";
  text: string;
}

export interface ScheduledSessionRecord {
  id: string;
  title: string;
  agent_id: null;
  agent_name: null;
  pinned: false;
  pinned_at: null;
  created_at: string;
  updated_at: string;
  thread_id: string;
  first_message: string;
  source: "schedule";
  schedule_id: string;
  run_at: string;
  messages: [UserMessage, AssistantMessage];
}

// ── Title helper (mirrors webui/web/src/views/Chat.vue:71) ───────────────────

function titleFrom(text: string): string {
  const words = text.trim().split(/\s+/);
  const sixWords = words.slice(0, 6).join(" ");
  return sixWords.length > 40 ? sixWords.slice(0, 40) : sixWords;
}

// ── Builder ───────────────────────────────────────────────────────────────────

export function buildScheduledSessionRecord(
  run: ScheduledRunInput,
  events: SessionEvent[],
): ScheduledSessionRecord {
  let text = "";
  const tools: SessionToolEntry[] = [];
  let error: string | null = null;

  for (const evt of events) {
    if (evt.kind === "text_delta") {
      text += evt.delta;
    } else if (evt.kind === "tool_start") {
      tools.push({
        id: evt.toolCallId,
        name: evt.toolName,
        argsJson: JSON.stringify(evt.input),
        result: null,
        isError: false,
        done: false,
      });
    } else if (evt.kind === "tool_end") {
      const entry = tools.find((t) => t.id === evt.toolCallId);
      if (entry) {
        entry.result = evt.result;
        entry.isError = !evt.ok;
        entry.done = true;
      }
    } else if (evt.kind === "error") {
      error = evt.message;
    }
    // "done" carries no fields — nothing to accumulate.
  }

  const userMsg: UserMessage = { role: "user", text: run.prompt };
  const assistantMsg: AssistantMessage = { role: "assistant", text, tools, error };

  return {
    id: run.runId,
    title: titleFrom(run.prompt),
    agent_id: null,
    agent_name: null,
    pinned: false,
    pinned_at: null,
    created_at: run.runAt,
    updated_at: run.finishedAt,
    thread_id: `sched-${run.scheduleId}`,
    first_message: run.prompt,
    source: "schedule",
    schedule_id: run.scheduleId,
    run_at: run.runAt,
    messages: [userMsg, assistantMsg],
  };
}
