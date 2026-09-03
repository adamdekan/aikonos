// Tests for resolveAutoApproveAllowlist (CP1).
// Validates: MCP id union, error degradation, per-server skip, dedup.
import { test } from "node:test";
import assert from "node:assert/strict";
import type { resolveAutoApproveAllowlist } from "../src/broker/auto-approve.js";

// Narrow south type matching the helper's Pick constraint.
type AllowlistSouth = Parameters<typeof resolveAutoApproveAllowlist>[0];

function makeSouth(
  servers: { id: string; name: string }[],
  toolsByServer: Record<string, { name: string }[]>,
): AllowlistSouth {
  return {
    listAccessibleMcpServersForAgent: async () => ({
      connections: servers.map((s) => ({
        id: s.id,
        name: s.name,
        url: "https://mcp.example.com",
        transport: "streamable_http" as const,
        authType: "bearer" as const,
        authSecretRef: "",
        createdBy: "",
        createdAt: undefined,
      })),
    }),
    listMcpServerToolsSouth: async (req: { connectorId: string }) => ({
      tools: (toolsByServer[req.connectorId] ?? []).map((t) => ({
        name: t.name,
        description: "",
        inputSchemaJson: "",
        readOnlyHint: false,
      })),
    }),
  };
}

test("resolveAutoApproveAllowlist: MCP id present — skill + mcp:<server>:<tool> in result", async () => {
  const { resolveAutoApproveAllowlist: resolve } = await import("../src/broker/auto-approve.js");

  const skills: string[] = ["web.fetch"];
  const south: AllowlistSouth = makeSouth(
    [{ id: "s1", name: "Server One" }],
    { s1: [{ name: "echo" }] },
  );

  const allowlist: string[] = await resolve(south, "t1", "alice-agent", skills);

  assert.ok(allowlist.includes("web.fetch"), "skills must be in allowlist");
  assert.ok(allowlist.includes("mcp:s1:echo"), "MCP tool not in skills must still be in allowlist");
  assert.equal(allowlist.length, 2);
});

test("resolveAutoApproveAllowlist: listAccessibleMcpServersForAgent rejects — degrades to skills only, no throw", async () => {
  const { resolveAutoApproveAllowlist: resolve } = await import("../src/broker/auto-approve.js");

  const skills: string[] = ["web.fetch", "doc.write"];
  const south: AllowlistSouth = {
    listAccessibleMcpServersForAgent: async () => { throw new Error("network failure"); },
    listMcpServerToolsSouth: async () => ({ tools: [] }),
  };

  const allowlist: string[] = await resolve(south, "t1", "alice-agent", skills);

  assert.deepEqual(allowlist.sort(), ["doc.write", "web.fetch"]);
});

test("resolveAutoApproveAllowlist: per-server listMcpServerToolsSouth rejects — other server tools still included", async () => {
  const { resolveAutoApproveAllowlist: resolve } = await import("../src/broker/auto-approve.js");

  const skills: string[] = [];
  const south: AllowlistSouth = {
    listAccessibleMcpServersForAgent: async () => ({
      connections: [
        { id: "good", name: "Good", url: "x", transport: "streamable_http" as const, authType: "bearer" as const, authSecretRef: "", createdBy: "", createdAt: undefined },
        { id: "bad", name: "Bad", url: "x", transport: "streamable_http" as const, authType: "bearer" as const, authSecretRef: "", createdBy: "", createdAt: undefined },
      ],
    }),
    listMcpServerToolsSouth: async (req: { connectorId: string }) => {
      if (req.connectorId === "bad") throw new Error("server unreachable");
      return { tools: [{ name: "ping", description: "", inputSchemaJson: "", readOnlyHint: false }] };
    },
  };

  const allowlist: string[] = await resolve(south, "t1", "alice-agent", skills);

  assert.ok(allowlist.includes("mcp:good:ping"), "good server's tool must be included");
  assert.ok(!allowlist.includes("mcp:bad:"), "bad server must be skipped");
  assert.equal(allowlist.length, 1);
});

test("resolveAutoApproveAllowlist: dedup — skill id equal to mcp id appears once", async () => {
  const { resolveAutoApproveAllowlist: resolve } = await import("../src/broker/auto-approve.js");

  // Contrived: skill list already contains the broker mcp: id.
  const skills: string[] = ["mcp:s1:echo"];
  const south: AllowlistSouth = makeSouth(
    [{ id: "s1", name: "Server One" }],
    { s1: [{ name: "echo" }] },
  );

  const allowlist: string[] = await resolve(south, "t1", "alice-agent", skills);

  const echoEntries = allowlist.filter((id) => id === "mcp:s1:echo");
  assert.equal(echoEntries.length, 1, "duplicate mcp id must appear exactly once");
});
