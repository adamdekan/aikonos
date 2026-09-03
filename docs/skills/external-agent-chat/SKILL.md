---
name: aikonos-external-agent-chat
description: Connect to the Aikonos external agent API (:8090) and build a bi-directional, multi-turn conversational client for a named agent. Use when integrating an external application, bot, or service with a Aikonos agent over API-key auth.
---

# Aikonos external agent API — conversational client

The agent gateway exposes a hardened, API-key-authenticated surface on port `8090`
(`AIKONOS_EXTERNAL_PORT`), separate from the OIDC-protected internal API. It has one
invocation endpoint that streams the agent's response as Server-Sent Events. There is no
server-side conversation store on this surface: **the client owns the conversation** and
replays it on every call via `history`. That is the entire multi-turn protocol.

## Prerequisites (one-time, done by a Aikonos admin)

1. The target agent must have **external access enabled** (`gateway_enabled: true`) — otherwise every call returns 403.
2. The agent's **approval mode must be `auto`** — agents requiring human approval are not externally drivable (409). Tool calls are still authorized per-call by the broker (OPA + OpenFGA + capability tokens); `auto` only removes the human-in-the-loop prompt.
3. Mint an **agent API key** in the webui: Admin → Agents → API keys. Keys look like `tk_…`, are shown once, and are bound to exactly one agent.
4. If the client is a browser app, its origin must be in `AIKONOS_EXTERNAL_CORS_ORIGINS` (comma-separated; empty = no CORS, same-origin only).

Base URL: `http://<gateway-host>:8090` (compose publishes `8090:8090`).
Health probe: `GET /healthz` → `{"ok":true,"surface":"external"}` (no auth).

## Endpoint

```
POST /v1/agents/{agentId}/invoke
Authorization: Bearer tk_…
Content-Type: application/json
```

`{agentId}` must be the agent the key was minted for — a mismatch returns 409 (the key,
not the URL, is authoritative).

### Request body

```json
{
  "prompt": "string, required, ≤ 16 KiB",
  "history": [
    { "role": "user",      "content": "earlier user turn" },
    { "role": "assistant", "content": "earlier agent reply" }
  ]
}
```

- `history` is optional. Omit it on the first turn.
- Caps: ≤ 100 turns, ≤ 200 KiB of total `content` bytes, whole body ≤ 256 KiB.
- Only `role` values `"user"` and `"assistant"` are accepted; `content` must be a string.
- History is content, not identity — it seeds the agent's context but never changes who
  the agent acts as or what it is authorized to do.

### Response — SSE stream

`200` with `Content-Type: text/event-stream`. One JSON object per `data:` frame:

| Event | Shape | Meaning |
|-------|-------|---------|
| `text_delta` | `{type, delta}` | Incremental assistant text. Concatenate deltas to build the reply. |
| `tool_start` | `{type, toolCallId, toolName}` | Agent began a tool call. |
| `tool_end` | `{type, toolCallId, ok, result}` | Tool call finished. `ok:false` = denied or failed; `result` is a string (JSON-stringified when the tool returned an object). Correlate via `toolCallId`. |
| `usage` | `{type, inputTokens, outputTokens}` | Token usage for the run. |
| `done` | `{type}` | Terminal. The run completed. |
| `error` | `{type, error}` | Terminal. Trimmed, user-facing message — never internal stack/gRPC detail. |

Every stream ends with exactly one `done` or `error` frame, then the connection closes.
Closing the connection early aborts the run server-side (no orphaned token burn).

### Error status codes (pre-stream, JSON body `{"error": "…"}`)

| Code | Cause |
|------|-------|
| 400 | malformed `history` (bad shape, >100 turns, >200 KiB content) |
| 401 | missing/invalid/revoked API key |
| 403 | agent not `gateway_enabled` |
| 409 | agent not `approvalMode:auto`, or URL id ≠ key-resolved agent |
| 413 | prompt > 16 KiB, or body > 256 KiB |
| 429 | rate limit — default 60 req/min per IP (`AIKONOS_EXTERNAL_RATE_LIMIT`) |
| 502 | agent spec unavailable (broker unreachable) |

