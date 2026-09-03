// externalApprover: per-gate approvalMode re-check for the external (:8090) surface.
//
// Unlike preAuthApprover (scheduler standing consent), this re-reads the agent's
// current approvalMode on every gate. A mid-run admin flip from "auto" to
// "needs_approval" causes all subsequent NEEDS_HUMAN gates to deny — the run
// continues but no new tool calls are auto-approved.
//
// WHY cachedApprovalMode: repeated gates in one run would otherwise hammer the
// broker south RPC. The cache TTL is short (5 s default) so a flip takes effect
// within one cache interval. On any fetch failure the gate denies (fail closed).
import type { Approver } from "../broker/governance.js";
import type { Logger } from "../log.js";

// cachedApprovalMode wraps a fetch function with a short-TTL memoize.
//
// nowFn is injectable for deterministic tests (defaults to Date.now).
// A fetch rejection resolves to a sentinel that is not "auto", ensuring the
// gate fails closed. Failures are never cached — the next call re-fetches.
export function cachedApprovalMode(
  fetch: () => Promise<string>,
  ttlMs = 5_000,
  nowFn: () => number = Date.now,
): () => Promise<string> {
  let cachedValue: string | undefined;
  let cachedAt: number | undefined;

  return async () => {
    const now = nowFn();
    if (cachedValue !== undefined && cachedAt !== undefined && now - cachedAt < ttlMs) {
      return cachedValue;
    }
    try {
      const value = await fetch();
      cachedValue = value;
      cachedAt = now;
      return value;
    } catch {
      // Failure: do not cache, fail closed — return a non-"auto" sentinel.
      cachedValue = undefined;
      cachedAt = undefined;
      return "error";
    }
  };
}

// externalApprover builds an Approver that re-checks approvalMode on each gate.
//
// If mode is not "auto" (including on south RPC error — cachedApprovalMode
// resolves to "error") the gate denies immediately. Otherwise behaves like
// preAuthApprover: allow iff toolId is in approvedTools.
export function externalApprover(
  approvedTools: string[],
  getApprovalMode: () => Promise<string>,
  log: Logger,
): Approver {
  const allow = new Set(approvedTools);
  return async (info) => {
    let mode: string;
    // WHY: in production, getApprovalMode is always a cachedApprovalMode-wrapped
    // function that swallows rejections and returns the "error" sentinel — it never
    // throws. This catch is defense-in-depth: if a caller passes a raw, unwrapped
    // getApprovalMode that does throw, we still fail closed rather than letting the
    // gate pass by default.
    try {
      mode = await getApprovalMode();
    } catch (err) {
      log.info({ tool: info.toolId, err: String(err) }, "external approvalMode no longer auto — denying");
      return false;
    }
    if (mode !== "auto") {
      log.info({ tool: info.toolId, mode }, "external approvalMode no longer auto — denying");
      return false;
    }
    return allow.has(info.toolId);
  };
}
