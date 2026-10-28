// AG-09: a "mcp:<id>" connect key reaches the broker as mcp_connection_id, and
// the external surface publishes the aikonOS OAuth client metadata document.
import { test } from "node:test";
import assert from "node:assert/strict";
import Fastify from "fastify";
import { generateKeyPair, exportJWK, SignJWT, importJWK } from "jose";
import { registerConnectorRoutes, type ConnectorsCtx } from "../src/routes/connectors.js";
import { registerClientMetadataRoute } from "../src/external/client-metadata.js";
import type { BeginConnectorAuthRequest } from "../gen/ts/proto/broker.js";
import { ConnectorProvider } from "../gen/ts/proto/broker.js";

const ISS = "http://localhost:18080/realms/aikonos";
const AUD = "aikonos-broker";

async function setup() {
  const { publicKey, privateKey } = await generateKeyPair("RS256");
  const key = await importJWK({ ...(await exportJWK(publicKey)), alg: "RS256" }, "RS256");
  const token = await new SignJWT({ sub: "alice", email: "alice@example.com", tenant_id: "11111111-1111-1111-1111-111111111111" })
    .setProtectedHeader({ alg: "RS256", kid: "k1" })
    .setIssuer(ISS)
    .setAudience(AUD)
    .setIssuedAt()
    .setExpirationTime("1h")
    .sign(privateKey);
  const seen: BeginConnectorAuthRequest[] = [];
  const north = {
    beginConnectorAuth: async (r: BeginConnectorAuthRequest) => {
      seen.push(r);
      return { authorizeUrl: "https://as.example.com/authorize?x=1", state: "s1" };
    },
  } as unknown as ConnectorsCtx["clients"]["north"];
  const app = Fastify();
  registerConnectorRoutes(app, { clients: { north }, jwksResolver: () => Promise.resolve(key), verifyOpts: { issuer: ISS, audience: AUD } });
  return { app, token, seen };
}

test("connect with an mcp:<id> key sends mcp_connection_id", async () => {
  const { app, token, seen } = await setup();
  const resp = await app.inject({
    method: "POST",
    url: "/connectors/begin",
    headers: { authorization: `Bearer ${token}` },
    payload: { provider: "mcp:0d7c0c5e-1f5a-4e57-9a4a-1b2c3d4e5f60" },
  });
  assert.equal(resp.statusCode, 200);
  assert.equal(resp.json().authorizeUrl, "https://as.example.com/authorize?x=1");
  assert.equal(seen[0].mcpConnectionId, "0d7c0c5e-1f5a-4e57-9a4a-1b2c3d4e5f60");
  assert.equal(seen[0].provider, ConnectorProvider.CONNECTOR_PROVIDER_UNSPECIFIED);

  const bad = await app.inject({ method: "POST", url: "/connectors/begin", headers: { authorization: `Bearer ${token}` }, payload: { provider: "mcp:" } });
  assert.equal(bad.statusCode, 400);

  const drive = await app.inject({ method: "POST", url: "/connectors/begin", headers: { authorization: `Bearer ${token}` }, payload: { provider: "google_drive" } });
  assert.equal(drive.statusCode, 200);
  assert.equal(seen[1].mcpConnectionId, "");
  assert.equal(seen[1].provider, ConnectorProvider.GOOGLE_DRIVE);
});

test("client metadata document names itself as client_id and the redirect URI", async () => {
  const app = Fastify();
  registerClientMetadataRoute(app, { externalBaseUrl: "https://aikonos.example.com", connectorsRedirectUri: "https://aikonos.example.com/connect/callback" });
  const resp = await app.inject({ method: "GET", url: "/oauth/client-metadata.json" });
  assert.equal(resp.statusCode, 200);
  const doc = resp.json();
  assert.equal(doc.client_id, "https://aikonos.example.com/oauth/client-metadata.json");
  assert.deepEqual(doc.redirect_uris, ["https://aikonos.example.com/connect/callback"]);
  assert.equal(doc.token_endpoint_auth_method, "none");

  const off = Fastify();
  registerClientMetadataRoute(off, { externalBaseUrl: "", connectorsRedirectUri: "https://x/cb" });
  assert.equal((await off.inject({ method: "GET", url: "/oauth/client-metadata.json" })).statusCode, 404);
});
