// CP7 tests: load_skill built-in + catalog injection in buildSession.
//
// WHY these tests exist:
//   load_skill is the only path by which the model can activate a skill bundle.
//   Activation must be fail-closed for unknown/rejected names and must handle
//   context_fork inline (no fork, exactly one warn log). Authorization is FGA +
//   the four governance gates (SubmitPlan → OPA → FGA → Biscuit) at each
//   individual tool_call — activating a bundle changes neither the registered
//   tool set nor the gate's decision, regardless of what the bundle's
//   allowed_tools says (, D5: the allowed_tools
//   narrowing machinery was dead code, computed and never read — removed).
//   The catalog injected into the system prompt must omit disable_model_invocation
//   bundles so the model is never offered a bundle it cannot call via load_skill.
import { test } from "node:test";
import assert from "node:assert/strict";

import {
  buildSkillCatalogText,
  makeLoadSkillTool,
  PERSONAL_SKILL_PREFIX,
  type SkillBodyFetcher,
} from "../src/pi/load-skill.js";
import type { SkillBundleEntry } from "../src/ipc/protocol.js";

// ── helpers ────────────────────────────────────────────────────────────────────

function bundle(overrides: Partial<SkillBundleEntry> = {}): SkillBundleEntry {
  return {
    id: overrides.id ?? "aaa",
    name: overrides.name ?? "my-skill",
    description: overrides.description ?? "Does something useful",
    body: overrides.body ?? "## My Skill\nUse web_fetch to look things up.",
    allowedTools: overrides.allowedTools ?? ["web.fetch"],
    contextFork: overrides.contextFork ?? false,
    disableModelInvocation: overrides.disableModelInvocation ?? false,
    keywords: overrides.keywords ?? [],
    filePaths: overrides.filePaths ?? [],
  };
}

// personalEntry builds a personal-skill union entry — body is always "" at this shape's construction (frontmatter-
// only until fetched), mirroring what resolveSessionPlan actually produces.
function personalEntry(bareName: string, overrides: Partial<SkillBundleEntry> = {}): SkillBundleEntry {
  const qualified = PERSONAL_SKILL_PREFIX + bareName;
  return {
    id: overrides.id ?? qualified,
    name: overrides.name ?? qualified,
    description: overrides.description ?? "A personal skill",
    body: "",
    allowedTools: overrides.allowedTools ?? ["web.fetch"],
    contextFork: overrides.contextFork ?? false,
    disableModelInvocation: overrides.disableModelInvocation ?? false,
    keywords: overrides.keywords ?? [],
    filePaths: overrides.filePaths ?? [],
    origin: "personal",
  };
}

// ── buildSkillCatalogText ──────────────────────────────────────────────────────

test("CP7 catalog: omits disable_model_invocation bundles", () => {
  // WHY: a bundle with disable_model_invocation=true must never appear in the
  // catalog so the model doesn't try to call load_skill with its name.
  const bundles: SkillBundleEntry[] = [
    bundle({ name: "visible", disableModelInvocation: false }),
    bundle({ name: "hidden",  disableModelInvocation: true }),
  ];
  const text = buildSkillCatalogText(bundles);
  assert.ok(text.includes("visible"), "visible bundle must appear in catalog");
  assert.ok(!text.includes("hidden"),  "disable_model_invocation bundle must not appear in catalog");
});

test("CP7 catalog: empty when all bundles have disable_model_invocation", () => {
  const bundles: SkillBundleEntry[] = [
    bundle({ name: "a", disableModelInvocation: true }),
    bundle({ name: "b", disableModelInvocation: true }),
  ];
  const text = buildSkillCatalogText(bundles);
  assert.equal(text, "", "catalog text must be empty string when nothing is visible");
});

test("CP7 catalog: empty string when bundle list is empty", () => {
  assert.equal(buildSkillCatalogText([]), "");
});

test("CP7 catalog: includes name and description for each visible bundle", () => {
  const bundles: SkillBundleEntry[] = [
    bundle({ name: "fetch-helper", description: "Helps with fetching", disableModelInvocation: false }),
  ];
  const text = buildSkillCatalogText(bundles);
  assert.ok(text.includes("fetch-helper"), "name must appear");
  assert.ok(text.includes("Helps with fetching"), "description must appear");
});

