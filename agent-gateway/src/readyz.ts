// CP3 (F9): pure /readyz evaluation, factored out of the route handler so it
// can be unit-tested without faking the full NorthClient/AuditConsumerHandle
// class shapes — the route in src/app.ts is a thin wrapper over this.
import { connectivityState } from "./broker/north.js";
import type { AuditConsumerStatus } from "./audit/stream.js";

export interface ReadyzDeps {
  // grpc-js ConnectivityState of the broker channel.
  brokerState: () => number;
  auditStatus: () => AuditConsumerStatus;
}

export interface ReadyzResult {
  ok: boolean;
  checks: { broker: string; audit: string };
}

/** READY or IDLE both count as "connectable" — IDLE means no RPC has been made yet (grpc-js lazily connects), not that the channel is broken. */
export function evaluateReadyz(deps: ReadyzDeps): ReadyzResult {
  const state = deps.brokerState();
  const brokerOk = state === connectivityState.READY || state === connectivityState.IDLE;

  const audit = deps.auditStatus();
  const auditOk = audit.state === "connected" || audit.state === "disabled";

  return {
    ok: brokerOk && auditOk,
    checks: {
      broker: brokerOk ? "ok" : `not-ready (state=${connectivityState[state]})`,
      audit: auditOk ? "ok" : `not-ready (state=${audit.state}${audit.lastError ? `: ${audit.lastError}` : ""})`,
    },
  };
}
