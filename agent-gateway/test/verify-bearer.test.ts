// Tests for verifyBearer — the gateway's JWKS-based JWT validator.
// Uses jose to generate a local RSA key + self-signed JWT so no Keycloak needed.
import { test } from "node:test";
import assert from "node:assert/strict";
import {
  generateKeyPair,
  exportJWK,
  SignJWT,
  importJWK,
} from "jose";
import type { JWK } from "jose";

// We need to inject a custom JWKS fetcher so the module doesn't try to reach
// the real Keycloak. verifyBearer accepts a custom key resolver via its
// optional second argument (the "fetch" seam used in tests).
//
// The production path uses createRemoteJWKSet; we expose the same interface:
// a function that takes (protectedHeader, token) and returns a KeyLike.
import { verifyBearer, type JwksResolver } from "../src/auth/verify.js";

// ── Key factory (shared across tests) ────────────────────────────────────────

async function makeKey(kid = "k1") {
  const { publicKey, privateKey } = await generateKeyPair("RS256");
  const jwk = await exportJWK(publicKey);
  const fullJwk: JWK = { ...jwk, kid, alg: "RS256", use: "sig" };
  return { publicKey, privateKey, jwk: fullJwk, kid };
}

async function mintToken(
  privateKey: Awaited<ReturnType<typeof generateKeyPair>>["privateKey"],
  kid: string,
  claims: Record<string, unknown>,
  options: { expiresIn?: string } = {},
): Promise<string> {
  const jwt = new SignJWT({ ...claims })
    .setProtectedHeader({ alg: "RS256", kid })
    .setIssuer("http://localhost:18080/realms/aikonos")
    .setAudience("aikonos-broker")
    .setIssuedAt();
  if (options.expiresIn !== "no-exp") {
    jwt.setExpirationTime(options.expiresIn ?? "1h");
  }
  return jwt.sign(privateKey);
}

// Build a JwksResolver that resolves keys from a local in-memory JWKS set.
async function localResolver(jwks: JWK[]): Promise<JwksResolver> {
  // Import each public key so we can match by kid.
  const keys = await Promise.all(
    jwks.map(async (jwk) => ({ kid: jwk.kid ?? "", key: await importJWK(jwk, "RS256") })),
  );
  return (_header, _token) => {
    // For simplicity always return the first key — tests use single-key sets.
    const k = keys[0];
    if (!k) throw new Error("no keys in local JWKS");
    return Promise.resolve(k.key);
  };
}

// ── Tests ────────────────────────────────────────────────────────────────────

test("verifyBearer: accepts a correctly-signed token", async () => {
  const { privateKey, jwk, kid } = await makeKey();
  const resolver = await localResolver([jwk]);
  const token = await mintToken(privateKey, kid, {
    sub: "user-uuid-1",
    email: "alice@example.com",
    tenant_id: "11111111-1111-1111-1111-111111111111",
  });

  const principal = await verifyBearer(token, resolver, {
    issuer: "http://localhost:18080/realms/aikonos",
    audience: "aikonos-broker",
  });

  assert.equal(principal.sub, "user-uuid-1");
  assert.equal(principal.email, "alice@example.com");
  assert.equal(principal.tenant, "11111111-1111-1111-1111-111111111111");
  assert.equal(principal.token, token);
});

test("verifyBearer: falls back to preferred_username when email is absent", async () => {
  const { privateKey, jwk, kid } = await makeKey();
  const resolver = await localResolver([jwk]);
  const token = await mintToken(privateKey, kid, {
    sub: "user-uuid-2",
    preferred_username: "bob@example.com",
    tenant_id: "11111111-1111-1111-1111-111111111111",
  });

  const principal = await verifyBearer(token, resolver, {
    issuer: "http://localhost:18080/realms/aikonos",
    audience: "aikonos-broker",
  });

  assert.equal(principal.email, "bob@example.com");
});

test("verifyBearer: rejects wrong issuer", async () => {
  const { privateKey, jwk, kid } = await makeKey();
  const resolver = await localResolver([jwk]);
  const token = await mintToken(privateKey, kid, {
    sub: "user-uuid-3",
    email: "eve@evil.com",
    tenant_id: "11111111-1111-1111-1111-111111111111",
  });

  await assert.rejects(
    () =>
      verifyBearer(token, resolver, {
        issuer: "http://different-issuer/realms/aikonos",
        audience: "aikonos-broker",
      }),
    /unexpected "iss" claim value/i,
  );
});

test("verifyBearer: rejects wrong audience", async () => {
  const { privateKey, jwk, kid } = await makeKey();
  const resolver = await localResolver([jwk]);
  const token = await mintToken(privateKey, kid, {
    sub: "user-uuid-4",
    email: "eve@evil.com",
    tenant_id: "11111111-1111-1111-1111-111111111111",
  });

  await assert.rejects(
    () =>
      verifyBearer(token, resolver, {
        issuer: "http://localhost:18080/realms/aikonos",
        audience: "wrong-audience",
      }),
    /unexpected "aud" claim value/i,
  );
});

