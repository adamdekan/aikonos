#!/usr/bin/env node
// scripts/sign-agent-assertion.mjs
// Sign an RFC 7523 client assertion for an aikonOS external agent (AG-05).
// Zero dependencies (node:crypto). Prints the JWT to stdout.
//
//   node scripts/sign-agent-assertion.mjs <private-key.pem> <agent-id> <audience> [kid]
//
// Key pair (register the PUBLIC key in Admin → Agents → Public keys):
//   openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out agent.pem   # ES256
//   openssl genpkey -algorithm ed25519 -out agent.pem                               # EdDSA
//   openssl pkey -in agent.pem -pubout -out agent.pub.pem
//
// Use it (one assertion per request; each jti is accepted once, exp ≤ 5 min):
//   curl -H "Authorization: Bearer $(node scripts/sign-agent-assertion.mjs agent.pem $AGENT $AUD k1)" \
//        -H 'Content-Type: application/json' -d '{"prompt":"hi"}' http://localhost:8090/v1/agents/$AGENT/invoke
//
// kid must match the kid shown for the key in the admin UI (the default kid is
// the key's RFC 7638 thumbprint).
import { createPrivateKey, randomUUID, sign } from "node:crypto";
import { readFileSync } from "node:fs";

const [keyFile, agentId, audience, kid = ""] = process.argv.slice(2);
if (!keyFile || !agentId || !audience) {
  console.error("usage: sign-agent-assertion.mjs <private-key.pem> <agent-id> <audience> [kid]");
  process.exit(2);
}

const key = createPrivateKey(readFileSync(keyFile));
const alg = key.asymmetricKeyType === "ed25519" ? "EdDSA" : key.asymmetricKeyType === "ec" ? "ES256" : null;
if (!alg) throw new Error(`unsupported key type ${key.asymmetricKeyType}; use P-256 or Ed25519`);

const b64 = (v) => Buffer.from(typeof v === "string" ? v : JSON.stringify(v)).toString("base64url");
const now = Math.floor(Date.now() / 1000);
const header = { alg, typ: "JWT", ...(kid ? { kid } : {}) };
const claims = { iss: agentId, sub: agentId, aud: audience, iat: now, exp: now + 120, jti: randomUUID() };
const input = `${b64(header)}.${b64(claims)}`;
// ES256 wants the raw r||s signature (ieee-p1363), not DER; Ed25519 takes no digest.
const sig = alg === "ES256"
  ? sign("sha256", Buffer.from(input), { key, dsaEncoding: "ieee-p1363" })
  : sign(null, Buffer.from(input), key);
process.stdout.write(`${input}.${sig.toString("base64url")}\n`);
