// NATS → ring-buffer → SSE audit bridge.
// Mirrors observability/server.mjs but lives inside the gateway so the unified
// webui can drop the standalone observability service.
//
// Resilience: NATS connect failures retry every 3s; the gateway continues
// serving all other routes while NATS is down.

import { connect, StringCodec } from "nats";
import type { Logger } from "pino";
import { status } from "@grpc/grpc-js";
import type { FastifyInstance } from "fastify";
import { requireUser } from "../auth/require-user.js";
import type { JwksResolver, VerifyOptions } from "../auth/verify.js";
import type { NorthClient } from "../broker/north.js";

const RING_CAP = 1000;
const REPLAY_N = 200;
const PING_MS  = 15_000;
const RETRY_MS = 3_000;
// H-07: live streams one principal may hold open at once.
const MAX_STREAMS_PER_USER = 3;
// F59: how often a live stream re-runs the admin probe, so a demotion, block
// or revoked session takes effect on an open stream, not only at reconnect.
const RECHECK_MS = 60_000;
// setTimeout's ceiling; a token living longer is re-checked by RECHECK_MS anyway.
const MAX_TIMER_MS = 2 ** 31 - 1;

function isRecord(v: unknown): v is Record<string, unknown> {
  return v !== null && typeof v === "object" && !Array.isArray(v);
}

// Exported so tests can drive the pure buffer/fan-out logic without NATS.
export const buffer: object[] = [];
// What one reader may see: its tenant, and only its own events unless it is a
// tenant admin (H-07). user undefined = the whole tenant.
export interface AuditScope {
  tenant: string;
  user?: string;
}

// Maps each connected SSE client to its scope — undefined means unfiltered.
const clients = new Map<import("node:http").ServerResponse, AuditScope | undefined>();
const streamsPerUser = new Map<string, number>();

// matchesScope is the single scoping rule, used by both the replay on
// connect and record()'s live fan-out. An event is a member's own when they
// acted or it was done on their behalf (context.obo.subject, AG-17).
function matchesScope(ev: object, scope: AuditScope | undefined): boolean {
  if (!scope) return true;
  const e = ev as { tenant_id?: unknown; actor_user_id?: unknown; context?: { obo?: { subject?: unknown } } };
  if (e.tenant_id !== scope.tenant) return false;
  if (scope.user === undefined) return true;
  return e.actor_user_id === scope.user || e.context?.obo?.subject === scope.user;
}

export function record(ev: object): void {
  buffer.push(ev);
  if (buffer.length > RING_CAP) buffer.shift();
  const line = `data: ${JSON.stringify(ev)}\n\n`;
  for (const [res, scope] of clients) {
    if (!matchesScope(ev, scope)) continue;
    // A slow client whose write() returns false is disconnected immediately
    // rather than allowed to accumulate an unbounded backlog — reconnection
    // relies on the SSE `retry:` hint + replay.
    if (res.write(line) === false) res.destroy();
  }
}

export function fanOutClients(): Map<import("node:http").ServerResponse, AuditScope | undefined> {
  return clients;
}

export interface AuditRoutesCtx {
  jwksResolver: JwksResolver;
  verifyOpts: VerifyOptions;
  north: Pick<NorthClient, "getObservabilityInfo">;
  // Test seam for the live-stream re-check interval (default RECHECK_MS).
  recheckMs?: number;
}

// scopeFor gives tenant admins the whole tenant and everyone else their own
// events (H-07). The probe is a cheap tenant-admin-gated broker RPC; any
// other error falls back to the member scope. F59: undefined means the broker
// no longer accepts the principal at all — UNAUTHENTICATED (expired or
// revoked session) or the RevocationInterceptor's "user is blocked" — and the
// caller refuses or ends the stream instead of downgrading it.
// ponytail: borrows GetObservabilityInfo's admin gate; a dedicated
// "am I admin" RPC if a second caller needs one.
async function scopeFor(ctx: AuditRoutesCtx, p: { tenant: string; sub: string; token: string }): Promise<AuditScope | undefined> {
  try {
    await ctx.north.getObservabilityInfo({}, p.token);
    return { tenant: p.tenant };
  } catch (err) {
    const e = err as { code?: unknown; details?: unknown };
    if (e?.code === status.UNAUTHENTICATED || (e?.code === status.PERMISSION_DENIED && e.details === "user is blocked")) {
      return undefined;
    }
    return { tenant: p.tenant, user: p.sub };
  }
}

