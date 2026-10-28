// API-key authentication for the external surface.
// Reads `Authorization: Bearer tk_…`, sha256-hashes the raw key (node crypto),
// calls south.resolveAgentApiKey with the hex digest, and returns the resolved
// principal or a 401 error (403 when the broker refuses a valid key on AG-17
// grounds). The raw key never leaves this module.
// AG-05: `Bearer <JWT>` is a signed client assertion (RFC 7523); the broker
// verifies it against the agent's registered public keys and `audience`
// (AIKONOS_EXTERNAL_AUDIENCE). No audience configured ⇒ assertions refused.
import { createHash } from "node:crypto";
import { status } from "@grpc/grpc-js";
import type { SouthClient } from "../broker/south.js";
import type { Logger } from "../log.js";

export interface ResolvedPrincipal {
  agentId: string;
  tenantId: string;
  principal: string; // assertion subject (AG-17 S6), else "svc-<agentId>"
  ownerGrant: string; // broker-minted HMAC grant; forwarded to createGatewayTask
}

type AuthOk = { ok: true; principal: ResolvedPrincipal };
type AuthFail = { ok: false; status: 401 | 403; message: string };
export type AuthResult = AuthOk | AuthFail;

function fail(message: string): AuthFail {
  return { ok: false, status: 401, message };
}

// sha256Hex computes the hex-encoded SHA-256 digest of a UTF-8 string.
function sha256Hex(input: string): string {
  return createHash("sha256").update(input, "utf8").digest("hex");
}

// authenticateApiKey validates the Authorization header value against the broker
// south resolve RPC. Exported for testing without a real Fastify instance.
export async function authenticateApiKey(
  authHeader: string | undefined,
  tenantId: string,
  south: SouthClient,
  log?: Logger,
  userAssertion?: string, // X-Aikonos-Subject-Token; the broker validates it
  audience = "", // AG-05: expected aud of a client assertion
): Promise<AuthResult> {
  if (!authHeader) return fail("missing Authorization header");

  // `Bearer tk_…`, or `Bearer <header>.<payload>.<signature>` (AG-05).
  const key = /^Bearer (tk_\S+)$/.exec(authHeader);
  const jwt = /^Bearer ([\w-]+\.[\w-]+\.[\w-]+)$/.exec(authHeader);
  if (!key && !jwt) return fail("invalid Authorization format; expected Bearer tk_… or a signed client assertion");
  if (jwt && !audience) return fail("client assertions are not enabled on this gateway");

  const req = key
    ? { keyHashHex: sha256Hex(key[1]), clientAssertion: "", audience: "" }
    : { keyHashHex: "", clientAssertion: jwt![1], audience };

  try {
    const resp = await south.resolveAgentApiKey({ ...req, tenantId, userAssertion: userAssertion ?? "" });
    if (!resp.valid) return fail(key ? "key revoked, expired or not found" : "client assertion rejected");
    // A pre-S6 broker drops user_assertion and answers as svc-; never run that.
    if (userAssertion && resp.principal.startsWith("svc-")) return fail("user assertion not honoured");
    return {
      ok: true,
      principal: {
        agentId: resp.agentId,
        tenantId: resp.tenantId,
        principal: resp.principal,
        ownerGrant: resp.ownerGrant,
      },
    };
  } catch (err) {
    // The key is valid but the broker refused it (bad user assertion, or
    // identity.obo=enforce); its details are a short safe reason.
    const e = err as { code?: unknown; details?: unknown };
    if (e?.code === status.PERMISSION_DENIED) {
      return { ok: false, status: 403, message: typeof e.details === "string" && e.details ? e.details : "forbidden" };
    }
    log?.warn({ err: String(err) }, "external auth: key resolution failed");
    return fail("key resolution failed");
  }
}
