// CP4 tests: resolveSessionPlan (parent half) + createSessionFromPlan (child half).
//
// WHY these tests exist:
//   resolveSessionPlan must return a secret-free plan — no api-key, bearer, or
//   grant in the serialised object. If a secret leaks into the plan it would flow
//   into the InitMessage sent to the untrusted child, defeating the Phase-2 split.
//
//   createSessionFromPlan must register exactly the allowed tools + MCP tools from
//   the plan and wire the provider at proxyBaseUrl with a dummy key when useProxy
//   is true, so the child never holds the real API key.
import { test } from "node:test";
import assert from "node:assert/strict";
import { piMcpToolName } from "../src/pi/mcp-alias.js";
import { mapTool } from "../src/broker/mapping.js";
import type { InitMessage } from "../src/ipc/protocol.js";
import {
  resolveSessionPlan,
  createSessionFromPlan,
  type SessionPlan,
  type ResolveIdentity,
  type ResolveSessionDeps,
  type CreateSessionDeps,
  type CreateAgentSessionFn,
  type PersonalSkillListLike,
} from "../src/pi/session-plan.js";
import type { BridgeClientLike } from "../src/ipc/bridge-client.js";
import type { AgentSessionEvent } from "@earendil-works/pi-coding-agent";
import type { ProviderLike } from "../src/pi/session-plan.js";

// ── Fake south client ──────────────────────────────────────────────────────────

function makeSouth(overrides: Partial<{
  providers: ProviderLike[];
  skills: string[];
  mcpServers: { id: string; name: string }[];
  mcpTools: { name: string; inputSchemaJson: string; description: string }[];
  // Personal skills (CP4). Undefined = the returned south object omits
  // listPersonalSkillsSouth entirely, matching a real caller that hasn't been
  // upgraded — resolveSessionPlan must treat that identically to "no personal
  // skills", never throw.
  personalSkills: PersonalSkillListLike[];
  // When true, listPersonalSkillsSouth (if present via personalSkills being
  // set) rejects instead of resolving — exercises the fail-open path.
  personalSkillsError: boolean;
}> = {}) {
  const providers = overrides.providers ?? [];
  const skills = overrides.skills ?? ["web.fetch", "doc.write"];
  const mcpServers = overrides.mcpServers ?? [];
  const mcpTools = overrides.mcpTools ?? [];

  return {
    getLlmProviders: async (_req: { tenantId: string }) => ({ providers }),
    getTenantModel: async (_req: { tenantId: string }) => ({ model: "" }),
    listUserSkills: async (_req: { tenantId: string; userId: string }) => ({
      skills,
    }),
    listUserAgentSkills: async (_req: { tenantId: string; userId: string }) => ({
      bundles: [],
    }),
    listAccessibleMcpServersForAgent: async (_req: { tenantId: string; agentId: string }) => ({
      connections: mcpServers,
    }),
    listMcpServerToolsSouth: async (_req: { tenantId: string; connectorId: string; userId: string }) => ({
      tools: mcpTools,
    }),
    getAgentSpec: async (_req: { tenantId: string; agentId: string }) => ({ found: false }),
    ...(overrides.personalSkills !== undefined
      ? {
          listPersonalSkillsSouth: async (_req: { tenantId: string; userId: string }) => {
            if (overrides.personalSkillsError) throw new Error("south unreachable");
            return { skills: overrides.personalSkills! };
          },
        }
      : {}),
  };
}

// ── Fake bridge client (no IPC needed for create-half tests) ───────────────────

function makeFakeBridge(): BridgeClientLike {
  return {
    gate: async () => ({ allow: true }),
    execute: async () => ({ ok: true, output: null }),
    delegate: async () => ({ ok: true }),
    saveWorkflow: async () => ({ ok: true }),
    runWorkflow: async () => ({ ok: true, result: null }),
    listWorkflows: async () => ({ ok: true, items: [] }),
    publishWorkflow: async () => ({ ok: true }),
    proposeWorkflow: async () => ({ ok: true }),
    analyzeImage: async () => ({ ok: true, text: "a red apple" }),
    scheduleWorkflow: async () => ({ ok: true }),
    reason: async () => ({ ok: true, output: "" }),
    setApprover: () => {},
    setToken: () => {},
    usageIdentity: () => ({ tenantId: "", userId: "", agentId: "" }),
  };
}

// ── Fake createAgentSession seam ───────────────────────────────────────────────
//
// createSessionFromPlan accepts an injected createAgentSession so tests don't
// need a real LLM or the full Pi SDK setup. We capture the calls to verify
// what was registered.

