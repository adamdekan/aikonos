// API-key authentication for the external surface.
// Reads `Authorization: Bearer tk_…`, sha256-hashes the raw key (node crypto),
// calls south.resolveAgentApiKey with the hex digest, and returns the resolved
// principal or a 401 error (403 when the broker refuses a valid key on AG-17
// grounds). The raw key never leaves this module.
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
): Promise<AuthResult> {
  if (!authHeader) return fail("missing Authorization header");

  // Require `Bearer tk_…` format.
  const match = /^Bearer (tk_\S+)$/.exec(authHeader);
  if (!match) return fail("invalid Authorization format; expected Bearer tk_…");

  const rawKey = match[1];
  const keyHashHex = sha256Hex(rawKey);

  try {
    const resp = await south.resolveAgentApiKey({ keyHashHex, tenantId, userAssertion: userAssertion ?? "" });
    if (!resp.valid) return fail("key revoked, expired or not found");
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