// The broker is single-tenant-per-deployment (callerIdentity forces `tenant`
// from the JWT; singleton.go enforces one broker per deployment) — a
// legitimate cross-tenant admin cannot exist on this surface, so the audit
// filter is always the verified principal's own tenant. A `?tenant=` is
// honored only when it already equals that tenant; any other value is
// silently ignored rather than treated as an error.
export function registerAuditRoutes(app: FastifyInstance, ctx: AuditRoutesCtx): void {
  app.get<{ Querystring: { limit?: string; tenant?: string } }>(
    "/api/audit",
    async (req, reply) => {
      const principal = await requireUser(req, reply, ctx.jwksResolver, ctx.verifyOpts);
      if (!principal) return;
      const scope  = await scopeFor(ctx, principal);
      if (!scope) {
        reply.code(401).send({ error: "session no longer valid" });
        return;
      }
      const limit  = Number(req.query.limit ?? 200) || 200;
      const src    = buffer.filter((e) => matchesScope(e, scope));
      reply.send({ events: src.slice(-limit).reverse() });
    },
  );

  app.get<{ Querystring: { tenant?: string } }>("/api/audit/stream", async (req, reply) => {
    const principal = await requireUser(req, reply, ctx.jwksResolver, ctx.verifyOpts);
    if (!principal) return;
    const open = streamsPerUser.get(principal.sub) ?? 0;
    if (open >= MAX_STREAMS_PER_USER) {
      reply.code(429).send({ error: "too many open audit streams" });
      return;
    }
    // Count the stream and register its teardown before the admin probe
    // awaits, so a parallel connect cannot slip past the cap and a client
    // that leaves mid-probe is still released.
    streamsPerUser.set(principal.sub, open + 1);
    let ping: ReturnType<typeof setInterval> | undefined;
    let recheck: ReturnType<typeof setInterval> | undefined;
    let expiry: ReturnType<typeof setTimeout> | undefined;
    let released = false;
    const release = (): void => {
      if (released) return;
      released = true;
      clearInterval(ping);
      clearInterval(recheck);
      clearTimeout(expiry);
      clients.delete(reply.raw);
      const n = (streamsPerUser.get(principal.sub) ?? 1) - 1;
      if (n > 0) streamsPerUser.set(principal.sub, n);
      else streamsPerUser.delete(principal.sub);
    };
    req.raw.on("close", release);
    const end = (): void => {
      release();
      reply.raw.end();
    };
    const scope = await scopeFor(ctx, principal);
    if (req.raw.destroyed) return;
    if (!scope) {
      release();
      reply.code(401).send({ error: "session no longer valid" });
      return;
    }
    reply.raw.writeHead(200, {
      "content-type":               "text/event-stream",
      "cache-control":              "no-cache",
      connection:                   "keep-alive",
      "access-control-allow-origin": "*",
    });
    reply.raw.write("retry: 3000\n\n");
    for (const ev of buffer.slice(-REPLAY_N).filter((e) => matchesScope(e, scope))) {
      reply.raw.write(`data: ${JSON.stringify(ev)}\n\n`);
    }
    clients.set(reply.raw, scope);
    ping = setInterval(() => {
      if (reply.raw.destroyed) return;
      reply.raw.write(": ping\n\n");
    }, PING_MS);
    // F59: the stream lives no longer than its bearer, and re-probes the
    // broker so a demotion narrows it and a block or revocation ends it.
    if (principal.exp !== undefined) {
      expiry = setTimeout(end, Math.min(Math.max(principal.exp * 1000 - Date.now(), 0), MAX_TIMER_MS));
    }
    recheck = setInterval(() => {
      void scopeFor(ctx, principal).then((next) => {
        if (released) return;
        if (next) clients.set(reply.raw, next);
        else end();
      });
    }, ctx.recheckMs ?? RECHECK_MS);
  });
}