interface FakeSession {
  prompt(text: string): Promise<void>;
  subscribe(listener: (event: AgentSessionEvent) => void): () => void;
  dispose(): void;
}

interface CapturedCreateCall {
  model: { id: string };
  customToolNames: string[];
  tools: string[];            // the allowed tool name list
  providerBaseUrl: string;
  providerApiKey: string;
  providerId: string;
  providerMaxRetries: number | undefined;
}

function makeCreateSeam(): {
  createAgentSession: CreateAgentSessionFn;
  captured: CapturedCreateCall[];
} {
  const captured: CapturedCreateCall[] = [];

  const createAgentSession: CreateAgentSessionFn = async (opts?) => {
    const model = (opts?.model ?? {}) as { id: string };
    const customToolNames = (opts?.customTools ?? []).map((t: { name: string }) => t.name);

    captured.push({
      model,
      customToolNames,
      tools: (opts?.tools as string[]) ?? [],
      // Filled by the onRegisterProvider spy — not via opts.
      providerBaseUrl: "",
      providerApiKey: "",
      providerId: "",
      providerMaxRetries: opts?.settingsManager?.getProviderRetrySettings().maxRetries,
    });

    const fakeSession: FakeSession = {
      prompt: async () => {},
      subscribe: () => () => {},
      dispose: () => {},
    };
    return { session: fakeSession };
  };

  return { createAgentSession, captured };
}

// ── Test identity ──────────────────────────────────────────────────────────────

const TEST_IDENTITY: ResolveIdentity = {
  tenantId: "11111111-1111-1111-1111-111111111111",
  userId: "user-abc",
  agentId: "agent-1",
};

const BASE_CFG = {
  openrouterApiKey: "sk-real-key",
  llmModel: "anthropic/claude-sonnet-4.6",
  defaultTenantId: "11111111-1111-1111-1111-111111111111",
};

// ── resolveSessionPlan ─────────────────────────────────────────────────────────

test("CP4 resolveSessionPlan: returns a plan with NO secret fields — api-key, bearer, and grant must be absent", async () => {
  // WHY: the plan is sent as an InitMessage to the untrusted child. Any secret
  // that leaks into the plan survives the process split — a child with code-exec
  // could read it from the message. This test is the first gate: if a secret field
  // name appears in the serialised plan, the split is broken.
  const south = makeSouth({ skills: ["web.fetch"] });
  const deps: ResolveSessionDeps = { south, cfg: BASE_CFG };

  const plan = await resolveSessionPlan(TEST_IDENTITY, deps);
  const serialised = JSON.stringify(plan);

  // Assert no secret field name appears anywhere in the plan's serialised form.
  // The exact key names are chosen to cover common abbreviations.
  for (const forbidden of ["apiKey", "api_key", "apikey", "bearer", "token", "ownerGrant", "grant", "secret"]) {
    assert.ok(
      !serialised.includes(forbidden),
      `plan must not contain '${forbidden}' — secret leaked into plan: ${serialised}`,
    );
  }
});

test("CP4 resolveSessionPlan: plan fields are exactly the allowed secret-free set", async () => {
  // WHY: a stricter key-presence check. The plan type has a fixed shape; extra
  // fields would indicate something was accidentally added. Checks all keys at
  // top level are within the declared SessionPlan interface.
  const south = makeSouth({ skills: ["web.fetch"] });
  const deps: ResolveSessionDeps = { south, cfg: BASE_CFG };

  const plan = await resolveSessionPlan(TEST_IDENTITY, deps);

  // These are the only allowed top-level keys — matches the SessionPlan type and
  // the InitMessage shape (kind + payload fields).
  // skillBundles (CP7): admin-authored SKILL.md bodies — no api-key, bearer, or grant.
  // providerId (spend-caps CP3): the real DB provider id backing modelId — not a
  // secret (no api-key/bearer/grant), carried so the child's usage relay can
  // attribute spend to the correct provider.
  const allowedKeys = new Set(["kind", "modelId", "providerId", "systemPrompt", "allowedToolNames", "mcpTools", "proxyBaseUrl", "proxyModelAllowlist", "skillBundles"]);
  for (const key of Object.keys(plan)) {
    assert.ok(allowedKeys.has(key), `unexpected key '${key}' in plan — secret-free field set violated`);
  }
});