Runs also time out server-side after 180 s (surfaced as an `error` frame).

## Building the conversation loop

The bi-directional pattern is: send `prompt` + accumulated `history`, collect the
assistant's `text_delta` frames into one reply, append both turns to your local history,
repeat. Trim your history from the front when you approach the 100-turn / 200 KiB caps —
the agent only ever sees what you send.

### Minimal Node/TypeScript client

```ts
type Turn = { role: "user" | "assistant"; content: string };

type AgentEvent =
  | { type: "text_delta"; delta: string }
  | { type: "tool_start"; toolCallId: string; toolName: string }
  | { type: "tool_end"; toolCallId: string; ok: boolean; result: string }
  | { type: "usage"; inputTokens: number; outputTokens: number }
  | { type: "done" }
  | { type: "error"; error: string };

async function* invoke(
  baseUrl: string, agentId: string, apiKey: string,
  prompt: string, history: Turn[],
): AsyncGenerator<AgentEvent> {
  const res = await fetch(`${baseUrl}/v1/agents/${agentId}/invoke`, {
    method: "POST",
    headers: { authorization: `Bearer ${apiKey}`, "content-type": "application/json" },
    body: JSON.stringify({ prompt, history }),
  });
  if (!res.ok || !res.body) {
    throw new Error(`invoke failed (${res.status}): ${await res.text()}`);
  }
  const reader = res.body.pipeThrough(new TextDecoderStream()).getReader();
  let buf = "";
  while (true) {
    const { value, done } = await reader.read();
    if (done) return;
    buf += value;
    let idx: number;
    while ((idx = buf.indexOf("\n\n")) >= 0) {
      const frame = buf.slice(0, idx);
      buf = buf.slice(idx + 2);
      const data = frame.split("\n").find((l) => l.startsWith("data: "));
      if (data) yield JSON.parse(data.slice(6));
    }
  }
}

// Conversation loop: client-held history is the multi-turn mechanism.
async function chatTurn(history: Turn[], userInput: string): Promise<Turn[]> {
  let reply = "";
  for await (const ev of invoke(BASE_URL, AGENT_ID, API_KEY, userInput, history)) {
    switch (ev.type) {
      case "text_delta": reply += ev.delta; process.stdout.write(ev.delta); break;
      case "tool_start": console.log(`\n[tool ${ev.toolName} started]`); break;
      case "tool_end":   console.log(`[tool ${ev.toolCallId} ${ev.ok ? "ok" : "FAILED"}] ${ev.result}`); break;
      case "usage":      console.log(`\n[tokens in=${ev.inputTokens} out=${ev.outputTokens}]`); break;
      case "error":      throw new Error(ev.error);
      case "done":       break;
    }
  }
  return [...history, { role: "user", content: userInput }, { role: "assistant", content: reply }];
}
```

### curl smoke test

```bash
curl -N -X POST "http://localhost:8090/v1/agents/$AGENT_ID/invoke" \
  -H "Authorization: Bearer $TK_KEY" \
  -H "Content-Type: application/json" \
  -d '{"prompt":"Say hello.","history":[]}'
```

`-N` disables buffering so frames render as they arrive.

## Behavior notes

- **Tool authorization is enforced per call, server-side.** A `tool_end` with `ok:false`
  usually means the broker denied the tool for this agent — that is policy working, not a
  transport error. The agent continues and typically explains the denial in text.
- **Denials, budgets, and rate limits inside a run** surface as `tool_end{ok:false}` or a
  terminal `error` frame with an actionable message (e.g. `llm credentials unavailable: …`
  when the deployment's provider key is missing).
- **Retries:** the invoke call is not idempotent — a retried prompt runs again. Retry only
  on pre-stream failures (non-200) or after an `error` frame, never after `done`.
- **Concurrency:** parallel invokes for the same agent are allowed; each is an independent
  run with its own context. There is no cross-call state beyond what you pass in `history`.
- **Keep secrets out of history.** Everything in `history` reaches the LLM provider
  configured for the tenant.
