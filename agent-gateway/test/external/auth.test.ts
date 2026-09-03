// Tests for the external API-key authentication middleware.
// Covers: missing bearer → 401; garbage token → 401; valid key → principal attached;
// revoked key (valid:false from south) → 401.
import { test } from "node:test";
import assert from "node:assert/strict";
import { authenticateApiKey, type ResolvedPrincipal } from "../../src/external/auth.js";

// ── Minimal south stub ────────────────────────────────────────────────────────

interface ResolveFn {
  (req: { keyHashHex: string; tenantId: string }): Promise<{
    valid: boolean;
    agentId: string;
    tenantId: string;
    principal: string;
  }>;
}

function makeSouth(resolveFn: ResolveFn) {
  return { resolveAgentApiKey: resolveFn } as unknown as import("../../src/broker/south.js").SouthClient;
}

function validResolve(agentId: string, tenantId: string): ResolveFn {
  return async () => ({
    valid: true,
    agentId,
    tenantId,
    principal: `svc-${agentId}`,
  });
}

function revokedResolve(): ResolveFn {
  return async () => ({ valid: false, agentId: "", tenantId: "", principal: "" });
}

function rejectingResolve(): ResolveFn {
  return async () => { throw new Error("grpc error"); };
}

const TENANT = "11111111-1111-1111-1111-111111111111";
const AGENT_ID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee";
const RAW_KEY = "tk_abc123def456ghi789jkl012mno345pqr678stu901vwx234yz";

test("missing Authorization header → 401", async () => {
  const south = makeSouth(validResolve(AGENT_ID, TENANT));
  const result = await authenticateApiKey(undefined, TENANT, south);
  assert.equal(result.ok, false);
  assert.equal(result.status, 401);
});

test("bearer without tk_ prefix → 401", async () => {
  const south = makeSouth(validResolve(AGENT_ID, TENANT));
  const result = await authenticateApiKey("Bearer not-a-real-key", TENANT, south);
  assert.equal(result.ok, false);
  assert.equal(result.status, 401);
});

test("garbage Authorization value → 401", async () => {
  const south = makeSouth(validResolve(AGENT_ID, TENANT));
  const result = await authenticateApiKey("gibberish", TENANT, south);
  assert.equal(result.ok, false);
  assert.equal(result.status, 401);
});

test("valid key → principal attached with correct agentId + tenantId", async () => {
  const south = makeSouth(validResolve(AGENT_ID, TENANT));
  const result = await authenticateApiKey(`Bearer ${RAW_KEY}`, TENANT, south);
  assert.equal(result.ok, true);
  if (!result.ok) return;
  const principal = result.principal as ResolvedPrincipal;
  assert.equal(principal.agentId, AGENT_ID);
  assert.equal(principal.tenantId, TENANT);
  assert.equal(principal.principal, `svc-${AGENT_ID}`);
});

test("south returns valid:false (revoked) → 401", async () => {
  const south = makeSouth(revokedResolve());
  const result = await authenticateApiKey(`Bearer ${RAW_KEY}`, TENANT, south);
  assert.equal(result.ok, false);
  assert.equal(result.status, 401);
});

test("south throws (gRPC error) → 401", async () => {
  const south = makeSouth(rejectingResolve());
  const result = await authenticateApiKey(`Bearer ${RAW_KEY}`, TENANT, south);
  assert.equal(result.ok, false);
  assert.equal(result.status, 401);
});

test("sha256 is computed before sending to south (south never sees raw key)", async () => {
  let capturedHashHex = "";
  const south = makeSouth(async (req) => {
    capturedHashHex = req.keyHashHex;
    return { valid: true, agentId: AGENT_ID, tenantId: TENANT, principal: `svc-${AGENT_ID}` };
  });
  await authenticateApiKey(`Bearer ${RAW_KEY}`, TENANT, south);
  // The hash must not equal the raw key
  assert.notEqual(capturedHashHex, RAW_KEY);
  // Must be a 64-char hex string (sha256 → 32 bytes → 64 hex chars)
  assert.match(capturedHashHex, /^[0-9a-f]{64}$/);
});