test("CP4 resolveSessionPlan: allowedToolNames reflects the user skill list from south ListUserSkills", async () => {
  // WHY: the user-skill filter is what limits which tools the LLM can invoke.
  // If the plan carries the wrong tool set, the LLM will either see tools it
  // shouldn't or miss tools it should have — both break the session.
  const south = makeSouth({ skills: ["web.fetch", "doc.write"] });
  const deps: ResolveSessionDeps = { south, cfg: BASE_CFG };

  const plan = await resolveSessionPlan(TEST_IDENTITY, deps);

  // web.fetch → web_fetch, doc.write → doc_write, delegate is always included.
  assert.ok(plan.allowedToolNames.includes("web_fetch"), "web_fetch must be in allowedToolNames");
  assert.ok(plan.allowedToolNames.includes("doc_write"), "doc_write must be in allowedToolNames");
  assert.ok(plan.allowedToolNames.includes("delegate"), "delegate must always be in allowedToolNames");
  // Ungranted tools must be absent.
  assert.ok(!plan.allowedToolNames.includes("email_draft"), "email_draft must not be in allowedToolNames");
});

test("resolveSessionPlan: agent-bound (agentSpec set) gates tools by agent.Skills, NOT ListUserSkills", async () => {
  // WHY: for agent-bound runs (external invoke) the agent's own configured skills
  // are authoritative — matching the broker's SubmitPlan enforcement. The service
  // principal svc-<agentId> carries no FGA skill grants, so gating on ListUserSkills
  // surfaces zero tools — the external-API skills gap. agentSpec.skills must win.
  const south = makeSouth({ skills: ["web.fetch"] }); // svc principal skills — must be IGNORED
  const agentSpec = { model: "", approvalMode: "auto", skills: ["doc.read", "doc.write"], soul: "" };
  const deps: ResolveSessionDeps = { south, cfg: BASE_CFG, agentSpec };

  const plan = await resolveSessionPlan(TEST_IDENTITY, deps);

  assert.ok(plan.allowedToolNames.includes("doc_read"), "agent skill doc.read must surface");
  assert.ok(plan.allowedToolNames.includes("doc_write"), "agent skill doc.write must surface");
  assert.ok(plan.allowedToolNames.includes("delegate"), "delegate always present");
  assert.ok(!plan.allowedToolNames.includes("web_fetch"), "ListUserSkills must NOT leak in for agent-bound runs");
});

test("resolveSessionPlan: agent-bound surfaces the agent's discovered MCP tools through the plan", async () => {
  // WHY (requirement): MCP access assigned to the agent must be available through
  // the external invoke API. MCP tools are discovered per-agent and are not in the
  // FGA skill registry, so they must survive the agent.Skills tool filter.
  const south = makeSouth({
    skills: [],
    mcpServers: [{ id: "siem-connector", name: "SIEM" }],
    mcpTools: [{ name: "search_alerts", description: "Search SIEM alerts", inputSchemaJson: "{}" }],
  });
  const agentSpec = { model: "", approvalMode: "auto", skills: ["doc.read"], soul: "" };
  const deps: ResolveSessionDeps = { south, cfg: BASE_CFG, agentSpec };

  const plan = await resolveSessionPlan(TEST_IDENTITY, deps);

  assert.ok(plan.allowedToolNames.includes("doc_read"), "agent skill still surfaces");
  assert.ok(
    plan.allowedToolNames.includes(piMcpToolName("siem-connector", "search_alerts")),
    "agent's discovered MCP tool must be available through the external API",
  );
});

test("CP4 resolveSessionPlan: mcpTools is populated from south MCP discovery RPCs", async () => {
  // WHY: MCP tools are discovered via two south RPCs. If the plan carries an
  // empty mcpTools list when servers exist, the child won't register them and
  // the LLM can't call them — a silent capability regression.
  const south = makeSouth({
    skills: ["web.fetch"],
    mcpServers: [{ id: "siem-connector", name: "SIEM" }],
    mcpTools: [
      { name: "search_alerts", description: "Search SIEM alerts", inputSchemaJson: "{}" },
    ],
  });
  const deps: ResolveSessionDeps = { south, cfg: BASE_CFG };

  const plan = await resolveSessionPlan(TEST_IDENTITY, deps);

  assert.equal(plan.mcpTools.length, 1, "one MCP tool must be in the plan");
  const t = plan.mcpTools[0];
  assert.ok(t, "mcp tool entry must exist");
  // The name carries a short alias of the connector id, not the id itself — the
  // full 36-char id would not fit the provider's 64-char function-name limit
  // (see src/pi/mcp-alias.ts). It must still resolve back to the full id.
  assert.equal(t.name, "mcp__siem-con__search_alerts");
  assert.equal(t.name, piMcpToolName("siem-connector", "search_alerts"));
  assert.equal(t.toolId, t.name);
  assert.equal(
    mapTool(t.name)?.toolId,
    "mcp:siem-connector:search_alerts",
    "plan name must map back to the full connector id",
  );
  assert.ok(typeof t.schema === "object", "schema must be an object");
});

