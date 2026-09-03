// 7.1 smoke: prove the gateway→broker south path under real enforcement.
// Creates a gateway-managed task (north, OIDC bearer), submits a 1-step plan
// (south, mTLS) → broker mints a capability token → InvokeTool with that
// token runs the tool through the Tool Proxy. No Pi, no LLM.
//
// Defaults to workspace_read/{} (its original hardcoded behavior). Override
// via SMOKE_TOOL_ID/SMOKE_ARGS/SMOKE_EFFECT_CLASS env vars (compose-verify.sh's
// docx.create round-trip check uses this — argv forwarded through
// `docker compose exec ... npm run --silent smoke -- ...` doesn't reliably
// survive that hop) or, for direct manual runs, positional argv:
//   tsx src/cli/smoke-south.ts docx.create '{"script":"...","output_path":"..."}' WRITE_LOCAL
//
// Prereqs: kubectl port-forward svc/broker 9090:9090 9091:9091; an OIDC token in
// AIKONOS_OIDC_TOKEN (get one from Keycloak — see scripts/smoke.sh); a minted
// SVID in .svid (agent-gateway/scripts/mint-svid.sh).
import { randomUUID } from "node:crypto";
import { pathToFileURL } from "node:url";
import { loadConfig } from "../config";
import { log } from "../log";
import { NorthClient } from "../broker/north";
import { SouthClient } from "../broker/south";
import { mapTool, type ToolMapping } from "../broker/mapping";
import { agentForUser } from "../broker/agent-identity";
import { oneStepPlan, GATEWAY_EXECUTION_HINT } from "../broker/planshim";
import { ValidationOutcome, EffectClass, effectClassFromJSON } from "../../gen/ts/proto/plan";
import { TaskStatus } from "../../gen/ts/proto/broker";

// resolveInvocationArgs: env > argv > default. compose-verify.sh runs this
// script through `docker compose exec ... npm run --silent smoke -- ...`,
// and argv forwarded through npm's `--` doesn't reliably survive that hop
// (confirmed live: the tool silently ran the workspace_read default instead
// of docx.create) — so env vars are the primary channel and argv remains a
// fallback for the documented direct-tsx manual usage in the header comment.
export function resolveInvocationArgs(
  argv: readonly string[],
  env: Readonly<Record<string, string | undefined>>,
): { toolArg: string; argsJson: string; effectClassArg: string | undefined } {
  return {
    toolArg: env.SMOKE_TOOL_ID ?? argv[2] ?? "workspace_read",
    argsJson: env.SMOKE_ARGS ?? argv[3] ?? "{}",
    effectClassArg: env.SMOKE_EFFECT_CLASS ?? argv[4],
  };
}

// resolveMapping: toolArg may be a known Pi/broker tool name (mapTool
// resolves it, e.g. "workspace_read") or a raw broker tool id not in the
// mapping table (e.g. "docx.create") — for the latter, effectClassArg must
// supply its effect class name since there's nowhere else to look it up from.
function resolveMapping(toolArg: string, effectClassArg: string | undefined): ToolMapping {
  const known = mapTool(toolArg);
  if (known) return known;
  if (!effectClassArg) {
    throw new Error(`unknown tool id ${toolArg} — pass its effect class as a 3rd argv (e.g. WRITE_LOCAL)`);
  }
  const effectClass = effectClassFromJSON(effectClassArg);
  if (effectClass === EffectClass.UNRECOGNIZED) {
    throw new Error(`unrecognized effect class ${effectClassArg}`);
  }
  return { toolId: toolArg, effectClass };
}

async function main(): Promise<void> {
  const cfg = loadConfig();
  const token = process.env.AIKONOS_OIDC_TOKEN;
  if (!token) throw new Error("set AIKONOS_OIDC_TOKEN (a Keycloak bearer for alice)");

  const { toolArg, argsJson, effectClassArg } = resolveInvocationArgs(process.argv, process.env);
  const args: Record<string, unknown> = JSON.parse(argsJson);
  const mapping = resolveMapping(toolArg, effectClassArg);

  const north = new NorthClient(cfg);
  const south = new SouthClient(cfg);

  // 1. Create a gateway-managed task (so the broker's executor stays out).
  const handle = await north.createTask(
    {
      tenantId: cfg.defaultTenantId,
      userId: "alice@example.com",
      prompt: "7.1 south smoke",
      costBudget: 500,
      skillHints: [GATEWAY_EXECUTION_HINT],
      parentTaskId: "",
      clientRequestId: "",
    },
    token,
  );
  log.info({ taskId: handle.taskId }, "CreateTask ok");

  // 2. SubmitPlan a 1-step plan → APPROVED + minted token.
  const plan = oneStepPlan({
    taskId: handle.taskId,
    tenantId: cfg.defaultTenantId,
    toolCallId: randomUUID(),
    mapping,
    args,
  });
  const result = await south.submitPlan({
    taskId: handle.taskId,
    sandboxSpiffeId: cfg.gatewaySpiffeId,
    plan,
  });
  log.info(
    { outcome: ValidationOutcome[result.outcome], tokens: Object.keys(result.capabilityTokenIds) },
    "SubmitPlan ok",
  );
  if (result.outcome !== ValidationOutcome.APPROVED) {
    throw new Error(`expected APPROVED, got ${ValidationOutcome[result.outcome]}: ${result.violations.join("; ")}`);
  }
  const capToken = result.capabilityTokenIds[1];
  if (!capToken) throw new Error("no capability token minted for step 1");
  log.info({ len: capToken.length }, "minted capability token for step 1");

  // 3. APPROVED → EXECUTING (the transition map only allows COMPLETED from
  // EXECUTING, not directly from APPROVED — mirror how a real gateway-managed
  // run drives the lifecycle: executing while the tool call is in flight).
  await south.emitStatus({
    taskId: handle.taskId,
    sandboxSpiffeId: cfg.gatewaySpiffeId,
    status: TaskStatus.EXECUTING,
    description: "7.1 smoke executing",
    costUnitsConsumed: 0,
    tenantId: cfg.defaultTenantId,
  });

  // 4. InvokeTool with the minted token → runs through the capability gate + proxy.
  const inv = await south.invokeTool({
    taskId: handle.taskId,
    stepId: "1",
    toolId: mapping.toolId,
    args,
    capabilityToken: capToken,
    sandboxSpiffeId: cfg.gatewaySpiffeId,
    tenantId: cfg.defaultTenantId,
    userId: "alice@example.com",
    agentId: agentForUser("alice@example.com", {}),
  });
  log.info({ success: inv.success, error: inv.error, cost: inv.costUnitsConsumed, result: inv.result }, "InvokeTool returned");
  if (!inv.success) throw new Error(`InvokeTool failed: ${inv.error}`);

  // 5. Close out the task.
  await south.emitStatus({
    taskId: handle.taskId,
    sandboxSpiffeId: cfg.gatewaySpiffeId,
    status: TaskStatus.COMPLETED,
    description: "7.1 smoke complete",
    costUnitsConsumed: inv.costUnitsConsumed,
    tenantId: cfg.defaultTenantId,
  });

  log.info("✅ 7.1 south smoke PASSED — gateway drove a governed tool call end-to-end");
  north.close();
  south.close();
}

// Guard so importing this module for its exported helpers (unit tests) doesn't
// also run the live smoke against a real broker.
if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main().catch((err) => {
    log.error({ err: String(err) }, "smoke failed");
    process.exit(1);
  });
}
