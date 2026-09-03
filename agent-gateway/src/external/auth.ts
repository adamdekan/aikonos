// API-key authentication for the external surface.
// Reads `Authorization: Bearer tk_…`, sha256-hashes the raw key (node crypto),
// calls south.resolveAgentApiKey with the hex digest, and returns the resolved
// principal or a 401 error. The raw key never leaves this module.
import { createHash } from "node:crypto";
import type { SouthClient } from "../broker/south.js";
import type { Logger } from "../log.js";

export interface ResolvedPrincipal {
  agentId: string;
  tenantId: string;
  principal: string; // "svc-<agentId>"
  ownerGrant: string; // broker-minted HMAC grant; forwarded to createGatewayTask
}

type AuthOk = { ok: true; principal: ResolvedPrincipal };
type AuthFail = { ok: false; status: 401; message: string };
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
): Promise<AuthResult> {
  if (!authHeader) return fail("missing Authorization header");

  // Require `Bearer tk_…` format.
  const match = /^Bearer (tk_\S+)$/.exec(authHeader);
  if (!match) return fail("invalid Authorization format; expected Bearer tk_…");

  const rawKey = match[1];
  const keyHashHex = sha256Hex(rawKey);

  try {
    const resp = await south.resolveAgentApiKey({ keyHashHex, tenantId });
    if (!resp.valid) return fail("key revoked or not found");
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
    log?.warn({ err: String(err) }, "external auth: key resolution failed");
    return fail("key resolution failed");
  }
}