test("CP4 resolveSessionPlan: plan.kind is 'init' so it satisfies InitMessage", async () => {
  // WHY: the plan is sent as an InitMessage over IPC. child-entry.ts pattern-
  // matches on kind:'init'. If the kind is wrong or absent the child silently
  // ignores it and every subsequent prompt fails with 'received prompt before init'.
  const south = makeSouth();
  const deps: ResolveSessionDeps = { south, cfg: BASE_CFG };

  const plan = await resolveSessionPlan(TEST_IDENTITY, deps);
  const asInit: InitMessage = plan; // compile-time check: SessionPlan extends InitMessage
  assert.equal(asInit.kind, "init");
});

test("CP4 resolveSessionPlan: proxyBaseUrl is threaded through from deps (placeholder for CP5)", async () => {
  // WHY: CP5 will supply the real egress proxy URL. For now it is a config
  // value / placeholder. The plan must carry it so the child can register the
  // provider against it. If proxyBaseUrl is empty the child registration fails.
  const south = makeSouth();
  const deps: ResolveSessionDeps = {
    south,
    cfg: BASE_CFG,
    proxyBaseUrl: "http://127.0.0.1:19999",
  };

  const plan = await resolveSessionPlan(TEST_IDENTITY, deps);
  assert.equal(plan.proxyBaseUrl, "http://127.0.0.1:19999");
});

test("CP5 resolveSessionPlan: isSubagentBranch omits spawn_subagents from allowedToolNames and the system prompt, upstream of buildSystemPrompt", async () => {
  // WHY upstream matters: filtering allowedToolNames AFTER the plan is built
  // would leave a system prompt (built from the unfiltered list) still
  // advertising spawn_subagents to a child that will be refused the tool —
  // 's depth-1 correctness trap.
  const south = makeSouth({ skills: ["subagents", "web.fetch"] });
  const deps: ResolveSessionDeps = { south, cfg: BASE_CFG, isSubagentBranch: true };

  const plan = await resolveSessionPlan(TEST_IDENTITY, deps);

  assert.ok(!plan.allowedToolNames.includes("spawn_subagents"), "a subagent branch must not carry spawn_subagents");
  assert.ok(!plan.systemPrompt.includes("spawn_subagents"), "the system prompt must not advertise spawn_subagents either");
});

test("CP5 resolveSessionPlan: isSubagentBranch absent (ordinary path) still surfaces spawn_subagents when granted", async () => {
  const south = makeSouth({ skills: ["subagents", "web.fetch"] });
  const deps: ResolveSessionDeps = { south, cfg: BASE_CFG };

  const plan = await resolveSessionPlan(TEST_IDENTITY, deps);

  assert.ok(plan.allowedToolNames.includes("spawn_subagents"), "an ordinary session must still surface a granted spawn_subagents");
});

test("CP4 resolveSessionPlan: proxyModelAllowlist contains the resolved modelId", async () => {
  // WHY: CP5's egress proxy rejects requests for models not in the child's
  // allowlist. The plan must include the resolved model so the proxy can
  // validate it. An empty allowlist would block every LLM call.
  const south = makeSouth({ skills: [] });
  const deps: ResolveSessionDeps = { south, cfg: BASE_CFG };

  const plan = await resolveSessionPlan(TEST_IDENTITY, deps);
  assert.ok(plan.proxyModelAllowlist.includes(plan.modelId),
    `proxyModelAllowlist must include modelId '${plan.modelId}'`);
});

// ── Personal skills ────────────────────────

function personalSkill(overrides: Partial<PersonalSkillListLike> = {}): PersonalSkillListLike {
  return {
    name: overrides.name ?? "my-notes",
    description: overrides.description ?? "A personal skill",
    keywords: overrides.keywords ?? [],
    allowedTools: overrides.allowedTools ?? ["web.fetch"],
    disableModelInvocation: overrides.disableModelInvocation ?? false,
    valid: overrides.valid ?? true,
  };
}

