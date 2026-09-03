// CP8 tests: gateway /command resolution (resolveCommandSkill).
//
// WHY these tests exist:
//   The gateway is the authoritative resolver for /command invocations. The
//   client palette (CP9) is discovery-only — it sends {skillName} but the
//   gateway re-resolves the bundle from south and re-checks the FGA-filtered
//   grant list before activating. These tests prove:
//     1. A granted skill name resolves to its bundle (body).
//     2. An unknown name returns a user-facing error (not a crash).
//     3. A non-granted name (not in the FGA-filtered south list) is denied.
//     4. A bundle with disable_model_invocation=true IS still activatable via
//        /command — it is only excluded from the model's auto-catalog.
//   The south client is mocked; no live broker is involved.
//
// Wire-format note (for CP9 client palette):
//   The client submits { skillName: string } in the POST /agui body (the
//   RunBody.skillName field). The gateway resolves it server-side and ignores
//   the bundle body the client sends — only the name is trusted from the client.
import { test } from "node:test";
import assert from "node:assert/strict";
import {
  resolveCommandSkill,
  type CommandSkillResult,
  type CommandSkillSouth,
} from "../src/routes/agui.js";
import type { SkillBundleEntry } from "../src/ipc/protocol.js";
import type { PersonalSkillListLike } from "../src/pi/session-plan.js";

// ── helpers ────────────────────────────────────────────────────────────────────

function bundle(overrides: Partial<SkillBundleEntry> = {}): SkillBundleEntry {
  return {
    id: overrides.id ?? "aaa",
    name: overrides.name ?? "my-skill",
    description: overrides.description ?? "Does something useful",
    body: overrides.body ?? "## My Skill\nUse web_fetch to look things up.",
    allowedTools: overrides.allowedTools ?? ["web.fetch"],
    contextFork: overrides.contextFork ?? false,
    keywords: overrides.keywords ?? [],
    disableModelInvocation: overrides.disableModelInvocation ?? false,
    filePaths: overrides.filePaths ?? [],
  };
}

// Minimal south stub matching CommandSkillSouth (the narrow interface that
// resolveCommandSkill actually uses — not the full SouthClient proto type).
function makeSouth(grantedBundles: SkillBundleEntry[]): CommandSkillSouth {
  return {
    listUserAgentSkills: async (_req: { tenantId: string; userId: string }) => ({
      bundles: grantedBundles,
    }),
  };
}

// ── CP8.1: granted skill resolves to bundle ────────────────────────────────────

test("CP8 resolveCommandSkill: granted skill name returns bundle body", async () => {
  // WHY: the primary happy path. Client sends {skillName:"my-skill"}; gateway
  // must resolve it from south, confirm grant, and return the bundle body.
  const b = bundle({ name: "my-skill", body: "## My Skill\nDo things.", allowedTools: ["web.fetch"] });
  const south = makeSouth([b]);

  const result: CommandSkillResult = await resolveCommandSkill({
    south,
    tenantId: "tenant-1",
    userId: "user-1",
    skillName: "my-skill",
  });

  assert.equal(result.kind, "ok", `expected ok, got: ${JSON.stringify(result)}`);
  if (result.kind !== "ok") return;
  assert.ok(result.bundle.body.includes("## My Skill"), "bundle body must be returned");
  assert.equal(result.bundle.name, "my-skill");
});

// ── CP8.2: unknown skill name → user-facing error ─────────────────────────────

test("CP8 resolveCommandSkill: unknown skill name returns user-facing error", async () => {
  // WHY: a name not in the FGA-filtered south list is unknown. The gateway must
  // return a structured error (not throw, not 500) so the AGUI route can emit
  // a text delta with the error message and finish the run cleanly.
  const south = makeSouth([bundle({ name: "other-skill" })]);

  const result: CommandSkillResult = await resolveCommandSkill({
    south,
    tenantId: "tenant-1",
    userId: "user-1",
    skillName: "does-not-exist",
  });

  assert.equal(result.kind, "error", `expected error, got: ${JSON.stringify(result)}`);
  if (result.kind !== "error") return;
  assert.ok(
    result.message.toLowerCase().includes("does-not-exist") ||
    result.message.toLowerCase().includes("unknown") ||
    result.message.toLowerCase().includes("not found"),
    `error message must mention the skill name or be recognisably an error; got: ${result.message}`,
  );
});

// ── CP8.3: non-granted skill (not in FGA list) → denied ──────────────────────

test("CP8 resolveCommandSkill: non-granted skill (absent from FGA-filtered list) is denied", async () => {
  // WHY: the south list is the FGA-filtered granted set. A name not in it means
  // the user does not hold can_use. Returning an error (not the bundle) is the
  // deny-by-default posture — fail-closed.
  // This is the same code path as "unknown name" because the gateway never
  // distinguishes "exists but not granted" from "unknown": the south returns
  // ONLY granted bundles, so a miss is always "not granted OR unknown".

  // south returns no bundles for this user (none granted)
  const south = makeSouth([]);

  const result: CommandSkillResult = await resolveCommandSkill({
    south,
    tenantId: "tenant-1",
    userId: "user-1",
    skillName: "my-skill",
  });

  assert.equal(result.kind, "error", `non-granted skill must be denied; got: ${JSON.stringify(result)}`);
});

// ── CP8.4: disable_model_invocation=true bundle IS activatable via /command ───

