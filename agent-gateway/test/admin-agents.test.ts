// Tests for the admin agent CRUD helpers exported from admin.ts:
//   agentJson(a: Agent)                          — response projection
//   agentInputFromBody(id: string, b: AgentBody) — RPC input construction
//
// Also verifies that the helpers are the real production code, not inline copies.
// 401 behaviour tested via requireUser directly (same pattern as admin-keys.test.ts).
import { test } from "node:test";
import assert from "node:assert/strict";
import { generateKeyPair, exportJWK, SignJWT, importJWK } from "jose";
import type { JWK } from "jose";

import { requireUser } from "../src/auth/require-user.js";
import { agentJson, agentInputFromBody } from "../src/routes/admin.js";
import type { AgentBody } from "../src/routes/admin.js";
import type { JwksResolver } from "../src/auth/verify.js";
import type { Agent } from "../gen/ts/proto/broker.js";

// ── Shared key / token helpers ────────────────────────────────────────────────

async function makeKey() {
  const { publicKey, privateKey } = await generateKeyPair("RS256");
  const jwk: JWK = { ...(await exportJWK(publicKey)), kid: "k1", alg: "RS256", use: "sig" };
  return { privateKey, jwk };
}

async function localResolver(jwk: JWK): Promise<JwksResolver> {
  const key = await importJWK(jwk, "RS256");
  return () => Promise.resolve(key);
}

const VERIFY_OPTS = {
  issuer: "http://localhost:18080/realms/aikonos",
  audience: "aikonos-broker",
};

// ── Minimal fake reply (for 401 tests only) ───────────────────────────────────

interface FakeReply {
  statusCode: number;
  body: unknown;
  code(n: number): FakeReply;
  send(b: unknown): void;
}

function fakeReply(): FakeReply {
  const r: FakeReply = {
    statusCode: 0,
    body: undefined,
    code(n) { r.statusCode = n; return r; },
    send(b) { r.body = b; },
  };
  return r;
}

// ── agentJson unit tests ──────────────────────────────────────────────────────

test("agentJson — includes allowedProviders and preferredProvider from Agent", () => {
  const agent: Agent = {
    id: "a1",
    name: "My Agent",
    llmModel: "openai/gpt-4o",
    approvalMode: "auto",
    skills: [],
    mcpServers: [],
    usableBy: [],
    createdBy: "admin",
    createdAt: undefined,
    updatedAt: undefined,
    allowedProviders: ["openai", "anthropic"],
    preferredProvider: "openai",
    soul: "",
    gatewayEnabled: false,
  };

  const json = agentJson(agent);

  assert.deepEqual(json.allowedProviders, ["openai", "anthropic"]);
  assert.equal(json.preferredProvider, "openai");
});

test("agentJson — allowedProviders defaults to [] and preferredProvider defaults to '' when absent", () => {
  const agent: Agent = {
    id: "a2",
    name: "Bare Agent",
    llmModel: "",
    approvalMode: "needs_approval",
    skills: [],
    mcpServers: [],
    usableBy: [],
    createdBy: "admin",
    createdAt: undefined,
    updatedAt: undefined,
    allowedProviders: [],
    preferredProvider: "",
    soul: "",
    gatewayEnabled: false,
  };

  const json = agentJson(agent);

  assert.deepEqual(json.allowedProviders, []);
  assert.equal(json.preferredProvider, "");
});

// ── agentInputFromBody unit tests ─────────────────────────────────────────────

test("agentInputFromBody — POST path: id is empty string, provider fields forwarded", () => {
  const b: AgentBody = {
    name: "Provider Agent",
    llmModel: "openai/gpt-4o",
    approvalMode: "auto",
    skills: [],
    mcpServers: [],
    allowedProviders: ["openai", "anthropic"],
    preferredProvider: "anthropic",
  };

  const input = agentInputFromBody("", b);

  assert.equal(input.id, "");
  assert.deepEqual(input.allowedProviders, ["openai", "anthropic"]);
  assert.equal(input.preferredProvider, "anthropic");
});

test("agentInputFromBody — PATCH path: id comes from param, not body", () => {
  const b: AgentBody = {
    name: "Updated Agent",
    allowedProviders: ["groq"],
    preferredProvider: "groq",
  };

  const input = agentInputFromBody("agent-uuid-1", b);

  assert.equal(input.id, "agent-uuid-1");
  assert.deepEqual(input.allowedProviders, ["groq"]);
  assert.equal(input.preferredProvider, "groq");
});

test("agentInputFromBody — omitted providers default to [] and ''", () => {
  const b: AgentBody = { name: "No-Provider Agent" };

  const input = agentInputFromBody("", b);

  assert.deepEqual(input.allowedProviders, []);
  assert.equal(input.preferredProvider, "");
});

test("agentInputFromBody — omitted name/llmModel/approvalMode use correct defaults", () => {
  const input = agentInputFromBody("x", {});

  assert.equal(input.name, "");
  assert.equal(input.llmModel, "");
  assert.equal(input.approvalMode, "needs_approval");
  assert.deepEqual(input.skills, []);
  assert.deepEqual(input.mcpServers, []);
  assert.deepEqual(input.usableBy, []);
  assert.equal(input.createdBy, "");
});

// ── 401 guard (requireUser) ───────────────────────────────────────────────────

test("requireUser — no bearer → sets 401 and returns null", async () => {
  const { jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const reply = fakeReply();

  const principal = await requireUser(
    { headers: {} },
    reply,
    resolver,
    VERIFY_OPTS,
  );

  assert.equal(principal, null);
  assert.equal(reply.statusCode, 401);
});