// ── personal skills ───────────────────────

test("CP4 catalog: personal entries render with a '(personal)' suffix under their qualified name", () => {
  const bundles: SkillBundleEntry[] = [
    bundle({ name: "admin-skill", description: "Admin skill" }),
    personalEntry("my-notes", { description: "My personal notes helper" }),
  ];
  const text = buildSkillCatalogText(bundles);
  assert.ok(text.includes("- admin-skill: Admin skill"), "bundle line renders unmarked");
  assert.ok(
    text.includes("- personal:my-notes: My personal notes helper (personal)"),
    `personal line must be qualified and marked; got: ${text}`,
  );
});

test("CP4 load_skill: personal entry fetches its body on demand — never from the plan-build-time entry", async () => {
  const p = personalEntry("my-notes", { allowedTools: ["web.fetch"] });
  const fetchCalls: string[] = [];
  const fetchPersonalBody: SkillBodyFetcher = async (name) => {
    fetchCalls.push(name);
    return { ok: true, body: "## My Notes\nFetched fresh.", allowedTools: ["web.fetch"] };
  };

  const tool = makeLoadSkillTool([p], { warn: () => {} }, fetchPersonalBody);
  const result = await tool.execute("call-p1", { name: "personal:my-notes" });

  assert.deepEqual(fetchCalls, ["my-notes"], "fetcher must receive the bare directory name, not the qualified key");
  assert.ok(result.content[0]?.text.includes("Fetched fresh"), "body must come from the fetch response");
});

test("CP4 load_skill: a failed personal-body fetch returns an error result, never a stale/empty body", async () => {
  const p = personalEntry("gone");
  const fetchPersonalBody: SkillBodyFetcher = async () => ({ ok: false, error: "NotFound" });

  const tool = makeLoadSkillTool([p], { warn: () => {} }, fetchPersonalBody);
  const result = await tool.execute("call-p2", { name: "personal:gone" });

  assert.ok(result.content[0]?.text.startsWith("ERROR:"), `expected an ERROR result; got: ${result.content[0]?.text}`);
});

test("CP4 load_skill: personal entry with no fetcher supplied cannot activate", async () => {
  // WHY: a bridge predating this feature (or a bridge with no south access at
  // all) leaves fetchPersonalBody undefined — activation must fail closed,
  // never silently return an empty body.
  const p = personalEntry("orphan");

  const tool = makeLoadSkillTool([p], { warn: () => {} }); // no 3rd arg
  const result = await tool.execute("call-p3", { name: "personal:orphan" });

  assert.ok(result.content[0]?.text.startsWith("ERROR:"));
});

test("CP4 collision: an exact name match resolves to the bundle; the personal entry stays reachable via its qualified key", async () => {
  const b = bundle({ name: "shared-name", body: "## Bundle body", allowedTools: ["web.fetch"] });
  const p = personalEntry("shared-name", { allowedTools: ["doc.write"] });
  const fetchPersonalBody: SkillBodyFetcher = async () => ({ ok: true, body: "## Personal body", allowedTools: ["doc.write"] });

  const tool = makeLoadSkillTool([b, p], { warn: () => {} }, fetchPersonalBody);

  const bundleResult = await tool.execute("call-p5", { name: "shared-name" });
  assert.ok(bundleResult.content[0]?.text.includes("Bundle body"), "the bare name must resolve to the bundle, never the personal entry");

  const personalResult = await tool.execute("call-p6", { name: "personal:shared-name" });
  assert.ok(personalResult.content[0]?.text.includes("Personal body"), "the qualified key must reach the personal entry");
});

// ── makeLoadSkillTool: granted name → body returned ───────────────────────────

test("CP7 load_skill: granted name returns body", async () => {
  // WHY: this is the primary happy path. Model calls load_skill("my-skill");
  // we must return the stored bundle body so the model can follow its instructions.
  const b = bundle({ name: "my-skill", body: "## My Skill\nFetch things.", allowedTools: ["web.fetch"] });
  const warnLogs: string[] = [];

  const tool = makeLoadSkillTool(
    [b],
    { warn: (msg: string) => { warnLogs.push(msg); } },
  );

  const result = await tool.execute("call-1", { name: "my-skill" });
  assert.ok(result.content[0]?.text.includes("## My Skill"), "body must be returned");
});