test("CP4 resolveSessionPlan: personal skills are unioned into skillBundles as origin:'personal' with a qualified name", async () => {
  const south = makeSouth({ skills: ["web.fetch"], personalSkills: [personalSkill({ name: "my-notes" })] });
  const deps: ResolveSessionDeps = { south, cfg: BASE_CFG };

  const plan = await resolveSessionPlan(TEST_IDENTITY, deps);

  const entry = plan.skillBundles?.find((b) => b.name === "personal:my-notes");
  assert.ok(entry, "personal entry must be present in skillBundles under its qualified name");
  assert.equal(entry?.origin, "personal");
  assert.equal(entry?.body, "", "body must be empty at plan-build time — fetched on demand at activation");
});

test("CP4 resolveSessionPlan: an invalid personal skill (valid:false) is excluded from the plan", async () => {
  const south = makeSouth({
    skills: ["web.fetch"],
    personalSkills: [personalSkill({ name: "broken", valid: false })],
  });
  const deps: ResolveSessionDeps = { south, cfg: BASE_CFG };

  const plan = await resolveSessionPlan(TEST_IDENTITY, deps);

  assert.ok(!plan.skillBundles?.some((b) => b.name === "personal:broken"),
    "invalid personal skill must never reach the chat catalog");
});

test("CP4 resolveSessionPlan: OIDC user with an agentSpec (agent-bound chat) still fetches personal skills", async () => {
  // WHY: personal skills are keyed on the caller's identity.userId, not on
  // agentSpec presence — an OIDC user chatting with an agent-bound persona
  // still has their own Skills/ folder. Gating on agentSpec alone (the old
  // behavior) wrongly hid it for this case.
  let called = false;
  const south = makeSouth({ skills: ["web.fetch"] });
  const spySouth = {
    ...south,
    listPersonalSkillsSouth: async (_req: { tenantId: string; userId: string }) => {
      called = true;
      return { skills: [personalSkill()] };
    },
  };
  const agentSpec = { model: "", approvalMode: "auto", skills: ["web.fetch"], soul: "" };
  const deps: ResolveSessionDeps = { south: spySouth, cfg: BASE_CFG, agentSpec };

  const plan = await resolveSessionPlan(TEST_IDENTITY, deps); // TEST_IDENTITY.userId = "user-abc" (OIDC)

  assert.equal(called, true, "listPersonalSkillsSouth must be called for an OIDC user even with agentSpec set");
  assert.ok(plan.skillBundles?.some((b) => b.origin === "personal"), "personal entry must be present");
});

test("CP4 resolveSessionPlan: svc-<agentId> principal (external invoke) never fetches personal skills", async () => {
  // WHY: svc-<agentId> (external invoke, see src/external/core.ts) and
  // group-<id> are the broker's own non-personal principal prefixes — they
  // have no Skills/ folder of their own.
  let called = false;
  const south = makeSouth({ skills: ["web.fetch"] });
  const spySouth = {
    ...south,
    listPersonalSkillsSouth: async (_req: { tenantId: string; userId: string }) => {
      called = true;
      return { skills: [personalSkill()] };
    },
  };
  const agentSpec = { model: "", approvalMode: "auto", skills: ["web.fetch"], soul: "" };
  const svcIdentity: ResolveIdentity = { ...TEST_IDENTITY, userId: "svc-agent-1" };
  const deps: ResolveSessionDeps = { south: spySouth, cfg: BASE_CFG, agentSpec };

  const plan = await resolveSessionPlan(svcIdentity, deps);

  assert.equal(called, false, "listPersonalSkillsSouth must not be called for a svc- principal");
  assert.ok(!plan.skillBundles?.some((b) => b.origin === "personal"), "no personal entries for a svc- principal");
});

test("CP4 resolveSessionPlan: listPersonalSkillsSouth failure yields an empty union, never a thrown error", async () => {
  const south = makeSouth({
    skills: ["web.fetch"],
    personalSkills: [personalSkill()],
    personalSkillsError: true,
  });
  const deps: ResolveSessionDeps = { south, cfg: BASE_CFG };

  const plan = await resolveSessionPlan(TEST_IDENTITY, deps); // must not throw

  assert.ok(!plan.skillBundles?.some((b) => b.origin === "personal"),
    "a failed fetch must leave the union with no personal entries, not crash the session build");
});

test("CP4 resolveSessionPlan: an injected south with no listPersonalSkillsSouth method builds a plan with no personal entries", async () => {
  // WHY: ResolveSouth declares listPersonalSkillsSouth optional so every
  // pre-existing stub (workflow/analyze-image tests, etc.) keeps compiling.
  const south = makeSouth({ skills: ["web.fetch"] }); // personalSkills omitted → method absent
  const deps: ResolveSessionDeps = { south, cfg: BASE_CFG };

  const plan = await resolveSessionPlan(TEST_IDENTITY, deps); // must not throw

  assert.ok(!plan.skillBundles?.some((b) => b.origin === "personal"));
});

