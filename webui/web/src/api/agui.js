// AG-UI SSE stream parser and dispatcher.
// runAgui POSTs to /agui, reads the response body stream, and dispatches
// typed handlers for each event. Returns the threadId used.
//
// fetchFn is injectable for tests; defaults to global fetch.
// getTokenFn is injectable for tests; defaults to getAccessToken from auth/oidc.js.

import { uuid } from "../lib/uuid.js";
import { getAccessToken } from "../auth/oidc.js";

export async function runAgui(
  { prompt, threadId, agentId, history, signal, skillName, userInstructions, sessionId },
  handlers = {},
  fetchFn = fetch,
  getTokenFn = getAccessToken,
) {
  const usedThreadId = threadId || uuid();

  const token = await getTokenFn();
  if (!token) {
    handlers.onError?.("not authenticated");
    return usedThreadId;
  }

  const extraHeaders = {};
  if (agentId) extraHeaders["x-aikonos-agent"] = agentId;

  let resp;
  try {
    resp = await fetchFn("/agui", {
      method: "POST",
      headers: {
        "content-type": "application/json",
        "Authorization": `Bearer ${token}`,
        ...extraHeaders,
      },
      body: JSON.stringify({ prompt, threadId: usedThreadId, agentId: agentId || undefined, history: (Array.isArray(history) && history.length > 0) ? history : undefined, ...(skillName ? { skillName } : {}), ...(userInstructions ? { userInstructions } : {}), ...(sessionId ? { session_id: sessionId } : {}) }),
      signal,
    });
  } catch (err) {
    // User-initiated abort: resolve quietly without calling onError.
    if (err?.name === "AbortError" || signal?.aborted) return usedThreadId;
    throw err;
  }

  if (!resp.ok) {
    // Read the body text so a CP1 remediation message (e.g. "re-enter it in
    // Admin -> LLM Providers") reaches the chat UI instead of being discarded
    //. Bounded — this is
    // untrusted response text rendered to the user.
    let detail = "";
    try {
      detail = (await resp.text()).slice(0, 300);
    } catch {
      // Body already consumed or unreadable — fall back to the bare status.
    }
    handlers.onError?.(detail ? `request failed (${resp.status}): ${detail}` : `request failed (${resp.status})`);
    return usedThreadId;
  }

  const reader = resp.body.getReader();
  const dec = new TextDecoder();
  let buf = "";

  try {
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      buf += dec.decode(value, { stream: true });

      // Events are delimited by double newline.
      let i;
      while ((i = buf.indexOf("\n\n")) >= 0) {
        const block = buf.slice(0, i);
        buf = buf.slice(i + 2);
        const line = block.split("\n").find(l => l.startsWith("data:"));
        if (!line) continue;
        let ev;
        try {
          ev = JSON.parse(line.slice(5).trim());
        } catch {
          continue;
        }
        dispatch(ev, handlers);
      }
    }
  } catch (err) {
    // User-initiated abort during streaming: release the reader lock and resolve
    // quietly (the fetch is already aborted; the gateway drops the connection).
    if (err?.name === "AbortError" || signal?.aborted) {
      Promise.resolve(reader.cancel?.()).catch(() => {});
      return usedThreadId;
    }
    throw err;
  }

  return usedThreadId;
}

function dispatch(ev, h) {
  switch (ev.type) {
    case "RUN_STARTED":
      h.onRunStarted?.();
      break;
    case "TEXT_MESSAGE_START":
      h.onTextStart?.();
      break;
    case "TEXT_MESSAGE_CONTENT":
      h.onText?.(ev.delta);
      break;
    case "TEXT_MESSAGE_END":
      h.onTextEnd?.();
      break;
    case "TOOL_CALL_START":
      // toolDescription is a aikonos extra field the gateway adds to the START frame.
      h.onToolCall?.({ id: ev.toolCallId, name: ev.toolCallName, description: ev.toolDescription });
      break;
    case "TOOL_CALL_ARGS":
      h.onToolArgs?.({ id: ev.toolCallId, argsJson: ev.delta });
      break;
    case "TOOL_CALL_END":
      h.onToolEnd?.(ev.toolCallId);
      break;
    case "TOOL_CALL_RESULT":
      // Error state comes via the separate aikonos.tool.error CUSTOM event, not this event.
      h.onToolResult?.({ id: ev.toolCallId, content: ev.content, isError: ev.isError ?? false });
      break;
    case "CUSTOM":
      if (ev.name === "aikonos.approval.request") {
        h.onApprovalRequest?.(ev.value);
      } else if (ev.name === "aikonos.tool.error") {
        h.onToolError?.(ev.value);
      } else if (ev.name === "aikonos.user") {
        h.onUser?.(ev.value);
      } else if (ev.name === "aikonos.skills.loaded") {
        h.onSkillsLoaded?.(ev.value);
      } else if (ev.name === "aikonos.memory.recalled") {
        h.onMemoryRecalled?.(ev.value);
      } else if (ev.name === "aikonos.subagent.spawned") {
        h.onSubagentSpawned?.(ev.value);
      } else if (ev.name === "aikonos.subagent.completed") {
        h.onSubagentCompleted?.(ev.value);
      }
      break;
    case "RUN_FINISHED":
      h.onFinished?.();
      break;
    case "RUN_ERROR":
      h.onError?.(ev.message);
      break;
  }
}
