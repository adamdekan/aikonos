import { describe, it, expect, vi, beforeEach } from "vitest";
import { runAgui } from "../api/agui.js";

// Build a ReadableStream from an array of SSE chunks (each is a raw string).
function makeStream(chunks) {
  const enc = new TextEncoder();
  let i = 0;
  return new ReadableStream({
    pull(controller) {
      if (i < chunks.length) {
        controller.enqueue(enc.encode(chunks[i++]));
      } else {
        controller.close();
      }
    },
  });
}

// Wrap chunks into a minimal Response-like object.
function makeResponse(chunks) {
  return { ok: true, body: makeStream(chunks) };
}

// Encode one or more events as SSE lines.
function sseLines(...events) {
  return events.map(ev => `data: ${JSON.stringify(ev)}\n\n`).join("");
}

// Default token getter — returns a test token.
const testToken = async () => "test-bearer-token";

describe("runAgui — event dispatch", () => {
  let calls;
  let handlers;

  beforeEach(() => {
    calls = [];
    handlers = {
      onRunStarted:       ()           => calls.push(["onRunStarted"]),
      onTextStart:        ()           => calls.push(["onTextStart"]),
      onText:             (delta)      => calls.push(["onText", delta]),
      onTextEnd:          ()           => calls.push(["onTextEnd"]),
      onToolCall:         (info)       => calls.push(["onToolCall", info]),
      onToolArgs:         (info)       => calls.push(["onToolArgs", info]),
      onToolEnd:          (id)         => calls.push(["onToolEnd", id]),
      onToolResult:       (info)       => calls.push(["onToolResult", info]),
      onApprovalRequest:  (info)       => calls.push(["onApprovalRequest", info]),
      onToolError:        (info)       => calls.push(["onToolError", info]),
      onUser:             (val)        => calls.push(["onUser", val]),
      onSkillsLoaded:     (val)        => calls.push(["onSkillsLoaded", val]),
      onMemoryRecalled:   (val)        => calls.push(["onMemoryRecalled", val]),
      onSubagentSpawned:  (val)        => calls.push(["onSubagentSpawned", val]),
      onSubagentCompleted: (val)       => calls.push(["onSubagentCompleted", val]),
      onFinished:         ()           => calls.push(["onFinished"]),
      onError:            (msg)        => calls.push(["onError", msg]),
    };
  });

  it("dispatches a full text + tool sequence in order", async () => {
    const events = [
      { type: "RUN_STARTED",           threadId: "t1", runId: "r1" },
      { type: "TEXT_MESSAGE_START",    messageId: "m1", role: "assistant" },
      { type: "TEXT_MESSAGE_CONTENT",  messageId: "m1", delta: "Hello " },
      { type: "TEXT_MESSAGE_CONTENT",  messageId: "m1", delta: "world" },
      { type: "TEXT_MESSAGE_END",      messageId: "m1" },
      { type: "TOOL_CALL_START",       toolCallId: "tc1", toolCallName: "web.fetch" },
      { type: "TOOL_CALL_ARGS",        toolCallId: "tc1", delta: JSON.stringify({ url: "https://example.com" }) },
      { type: "TOOL_CALL_END",         toolCallId: "tc1" },
      { type: "TOOL_CALL_RESULT",      toolCallId: "tc1", messageId: "mr1", content: "fetched", role: "tool" },
      { type: "RUN_FINISHED",          threadId: "t1", runId: "r1" },
    ];

    const fetchMock = vi.fn().mockResolvedValue(makeResponse([sseLines(...events)]));

    await runAgui({ prompt: "hi", threadId: "t1" }, handlers, fetchMock, testToken);

    expect(calls[0]).toEqual(["onRunStarted"]);
    expect(calls[1]).toEqual(["onTextStart"]);
    expect(calls[2]).toEqual(["onText", "Hello "]);
    expect(calls[3]).toEqual(["onText", "world"]);
    expect(calls[4]).toEqual(["onTextEnd"]);
    expect(calls[5]).toEqual(["onToolCall", { id: "tc1", name: "web.fetch" }]);
    expect(calls[6]).toEqual(["onToolArgs", { id: "tc1", argsJson: JSON.stringify({ url: "https://example.com" }) }]);
    expect(calls[7]).toEqual(["onToolEnd", "tc1"]);
    expect(calls[8]).toEqual(["onToolResult", { id: "tc1", content: "fetched", isError: false }]);
    expect(calls[9]).toEqual(["onFinished"]);
  });

  it("surfaces approval CUSTOM event via onApprovalRequest", async () => {
    const approvalInfo = { toolId: "web.fetch", toolCallId: "tc2", stepUp: true, reason: "network", args: {} };
    const events = [
      { type: "CUSTOM", name: "aikonos.approval.request", value: approvalInfo },
      { type: "RUN_FINISHED", threadId: "t1", runId: "r1" },
    ];

    const fetchMock = vi.fn().mockResolvedValue(makeResponse([sseLines(...events)]));
    await runAgui({ prompt: "go", threadId: "t1" }, handlers, fetchMock, testToken);

    expect(calls[0]).toEqual(["onApprovalRequest", approvalInfo]);
    expect(calls[1]).toEqual(["onFinished"]);
  });

  it("surfaces tool error CUSTOM via onToolError", async () => {
    const errInfo = { toolCallId: "tc3", content: "boom" };
    const events = [
      { type: "CUSTOM", name: "aikonos.tool.error", value: errInfo },
      { type: "RUN_FINISHED", threadId: "t1", runId: "r1" },
    ];

    const fetchMock = vi.fn().mockResolvedValue(makeResponse([sseLines(...events)]));
    await runAgui({ prompt: "go", threadId: "t1" }, handlers, fetchMock, testToken);

    expect(calls[0]).toEqual(["onToolError", errInfo]);
    expect(calls[1]).toEqual(["onFinished"]);
  });

  it("dispatches onUser for aikonos.user CUSTOM", async () => {
    const events = [
      { type: "CUSTOM", name: "aikonos.user", value: { user: "alice@example.com" } },
      { type: "RUN_FINISHED", threadId: "t1", runId: "r1" },
    ];
    const fetchMock = vi.fn().mockResolvedValue(makeResponse([sseLines(...events)]));
    await runAgui({ prompt: "go", threadId: "t1" }, handlers, fetchMock, testToken);

    expect(calls[0]).toEqual(["onUser", { user: "alice@example.com" }]);
  });

  it("dispatches onSkillsLoaded for aikonos.skills.loaded CUSTOM", async () => {
    const payload = {
      skills: [
        { name: "billing", description: "Billing helper", status: "loaded" },
        { name: "hr-tools", description: "HR helper", status: "suppressed", reason: "cap-overflow" },
      ],
    };
    const events = [
      { type: "CUSTOM", name: "aikonos.skills.loaded", value: payload },
      { type: "RUN_FINISHED", threadId: "t1", runId: "r1" },
    ];
    const fetchMock = vi.fn().mockResolvedValue(makeResponse([sseLines(...events)]));
    await runAgui({ prompt: "go", threadId: "t1" }, handlers, fetchMock, testToken);

    expect(calls[0]).toEqual(["onSkillsLoaded", payload]);
    expect(calls[1]).toEqual(["onFinished"]);
  });

  it("dispatches onMemoryRecalled for aikonos.memory.recalled CUSTOM", async () => {
    const payload = {
      concepts: [
        { id: "deploy-runbook", scope: "group", groupId: "security-team", title: "Deploy runbook", status: "stable", trustTier: "human-reviewed", stale: false },
        { id: "foo", scope: "user", title: "Foo", status: "draft", trustTier: "unverified", stale: true },
      ],
    };
    const events = [
      { type: "CUSTOM", name: "aikonos.memory.recalled", value: payload },
      { type: "RUN_FINISHED", threadId: "t1", runId: "r1" },
    ];
    const fetchMock = vi.fn().mockResolvedValue(makeResponse([sseLines(...events)]));
    await runAgui({ prompt: "go", threadId: "t1" }, handlers, fetchMock, testToken);

    expect(calls[0]).toEqual(["onMemoryRecalled", payload]);
    expect(calls[1]).toEqual(["onFinished"]);
  });

  it("dispatches onSubagentSpawned for aikonos.subagent.spawned CUSTOM", async () => {
    const payload = { index: 0, task: "research A", role: "writer" };
    const events = [
      { type: "CUSTOM", name: "aikonos.subagent.spawned", value: payload },
      { type: "RUN_FINISHED", threadId: "t1", runId: "r1" },
    ];
    const fetchMock = vi.fn().mockResolvedValue(makeResponse([sseLines(...events)]));
    await runAgui({ prompt: "go", threadId: "t1" }, handlers, fetchMock, testToken);

    expect(calls[0]).toEqual(["onSubagentSpawned", payload]);
    expect(calls[1]).toEqual(["onFinished"]);
  });

  it("dispatches onSubagentCompleted for aikonos.subagent.completed CUSTOM", async () => {
    const payload = { index: 0, task: "research A", ok: false, failure: "timeout", cost: 0.02 };
    const events = [
      { type: "CUSTOM", name: "aikonos.subagent.completed", value: payload },
      { type: "RUN_FINISHED", threadId: "t1", runId: "r1" },
    ];
    const fetchMock = vi.fn().mockResolvedValue(makeResponse([sseLines(...events)]));
    await runAgui({ prompt: "go", threadId: "t1" }, handlers, fetchMock, testToken);

    expect(calls[0]).toEqual(["onSubagentCompleted", payload]);
    expect(calls[1]).toEqual(["onFinished"]);
  });

  it("dispatches onError on RUN_ERROR", async () => {
    const events = [
      { type: "RUN_ERROR", message: "something went wrong" },
    ];
    const fetchMock = vi.fn().mockResolvedValue(makeResponse([sseLines(...events)]));
    await runAgui({ prompt: "go", threadId: "t1" }, handlers, fetchMock, testToken);

    expect(calls[0]).toEqual(["onError", "something went wrong"]);
  });

  it("reassembles partial chunks (JSON line split across two reads)", async () => {
    const ev = { type: "TEXT_MESSAGE_CONTENT", messageId: "m1", delta: "split" };
    const full = `data: ${JSON.stringify(ev)}\n\n`;
    const mid = Math.floor(full.length / 2);
    const chunk1 = full.slice(0, mid);
    const chunk2 = full.slice(mid);

    const fetchMock = vi.fn().mockResolvedValue(
      makeResponse([chunk1, chunk2])
    );
    await runAgui({ prompt: "go", threadId: "t1" }, handlers, fetchMock, testToken);

    const textCalls = calls.filter(c => c[0] === "onText");
    expect(textCalls).toHaveLength(1);
    expect(textCalls[0][1]).toBe("split");
  });

  it("generates a threadId when none is provided and returns it", async () => {
    const fetchMock = vi.fn().mockResolvedValue(makeResponse([]));
    const used = await runAgui({ prompt: "hi" }, handlers, fetchMock, testToken);
    expect(used).toBeTruthy();
    expect(typeof used).toBe("string");
  });

  it("uses provided threadId and returns it", async () => {
    const fetchMock = vi.fn().mockResolvedValue(makeResponse([]));
    const used = await runAgui({ prompt: "hi", threadId: "abc-123" }, handlers, fetchMock, testToken);
    expect(used).toBe("abc-123");
  });

  it("sends Authorization: Bearer header; does NOT send x-aikonos-user", async () => {
    const fetchMock = vi.fn().mockResolvedValue(makeResponse([]));
    await runAgui({ prompt: "do thing", threadId: "t9" }, handlers, fetchMock, testToken);

    const [, opts] = fetchMock.mock.calls[0];
    expect(opts.headers["Authorization"]).toBe("Bearer test-bearer-token");
    expect(opts.headers["x-aikonos-user"]).toBeUndefined();
  });

  it("sends correct url and method", async () => {
    const fetchMock = vi.fn().mockResolvedValue(makeResponse([]));
    await runAgui({ prompt: "do thing", threadId: "t9" }, handlers, fetchMock, testToken);

    expect(fetchMock).toHaveBeenCalledWith("/agui", expect.objectContaining({
      method: "POST",
    }));
  });

  it("request body does NOT include user field", async () => {
    const fetchMock = vi.fn().mockResolvedValue(makeResponse([]));
    await runAgui({ prompt: "do thing", threadId: "t9" }, handlers, fetchMock, testToken);

    const [, opts] = fetchMock.mock.calls[0];
    const body = JSON.parse(opts.body);
    expect(body.user).toBeUndefined();
    expect(body.prompt).toBe("do thing");
    expect(body.threadId).toBe("t9");
  });

  it("includes userInstructions in the body only when non-empty", async () => {
    const fetchMock = vi.fn().mockImplementation(() => Promise.resolve(makeResponse([])));
    await runAgui({ prompt: "hi", threadId: "t9", userInstructions: "answer in German" }, handlers, fetchMock, testToken);
    expect(JSON.parse(fetchMock.mock.calls[0][1].body).userInstructions).toBe("answer in German");

    await runAgui({ prompt: "hi", threadId: "t9" }, handlers, fetchMock, testToken);
    expect(JSON.parse(fetchMock.mock.calls[1][1].body).userInstructions).toBeUndefined();
  });

  // Spend attribution. Sent snake_case
  // because the gateway reads it as session_id; omitted entirely for a run with
  // no session yet, so the gateway's empty-string default applies.
  it("includes session_id in the body only when a session is active", async () => {
    const fetchMock = vi.fn().mockImplementation(() => Promise.resolve(makeResponse([])));
    await runAgui({ prompt: "hi", threadId: "t9", sessionId: "sess-7" }, handlers, fetchMock, testToken);
    expect(JSON.parse(fetchMock.mock.calls[0][1].body).session_id).toBe("sess-7");

    await runAgui({ prompt: "hi", threadId: "t9" }, handlers, fetchMock, testToken);
    expect(JSON.parse(fetchMock.mock.calls[1][1].body).session_id).toBeUndefined();
  });

  it("calls onError when no token is available", async () => {
    const fetchMock = vi.fn();
    const noToken = async () => null;
    await runAgui({ prompt: "hi", threadId: "t1" }, handlers, fetchMock, noToken);

    expect(calls[0]).toEqual(["onError", "not authenticated"]);
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("tool args delta is preserved as raw JSON string in onToolArgs", async () => {
    const argsObj = { query: "cats", limit: 5 };
    const argsStr = JSON.stringify(argsObj);
    const events = [
      { type: "TOOL_CALL_ARGS", toolCallId: "tc4", delta: argsStr },
    ];
    const fetchMock = vi.fn().mockResolvedValue(makeResponse([sseLines(...events)]));
    await runAgui({ prompt: "go", threadId: "t1" }, handlers, fetchMock, testToken);

    const toolArgsCalls = calls.filter(c => c[0] === "onToolArgs");
    expect(toolArgsCalls[0][1].argsJson).toBe(argsStr);
  });

  it("TOOL_CALL_RESULT with isError derived from false when not an error", async () => {
    const events = [
      { type: "TOOL_CALL_RESULT", toolCallId: "tc5", messageId: "mr5", content: "ok result", role: "tool" },
    ];
    const fetchMock = vi.fn().mockResolvedValue(makeResponse([sseLines(...events)]));
    await runAgui({ prompt: "go", threadId: "t1" }, handlers, fetchMock, testToken);

    expect(calls[0]).toEqual(["onToolResult", { id: "tc5", content: "ok result", isError: false }]);
  });

  it("x-aikonos-agent header is ABSENT when agentId is not supplied", async () => {
    const fetchMock = vi.fn().mockResolvedValue(makeResponse([]));
    await runAgui({ prompt: "hi", threadId: "t1" }, handlers, fetchMock, testToken);

    const headers = fetchMock.mock.calls[0][1].headers;
    expect(Object.keys(headers)).not.toContain("x-aikonos-agent");
  });

  it("x-aikonos-agent header is set to agentId when agentId is supplied", async () => {
    const fetchMock = vi.fn().mockResolvedValue(makeResponse([]));
    await runAgui({ prompt: "hi", threadId: "t1", agentId: "bot-42" }, handlers, fetchMock, testToken);

    const headers = fetchMock.mock.calls[0][1].headers;
    expect(headers["x-aikonos-agent"]).toBe("bot-42");
  });

  // CP1 correction: a non-2xx
  // /agui response must surface its body text so a CP1 remediation message
  // reaches the chat UI instead of being discarded.
  it("onError includes the response body text on a non-2xx response", async () => {
    const body = JSON.stringify({
      error: 'llm credentials unavailable: provider "openai-prod" has no API key available — re-enter it in Admin → LLM Providers',
    });
    const fetchMock = vi.fn().mockResolvedValue({ ok: false, status: 409, text: async () => body });
    await runAgui({ prompt: "hi", threadId: "t1" }, handlers, fetchMock, testToken);

    expect(calls).toHaveLength(1);
    expect(calls[0][0]).toBe("onError");
    expect(calls[0][1]).toContain("409");
    expect(calls[0][1]).toContain("re-enter it in Admin");
  });

  it("onError falls back to the bare status when the response body can't be read", async () => {
    const fetchMock = vi.fn().mockResolvedValue({
      ok: false,
      status: 500,
      text: async () => { throw new Error("body already consumed"); },
    });
    await runAgui({ prompt: "hi", threadId: "t1" }, handlers, fetchMock, testToken);

    expect(calls[0]).toEqual(["onError", "request failed (500)"]);
  });

  it("onError truncates an oversized response body to 300 characters", async () => {
    const body = "x".repeat(1000);
    const fetchMock = vi.fn().mockResolvedValue({ ok: false, status: 500, text: async () => body });
    await runAgui({ prompt: "hi", threadId: "t1" }, handlers, fetchMock, testToken);

    expect(calls[0][1].length).toBeLessThanOrEqual(300 + "request failed (500): ".length);
  });
});