// AuditConsumerStatus reports the consumer's readiness for /readyz. "disabled"
// means no NATS url is configured (caller passes natsUrl: undefined) — that is
// a deliberate opt-out, not a readiness failure.
export type AuditConsumerState = "connected" | "disconnected" | "disabled";

export interface AuditConsumerStatus {
  state: AuditConsumerState;
  lastError?: string;
}

export interface AuditConsumerHandle {
  stop(): Promise<void>;
  status(): AuditConsumerStatus;
}

// natsUrl/subject are injected by the caller (server.ts, from validated
// Config) rather than read from process.env here (F26) — undefined natsUrl
// disables the consumer, same semantics as the former explicit-empty-string
// env convention.
export interface AuditConsumerOptions {
  natsUrl?: string;
  subject?: string;
}

// nats.js ignores credentials in a server URL (nats.go reads them), so a
// compose-style nats://<token>@nats:4222 is split here: the token goes to
// connect() and only the bare URL is ever logged. user:pass@ maps to user/pass.
export function natsConnectOptions(natsUrl: string): { servers: string; token?: string; user?: string; pass?: string } {
  let u: URL;
  try {
    u = new URL(natsUrl);
  } catch {
    return { servers: natsUrl };
  }
  if (!u.username) return { servers: natsUrl };
  const user = decodeURIComponent(u.username);
  const pass = decodeURIComponent(u.password);
  u.username = "";
  u.password = "";
  const servers = u.toString();
  return pass ? { servers, user, pass } : { servers, token: user };
}

export function startAuditConsumer(log: Logger, opts: AuditConsumerOptions = {}): AuditConsumerHandle {
  const natsUrl = opts.natsUrl;
  const subject = opts.subject ?? "aikonos.audit.>";
  const sc = StringCodec();

  if (natsUrl === undefined) {
    log.info("audit consumer disabled (no NATS url configured)");
    return {
      stop: () => Promise.resolve(),
      status: () => ({ state: "disabled" }),
    };
  }

  let stopped = false;
  let connected = false;
  let lastError: string | undefined;
  // Awaitable<NatsConnection> — kept loosely typed to match `connect()`'s
  // return type without importing it just for this local.
  let currentConn: Awaited<ReturnType<typeof connect>> | undefined;

  // Fire-and-forget; never throws (all errors logged + retried). Exits the
  // reconnect loop once stopped is set by stop().
  void (async () => {
    while (!stopped) {
      try {
        const conn = natsConnectOptions(natsUrl);
        const nc = await connect({ ...conn, reconnect: true, maxReconnectAttempts: -1 });
        currentConn = nc;
        connected = true;
        lastError = undefined;
        log.info({ natsUrl: conn.servers, subject }, "audit consumer connected to NATS");
        const sub = nc.subscribe(subject);
        for await (const m of sub) {
          if (stopped) break;
          try {
            const parsed: unknown = JSON.parse(sc.decode(m.data));
            if (!isRecord(parsed)) {
              log.warn({ subject: m.subject }, "audit NATS message is not a JSON object — skipping");
              continue;
            }
            record(parsed);
          } catch {
            // unparseable JSON — skip malformed message, loop continues
          }
        }
        connected = false;
      } catch (err) {
        connected = false;
        lastError = String(err);
        if (!stopped) log.warn({ err: String(err) }, "audit NATS connect failed, retrying");
      }
      if (stopped) break;
      await new Promise<void>((r) => setTimeout(r, RETRY_MS));
    }
  })();

  return {
    stop: async () => {
      stopped = true;
      if (currentConn) {
        try {
          await currentConn.close();
        } catch {
          // already closed — ignore
        }
      }
    },
    status: () => (stopped ? { state: "disconnected", lastError } : { state: connected ? "connected" : "disconnected", lastError }),
  };
}
