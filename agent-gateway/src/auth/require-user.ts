// requireUser — extract and verify the bearer from an incoming Fastify request.
// Returns the verified Principal on success. On failure, writes 401 and returns
// null so the route handler can bail immediately.
import { verifyBearer } from "./verify.js";
import type { Principal, JwksResolver, VerifyOptions } from "./verify.js";

interface RequestLike {
  headers: Record<string, string | string[] | undefined>;
  // Fastify attaches a pino logger to every request; optional so tests can pass
  // a bare header bag.
  log?: { warn(obj: object, msg: string): void };
}

interface ReplyLike {
  code(n: number): ReplyLike;
  send(body: unknown): void;
}

/** Extracts and verifies the Authorization: Bearer token from a Fastify request; writes 401 and returns null when the token is missing, invalid, or missing required claims (e.g. subject/tenant). */
export async function requireUser(
  req: RequestLike,
  reply: ReplyLike,
  resolver: JwksResolver,
  opts: VerifyOptions,
): Promise<Principal | null> {
  const authHeader = req.headers["authorization"];
  const headerStr = Array.isArray(authHeader) ? authHeader[0] : authHeader;

  if (!headerStr?.startsWith("Bearer ")) {
    reply.code(401).send({ error: "Authorization: Bearer <token> required" });
    return null;
  }

  const token = headerStr.slice(7);
  try {
    return await verifyBearer(token, resolver, opts);
  } catch (err) {
    // The client sees a generic 401, but the real jose reason (expired vs
    // audience vs signature) goes to the server log so cadence/cause is
    // diagnosable without a browser.
    req.log?.warn({ err: err instanceof Error ? err.message : String(err) }, "bearer verification failed");
    reply.code(401).send({ error: "invalid or expired bearer token" });
    return null;
  }
}
