// NATS → ring-buffer → SSE audit bridge.
// Mirrors observability/server.mjs but lives inside the gateway so the unified
// webui can drop the standalone observability service.
//
// Resilience: NATS connect failures retry every 3s; the gateway continues
// serving all other routes while NATS is down.

import { connect, StringCodec } from "nats";
import type { Logger } from "pino";
import type { FastifyInstance } from "fastify";
import { requireUser } from "../auth/require-user.js";
import type { JwksResolver, VerifyOptions } from "../auth/verify.js";

const RING_CAP = 1000;
const REPLAY_N = 200;
const PING_MS  = 15_000;
const RETRY_MS = 3_000;

function isRecord(v: unknown): v is Record<string, unknown> {
  return v !== null && typeof v === "object" && !Array.isArray(v);
}

// Exported so tests can drive the pure buffer/fan-out logic without NATS.
export const buffer: object[] = [];
// Maps each connected SSE client to its optional tenant filter (F44) —
// undefined means unfiltered (today's behavior).
const clients = new Map<import("node:http").ServerResponse, string | undefined>();

// matchesTenant is the single tenant-scoping rule, used both by filterByTenant
// (replay) and record() (live fan-out) so it lives in exactly one place.
function matchesTenant(ev: object, tenant: string | undefined): boolean {
  if (!tenant) return true;
  return (ev as Record<string, unknown>).tenant_id === tenant;
}

// filterByTenant is the single replay-filter used both by the initial replay
// on connect and (indirectly, per-client) by record()'s live fan-out.
export function filterByTenant(events: object[], tenant: string | undefined): object[] {
  return events.filter((e) => matchesTenant(e, tenant));
}

export function record(ev: object): void {
  buffer.push(ev);
  if (buffer.length > RING_CAP) buffer.shift();
  const line = `data: ${JSON.stringify(ev)}\n\n`;
  for (const [res, tenant] of clients) {
    if (!matchesTenant(ev, tenant)) continue;
    // A slow client whose write() returns false is disconnected immediately
    // rather than allowed to accumulate an unbounded backlog — reconnection
    // relies on the SSE `retry:` hint + replay.
    if (res.write(line) === false) res.destroy();
  }
}

export function fanOutClients(): Map<import("node:http").ServerResponse, string | undefined> {
  return clients;
}

export interface AuditRoutesCtx {
  jwksResolver: JwksResolver;
  verifyOpts: VerifyOptions;
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
      const tenant = principal.tenant;
      const limit  = Number(req.query.limit ?? 200) || 200;
      const src    = buffer.filter((e) => matchesTenant(e, tenant));
      reply.send({ events: src.slice(-limit).reverse() });
    },
  );

  app.get<{ Querystring: { tenant?: string } }>("/api/audit/stream", async (req, reply) => {
    const principal = await requireUser(req, reply, ctx.jwksResolver, ctx.verifyOpts);
    if (!principal) return;
    const tenant = principal.tenant;
    reply.raw.writeHead(200, {
      "content-type":               "text/event-stream",
      "cache-control":              "no-cache",
      connection:                   "keep-alive",
      "access-control-allow-origin": "*",
    });
    reply.raw.write("retry: 3000\n\n");
    for (const ev of filterByTenant(buffer.slice(-REPLAY_N), tenant)) {
      reply.raw.write(`data: ${JSON.stringify(ev)}\n\n`);
    }
    clients.set(reply.raw, tenant);
    const ping = setInterval(() => {
      if (reply.raw.destroyed) return;
      reply.raw.write(": ping\n\n");
    }, PING_MS);
    req.raw.on("close", () => {
      clearInterval(ping);
      clients.delete(reply.raw);
    });
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
        const nc = await connect({ servers: natsUrl, reconnect: true, maxReconnectAttempts: -1 });
        currentConn = nc;
        connected = true;
        lastError = undefined;
        log.info({ natsUrl, subject }, "audit consumer connected to NATS");
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
