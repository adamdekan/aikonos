import { get, post, del } from "./client.js";
import { getAccessToken } from "../auth/oidc.js";

// Returns the whole response body: { workflows, nextCursor, sharedUnavailable }.
// Absent limit/cursor = legacy full listing (gateway contract).
export function listWorkflows({ limit, cursor } = {}) {
  const params = new URLSearchParams();
  if (limit != null) params.set("limit", String(limit));
  if (cursor) params.set("cursor", cursor);
  const qs = params.toString();
  return get(qs ? `/workflows?${qs}` : "/workflows");
}

export function getWorkflow(lineageId) {
  return get(`/workflows/${lineageId}`);
}

export function saveWorkflow(body) {
  return post("/workflows", { body });
}

// sessionId is the id the caller will file this run's session record under. It
// is sent so the run's reason-step LLM usage is attributed to that session and
// shows up in the chat view's usage strip; attribution only, never authority.
export function runWorkflow(lineageId, inputs, sessionId) {
  return post(`/workflows/${lineageId}/run`, {
    body: { inputs, ...(sessionId ? { session_id: sessionId } : {}) },
  });
}

export function rateWorkflow(lineageId, body) {
  return post(`/workflows/${lineageId}/rate`, { body });
}

export function publishWorkflow(lineageId, body) {
  return post(`/workflows/${lineageId}/publish`, { body });
}

export function forkWorkflow(lineageId, body) {
  return post(`/workflows/${lineageId}/fork`, { body });
}

export function deleteWorkflow(lineageId) {
  return del(`/workflows/${lineageId}`);
}

export function pinVersion(lineageId, body) {
  return post(`/workflows/${lineageId}/pin`, { body });
}

export function clearPin(lineageId) {
  return del(`/workflows/${lineageId}/pin`);
}

export function listVersions(lineageId) {
  return get(`/workflows/${lineageId}/versions`);
}

export function proposeVersion(lineageId, body) {
  return post(`/workflows/${lineageId}/propose`, { body });
}

export function decideVersion(lineageId, body) {
  return post(`/workflows/${lineageId}/decide`, { body });
}

// Streaming run. POSTs with ?stream=1 and reads the SSE body: one `event: step`
// per settled step ({index, skill, ok, denyReason?}) then a terminal
// `event: result` whose data is the exact JSON the blocking run returns.
// Follows the fetch-SSE parsing precedent in api/agui.js (event+data per block).
// Returns the result payload; throws if the stream errors or yields no result
// frame — the caller falls back to the blocking runWorkflow() in that case.
// fetchFn / getTokenFn are injectable for tests.
// opts carries the step/result callbacks plus an optional sessionId (the id the
// caller will file the run's session record under — see runWorkflow above).
// sessionId rides here rather than as a 6th positional param so the existing
// fetchFn/getTokenFn test-injection positions stay put.
export async function runWorkflowStream(
  lineageId,
  inputs,
  opts = {},
  fetchFn = fetch,
  getTokenFn = getAccessToken,
) {
  const token = await getTokenFn();
  if (!token) throw new Error("no token — user is not authenticated");

  const resp = await fetchFn(`/api/workflows/${lineageId}/run?stream=1`, {
    method: "POST",
    headers: {
      "content-type": "application/json",
      "Authorization": `Bearer ${token}`,
      "Accept": "text/event-stream",
    },
    body: JSON.stringify({ inputs, ...(opts.sessionId ? { session_id: opts.sessionId } : {}) }),
  });
  if (!resp.ok || !resp.body) throw new Error(`request failed (${resp.status})`);

  const reader = resp.body.getReader();
  const dec = new TextDecoder();
  let buf = "";
  let result;

  for (;;) {
    const { value, done } = await reader.read();
    if (done) break;
    buf += dec.decode(value, { stream: true });

    let i;
    while ((i = buf.indexOf("\n\n")) >= 0) {
      const block = buf.slice(0, i);
      buf = buf.slice(i + 2);
      const lines = block.split("\n");
      const evName = (lines.find((l) => l.startsWith("event:")) ?? "").slice(6).trim();
      const dataLine = lines.find((l) => l.startsWith("data:"));
      if (!dataLine) continue;
      let payload;
      try {
        payload = JSON.parse(dataLine.slice(5).trim());
      } catch {
        continue;
      }
      if (evName === "step") opts.onStep?.(payload);
      else if (evName === "result") { result = payload; opts.onResult?.(payload); }
    }
  }

  if (result === undefined) throw new Error("stream produced no result");
  return result;
}
