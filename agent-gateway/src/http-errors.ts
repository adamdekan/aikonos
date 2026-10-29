// Shared gRPC → HTTP error mapping for every route's catch path (F27, CP2 of
// ). Replaces the per-file adminErrorCode /
// soulErrorCode copies and the `reply.code(...).send({ error: String(err) })`
// idiom, which leaked internal error text (stack fragments, gRPC status
// prefixes) straight into HTTP response bodies.
import { status } from "@grpc/grpc-js";
import type { Logger } from "./log.js";

// Minimal reply shape sendError needs — satisfied by FastifyReply and by the
// hand-rolled UploadReply used in admin.ts's non-Fastify skill-upload tests.
export interface ErrorReply {
  code(statusCode: number): { send(body: unknown): void };
}

function grpcCode(err: unknown): number | undefined {
  return typeof err === "object" && err !== null && "code" in err && typeof err.code === "number"
    ? err.code
    : undefined;
}

// grpcToHttp maps a gRPC status code carried on `err.code` to the HTTP status
// the caller should see. Non-gRPC errors (no numeric `code`) — and any gRPC
// code outside this table — map to 500.
export function grpcToHttp(err: unknown): number {
  switch (grpcCode(err)) {
    // A revoked session (AG-06) or a rejected bearer: 401 sends the web UI back
    // to sign-in instead of a dead-end 500.
    case status.UNAUTHENTICATED:
      return 401;
    case status.PERMISSION_DENIED:
      return 403;
    case status.NOT_FOUND:
      return 404;
    case status.INVALID_ARGUMENT:
      return 400;
    case status.FAILED_PRECONDITION:
      return 409;
    case status.ALREADY_EXISTS:
      return 409;
    case status.RESOURCE_EXHAUSTED:
      return 429;
    case status.UNAVAILABLE:
      return 502;
    case status.DEADLINE_EXCEEDED:
      return 504;
    default:
      return 500;
  }
}

// grpc-js formats ServiceError#message as "<code> <STATUS_NAME>: <details>"
// (callErrorFromStatus) but carries the plain detail separately on `.details`.
// Prefer `.details`; fall back to stripping the "<code> <STATUS_NAME>: " prefix
// off `.message` for non-ServiceError shapes that still carry a code.
function callerDetail(err: unknown): string {
  if (typeof err === "object" && err !== null && "details" in err && typeof (err as { details: unknown }).details === "string") {
    return (err as { details: string }).details;
  }
  const message = err instanceof Error ? err.message : String(err);
  const match = /^\d+ [A-Z_]+: ([\s\S]*)$/.exec(message);
  return match ? match[1] : message;
}

// failedPreconditionError builds an Error carrying the grpc-js `.code`
// convention `grpcCode`/`grpcToHttp` read (CP1,
// ) — a caller-class code so
// `trimmedErrorMessage` surfaces the message instead of collapsing it to the
// generic "internal error" 500. Use for gateway-local (non-RPC) faults that
// must still reach the /agui caller verbatim, e.g. spawn-time credential
// resolution failures.
export function failedPreconditionError(message: string): Error {
  const err = new Error(message) as Error & { code: number };
  err.code = status.FAILED_PRECONDITION;
  return err;
}

export interface SendErrorOptions {
  // Route/context identifiers attached to the server-side log line only —
  // never sent to the caller.
  route?: string;
  context?: Record<string, unknown>;
  // Extra fields merged into the response body alongside `error` (e.g. the
  // route's existing `{ schedules: [] }` empty-collection convention on error).
  body?: Record<string, unknown>;
}

// trimmedErrorMessage maps any thrown value to a caller-safe message: gRPC
// caller-class errors (4xx-mapped codes, per grpcToHttp) surface the trimmed
// detail; gRPC infra-class errors and non-gRPC errors surface a generic
// "upstream unavailable" / "internal error" string. Never returns String(err)
// — that leaks stack fragments and internal gRPC status prefixes to the
// caller. Shared by sendError (HTTP routes) and any other surface that must
// not leak raw error text to a client (e.g. the /agui RUN_ERROR SSE frame).
export function trimmedErrorMessage(err: unknown): string {
  const httpStatus = grpcToHttp(err);
  return httpStatus < 500
    ? callerDetail(err)
    : httpStatus === 502 || httpStatus === 504
      ? "upstream unavailable"
      : "internal error";
}

// AG-13: the broker refuses a yes on a step-up approval without a recent
// multi-factor sign-in with PERMISSION_DENIED "step_up_required", and names the
// tenant's acr (if any) in a trailer for the web UI's re-authentication.
export function isStepUpRequired(err: unknown): boolean {
  return grpcCode(err) === status.PERMISSION_DENIED && callerDetail(err) === "step_up_required";
}

// F129 / AG-06: the broker refuses a blocked user with PERMISSION_DENIED "…user
// is blocked" and a revoked session with UNAUTHENTICATED. Either means the run's
// user may no longer act, so the run ends rather than carrying on.
export function isAccessRevoked(err: unknown): boolean {
  const code = grpcCode(err);
  return code === status.UNAUTHENTICATED || (code === status.PERMISSION_DENIED && callerDetail(err).endsWith("user is blocked"));
}

export function stepUpAcr(err: unknown): string {
  const md = (err as { metadata?: { get(key: string): unknown[] } })?.metadata;
  const v = md?.get?.("aikonos-step-up-acr")?.[0];
  return typeof v === "string" ? v : "";
}

// sendError logs the full error server-side and replies with the mapped HTTP
// status and a trimmed body (see trimmedErrorMessage).
export function sendError(reply: ErrorReply, log: Logger, err: unknown, opts: SendErrorOptions = {}): void {
  const httpStatus = grpcToHttp(err);
  log.error({ err, route: opts.route, ...opts.context }, "request failed");
  reply.code(httpStatus).send({ ...opts.body, error: trimmedErrorMessage(err) });
}

// providerErrorMessage turns a raw LLM-provider failure (Pi's errorMessage on a
// stopReason "error" turn — e.g. an Anthropic refusal or a 4xx/5xx SDK error
// such as `400 {"type":"error","error":{"message":"..."}}`) into a chat-safe
// line. The provider's own explanation is what the user needs; URLs, key-like
// tokens and anything past the first ~300 chars are dropped so a credential or
// endpoint echoed back by an SDK cannot reach the browser.
export function providerErrorMessage(raw: string): string {
  let msg = raw;
  const json = /"message"\s*:\s*("(?:[^"\\]|\\.)*")/.exec(raw);
  if (json) {
    try { msg = JSON.parse(json[1]) as string; } catch { /* keep raw */ }
  }
  msg = msg
    .replace(/https?:\/\/\S+/gi, "[url]")
    .replace(/\b(bearer|authorization|x-api-key|api[_-]?key|token|secret)\b\s*[:=]?\s*\S+/gi, "$1 [redacted]")
    .replace(/\bsk-[A-Za-z0-9_-]{8,}/g, "[redacted]")
    .replace(/[A-Za-z0-9_\-+/=]{32,}/g, "[redacted]")
    .replace(/\s+/g, " ")
    .trim();
  if (msg.length > 300) msg = `${msg.slice(0, 300)}…`;
  return `The model provider refused or failed this request${msg ? `: ${msg}` : "."}`;
}
