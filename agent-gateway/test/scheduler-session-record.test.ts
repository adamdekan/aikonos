// TDD tests for buildScheduledSessionRecord (CP2 — Gateway transcript capture).
//
// WHY these tests exist: the builder is a pure function that maps a fixed event
// sequence onto a session-record JSON. Keeping the accumulation logic out of
// runViaChild's IPC-wiring lets us drive it with synthetic events here, without
// standing up a child process.
import { test } from "node:test";
import assert from "node:assert/strict";
import {
  buildScheduledSessionRecord,
  type ScheduledRunInput,
  type SessionEvent,
} from "../src/scheduler/session-record.js";

// ── helpers ────────────────────────────────────────────────────────────────────

function makeRun(overrides: Partial<ScheduledRunInput> = {}): ScheduledRunInput {
  return {
    runId: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
    scheduleId: "sched-0001",
    prompt: "summarize the latest report from finance",
    runAt: "2026-06-16T10:00:00.000Z",
    finishedAt: "2026-06-16T10:00:05.000Z",
    ...overrides,
  };
}

// ── Test 1: happy path — text, tool round-trip, done ─────────────────────────

test("buildScheduledSessionRecord: text_delta + tool round-trip + done → full record", () => {
  // WHY: the primary path — the model emits text, calls a tool, gets a result,
  // emits more text, then finishes. All fields must be populated correctly.
  const run = makeRun();
  const events: SessionEvent[] = [
    { kind: "text_delta", delta: "Hello, " },
    { kind: "tool_start", toolCallId: "tc-1", toolName: "doc.read", input: { path: "/report.txt" } },
    { kind: "tool_end", toolCallId: "tc-1", ok: true, result: "file contents" },
    { kind: "text_delta", delta: "here is the summary." },
    { kind: "done" },
  ];

  const record = buildScheduledSessionRecord(run, events);

  // Top-level discriminator fields.
  assert.equal(record.id, run.runId);
  assert.equal(record.schedule_id, run.scheduleId);
  assert.equal(record.thread_id, `sched-${run.scheduleId}`);
  assert.equal(record.source, "schedule");
  assert.equal(record.agent_id, null);
  assert.equal(record.agent_name, null);
  assert.equal(record.pinned, false);
  assert.equal(record.pinned_at, null);
  assert.equal(record.run_at, run.runAt);
  assert.equal(record.created_at, run.runAt);
  assert.equal(record.updated_at, run.finishedAt);
  assert.equal(record.first_message, run.prompt);

  // Title: first 6 words, ≤40 chars.
  assert.equal(record.title, "summarize the latest report from finance");

  // Messages: user prompt + assistant turn.
  assert.equal(record.messages.length, 2);
  assert.equal(record.messages[0].role, "user");
  assert.equal(record.messages[0].text, run.prompt);

  const asst = record.messages[1];
  assert.equal(asst.role, "assistant");
  assert.equal(asst.text, "Hello, here is the summary.");
  assert.equal(asst.error, null);

  // Tool entries.
  assert.equal(asst.tools.length, 1);
  const tool = asst.tools[0];
  assert.equal(tool.id, "tc-1");
  assert.equal(tool.name, "doc.read");
  assert.equal(tool.argsJson, JSON.stringify({ path: "/report.txt" }));
  assert.equal(tool.result, "file contents");
  assert.equal(tool.isError, false);
  assert.equal(tool.done, true);
});

// ── Test 2: tool_end ok:false → isError:true ──────────────────────────────────

test("buildScheduledSessionRecord: tool_end ok:false → tool isError:true", () => {
  // WHY: failed tool calls must be surfaced for debugging in the sessions list.
  const run = makeRun({ prompt: "fetch the page" });
  const events: SessionEvent[] = [
    { kind: "tool_start", toolCallId: "tc-fail", toolName: "web.fetch", input: { url: "https://example.com" } },
    { kind: "tool_end", toolCallId: "tc-fail", ok: false, result: "connection refused" },
    { kind: "done" },
  ];

  const record = buildScheduledSessionRecord(run, events);
  const asst = record.messages[1];
  assert.equal(asst.tools.length, 1);
  const tool = asst.tools[0];
  assert.equal(tool.isError, true);
  assert.equal(tool.done, true);
  assert.equal(tool.result, "connection refused");
});

// ── Test 3: error event → assistantMsg.error set ──────────────────────────────

test("buildScheduledSessionRecord: error event → messages[1].error set", () => {
  // WHY: a run that terminates via an error event (LLM failure, timeout, etc.)
  // must still produce a record — the partial transcript + error field is the
  // value, letting the user see what went wrong when they open the session.
  const run = makeRun({ prompt: "do something" });
  const events: SessionEvent[] = [
    { kind: "text_delta", delta: "Starting..." },
    { kind: "error", message: "LLM provider unavailable" },
  ];

  const record = buildScheduledSessionRecord(run, events);
  const asst = record.messages[1];
  assert.equal(asst.text, "Starting...");
  assert.equal(asst.error, "LLM provider unavailable");
});

// ── Test 4: orphan tool_end is silently ignored ───────────────────────────────

test("buildScheduledSessionRecord: tool_end with no preceding tool_start → tools stays empty", () => {
  // WHY: the builder looks up the entry by toolCallId; if no tool_start arrived
  // (e.g. the event stream was truncated or the child sent an orphan end), the
  // find() returns undefined and the branch is skipped. This is deliberate
  // no-op behaviour — pin it so a future refactor can't accidentally push a
  // half-populated entry.
  const run = makeRun({ prompt: "do something" });
  const events: SessionEvent[] = [
    { kind: "tool_end", toolCallId: "orphan-id", ok: true, result: "ghost result" },
    { kind: "done" },
  ];

  const record = buildScheduledSessionRecord(run, events);
  const asst = record.messages[1];
  assert.equal(asst.tools.length, 0, "orphan tool_end must not create a tool entry");
});

// ── Test 5: title truncation ───────────────────────────────────────────────────

test("buildScheduledSessionRecord: title = first 6 words ≤40 chars", () => {
  // WHY: mirrors webui titleFrom — a very long prompt must be truncated both by
  // word count and by character count so the UI title column stays readable.

  // Case A: prompt with more than 6 words → exactly 6 words joined.
  const sixWordRun = makeRun({
    prompt: "one two three four five six seven eight nine ten",
  });
  const sixWordRecord = buildScheduledSessionRecord(sixWordRun, [{ kind: "done" }]);
  assert.equal(sixWordRecord.title, "one two three four five six");

  // Case B: 6 words that together exceed 40 chars → sliced at 40.
  const longRun = makeRun({
    prompt: "superlongwordone superlongwordtwo superlongwordthree four five six extra",
  });
  const longRecord = buildScheduledSessionRecord(longRun, [{ kind: "done" }]);
  assert.ok(
    longRecord.title.length <= 40,
    `title must be ≤40 chars, got ${longRecord.title.length}: "${longRecord.title}"`,
  );

  // Case C: prompt shorter than 6 words → all words used, no truncation.
  const shortRun = makeRun({ prompt: "short prompt" });
  const shortRecord = buildScheduledSessionRecord(shortRun, [{ kind: "done" }]);
  assert.equal(shortRecord.title, "short prompt");
});