// ── createSessionFromPlan ──────────────────────────────────────────────────────

test("CP4 createSessionFromPlan (useProxy:true): registers provider with dummy key at proxyBaseUrl — real key never enters", async () => {
  // WHY: this is the load-bearing key-isolation test for the child half. When
  // useProxy:true the provider registration must use proxyBaseUrl + a dummy key.
  // If the real key is used instead, the child holds a live LLM credential and
  // the Phase-2 threat model is broken.
  const plan: SessionPlan = {
    kind: "init",
    modelId: "anthropic/claude-sonnet-4.6",
    systemPrompt: "You are helpful.",
    allowedToolNames: ["web_fetch", "delegate"],
    mcpTools: [],
    proxyBaseUrl: "http://127.0.0.1:19999",
    proxyModelAllowlist: ["anthropic/claude-sonnet-4.6"],
  };

  const registeredProviders: Array<{ id: string; baseUrl: string; apiKey: string }> = [];

  const seam = makeCreateSeam();
  const deps: CreateSessionDeps = {
    createAgentSession: seam.createAgentSession,
    onRegisterProvider: (id, baseUrl, apiKey) => {
      registeredProviders.push({ id, baseUrl, apiKey });
    },
  };

  await createSessionFromPlan(plan, makeFakeBridge(), deps, { useProxy: true });

  assert.equal(registeredProviders.length, 1, "exactly one provider must be registered");
  const reg = registeredProviders[0];
  assert.ok(reg, "provider registration must exist");
  assert.equal(reg.baseUrl, "http://127.0.0.1:19999", "provider must be registered at proxyBaseUrl");
  assert.ok(reg.apiKey !== "sk-real-key", "real api key must NOT be in the child's provider registration");
  // The dummy key is a non-secret placeholder; assert it is present but not the real key.
  assert.ok(typeof reg.apiKey === "string" && reg.apiKey.length > 0, "a dummy key string must be present");
});

test("createSessionFromPlan: provider retries are enabled so a transient 5xx doesn't kill the run", async () => {
  // WHY: the Pi provider default is maxRetries:0 (retries off). A single
  // transient provider failure (5xx/429/connection) would otherwise abort the
  // whole run and discard the tool results (web_search/web_fetch) already
  // gathered this turn. This pins that we configure a non-zero retry count.
  const plan: SessionPlan = {
    kind: "init",
    modelId: "anthropic/claude-sonnet-4.6",
    systemPrompt: "You are helpful.",
    allowedToolNames: ["web_fetch"],
    mcpTools: [],
    proxyBaseUrl: "http://127.0.0.1:19999",
    proxyModelAllowlist: ["anthropic/claude-sonnet-4.6"],
  };

  const seam = makeCreateSeam();
  const deps: CreateSessionDeps = {
    createAgentSession: seam.createAgentSession,
    onRegisterProvider: () => {},
  };

  await createSessionFromPlan(plan, makeFakeBridge(), deps, { useProxy: true });

  assert.equal(seam.captured.length, 1, "exactly one session must be created");
  const maxRetries = seam.captured[0]?.providerMaxRetries;
  assert.ok(
    typeof maxRetries === "number" && maxRetries > 0,
    `provider maxRetries must be > 0 (got ${String(maxRetries)}) so transient 5xx retries instead of failing the run`,
  );
});

test("CP4 createSessionFromPlan: registers exactly the allowed tools from the plan", async () => {
  // WHY: the child must only expose tools listed in allowedToolNames. Extra tools
  // would bypass the skill gate; missing tools would break the session. The
  // allowed set is authoritative — it was derived from the user's FGA grants
  // parent-side.
  const plan: SessionPlan = {
    kind: "init",
    modelId: "anthropic/claude-sonnet-4.6",
    systemPrompt: "System.",
    allowedToolNames: ["web_fetch", "delegate"],
    mcpTools: [],
    proxyBaseUrl: "http://127.0.0.1:0",
    proxyModelAllowlist: ["anthropic/claude-sonnet-4.6"],
  };

  let capturedToolNames: string[] = [];
  let capturedAllowedList: string[] = [];

  const captureCreate: CreateAgentSessionFn = async (opts?) => {
    capturedToolNames = (opts?.customTools ?? []).map((t: { name: string }) => t.name);
    capturedAllowedList = (opts?.tools as string[]) ?? [];
    const fakeSession: FakeSession = {
      prompt: async () => {},
      subscribe: () => () => {},
      dispose: () => {},
    };
    return { session: fakeSession };
  };
  const deps: CreateSessionDeps = {
    createAgentSession: captureCreate,
    onRegisterProvider: () => {},
  };

  await createSessionFromPlan(plan, makeFakeBridge(), deps, { useProxy: true });

  // customTools registered must be exactly the static tools in allowedToolNames.
  // delegate is a static tool. web_fetch is a static tool.
  assert.ok(capturedToolNames.includes("web_fetch"), "web_fetch must be registered as a custom tool");
  assert.ok(capturedToolNames.includes("delegate"), "delegate must be registered as a custom tool");
  assert.ok(!capturedToolNames.includes("doc_write"), "doc_write must NOT be registered (not in plan)");

  // The tools allowlist passed to createAgentSession must match allowedToolNames.
  assert.deepEqual(
    capturedAllowedList.slice().sort(),
    plan.allowedToolNames.slice().sort(),
    "tools allowlist passed to createAgentSession must equal plan.allowedToolNames",
  );
});