test("verifyBearer: rejects a token expired beyond the clock-skew tolerance", async () => {
  const { privateKey, jwk, kid } = await makeKey();
  const resolver = await localResolver([jwk]);
  // 120s past — beyond the 60s clockTolerance, so still rejected.
  const jwt = new SignJWT({
    sub: "user-uuid-5",
    email: "eve@evil.com",
    tenant_id: "11111111-1111-1111-1111-111111111111",
    exp: Math.floor(Date.now() / 1000) - 120,
  })
    .setProtectedHeader({ alg: "RS256", kid })
    .setIssuer("http://localhost:18080/realms/aikonos")
    .setAudience("aikonos-broker")
    .setIssuedAt();
  const token = await jwt.sign(privateKey);

  await assert.rejects(
    () =>
      verifyBearer(token, resolver, {
        issuer: "http://localhost:18080/realms/aikonos",
        audience: "aikonos-broker",
      }),
    /"exp" claim timestamp check failed/i,
  );
});

test("verifyBearer: accepts a token expired within the clock-skew tolerance", async () => {
  const { privateKey, jwk, kid } = await makeKey();
  const resolver = await localResolver([jwk]);
  // 30s past — inside the 60s clockTolerance, so accepted (absorbs host↔IdP skew).
  const jwt = new SignJWT({
    sub: "user-uuid-8",
    email: "alice@example.com",
    tenant_id: "11111111-1111-1111-1111-111111111111",
    exp: Math.floor(Date.now() / 1000) - 30,
  })
    .setProtectedHeader({ alg: "RS256", kid })
    .setIssuer("http://localhost:18080/realms/aikonos")
    .setAudience("aikonos-broker")
    .setIssuedAt();
  const token = await jwt.sign(privateKey);

  const principal = await verifyBearer(token, resolver, {
    issuer: "http://localhost:18080/realms/aikonos",
    audience: "aikonos-broker",
  });
  assert.equal(principal.sub, "user-uuid-8");
});

test("verifyBearer: rejects a tampered signature", async () => {
  const { privateKey, jwk, kid } = await makeKey();
  const resolver = await localResolver([jwk]);
  const token = await mintToken(privateKey, kid, {
    sub: "user-uuid-6",
    email: "alice@example.com",
    tenant_id: "11111111-1111-1111-1111-111111111111",
  });

  // Corrupt the last character of the signature (third JWT segment).
  const parts = token.split(".");
  const sig = parts[2];
  const tampered = sig.slice(0, -4) + (sig.endsWith("AAAA") ? "BBBB" : "AAAA");
  const tamperedToken = [parts[0], parts[1], tampered].join(".");

  await assert.rejects(
    () =>
      verifyBearer(tamperedToken, resolver, {
        issuer: "http://localhost:18080/realms/aikonos",
        audience: "aikonos-broker",
      }),
    /signature verification failed/i,
  );
});

test("verifyBearer: Entra claim mapping resolves principal from oid and tenant from tid", async () => {
  const { privateKey, jwk, kid } = await makeKey();
  const resolver = await localResolver([jwk]);
  const token = await mintToken(privateKey, kid, {
    sub: "pairwise-per-app-do-not-use",
    oid: "00000000-aaaa-bbbb-cccc-111111111111",
    tid: "22222222-3333-4444-5555-666666666666",
    preferred_username: "alice@contoso.com",
  });

  const principal = await verifyBearer(token, resolver, {
    issuer: "http://localhost:18080/realms/aikonos",
    audience: "aikonos-broker",
    subjectClaim: "oid",
    tenantClaim: "tid",
  });

  assert.equal(principal.sub, "00000000-aaaa-bbbb-cccc-111111111111");
  assert.equal(principal.tenant, "22222222-3333-4444-5555-666666666666");
  assert.equal(principal.email, "alice@contoso.com");
});

test("verifyBearer: throws naming the configured tenant claim when absent", async () => {
  const { privateKey, jwk, kid } = await makeKey();
  const resolver = await localResolver([jwk]);
  const token = await mintToken(privateKey, kid, {
    sub: "x",
    oid: "o-1",
    preferred_username: "a@contoso.com",
    // no tid
  });

  await assert.rejects(
    () =>
      verifyBearer(token, resolver, {
        issuer: "http://localhost:18080/realms/aikonos",
        audience: "aikonos-broker",
        subjectClaim: "oid",
        tenantClaim: "tid",
      }),
    /missing tid claim/i,
  );
});

test("verifyBearer: throws when tenant_id claim is missing", async () => {
  const { privateKey, jwk, kid } = await makeKey();
  const resolver = await localResolver([jwk]);
  const token = await mintToken(privateKey, kid, {
    sub: "user-uuid-7",
    email: "alice@example.com",
    // no tenant_id
  });

  await assert.rejects(
    () =>
      verifyBearer(token, resolver, {
        issuer: "http://localhost:18080/realms/aikonos",
        audience: "aikonos-broker",
      }),
    /missing tenant_id claim/i,
  );
});