// ── makeLoadSkillTool: ungranted name → error ─────────────────────────────────

test("CP7 load_skill: ungranted name returns error", async () => {
  // WHY: a bundle with disable_model_invocation=true must not be activatable
  // via load_skill. The model has no business calling it — it was excluded from
  // the catalog. Accepting it would let the model bypass the catalog filter.
  const b = bundle({ name: "secret-skill", disableModelInvocation: true });

  const tool = makeLoadSkillTool(
    [b],
    { warn: () => {} },
  );

  // "secret-skill" is not in the load_skill accepted names (disable_model_invocation)
  const result = await tool.execute("call-2", { name: "secret-skill" });
  assert.ok(result.content[0]?.text.toLowerCase().includes("error") ||
            result.content[0]?.text.toLowerCase().includes("unknown") ||
            result.content[0]?.text.toLowerCase().includes("not found"),
    `expected error response, got: ${result.content[0]?.text}`);
});

test("CP7 load_skill: completely unknown name returns error", async () => {
  const b = bundle({ name: "my-skill" });

  const tool = makeLoadSkillTool(
    [b],
    { warn: () => {} },
  );

  const result = await tool.execute("call-3", { name: "does-not-exist" });
  assert.ok(result.content[0]?.text.toLowerCase().includes("error") ||
            result.content[0]?.text.toLowerCase().includes("unknown") ||
            result.content[0]?.text.toLowerCase().includes("not found"),
    `expected error response, got: ${result.content[0]?.text}`);
});

// ── makeLoadSkillTool: allowed_tools is dormant (D5) ──────────────────────────
//
// WHY these tests exist:  D5 established that
// bundle.allowed_tools was validated at write, carried south, and intersected
// with FGA grants into session state that no dispatcher ever read. That
// machinery is gone. These tests pin the new truth directly: activation
// succeeds and returns the body regardless of what allowed_tools contains —
// populated, empty, or naming a tool that no longer exists — because
// authorization now lives entirely at the tool-call gate (gate-tool-call.ts),
// not at skill activation.

test("D5: activation succeeds and returns the body regardless of allowed_tools content", async () => {
  const b = bundle({
    name: "my-skill",
    body: "## My Skill\nDo things.",
    allowedTools: ["web.fetch", "doc.write"], // whatever this lists has no bearing on activation
  });

  const tool = makeLoadSkillTool([b], { warn: () => {} });
  const result = await tool.execute("call-4", { name: "my-skill" });

  assert.ok(result.content[0]?.text.includes("## My Skill\nDo things."), "body must be returned unchanged");
});

test("D5: an empty allowed_tools list does not block activation", async () => {
  const b = bundle({ name: "my-skill", body: "## My Skill", allowedTools: [] });

  const tool = makeLoadSkillTool([b], { warn: () => {} });
  const result = await tool.execute("call-5", { name: "my-skill" });

  assert.ok(result.content[0]?.text.includes("## My Skill"), "empty allowed_tools must not prevent activation");
});

test("D5: a stale tool id (e.g. a deleted connector) in allowed_tools has no effect on activation", async () => {
  // A connector deleted post-upload leaves a tool id in allowed_tools that
  // resolves to nothing. Under the old machinery this was silently dropped
  // from a computed allowlist; now there is no allowlist to drop it from —
  // activation simply succeeds.
  const b = bundle({
    name: "my-skill",
    allowedTools: ["mcp:deleted-connector:search_alerts"],
  });

  const tool = makeLoadSkillTool([b], { warn: () => {} });
  const result = await tool.execute("call-6", { name: "my-skill" });

  assert.ok(!result.content[0]?.text.startsWith("ERROR:"), "a stale allowed_tools entry must not error the activation");
});

// ── makeLoadSkillTool: context_fork inline fallback ───────────────────────────