test("CP4 createSessionFromPlan: registers MCP tools from plan.mcpTools (each routes through bridgeClient)", async () => {
  // WHY: MCP tools in the plan were discovered parent-side. The child must
  // register them as Pi tool definitions that route execute() through the
  // bridge client. If they are not registered, MCP tool calls silently fail.
  const plan: SessionPlan = {
    kind: "init",
    modelId: "anthropic/claude-sonnet-4.6",
    systemPrompt: "System.",
    allowedToolNames: ["web_fetch", "mcp__siem-connector__search_alerts"],
    mcpTools: [
      {
        name: "mcp__siem-connector__search_alerts",
        schema: { type: "object", properties: {} },
        toolId: "mcp__siem-connector__search_alerts",
      },
    ],
    proxyBaseUrl: "http://127.0.0.1:0",
    proxyModelAllowlist: ["anthropic/claude-sonnet-4.6"],
  };

  let capturedToolNames: string[] = [];

  const captureMcp: CreateAgentSessionFn = async (opts?) => {
    capturedToolNames = (opts?.customTools ?? []).map((t: { name: string }) => t.name);
    const fakeSession: FakeSession = {
      prompt: async () => {},
      subscribe: () => () => {},
      dispose: () => {},
    };
    return { session: fakeSession };
  };
  const deps: CreateSessionDeps = {
    createAgentSession: captureMcp,
    onRegisterProvider: () => {},
  };

  await createSessionFromPlan(plan, makeFakeBridge(), deps, { useProxy: true });

  assert.ok(
    capturedToolNames.includes("mcp__siem-connector__search_alerts"),
    "MCP tool must be registered as a custom tool",
  );
});

test("CP4 createSessionFromPlan (useProxy:false): registers provider with real key from cfg — legacy in-process path", async () => {
  // WHY: the existing buildSession path runs in the trusted parent and uses the
  // real key directly. When useProxy:false the create half must fall back to the
  // real key so the legacy interactive path is not regressed. This is the explicit
  // flag the spec requires to distinguish the forked-child path from the legacy path.
  const plan: SessionPlan = {
    kind: "init",
    modelId: "anthropic/claude-sonnet-4.6",
    systemPrompt: "System.",
    allowedToolNames: ["delegate"],
    mcpTools: [],
    proxyBaseUrl: "http://127.0.0.1:0",
    proxyModelAllowlist: ["anthropic/claude-sonnet-4.6"],
  };

  const registeredProviders: Array<{ id: string; baseUrl: string; apiKey: string }> = [];

  const deps: CreateSessionDeps = {
    createAgentSession: async () => {
      const fakeSession: FakeSession = {
        prompt: async () => {},
        subscribe: () => () => {},
        dispose: () => {},
      };
      return { session: fakeSession };
    },
    onRegisterProvider: (id, baseUrl, apiKey) => {
      registeredProviders.push({ id, baseUrl, apiKey });
    },
    realApiKey: "sk-real-key-for-legacy",
  };

  await createSessionFromPlan(plan, makeFakeBridge(), deps, { useProxy: false });

  assert.equal(registeredProviders.length, 1);
  const reg = registeredProviders[0];
  assert.ok(reg, "provider registration must exist");
  assert.equal(reg.apiKey, "sk-real-key-for-legacy", "real api key must be used when useProxy:false");
});

