// 7.2 CLI: drive the Pi agent (OpenRouter) with governance through the broker.
// Streams assistant text + tool lifecycle to stdout. NEEDS_HUMAN auto-approves.
//
// Usage: AIKONOS_OIDC_TOKEN=<bearer> npm run cli -- "fetch https://example.com and save a summary to notes.md"
import { loadConfig } from "../config";
import { log } from "../log";
import { BrokerClients } from "../broker/clients";
import { GovernanceBridge, autoApprover, type Identity } from "../broker/governance";
import { createRateLimitBreaker } from "../llm/rate-limit-breaker.js";
import { agentForUser } from "../broker/agent-identity";
import { resolveSessionPlan, createSessionFromPlan, type ResolveIdentity } from "../pi/session-plan";

async function main(): Promise<void> {
  const prompt = process.argv.slice(2).join(" ").trim() ||
    "Fetch https://example.com, then write a one-paragraph summary to summary.md.";
  const cfg = loadConfig();
  if (!cfg.openrouterApiKey) throw new Error("OPENROUTER_API_KEY not set");

  const userId = process.env.AIKONOS_USER ?? "alice@example.com";
  const agentId = agentForUser(userId, cfg.agentForUserOverrides);
  const identity: Identity = {
    token: process.env.AIKONOS_OIDC_TOKEN,
    tenantId: cfg.defaultTenantId,
    userId,
    agentId,
  };

  const clients = new BrokerClients(cfg);
  // Same breaker-wrapped rate-limit pre-gate server.ts builds. Without it the
  // CLI's bridge fell back to the permissive no-op checker, making this an
  // unmetered path around every RPM/spend cap — a dev tool, but an unmetered
  // path shouldn't exist at all.
  const rateLimitChecker = createRateLimitBreaker(
    (tenantId, agentId, provider, userId) =>
      clients.south.checkRateLimit({ tenantId, agentId, provider, userId: userId ?? "" }),
    { threshold: cfg.rateLimitBreakerThreshold },
    log,
  );
  const bridge = new GovernanceBridge(cfg, clients, identity, autoApprover(log), log, rateLimitChecker);

  // In-process (unforked) path: useProxy:false so the session registers the
  // provider directly with the real OpenRouter key, mirroring the legacy
  // buildSession behaviour — the CLI is a trusted local process, not the
  // untrusted forked child.
  const resolveIdentity: ResolveIdentity = { tenantId: identity.tenantId, userId, agentId };
  const plan = await resolveSessionPlan(resolveIdentity, {
    south: {
      getLlmProviders: (req) => clients.south.getLlmProviders(req),
      getTenantModel: (req) => clients.south.getTenantModel(req),
      // Adapter: the real south returns SkillEntry[] with .toolId; ResolveSouth
      // expects string[] so resolveSessionPlan can use them directly as tool ids
      // (mirrors server.ts's supervisorDeps.south.listUserSkills adapter).
      listUserSkills: async (req) => {
        const resp = await clients.south.listUserSkills(req);
        return { skills: (resp.skills ?? []).map((s) => s.toolId) };
      },
      listUserAgentSkills: async (req) => {
        const resp = await clients.south.listUserAgentSkills(req);
        return { bundles: resp.bundles ?? [] };
      },
      listAccessibleMcpServersForAgent: (req) => clients.south.listAccessibleMcpServersForAgent(req),
      listMcpServerToolsSouth: (req) => clients.south.listMcpServerToolsSouth(req),
      getAgentSpec: (req) => clients.south.getAgentSpec(req),
    },
    cfg: { llmModel: cfg.llmModel, defaultTenantId: cfg.defaultTenantId },
  });
  const { session } = await createSessionFromPlan(
    plan,
    bridge,
    { realApiKey: cfg.openrouterApiKey },
    { useProxy: false },
  );

  session.subscribe((event) => {
    if (event.type === "message_update" && event.assistantMessageEvent.type === "text_delta") {
      process.stdout.write(event.assistantMessageEvent.delta);
    } else if (event.type === "tool_execution_start") {
      process.stdout.write(`\n[tool ▶ ${event.toolName}] ${JSON.stringify(event.args)}\n`);
    } else if (event.type === "tool_execution_end") {
      process.stdout.write(`\n[tool ✔ ${event.toolName}] ${event.isError ? "ERROR" : "ok"}\n`);
    }
  });

  process.stdout.write(`\n=== PROMPT ===\n${prompt}\n=== RESPONSE ===\n`);
  await session.prompt(prompt);
  process.stdout.write("\n=== DONE ===\n");

  session.dispose();
  clients.north.close();
  clients.south.close();
}

main().catch((err) => {
  log.error({ err: String(err), stack: (err as Error)?.stack }, "agent-cli failed");
  process.exit(1);
});