test("CP7 load_skill context_fork=true: activation completes inline, exactly one warn log", async () => {
  // WHY: spec A4 + Known limitations. context_fork=true is stored but not wired
  // to ChildSupervisor in v1. The turn must still complete (return the body)
  // and emit exactly ONE warn-level log naming the skill and the ignored
  // field, so the gap is visible in logs without breaking the user turn.
  const b = bundle({
    name: "forked-skill",
    allowedTools: ["web.fetch"],
    contextFork: true,
  });
  const warnLogs: string[] = [];

  const tool = makeLoadSkillTool(
    [b],
    { warn: (msg: string) => { warnLogs.push(msg); } },
  );

  const result = await tool.execute("call-7", { name: "forked-skill" });

  // 1. activation completes inline — body returned
  assert.ok(result.content[0]?.text.includes(b.body) || result.content.length > 0,
    "body must be returned for context_fork=true bundle");

  // 2. exactly one warn log
  assert.equal(warnLogs.length, 1, `expected exactly one warn log, got ${warnLogs.length}: ${JSON.stringify(warnLogs)}`);

  // 3. warn log names the skill and the ignored field
  const msg = warnLogs[0]!;
  assert.ok(msg.includes("forked-skill"), `warn log must name the skill; got: ${msg}`);
  assert.ok(msg.includes("context_fork"), `warn log must name the ignored field; got: ${msg}`);
});

test("CP7 load_skill context_fork=false: no warn log emitted", async () => {
  // WHY: the warn is specific to context_fork=true. Non-forked bundles must
  // not produce spurious warn output.
  const b = bundle({ name: "normal-skill", allowedTools: ["web.fetch"], contextFork: false });
  const warnLogs: string[] = [];

  const tool = makeLoadSkillTool(
    [b],
    { warn: (msg: string) => { warnLogs.push(msg); } },
  );

  await tool.execute("call-8", { name: "normal-skill" });

  assert.equal(warnLogs.length, 0, "no warn log for context_fork=false bundle");
});

// ── commandActivate: the /command path (byName-independent for personal, DMI-inclusive) ──
//
// WHY these tests exist: the /command palette pre-activates a skill the parent
// already authorized (resolveCommandSkill validated existence+grant server-side).
// That path must NOT depend on the child's frozen, DMI-filtered byName map — a
// personal skill created after the child spawned, or an admin bundle marked
// disable_model_invocation, is legitimately reachable via /command but absent
// from byName. commandActivate is the chokepoint that bypasses byName for these.

test("commandActivate: personal skill absent from bundles still activates via fresh fetch (after-spawn / staleness fix)", async () => {
  // The personal entry is NOT in the bundles list handed to makeLoadSkillTool
  // (it was created after the child spawned, so byName never learned about it).
  // commandActivate must resolve it purely through the fresh fetchPersonalBody.
  const fetchCalls: string[] = [];
  const fetchPersonalBody: SkillBodyFetcher = async (name) => {
    fetchCalls.push(name);
    return { ok: true, body: "BODY", allowedTools: ["web.fetch", "doc.write"] };
  };

  const tool = makeLoadSkillTool([], { warn: () => {} }, fetchPersonalBody);
  const body = await tool.commandActivate("personal:foo");

  assert.equal(body, "BODY", "body must come from the fresh fetch, not byName");
  assert.deepEqual(fetchCalls, ["foo"], "fetcher receives the bare directory name");
});

test("commandActivate: a failed personal fetch returns an ERROR string", async () => {
  const fetchPersonalBody: SkillBodyFetcher = async () => ({ ok: false, error: "NotFound" });

  const tool = makeLoadSkillTool([], { warn: () => {} }, fetchPersonalBody);
  const result = await tool.commandActivate("personal:gone");

  assert.ok(result.startsWith("ERROR:"), `expected an ERROR string; got: ${result}`);
});

test("commandActivate: an admin disable_model_invocation bundle activates via /command, yet execute() still rejects it", async () => {
  // Proves both halves of the invariant on ONE tool: byNameAll lets /command
  // reach a DMI bundle, while the model's execute() path (byName) still refuses it.
  const dmi = bundle({ name: "admin-dmi", body: "## DMI body", allowedTools: ["web.fetch"], disableModelInvocation: true });

  const tool = makeLoadSkillTool([dmi], { warn: () => {} });

  const commandBody = await tool.commandActivate("admin-dmi");
  assert.ok(commandBody.includes("## DMI body"), `/command must activate a DMI bundle; got: ${commandBody}`);

  const modelResult = await tool.execute("call-dmi", { name: "admin-dmi" });
  assert.ok(
    modelResult.content[0]?.text.startsWith("ERROR:"),
    `the model's load_skill (execute) must still reject a DMI bundle; got: ${modelResult.content[0]?.text}`,
  );
});