test("CP8 resolveCommandSkill: disable_model_invocation=true bundle is activatable via /command", async () => {
  // WHY: spec §CP8 success criterion — disable_model_invocation only excludes
  // the bundle from the model's auto-catalog (load_skill rejects it). Explicit
  // /command invocation by the user must still work. This is the key difference
  // between /command and load_skill: /command does NOT filter on
  // disableModelInvocation; load_skill does.
  const b = bundle({
    name: "admin-skill",
    disableModelInvocation: true,
    body: "## Admin Skill\nFor admins only.",
    allowedTools: ["web.fetch"],
  });
  const south = makeSouth([b]);

  const result: CommandSkillResult = await resolveCommandSkill({
    south,
    tenantId: "tenant-1",
    userId: "user-1",
    skillName: "admin-skill",
  });

  assert.equal(result.kind, "ok",
    `disable_model_invocation=true bundle must be activatable via /command; got: ${JSON.stringify(result)}`);
  if (result.kind !== "ok") return;
  assert.equal(result.bundle.name, "admin-skill");
  assert.ok(result.bundle.disableModelInvocation, "disableModelInvocation flag must be preserved on bundle");
});

// ── CP8.5: allowed_tools is dormant (D5) ──────────────────────────────────────

test("D5 resolveCommandSkill: bundle.allowedTools is returned verbatim, unfiltered — no intersection computed", async () => {
  // WHY (D5): the allowed_tools ∩ user_grants intersection used to be computed
  // here but fed nothing downstream (the child session's tool_call gate never read it).
  // Authorization now happens solely at the tool-call gate; resolveCommandSkill just
  // returns the bundle as stored, allowedTools included verbatim for display/discovery.
  const b = bundle({
    name: "my-skill",
    allowedTools: ["web.fetch", "doc.write"],
  });
  const south = makeSouth([b]);

  const result: CommandSkillResult = await resolveCommandSkill({
    south,
    tenantId: "tenant-1",
    userId: "user-1",
    skillName: "my-skill",
  });

  assert.equal(result.kind, "ok", `expected ok; got: ${JSON.stringify(result)}`);
  if (result.kind !== "ok") return;
  assert.deepEqual(result.bundle.allowedTools, ["web.fetch", "doc.write"], "allowedTools must pass through unfiltered");
  assert.ok(!("effectiveAllowlist" in result), "no effectiveAllowlist field on the result (removed, D5)");
});

// ── CP8.6: south RPC failure → error result (no crash) ────────────────────────

test("CP8 resolveCommandSkill: south RPC failure returns error result, does not throw", async () => {
  // WHY: south failures must degrade gracefully. The AGUI route surfaces the
  // error as a user-facing message (text delta + run_finished), not a 500.
  const { resolveCommandSkill: resolve } = await import("../src/routes/agui.js");

  const failSouth: CommandSkillSouth = {
    listUserAgentSkills: async (_req: { tenantId: string; userId: string }) => {
      throw new Error("south unavailable");
    },
  };

  let result: CommandSkillResult | undefined;
  await assert.doesNotReject(async () => {
    result = await resolveCommandSkill({
      south: failSouth,
      tenantId: "tenant-1",
      userId: "user-1",
      skillName: "my-skill",
    });
  }, "resolveCommandSkill must not throw when south fails");

  assert.ok(result !== undefined);
  assert.equal(result!.kind, "error", `south failure must produce an error result; got: ${JSON.stringify(result)}`);
});

// ── CP5: /command must
// resolve a personal entry by its qualified "personal:<name>" ────────────────

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

test("CP5 resolveCommandSkill: resolves a personal skill by its qualified 'personal:<name>'", async () => {
  const south: CommandSkillSouth = {
    listUserAgentSkills: async () => ({ bundles: [] }),
    listPersonalSkillsSouth: async () => ({ skills: [personalSkill({ name: "my-notes" })] }),
  };

  const result = await resolveCommandSkill({
    south, tenantId: "tenant-1", userId: "user-1", skillName: "personal:my-notes",
  });

  assert.equal(result.kind, "ok", `expected ok, got: ${JSON.stringify(result)}`);
  if (result.kind !== "ok") return;
  assert.equal(result.bundle.origin, "personal");
});

test("CP5 resolveCommandSkill: a bare (unqualified) name resolving both a bundle and a same-named personal skill picks the bundle", async () => {
  // Collision rule: exact /command name →
  // bundle wins; the personal entry stays reachable under its qualified
  // "personal:<name>" name. Structural, not a special-cased branch — the
  // personal entry's name/id is always prefixed, so a bare name can only
  // ever match the bundle.
  const b = bundle({ name: "my-notes", body: "## Bundle version" });
  const south: CommandSkillSouth = {
    listUserAgentSkills: async () => ({ bundles: [b] }),
    listPersonalSkillsSouth: async () => ({ skills: [personalSkill({ name: "my-notes" })] }),
  };

  const result = await resolveCommandSkill({
    south, tenantId: "tenant-1", userId: "user-1", skillName: "my-notes",
  });

  assert.equal(result.kind, "ok");
  if (result.kind !== "ok") return;
  assert.equal(result.bundle.body, "## Bundle version", "the admin bundle must win the bare-name lookup");
});

test("CP5 resolveCommandSkill: a south lacking listPersonalSkillsSouth resolves bundles unaffected (fail-open)", async () => {
  const b = bundle({ name: "my-skill" });
  const south: CommandSkillSouth = { listUserAgentSkills: async () => ({ bundles: [b] }) };

  const result = await resolveCommandSkill({
    south, tenantId: "tenant-1", userId: "user-1", skillName: "my-skill",
  });

  assert.equal(result.kind, "ok");
});
