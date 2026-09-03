// Maps Pi AgentSession events to AG-UI protocol events and writes them as SSE
// using @ag-ui/encoder. One AGUIStream per /agui run.
import { EventType } from "@ag-ui/core";
import { EventEncoder } from "@ag-ui/encoder";
import { randomUUID } from "node:crypto";
import { AIKONOS_TOOL_ERROR } from "./events.js";

type Writable = { write: (chunk: string) => boolean };

// Same idiom as audit/stream.ts: a retry hint once at stream start, then a
// periodic comment to keep the connection alive through proxies/load balancers.
const PING_MS = 15_000;
const RETRY_MS = 3_000;

// Cap on bytes buffered by a stalled sink (write() returning false) before we
// give up on the connection — a slow/dead client must not grow memory unbounded.
const MAX_PENDING_BYTES = 4 * 1024 * 1024;

export interface AGUIStreamOptions {
  // Invoked at most once, the first time pendingBytes exceeds MAX_PENDING_BYTES.
  // The route wires this to destroy the connection.
  onOverflow?: () => void;
  // Injectable ping interval — tests use short values instead of 15s.
  pingMs?: number;
}

export class AGUIStream {
  private readonly enc = new EventEncoder();
  private textMsgId: string | null = null;
  private readonly onOverflow?: () => void;
  private readonly pingTimer: ReturnType<typeof setInterval>;
  private pendingBytes = 0;
  private overflowed = false;

  constructor(
    private readonly out: Writable,
    private readonly threadId: string,
    private readonly runId: string,
    opts: AGUIStreamOptions = {},
  ) {
    this.onOverflow = opts.onOverflow;
    this.out.write(`retry: ${RETRY_MS}\n\n`);
    this.pingTimer = setInterval(() => this.out.write(": ping\n\n"), opts.pingMs ?? PING_MS);
  }

  // Called by the route's close handler alongside the rest of per-run teardown.
  stopHeartbeat(): void {
    clearInterval(this.pingTimer);
  }

  // Called by the route on the raw response's 'drain' event.
  notifyDrain(): void {
    this.pendingBytes = 0;
  }

  private send(event: Record<string, unknown>): void {
    const chunk = this.enc.encode(event as never);
    const ok = this.out.write(chunk);
    if (!ok) {
      this.pendingBytes += Buffer.byteLength(chunk);
      if (this.pendingBytes > MAX_PENDING_BYTES && !this.overflowed) {
        this.overflowed = true;
        this.onOverflow?.();
      }
    }
  }

  runStarted(): void {
    this.send({ type: EventType.RUN_STARTED, threadId: this.threadId, runId: this.runId });
  }

  runFinished(): void {
    this.endTextIfOpen();
    this.send({ type: EventType.RUN_FINISHED, threadId: this.threadId, runId: this.runId });
  }

  runError(message: string): void {
    this.endTextIfOpen();
    this.send({ type: EventType.RUN_ERROR, message });
  }

  textDelta(delta: string): void {
    if (!this.textMsgId) {
      this.textMsgId = randomUUID();
      this.send({ type: EventType.TEXT_MESSAGE_START, messageId: this.textMsgId, role: "assistant" });
    }
    this.send({ type: EventType.TEXT_MESSAGE_CONTENT, messageId: this.textMsgId, delta });
  }

  private endTextIfOpen(): void {
    if (this.textMsgId) {
      this.send({ type: EventType.TEXT_MESSAGE_END, messageId: this.textMsgId });
      this.textMsgId = null;
    }
  }

  toolCall(toolCallId: string, toolName: string, args: unknown, description?: string): void {
    this.endTextIfOpen();
    // toolDescription is a aikonos extra field on the START frame; standard AG-UI
    // consumers ignore unknown fields, the webui is the only reader.
    this.send({
      type: EventType.TOOL_CALL_START,
      toolCallId,
      toolCallName: toolName,
      ...(description !== undefined ? { toolDescription: description } : {}),
    });
    this.send({ type: EventType.TOOL_CALL_ARGS, toolCallId, delta: JSON.stringify(args ?? {}) });
    this.send({ type: EventType.TOOL_CALL_END, toolCallId });
  }

  toolResult(toolCallId: string, content: string, isError: boolean): void {
    this.send({
      type: EventType.TOOL_CALL_RESULT,
      messageId: randomUUID(),
      toolCallId,
      content,
      role: "tool",
    });
    if (isError) {
      // also surface as a custom event so the UI can flag the failure
      this.custom(AIKONOS_TOOL_ERROR, { toolCallId, content });
    }
  }

  // Generic side-channel events the demo UI understands.
  custom(name: string, value: unknown): void {
    this.send({ type: EventType.CUSTOM, name, value });
  }
}
