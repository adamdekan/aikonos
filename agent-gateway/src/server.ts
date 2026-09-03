// Agent-gateway process bootstrap: config load, credential-holding clients,
// the forked-child supervisor, the LLM egress proxy, and the HTTP surface
// built by src/app.ts. This file is the trusted parent — it never forwards
// long-lived secrets into the forked Pi-loop child (see src/ipc/supervisor.ts).
//   GET  /            → the self-contained demo UI
//   GET  /healthz     → liveness
//   POST /agui        → run the agent for a prompt; streams AG-UI events (SSE)
//   POST /approve/:id → resolve a pending human-in-the-loop approval
import { loadConfig } from "./config";
import { log } from "./log";
import { setUnaryDeadlineMs } from "./broker/unary.js";
import { BrokerClients } from "./broker/clients";
import { GovernanceBridge } from "./broker/governance";
import { ApprovalRegistry } from "./agui/hitl";
import { productionResolver } from "./auth/verify.js";
import { startScheduler } from "./scheduler/ticker";
import { ChildSupervisor, defaultSpawnChild } from "./ipc/supervisor.js";
import { resolveProviderCredentials } from "./pi/session.js";
import { EgressProxy } from "./llm/egress-proxy.js";
import { createRateLimitBreaker } from "./llm/rate-limit-breaker.js";
import type { SupervisorDeps } from "./ipc/supervisor.js";
import { startAuditConsumer } from "./audit/stream";
import { startExternalServer } from "./external/server";
import { buildApp } from "./app.js";
import { installShutdownHandlers } from "./shutdown.js";

const cfg = loadConfig();
// Applies to every unary north/south RPC (src/broker/unary.ts) built anywhere
// in this process, including src/external/server.ts which reuses `clients`
// below. cli/agent-cli.ts and smoke-south.ts are separate entrypoints that
// never call this — they're safe on the module's 30s default (CP1).
setUnaryDeadlineMs(cfg.brokerTimeoutMs);
const clients = new BrokerClients(cfg);
const approvals = new ApprovalRegistry(cfg.approvalTimeoutMs);

// ── Supervisor (CP6/CP7) ──────────────────────────────────────────────────────
//
// The supervisor owns the forked child pool. The /agui interactive path calls
// getOrSpawn() + run() instead of building an inline Pi session. The south
// client adapter maps SkillEntry[] → string[] so ResolveSouth is satisfied
// without changing the real south client's generated types.
const egressProxy = new EgressProxy(undefined, {
  egressTimeoutMs: cfg.egressTimeoutMs,
  maxLlmCallsPerRun: cfg.maxLlmCallsPerRun,
});

// CP4.1: circuit breaker over consecutive TRANSPORT FAILURES of this RPC
// (broker unreachable, DEADLINE_EXCEEDED, etc). Below threshold, stays
// fail-open (unchanged behavior — a broker restart must not black out LLM
// egress). At/above threshold, denies until a half-open probe succeeds. An
// explicit allowed=false response is a real rate-limit denial, not a
// transport failure, and never touches the breaker counter.
//
// Spend-caps CP3: this same breaker instance is also injected into every
// GovernanceBridge (below) so workflow reason/vision calls run the identical
// pre-gate check the egress proxy runs for interactive chat, instead of a
// second independently-drifting rate-limit call site.
const rateLimitChecker = createRateLimitBreaker(
  (tenantId, agentId, provider, userId) =>
    clients.south.checkRateLimit({ tenantId, agentId, provider, userId: userId ?? "" }),
  { threshold: cfg.rateLimitBreakerThreshold },
  log,
);
egressProxy.setRateLimitChecker(rateLimitChecker);

const supervisorDeps: SupervisorDeps = {
  south: {
    getLlmProviders: (req) => clients.south.getLlmProviders(req),
    getTenantModel: (req) => clients.south.getTenantModel(req),
    listUserSkills: async (req) => {
      const resp = await clients.south.listUserSkills(req);
      // Adapter: the real south returns SkillEntry[] with .toolId; ResolveSouth
      // expects string[] so resolveSessionPlan can use them directly as tool ids.
      return { skills: (resp.skills ?? []).map((s) => s.toolId) };
    },
    listUserAgentSkills: async (req) => {
      const resp = await clients.south.listUserAgentSkills(req);
      // Adapter: proto AgentSkillBundle fields are camelCased by ts-proto.
      // SkillBundleEntry uses the same camelCase names, so pass-through is safe.
      return { bundles: resp.bundles ?? [] };
    },
    listAccessibleMcpServersForAgent: (req) => clients.south.listAccessibleMcpServersForAgent(req),
    listMcpServerToolsSouth: (req) => clients.south.listMcpServerToolsSouth(req),
    getAgentSpec: (req) => clients.south.getAgentSpec(req),
  },
  // Spend-caps CP3: forwards every child usage relay to the broker.
  emitLlmUsage: (req) => clients.south.emitLlmUsage(req),
  cfg: {
    llmModel: cfg.llmModel,
    defaultTenantId: cfg.defaultTenantId,
  },
};

