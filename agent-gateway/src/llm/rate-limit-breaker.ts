// CP4.1 — circuit breaker over the south
// CheckRateLimit RPC. Wraps the raw RPC call so it can distinguish a
// transport failure (broker unreachable, DEADLINE_EXCEEDED, connection
// refused — the RPC itself rejects) from an explicit allowed=false response
// (a real rate-limit denial — the RPC resolved normally). Only transport
// failures count toward the breaker; an explicit denial always resets it,
// same as a success.
import type { CheckRateLimitResponse } from "../../gen/ts/proto/broker.js";
import type { RateLimitChecker } from "./egress-proxy.js";

// Spend-caps CP4: userId is optional so every pre-existing caller (production
// and test) that predates per-user spend caps keeps compiling unchanged.
export type CheckRateLimitCall = (
  tenantId: string,
  agentId: string,
  provider: string,
  userId?: string,
) => Promise<CheckRateLimitResponse>;

export interface RateLimitBreakerLog {
  warn: (obj: Record<string, unknown>, msg: string) => void;
}

export interface RateLimitBreakerOptions {
  // Consecutive transport failures before the breaker opens.
  threshold: number;
}

// Once OPEN, only every Nth request attempts the real RPC (a half-open
// probe) — every other open-state request fast-fails without calling out to
// the broker at all. This is what makes the breaker a load-shedding
// mechanism instead of just a slow deny: a down broker stops being hit on
// every request. A plain request-count interval (not wall-clock time) keeps
// this deterministic under test and avoids a banned Date.now() dependency.
// 10 is a simple, generous interval — frequent enough to recover quickly
// once the broker comes back, sparse enough that a sustained outage isn't
// still hammering it once per request.
const OPEN_STATE_PROBE_INTERVAL = 10;

/**
 * Below `threshold` consecutive transport failures: fail-open (the design
 * intent that a broker restart must not black out LLM egress) — every call
 * still attempts the real RPC.
 *
 * At/above `threshold`: the breaker is OPEN. Every open-state call fast-fails
 * (denies immediately, without calling the RPC) EXCEPT every
 * `OPEN_STATE_PROBE_INTERVAL`th call, which is a half-open probe: it attempts
 * the real RPC. A probe that resolves — allowed true or false — is a
 * transport success and immediately closes the breaker (resets the
 * counter); a probe that rejects keeps the breaker open.
 */
export function createRateLimitBreaker(
  call: CheckRateLimitCall,
  opts: RateLimitBreakerOptions,
  log: RateLimitBreakerLog,
): RateLimitChecker {
  let consecutiveFailures = 0;
  // Count of requests seen while OPEN, since the breaker last opened (or
  // since the previous probe) — used only to decide when the next probe is
  // due. Reset to 0 whenever the breaker closes.
  let openRequestCount = 0;

  return async (tenantId, agentId, provider, userId) => {
    const isOpen = consecutiveFailures >= opts.threshold;
    if (isOpen) {
      openRequestCount += 1;
      const isProbe = openRequestCount % OPEN_STATE_PROBE_INTERVAL === 0;
      if (!isProbe) {
        throw new Error("rate limit circuit breaker open: broker unreachable");
      }
      // Falls through to attempt the real RPC as a half-open probe.
    }

    let resp: CheckRateLimitResponse;
    try {
      resp = await call(tenantId, agentId, provider, userId);
    } catch (err) {
      consecutiveFailures += 1;
      if (consecutiveFailures >= opts.threshold) {
        throw new Error("rate limit circuit breaker open: broker unreachable");
      }
      log.warn({ err, consecutiveFailures }, "rate-limit check RPC failed — allowing request");
      return;
    }
    // The RPC resolved — whether allowed or an explicit denial, this is a
    // transport success and must reset the breaker.
    consecutiveFailures = 0;
    openRequestCount = 0;
    if (!resp.allowed) {
      throw new Error(resp.limitType ? `rate limit exceeded: ${resp.limitType}` : "rate limit exceeded");
    }
  };
}
