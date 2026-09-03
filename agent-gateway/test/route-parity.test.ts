// Route-table parity pin (CP1 of fable-mbatch-gateway, F25).
//
// WHY: the F25 extraction moves every route handler out of the monolithic
// server.ts into src/routes/*.ts modules composed by src/app.ts's buildApp().
// The one invariant that extraction must never violate is the route table
// itself — webui/server.mjs proxies /api/* by prefix-strip, and the external
// :8090 surface + /agui are untouched, so a route path or method that shifts
// silently breaks the proxy. This test boots the real buildApp() (the same
// composition src/server.ts calls) and pins the exact registered
// "<METHOD> <path>" set. If a checkpoint changes a route path or method, this
// test fails — it is the safety net the CP table's Invariants section calls
// for. HEAD is Fastify's automatic GET twin and is excluded to keep the pin
// readable; the parser strips it consistently on both sides of the diff.
import { test } from "node:test";
import assert from "node:assert/strict";
import pino from "pino";
import { buildApp } from "../src/app.js";
import { BrokerClients } from "../src/broker/clients.js";
import { ApprovalRegistry } from "../src/agui/hitl.js";
import { ChildSupervisor } from "../src/ipc/supervisor.js";
import type { Config } from "../src/config.js";
import type { JwksResolver } from "../src/auth/verify.js";

