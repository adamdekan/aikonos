// Single callback→promise conversion point for grpc-js unary client calls.
// Every north/south RPC wrapper routes through this instead of hand-writing
// `new Promise((resolve, reject) => this.client.x(req, meta, cb))`.
//
// CP1 (rpc-twins-tails): every call gets a deadline, so a hung broker call
// fails with DEADLINE_EXCEEDED instead of hanging the gateway request
// forever. The deadline is a module-level default (30s) settable once at
// startup via setUnaryDeadlineMs() — the generated grpc-js client methods
// already accept the 4-arg (req, metadata, options, callback) form.
import type { CallOptions, Metadata, ServiceError } from "@grpc/grpc-js";

export type UnaryCall<TReq, TResp> = (
  req: TReq,
  metadata: Metadata,
  options: CallOptions,
  callback: (error: ServiceError | null, response: TResp) => void,
) => unknown;

let deadlineMs = 30_000;

/** Sets the deadline (ms) applied to every subsequent unary() call. Called once at startup. */
export function setUnaryDeadlineMs(ms: number): void {
  deadlineMs = ms;
}

export function unary<TReq, TResp>(
  fn: UnaryCall<TReq, TResp>,
  req: TReq,
  metadata: Metadata,
): Promise<TResp> {
  const options: CallOptions = { deadline: new Date(Date.now() + deadlineMs) };
  return new Promise((resolve, reject) => {
    fn(req, metadata, options, (err, resp) => (err ? reject(err) : resolve(resp)));
  });
}