// Explicit type annotation (not inferred): the BridgeFactory closure below
// references `supervisor` before this declaration finishes evaluating, which
// is fine at runtime (the closure only runs at fork time, long after this
// line completes — see the comment inline) but defeats TypeScript's own
// self-referential inference without an annotation naming the type up front.
const supervisor: ChildSupervisor = new ChildSupervisor(
  egressProxy,
  supervisorDeps,
  // BridgeFactory: constructs a GovernanceBridge per run with the verified
  // identity and approver for that run. Construction is cheap — the bridge
  // holds no long-lived resources at construction time; it opens connections
  // on demand when gate/execute/delegate are called.
  //
  // The `supervisor` reference below is an apparent cycle, not a real one
  //: this arrow function's body only runs at
  // fork time (ChildSupervisor never calls the factory during its own
  // constructor), by which point `const supervisor = ...` below has long
  // finished — the same shape as the pre-existing consumeLlmBudget closure.
  (identity, approver, consumeLlmBudget, runId, sessionId, onSubagentEvent) =>
    new GovernanceBridge(cfg, clients, identity, approver, log, rateLimitChecker, consumeLlmBudget, runId, sessionId, supervisor, onSubagentEvent),
  defaultSpawnChild,
  // Provider-aware credential resolver.
  // Resolves the tenant's configured DB providers first (so the egress proxy
  // dials the real upstream/key/dialect), honouring the agent's preferred
  // provider/model; falls back to the env OpenRouter key ONLY when the tenant
  // has zero enabled DB providers (or the RPC itself failed). A resolved
  // provider with no key, or an empty env fallback key, throws instead of
  // silently degrading into a broken config — see resolveProviderCredentials.
  (identity, agentSpec) => resolveProviderCredentials(cfg, clients.south, identity.tenantId, agentSpec),
  { maxChildren: cfg.maxChildren, childTtlMs: cfg.childTtlMs },
);

// Shared OIDC verify options; the JWKS set is cached at module scope inside
// productionResolver. When oidcIssuer is empty we run in dev passthrough mode.
const jwksResolver = productionResolver(cfg);
const verifyOpts = {
  issuer: cfg.oidcIssuer,
  audience: cfg.oidcAudience,
  subjectClaim: cfg.oidcSubjectClaim,
  tenantClaim: cfg.oidcTenantClaim,
};

// Started here (not after HTTP listen) so /readyz has a live status to report
// as soon as the app is built; the consumer's own reconnect loop is
// independent of the HTTP surface's lifecycle.
const auditConsumer = startAuditConsumer(log, { natsUrl: cfg.natsUrl || undefined, subject: cfg.auditSubject });

const app = buildApp({ clients, jwksResolver, verifyOpts, approvals, supervisor, cfg, log, auditConsumer, rateLimitChecker });

// Bind the LLM-egress proxy to its loopback port BEFORE the HTTP server accepts
// requests. register() returns childBaseUrl = http://127.0.0.1:<port>/<token>;
// until start() runs, port is 0 and a child's LLM POST hangs against a dead
// address — the run never settles and the SSE stream produces no response.
egressProxy
  .start()
  .then(() => {
    log.info({ port: egressProxy.address().port }, "llm egress proxy listening");
    return app.listen({ port: cfg.port, host: "0.0.0.0" });
  })
  .then((addr) => {
    log.info({ addr, model: cfg.llmModel }, "agent-gateway listening");

    let stopScheduler: (() => void) | undefined;
    if (cfg.schedulerEnabled) {
      stopScheduler = startScheduler(cfg, clients, log, supervisor, rateLimitChecker);
    } else {
      log.info("scheduler disabled (AIKONOS_SCHEDULER_ENABLED!=true)");
    }

    // Start the external API surface on a separate port; errors are non-fatal
    // for the internal surface. closeExternal stays undefined until (unless)
    // it finishes starting, so shutdown skips it cleanly on failure.
    let closeExternal: (() => Promise<void>) | undefined;
    startExternalServer(cfg, clients, log, supervisor)
      .then((close) => {
        closeExternal = close;
      })
      .catch((err) => {
        log.error({ err: String(err) }, "external server failed to start");
      });

    installShutdownHandlers({
      log,
      stopScheduler,
      closeApp: () => app.close(),
      closeExternal: () => closeExternal?.() ?? Promise.resolve(),
      approvals,
      supervisor,
      egressProxy,
      auditConsumer,
      clients,
      exit: (code) => process.exit(code),
    });
  })
  .catch((err) => {
    log.error({ err: String(err) }, "failed to start");
    process.exit(1);
  });
