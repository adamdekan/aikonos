// externalApprover: per-gate approvalMode re-check for the external (:8090) surface.
//
// Unlike a scheduled run's standing consent, this re-reads the agent's current
// approvalMode on every gate. A mid-run admin flip from "auto" to
// "needs_approval" makes every later NEEDS_HUMAN gate wait for a human — the
// run continues but no new tool call is auto-approved.
//
// WHY cachedApprovalMode: repeated gates in one run would otherwise hammer the
// broker south RPC. The cache TTL is short (5 s default) so a flip takes effect
// within one cache interval. On any fetch failure the gate denies (fail closed).
import type { Approver } from "../broker/governance.js";
import type { Logger } from "../log.js";
import { unattendedApprover } from "../scheduler/ticker.js";

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
// A tool in approvedTools is pre-approved only while mode is "auto" and the
// org master allows unattended runs (H-06). Anything else — including a south
// RPC error (cachedApprovalMode resolves to "error") — is not pre-approved:
// the run waits for a human to decide it in the Inbox (AG-07 slice 2).
export function externalApprover(
  approvedTools: string[],
  getApprovalMode: () => Promise<string>,
  log: Logger,
  unattended = true,
): Approver {
  const allow = new Set(unattended ? approvedTools : []);
  const waitForHuman = unattendedApprover([], log);
  return async (info, decided) => {
    let mode: string;
    // WHY: in production, getApprovalMode is always a cachedApprovalMode-wrapped
    // function that swallows rejections and returns the "error" sentinel — it never
    // throws. This catch is defense-in-depth: if a caller passes a raw, unwrapped
    // getApprovalMode that does throw, we still fail closed rather than letting the
    // gate pass by default.
    try {
      mode = await getApprovalMode();
    } catch {
      mode = "error";
    }
    if (mode === "auto" && allow.has(info.toolId)) return true;
    log.info({ tool: info.toolId, mode }, "external run: not pre-approved — waiting for a human");
    return waitForHuman(info, decided);
  };
}
