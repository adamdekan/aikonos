// Separate hardened Fastify server for the external API surface (`:8090`).
// This instance NEVER mounts requireUser / OIDC routes. It registers:
//   - @fastify/rate-limit (cfg.externalRateLimit req/min per client IP, and
//     again per authenticated agent in rest.ts)
//   - @fastify/cors (cfg.externalCorsOrigins allowlist)
//   - body-size cap (256 KiB; the route enforces a 16 KiB prompt cap and a
//     200 KiB history-content cap, leaving room for JSON framing + up to
//     100 history turns)
//   - POST /v1/agents/:id/invoke
//   - /scim/v2/* (AG-16, SCIM 2.0 provisioning; token-authenticated by the broker)
//   - GET /oauth/client-metadata.json (AG-09, public OAuth client metadata)
import Fastify, { type FastifyInstance } from "fastify";
import cors from "@fastify/cors";
import rateLimit from "@fastify/rate-limit";
import type { Config } from "../config.js";
import type { Logger } from "../log.js";
import type { BrokerClients } from "../broker/clients.js";
import type { ChildSupervisor } from "../ipc/supervisor.js";
import { registerExternalRoutes } from "./rest.js";
import { registerScimRoutes } from "./scim.js";
import { registerClientMetadataRoute } from "./client-metadata.js";

// Body cap for the external surface. Sized to cover the 16 KiB prompt cap
// plus up to 100 history turns totalling 200 KiB of content, with headroom
// for JSON framing.
const BODY_LIMIT = 256 * 1024;

// buildExternalApp constructs and configures a Fastify instance for the
// external surface. Exported separately so tests can call it without starting
// a real TCP listener.
export function buildExternalApp(
  cfg: Config,
  clients: BrokerClients,
  log: Logger,
  supervisor: ChildSupervisor,
): FastifyInstance {
  // F56: trustProxy is the configured hop count or proxy list (default one
  // hop: Traefik), so req.ip is the real client and not the proxy every
  // request shares. Never `true`: a direct client could then pick its bucket.
  // A hop count becomes the same per-hop function Fastify builds from a number
  // at runtime (its types only admit the function form).
  const trust = cfg.externalTrustProxy ?? 1;
  const app = Fastify({
    logger: false,
    bodyLimit: BODY_LIMIT,
    trustProxy: typeof trust === "number" ? (_addr: string, hop: number) => hop < trust : trust,
  });

  // Rate-limit: max cfg.externalRateLimit requests per minute per client IP.
  // Runs before auth, so it is the bound on unauthenticated traffic (and the
  // south RPC each invoke's key resolution costs); SCIM shares it.
  void app.register(rateLimit, {
    max: cfg.externalRateLimit,
    timeWindow: "1 minute",
  });

  // CORS: allow listed origins (empty = no CORS headers, same-origin only).
  if (cfg.externalCorsOrigins.length > 0) {
    void app.register(cors, {
      origin: cfg.externalCorsOrigins,
      methods: ["POST", "OPTIONS"],
    });
  }

  // Health check — separate from the internal :8080 /healthz.
  app.get("/healthz", async () => ({ ok: true, surface: "external" }));

  // Registered as a plugin so the routes load after @fastify/rate-limit: its
  // onRoute hook only limits routes added once it has loaded, and routes added
  // directly here were never limited at all (F56).
  void app.register(async (scope) => {
    registerExternalRoutes(scope, cfg, clients, log, supervisor);
    registerScimRoutes(scope, cfg, clients, log);
    registerClientMetadataRoute(scope, cfg);
  });

  return app;
}

// startExternalServer starts the external Fastify instance and resolves when
// the server is listening.
export async function startExternalServer(
  cfg: Config,
  clients: BrokerClients,
  log: Logger,
  supervisor: ChildSupervisor,
): Promise<() => Promise<void>> {
  const app = buildExternalApp(cfg, clients, log, supervisor);

  const addr = await app.listen({ port: cfg.externalPort, host: "0.0.0.0" });
  log.info({ addr, port: cfg.externalPort }, "external API surface listening");

  return () => app.close();
}
