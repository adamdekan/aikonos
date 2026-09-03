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

// sendError logs the full error server-side and replies with the mapped HTTP
// status and a trimmed body (see trimmedErrorMessage).
export function sendError(reply: ErrorReply, log: Logger, err: unknown, opts: SendErrorOptions = {}): void {
  const httpStatus = grpcToHttp(err);
  log.error({ err, route: opts.route, ...opts.context }, "request failed");
  reply.code(httpStatus).send({ ...opts.body, error: trimmedErrorMessage(err) });
}