// Baseline pinned set — captured from the real buildApp() output at the point
// the F25 extraction landed. Any addition, removal, or rename of a route must
// update this list deliberately, in the same commit that changes the route.
// Route counts are deliberately not tracked in these comments (F-7) — they
// rot as the list grows; the assertion below (deepEqual against the actual
// route table) is the real pin, not the comment.
//
// CP3 (F9) authorized addition: GET /readyz — new dependency-aware readiness
// endpoint, distinct from the existing static /healthz.
// A1 authorized addition: GET + PUT /admin/org-settings — org governance
// control plane (A-series org-global instruction preamble).
// A3 authorized addition: GET /admin/members — unified roster.
// A9 authorized addition: GET /admin/observability — read-only
// telemetry-export state (OTLP endpoint is a broker deploy-time env var).
// CP5 (tenant-onedrive-obo) authorized addition: GET+PUT /workspace/backend
// + GET /workspace/onedrive/folders — per-user working-folder preference +
// OneDrive folder picker discovery.
// CP8 (tenant-onedrive-obo) authorized addition: GET+PUT+DELETE /admin/m365
// + POST /admin/m365/test — tenant M365 admin connection panel.
// CP3 (web-search) authorized addition: GET+PUT+DELETE /admin/websearch
// + POST /admin/websearch/test — org-wide web.search engine config panel.
// CP4 authorized addition:
// GET /admin/spend-caps + GET /admin/spend-caps/summary + POST /admin/spend-caps
// + DELETE /admin/spend-caps/:id — tenant-admin CRUD + dashboard summary for
// monthly org/user/agent LLM spend caps.
// CP8 authorized addition:
// GET /memory/groups + GET /memory + GET /memory/concept + POST /memory/verify
// + POST /memory/deprecate + POST /memory/delete — the settings-modal Memory
// pane's management surface (broker-enforced, gateway forwards only).
// CP5 authorized addition:
// GET /skills + POST /skills/import + DELETE /skills/:name +
// POST /skills/:name/share + GET /skills/transfers/:envelopeId +
// POST /skills/transfers/:envelopeId/accept — the Skills view's management
// and transfer surface (broker-enforced, gateway forwards only).
// CP2 authorized
// addition: POST /admin/providers/:id/default-for — per-capability provider
// default (supersedes the three single-capability routes, which stay).
// Authorized addition: GET /sessions/:id/usage — per-session model/tokens/cost
// for the chat view's usage strip. Self-service (not admin-gated); the broker
// scopes the read to the verified caller's own sessions.
const EXPECTED_ROUTES = [
  "DELETE /admin/agents/:id",
  "DELETE /admin/agents/:id/keys/:keyId",
  "DELETE /admin/m365",
  "DELETE /admin/mcp/:id",
  "DELETE /admin/providers/:id",
  "DELETE /admin/rate-limits/:id",
  "DELETE /admin/skill-bundles/:id",
  "DELETE /admin/skills/:toolId",
  "DELETE /admin/spend-caps/:id",
  "DELETE /admin/websearch",
  "DELETE /files",
  "DELETE /schedules/:id",
  "DELETE /skills/:name",
  "DELETE /workflows/:lineageId",
  "DELETE /workflows/:lineageId/pin",
  "GET /",
  "GET /admin/agents",
  "GET /admin/agents/:id/keys",
  "GET /admin/alerts",
  "GET /admin/assignments",
  "GET /admin/audit/query",
  "GET /admin/audit/verify",
  "GET /admin/config",
  "GET /admin/decisions/:taskId",
  "GET /admin/m365",
  "GET /admin/mcp",
  "GET /admin/members",
  "GET /admin/network",
  "GET /admin/observability",
  "GET /admin/org-settings",
  "GET /admin/providers",
  "GET /admin/provisioning",
  "GET /admin/rate-limits",
  "GET /admin/scheduled-runs",
  "GET /admin/skill-bundles",
  "GET /admin/skills",
  "GET /admin/spend-caps",
  "GET /admin/spend-caps/summary",
  "GET /admin/websearch",
  "GET /agents",
  "GET /agents/:id/mcp-servers",
  "GET /agents/:id/soul",
  "GET /api/audit",
  "GET /api/audit/stream",
  "GET /approvals",
  "GET /connectors",
  "GET /connectors/providers",
  "GET /delegatable-users",
  "GET /files",
  "GET /files/content",
  "GET /healthz",
  "GET /inbox",
  "GET /memory",
  "GET /memory/concept",
  "GET /memory/groups",
  "GET /readyz",
  "GET /schedules",
  "GET /sessions/:id/usage",
  "GET /skills",
  "GET /skills/transfers/:envelopeId",
  "GET /user/skill-bundles",
  "GET /user/skills",
  "GET /workflows",
  "GET /workflows/:lineageId",
  "GET /workflows/:lineageId/versions",
  "GET /workspace/backend",
  "GET /workspace/onedrive/folders",
  "PATCH /admin/agents/:id",
  "PATCH /admin/mcp/:id",
  "PATCH /schedules/:id",
  "POST /admin/agents",
  "POST /admin/agents/:id/keys",
  "POST /admin/assignments",
  "POST /admin/assignments/revoke",
  "POST /admin/m365/test",
  "POST /admin/mcp",
  "POST /admin/network",
  "POST /admin/network/delete",
  "POST /admin/policy/simulate",
  "POST /admin/providers",
  "POST /admin/providers/:id/default",
  "POST /admin/providers/:id/default-for",
  "POST /admin/providers/:id/default-vision",
  "POST /admin/providers/:id/fallback",
  "POST /admin/providers/test",
  "POST /admin/provisioning",
  "POST /admin/provisioning/delete",
  "POST /admin/rate-limits",
  "POST /admin/skills",
  "POST /admin/skills/upload",
  "POST /admin/spend-caps",
  "POST /admin/websearch/test",
  "POST /agui",
  "POST /approve/:id",
  "POST /connectors/:id/revoke",
  "POST /connectors/begin",
  "POST /connectors/complete",
  "POST /delegate",
  "POST /files",
  "POST /files/dir",
  "POST /files/move",
  "POST /inbox/:id/dismiss",
  "POST /inbox/:id/respond",
  "POST /memory/delete",
  "POST /memory/deprecate",
  "POST /memory/verify",
  "POST /schedules",
  "POST /skills/:name/share",
  "POST /skills/import",
  "POST /skills/transfers/:envelopeId/accept",
  "POST /workflows",
  "POST /workflows/:lineageId/decide",
  "POST /workflows/:lineageId/fork",
  "POST /workflows/:lineageId/pin",
  "POST /workflows/:lineageId/propose",
  "POST /workflows/:lineageId/publish",
  "POST /workflows/:lineageId/rate",
  "POST /workflows/:lineageId/run",
  "PUT /admin/config",
  "PUT /admin/m365",
  "PUT /admin/org-settings",
  "PUT /admin/skill-bundles/:id",
  "PUT /admin/websearch",
  "PUT /agents/:id/soul",
  "PUT /workspace/backend",
];

