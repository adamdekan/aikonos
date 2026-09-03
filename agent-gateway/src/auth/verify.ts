// JWT verification via JWKS — the gateway's trust anchor for incoming bearers.
// Production: createRemoteJWKSet against the container-facing Keycloak URL,
// cached at module scope. Tests: inject a local key resolver via the second
// argument (avoids any network call).
import { jwtVerify, createRemoteJWKSet } from "jose";
import type { JWTVerifyGetKey, JWTPayload } from "jose";
import type { Config } from "../config.js";

export interface Principal {
  sub: string;
  email: string;
  tenant: string;
  token: string;
}

// The resolver is the key-getter jwtVerify accepts as its second argument.
// Tests pass a local variant resolving a single in-memory key; production
// passes the cached remote JWKS set.
export type JwksResolver = JWTVerifyGetKey;

export interface VerifyOptions {
  issuer: string;
  audience: string;
  // Claim names for the principal + tenant. Default sub/tenant_id (Keycloak);
  // set oid/tid for Entra. Kept here so the Entra swap is config-only.
  subjectClaim?: string;
  tenantClaim?: string;
}

// JWTPayload is index-signed (`[k: string]: unknown`), so custom Keycloak claims
// (email / preferred_username / tenant_id) read as `unknown`. Narrow each to a
// string without casting — a non-string claim is treated as absent.
function stringClaim(payload: JWTPayload, key: string): string | undefined {
  const v = payload[key];
  return typeof v === "string" ? v : undefined;
}

/** Validates an OIDC bearer JWT via JWKS, returns a Principal on success, throws on invalid/expired token or missing required claims. */
export async function verifyBearer(
  token: string,
  resolver: JwksResolver,
  opts: VerifyOptions,
): Promise<Principal> {
  const { payload } = await jwtVerify(token, resolver, {
    issuer: opts.issuer,
    audience: opts.audience,
    // Absorb host↔IdP clock skew; jose defaults to 0, which rejects tokens a
    // single second past exp and any token whose iat/nbf is barely in the future.
    clockTolerance: 60,
  });

  const subjectClaim = opts.subjectClaim ?? "sub";
  const sub = stringClaim(payload, subjectClaim);
  if (!sub) {
    throw new Error(`JWT payload missing required ${subjectClaim} claim`);
  }

  const email = stringClaim(payload, "email") ?? stringClaim(payload, "preferred_username");
  if (!email) {
    throw new Error("JWT payload missing email or preferred_username claim");
  }

  const tenantClaim = opts.tenantClaim ?? "tenant_id";
  const tenant = stringClaim(payload, tenantClaim);
  if (!tenant) {
    throw new Error(`missing ${tenantClaim} claim in JWT payload`);
  }

  return { sub, email, tenant, token };
}

// Production JWKS set, lazily created and then cached for the process lifetime.
// createRemoteJWKSet already re-fetches when a kid is not found in the local
// cache, so rotation is handled automatically. When the JWKS URL is unset
// (OIDC not configured) we return a resolver that fails clearly on use, rather
// than throwing `new URL("")` at module load — the compose default always sets
// AIKONOS_OIDC_JWKS_URL.
let cachedJwks: JwksResolver | undefined;

export function productionResolver(cfg: Config): JwksResolver {
  if (!cachedJwks) {
    if (!cfg.oidcJwksUrl) {
      cachedJwks = async () => {
        throw new Error("OIDC not configured: AIKONOS_OIDC_JWKS_URL is empty");
      };
    } else {
      cachedJwks = createRemoteJWKSet(new URL(cfg.oidcJwksUrl));
    }
  }
  return cachedJwks;
}