test("CP4 createSessionFromPlan (useProxy:false, no realApiKey): THROWS — missing key must not silently fall back to DUMMY_KEY", async () => {
  // WHY: a missing realApiKey on the direct (non-proxy) path would previously
  // silently use DUMMY_KEY, causing an opaque LLM auth failure at request time
  // instead of a clear error at session-creation time. Fail loud here so the
  // caller knows exactly what is wrong.
  const plan: SessionPlan = {
    kind: "init",
    modelId: "anthropic/claude-sonnet-4.6",
    systemPrompt: "System.",
    allowedToolNames: ["delegate"],
    mcpTools: [],
    proxyBaseUrl: "http://127.0.0.1:0",
    proxyModelAllowlist: ["anthropic/claude-sonnet-4.6"],
  };

  const deps: CreateSessionDeps = {
    createAgentSession: async () => {
      const fakeSession: FakeSession = {
        prompt: async () => {},
        subscribe: () => () => {},
        dispose: () => {},
      };
      return { session: fakeSession };
    },
    // realApiKey deliberately omitted
  };

  await assert.rejects(
    () => createSessionFromPlan(plan, makeFakeBridge(), deps, { useProxy: false }),
    (err: unknown) => {
      assert.ok(err instanceof Error, "must throw an Error");
      assert.ok(
        err.message.includes("realApiKey"),
        `error message must mention 'realApiKey'; got: ${err.message}`,
      );
      return true;
    },
  );
});

test("CP4 resolveSessionPlan: THROWS when proxyBaseUrl embeds credentials — key must never travel in the plan", async () => {
  // WHY: a proxyBaseUrl like http://user:pass@host/ would embed a credential in
  // the InitMessage (SessionPlan) sent to the untrusted child. This is the
  // defense-in-depth guard at plan-resolve time — CP5 constructs the URL
  // internally, but misconfiguration or supply-chain tamper must be caught early.
  const south = makeSouth();
  const deps: ResolveSessionDeps = {
    south,
    cfg: BASE_CFG,
    proxyBaseUrl: "http://user:s3cr3t@127.0.0.1:19999",
  };

  await assert.rejects(
    () => resolveSessionPlan(TEST_IDENTITY, deps),
    (err: unknown) => {
      assert.ok(err instanceof Error, "must throw an Error");
      assert.ok(
        err.message.toLowerCase().includes("credential"),
        `error message must mention 'credential'; got: ${err.message}`,
      );
      return true;
    },
  );
});

test("CP4 createSessionFromPlan: personal skill activation wires bridge.getSkillBody as the on-demand fetcher", async () => {
  // WHY: load-skill.ts's fetch logic is unit-tested directly in
  // session-skill.test.ts; this is the piece unique to session-plan.ts —
  // proving createSessionFromPlan actually binds bridge.getSkillBody into the
  // fetcher it hands to makeLoadSkillTool, end to end via activateSkill().
  const plan: SessionPlan = {
    kind: "init",
    modelId: "anthropic/claude-sonnet-4.6",
    systemPrompt: "System.",
    allowedToolNames: ["web_fetch", "delegate"],
    mcpTools: [],
    proxyBaseUrl: "http://127.0.0.1:0",
    proxyModelAllowlist: ["anthropic/claude-sonnet-4.6"],
    skillBundles: [
      {
        id: "personal:my-notes",
        name: "personal:my-notes",
        description: "My notes",
        body: "",
        allowedTools: ["web.fetch"],
        contextFork: false,
        disableModelInvocation: false,
        keywords: [],
        filePaths: [],
        origin: "personal",
      },
    ],
  };

  const fetchCalls: string[] = [];
  const bridge = {
    ...makeFakeBridge(),
    getSkillBody: async (name: string) => {
      fetchCalls.push(name);
      return { ok: true, body: "## My Notes\nFetched via the bridge.", allowedTools: ["web.fetch"] };
    },
  };

  const { createAgentSession } = makeCreateSeam();
  const deps: CreateSessionDeps = { createAgentSession, onRegisterProvider: () => {} };

  const { session } = await createSessionFromPlan(plan, bridge, deps, { useProxy: true });

  const activateSkill = session.activateSkill;
  if (!activateSkill) throw new Error("activateSkill must exist when skillBundles is non-empty");
  const result = await activateSkill("personal:my-notes");

  // Bugfix regression: activation must return the fetched body, not discard it
  // as null — /command and keyword-auto-load activation are otherwise
  // authorization-only (child-entry.ts has no body to prepend to the prompt).
  assert.ok(!result.startsWith("ERROR:"), "activation must succeed");
  assert.match(result, /Fetched via the bridge\./, "the fetched body must be returned, not discarded");
  assert.deepEqual(fetchCalls, ["my-notes"], "bridge.getSkillBody must be called with the bare directory name");
});
