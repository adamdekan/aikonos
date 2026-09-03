// Tests for selectRunApprover (CP2) — extracted pure function from agui.ts.
// Validates: auto agent + MCP tools → auto-approved; non-auto → hitl; spec
// undefined → hitl.
import { test } from "node:test";
import assert from "node:assert/strict";
import type { selectRunApprover } from "../src/routes/agui.js";
import type { Approver } from "../src/broker/governance.js";
import type { AgentSpec } from "../src/pi/session.js";
import pino from "pino";
import type { Config } from "../src/config.js";

// Narrow south type matching resolveAutoApproveAllowlist's Pick constraint.
import type { resolveAutoApproveAllowlist } from "../src/broker/auto-approve.js";
type AllowlistSouth = Parameters<typeof resolveAutoApproveAllowlist>[0];

// Minimal no-op logger — real pino instance at "silent" level so no output
// leaks and no cast is needed.
function makeLog() {
  return pino({ level: "silent" });
}

// A hitl approver whose invocations are tracked.
function makeHitlApprover(): { approver: Approver; calls: string[] } {
  const calls: string[] = [];
  const approver: Approver = async (info) => {
    calls.push(info.toolId);
    return false;
  };
  return { approver, calls };
}

function makeSouth(
  servers: { id: string }[],
  toolsByServer: Record<string, { name: string }[]>,
): AllowlistSouth {
  return {
    listAccessibleMcpServersForAgent: async () => ({
      connections: servers.map((s) => ({
        id: s.id,
        name: s.id,
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

// Minimal deps shape matching selectRunApprover's first argument.
function makeDeps(south: AllowlistSouth, tenantId = "tenant-1"): Parameters<typeof selectRunApprover>[0] {
  const cfg: Pick<Config, "defaultTenantId"> = { defaultTenantId: tenantId };
  return {
    south,
    cfg,
    log: makeLog(),
  };
}

test("selectRunApprover: auto agent + MCP tool granted only via mcp_servers → approver auto-approves that tool", async () => {
  // WHY: the MCP tool "mcp:conn1:echo" is NOT in spec.skills, but the south
  // returns it via listAccessibleMcpServersForAgent. The resolved allowlist must
  // include it so the auto-approver grants it.
  const { selectRunApprover: select } = await import("../src/routes/agui.js");

  const spec: AgentSpec = {
    model: "test-model",
    approvalMode: "auto",
    skills: ["web.fetch"], // mcp:conn1:echo deliberately absent
    preferredProvider: "",
    allowedProviders: [],
  };
  const south = makeSouth(
    [{ id: "conn1" }],
    { conn1: [{ name: "echo" }] },
  );
  const { approver: hitl } = makeHitlApprover();
  const deps = makeDeps(south);

  const approver: Approver = await select(deps, spec, "agent:test-agent", hitl);

  // MCP tool id in the same id space as ApprovalInfo.toolId.
  const grantedMcp = await approver({
    toolCallId: "tc-1",
    toolName: "mcp__conn1__echo",
    toolId: "mcp:conn1:echo",
    effectClass: 0,
    reason: "needs_human",
    args: {},
    stepUp: false,
  });
  assert.equal(grantedMcp, true, "MCP tool granted via mcp_servers must be auto-approved");

  // Tool not in skills or MCP → not approved.
  const blockedTool = await approver({
    toolCallId: "tc-2",
    toolName: "doc_write",
    toolId: "doc.write",
    effectClass: 0,
    reason: "needs_human",
    args: {},
    stepUp: false,
  });
  assert.equal(blockedTool, false, "tool not in skills/MCP must NOT be auto-approved");
});

test("selectRunApprover: non-auto spec (approvalMode needs_approval) → returns hitl approver", async () => {
  // WHY: approvalMode "needs_approval" must produce the injected HITL approver,
  // not a preAuthApprover — the approval card must surface for every tool.
  const { selectRunApprover: select } = await import("../src/routes/agui.js");

  const spec: AgentSpec = {
    model: "test-model",
    approvalMode: "needs_approval",
    skills: ["web.fetch"],
    preferredProvider: "",
    allowedProviders: [],
  };
  const south = makeSouth([], {});
  const { approver: hitl, calls } = makeHitlApprover();
  const deps = makeDeps(south);

  const approver: Approver = await select(deps, spec, "agent:test-agent", hitl);

  // The returned approver must be (or delegate to) the injected hitl.
  await approver({
    toolCallId: "tc-1",
    toolName: "web_fetch",
    toolId: "web.fetch",
    effectClass: 0,
    reason: "needs_human",
    args: {},
    stepUp: false,
  });

  assert.deepEqual(calls, ["web.fetch"], "non-auto spec must invoke the hitl approver");
});

test("selectRunApprover: spec undefined (default agent, no DB record) → returns hitl approver", async () => {
  // WHY: when the default agent has no DB record getAgentSpec returns found=false
  // and spec stays undefined. selectRunApprover must fall back to HITL — no crash,
  // no auto-approve for an unrecognised default agent.
  const { selectRunApprover: select } = await import("../src/routes/agui.js");

  const south = makeSouth([], {});
  const { approver: hitl, calls } = makeHitlApprover();
  const deps = makeDeps(south);

  const approver: Approver = await select(deps, undefined, "alice-agent", hitl);

  await approver({
    toolCallId: "tc-1",
    toolName: "web_fetch",
    toolId: "web.fetch",
    effectClass: 0,
    reason: "needs_human",
    args: {},
    stepUp: false,
  });

  assert.deepEqual(calls, ["web.fetch"], "undefined spec must delegate to hitl approver");
});

test("selectRunApprover: A8 org master off → auto agent downgraded to hitl", async () => {
  // WHY: when the org disallows unattended mode, an approvalMode:auto agent must
  // NOT get a preAuthApprover — every tool must surface a HITL card. The org
  // master overrides the per-agent setting.
  const { selectRunApprover: select } = await import("../src/routes/agui.js");

  const spec: AgentSpec = {
    model: "test-model",
    approvalMode: "auto",
    skills: ["web.fetch"],
    preferredProvider: "",
    allowedProviders: [],
  };
  const south = makeSouth([], {});
  const { approver: hitl, calls } = makeHitlApprover();
  const deps = makeDeps(south);

  // unattendedAllowed = false → hitl regardless of approvalMode:auto.
  const approver: Approver = await select(deps, spec, "agent:test-agent", hitl, false);

  await approver({
    toolCallId: "tc-1",
    toolName: "web_fetch",
    toolId: "web.fetch",
    effectClass: 0,
    reason: "needs_human",
    args: {},
    stepUp: false,
  });

  assert.deepEqual(calls, ["web.fetch"], "org-disabled unattended mode must route auto agents to hitl");
});

test("selectRunApprover: A8 unattendedAllowed defaults true (omitted arg) → auto still auto-approves", async () => {
  // Regression guard: existing callers that omit the flag must behave as before.
  const { selectRunApprover: select } = await import("../src/routes/agui.js");

  const spec: AgentSpec = {
    model: "test-model",
    approvalMode: "auto",
    skills: ["web.fetch"],
    preferredProvider: "",
    allowedProviders: [],
  };
  const south = makeSouth([], {});
  const { approver: hitl } = makeHitlApprover();
  const deps = makeDeps(south);

  const approver: Approver = await select(deps, spec, "agent:test-agent", hitl);
  const granted = await approver({
    toolCallId: "tc-1",
    toolName: "web_fetch",
    toolId: "web.fetch",
    effectClass: 0,
    reason: "needs_human",
    args: {},
    stepUp: false,
  });
  assert.equal(granted, true, "omitted A8 flag must default to allowed (auto-approve preserved)");
});

test("selectRunApprover: south RPC error during auto-approve → allowlist degrades to skills only, run continues", async () => {
  // WHY: mirrors resolveAutoApproveAllowlist's error-degrade guarantee. If south
  // is unreachable the preAuthApprover still covers spec.skills (safe), and the
  // run is not aborted — consistent with buildMcpTools's catch-and-continue posture.
  const { selectRunApprover: select } = await import("../src/routes/agui.js");

  const spec: AgentSpec = {
    model: "test-model",
    approvalMode: "auto",
    skills: ["web.fetch"],
    preferredProvider: "",
    allowedProviders: [],
  };
  const south: AllowlistSouth = {
    listAccessibleMcpServersForAgent: async () => { throw new Error("south down"); },
    listMcpServerToolsSouth: async () => ({ tools: [] }),
  };
  const { approver: hitl } = makeHitlApprover();
  const deps = makeDeps(south);

  // Must not throw — degrade happens inside resolveAutoApproveAllowlist.
  let approver: Approver;
  await assert.doesNotReject(async () => {
    approver = await select(deps, spec, "agent:test-agent", hitl);
  }, "selectRunApprover must not throw when south is unreachable");

  // Skills-only allowlist: web.fetch must still be approved.
  const skillApproved = await approver!({
    toolCallId: "tc-1",
    toolName: "web_fetch",
    toolId: "web.fetch",
    effectClass: 0,
    reason: "needs_human",
    args: {},
    stepUp: false,
  });
  assert.equal(skillApproved, true, "skill in spec.skills must be auto-approved even when south fails");
});