test("commandActivate regression: execute() on a normal non-DMI bundle is unchanged (body returned)", async () => {
  const b = bundle({ name: "my-skill", body: "## My Skill\nBody.", allowedTools: ["web.fetch"] });

  const tool = makeLoadSkillTool([b], { warn: () => {} });
  const result = await tool.execute("call-reg", { name: "my-skill" });

  assert.ok(result.content[0]?.text.includes("## My Skill"), "model path still returns the body");
});

// ── activation manifest injection ─────────
//
// WHY these tests exist: activate() is the single shared function behind both
// execute() (the model's load_skill) and commandActivate() (/command) — the
// "## Skill files" manifest must be appended by both, since both call
// activate() internally.

test("CP3 manifest: execute() appends the Skill files block when the bundle has files", async () => {
  const b = bundle({ name: "my-skill", body: "## My Skill\nDo things.", filePaths: ["references/guide.md", "scripts/run.py"] });

  const tool = makeLoadSkillTool([b], { warn: () => {} });
  const result = await tool.execute("call-manifest-1", { name: "my-skill" });
  const text = result.content[0]?.text ?? "";

  assert.ok(text.startsWith("## My Skill\nDo things."), "original body must come first");
  assert.ok(text.includes("## Skill files"), "manifest heading must be present");
  assert.ok(text.includes("- references/guide.md"), "must list the first file");
  assert.ok(text.includes("- scripts/run.py"), "must list the second file");
  assert.ok(text.includes('read_skill_file(skill="my-skill", path="<path>")'), "must teach the exact call convention with the catalog name");
});

test("CP3 manifest: execute() omits the Skill files block when the bundle has no files", async () => {
  const b = bundle({ name: "my-skill", body: "## My Skill\nDo things.", filePaths: [] });

  const tool = makeLoadSkillTool([b], { warn: () => {} });
  const result = await tool.execute("call-manifest-2", { name: "my-skill" });

  assert.equal(result.content[0]?.text, "## My Skill\nDo things.", "no manifest block when filePaths is empty");
});

test("CP3 manifest: commandActivate() (/command path) appends the same Skill files block", async () => {
  // WHY: commandActivate shares the same internal activate() as execute() — this
  // proves the /command pre-activation path gets the identical manifest, not a
  // separately-maintained copy.
  const dmi = bundle({ name: "admin-dmi", body: "## DMI body", disableModelInvocation: true, filePaths: ["LICENSE.txt"] });

  const tool = makeLoadSkillTool([dmi], { warn: () => {} });
  const body = await tool.commandActivate("admin-dmi");

  assert.ok(body.startsWith("## DMI body"));
  assert.ok(body.includes("## Skill files"));
  assert.ok(body.includes("- LICENSE.txt"));
  assert.ok(body.includes('read_skill_file(skill="admin-dmi", path="<path>")'));
});

test("CP3 manifest: commandActivate() omits the block when the bundle has no files", async () => {
  const b = bundle({ name: "my-skill", body: "## My Skill", filePaths: [] });

  const tool = makeLoadSkillTool([b], { warn: () => {} });
  const body = await tool.commandActivate("my-skill");

  assert.equal(body, "## My Skill");
});

test("CP3 manifest: a personal entry's fetched filePaths (not the plan-build-time []) drive the manifest", async () => {
  const p = personalEntry("my-notes");
  const fetchPersonalBody: SkillBodyFetcher = async () => ({
    ok: true,
    body: "## My Notes",
    allowedTools: ["web.fetch"],
    filePaths: ["notes.md"],
  });

  const tool = makeLoadSkillTool([p], { warn: () => {} }, fetchPersonalBody);
  const result = await tool.execute("call-manifest-3", { name: "personal:my-notes" });
  const text = result.content[0]?.text ?? "";

  assert.ok(text.includes("## Skill files"));
  assert.ok(text.includes("- notes.md"));
  assert.ok(text.includes('read_skill_file(skill="personal:my-notes", path="<path>")'));
});