// Fastify's printRoutes({ commonPrefix: false }) renders one line per node in
// the routing trie, indented 4 spaces per depth level with a box-drawing
// branch marker ("├── " / "└── "). Each node's text is either a bare path
// segment (an intermediate node with children) or "segment (METHOD, METHOD)"
// for a leaf that has handlers. Reconstructing the full registered path
// requires concatenating a node's segment with every ancestor's segment —
// there is no simpler public API to enumerate (method, path) pairs directly.
function parseRegisteredRoutes(printRoutesOutput: string): string[] {
  const lines = printRoutesOutput.split("\n").filter((l) => l.trim().length > 0);
  const stack: { depth: number; segment: string }[] = [];
  const routes: string[] = [];

  for (const line of lines) {
    const branch = line.match(/^((?:[│ ]   )*)(├── |└── )(.*)$/);
    if (!branch) continue;
    const depth = branch[1].length / 4;
    const rest = branch[3];
    const methodMatch = rest.match(/^(.*?) \(([^)]+)\)$/);
    const segment = methodMatch ? methodMatch[1] : rest;
    const methods = methodMatch ? methodMatch[2].split(", ") : [];

    while (stack.length > depth) stack.pop();
    stack.push({ depth, segment });
    const fullPath = "/" + stack.map((s) => s.segment).join("").replace(/^\/+/, "");

    for (const method of methods) {
      if (method === "HEAD") continue; // Fastify's automatic GET twin
      routes.push(`${method} ${fullPath}`);
    }
  }
  return routes.sort();
}

function fakeConfig(): Config {
  return {
    openrouterApiKey: "",
    llmModel: "",
    brokerNorthAddr: "",
    brokerSouthAddr: "",
    brokerServerName: "",
    tlsCert: "",
    tlsKey: "",
    tlsCa: "",
    gatewaySpiffeId: "",
    port: 8080,
    defaultTenantId: "",
    oidcIssuer: "",
    oidcJwksUrl: "",
    oidcAudience: "",
    oidcSubjectClaim: "sub",
    oidcTenantClaim: "tenant_id",
    schedulerEnabled: false,
    schedulerTickMs: 30000,
    schedulerClaimLimit: 10,
    schedulerRunTimeoutMs: 180000,
    agentForUserOverrides: {},
    externalPort: 8090,
    externalCorsOrigins: [],
    externalRateLimit: 60,
    threadTtlMs: 1800000,
    maxChildren: 32,
    childTtlMs: 1800000,
    natsUrl: "nats://nats:4222",
    auditSubject: "aikonos.audit.>",
    egressTimeoutMs: 120000, brokerTimeoutMs: 30000, rateLimitBreakerThreshold: 5, workflowReasonMaxTokens: 2048, maxLlmCallsPerRun: 100, approvalTimeoutMs: 900000, memorySemanticRecall: true, memoryEmbedTimeoutMs: 10000, subagentMaxWidth: 3, subagentBranchTimeoutMs: 180000,
  };
}

test("route-parity: buildApp() registers exactly the pinned route table", async () => {
  // Object.create avoids calling BrokerClients'/ChildSupervisor's constructors
  // (which open real TLS/gRPC connections) while satisfying the class type
  // check without a cast — route registration never calls into either at
  // buildApp() time, only inside handler closures invoked per-request.
  const clients: BrokerClients = Object.create(BrokerClients.prototype);
  const supervisor: ChildSupervisor = Object.create(ChildSupervisor.prototype);
  const jwksResolver: JwksResolver = () => Promise.reject(new Error("not used in this test"));

  const app = buildApp({
    clients,
    jwksResolver,
    verifyOpts: { issuer: "", audience: "" },
    approvals: new ApprovalRegistry(),
    supervisor,
    cfg: fakeConfig(),
    log: pino({ level: "silent" }),
    auditConsumer: { stop: () => Promise.resolve(), status: () => ({ state: "disabled" }) },
  });
  await app.ready();

  const actual = parseRegisteredRoutes(app.printRoutes({ commonPrefix: false }));

  assert.deepEqual(
    actual,
    EXPECTED_ROUTES,
    "registered route set changed — update EXPECTED_ROUTES deliberately if this route change is intentional",
  );

  await app.close();
});
