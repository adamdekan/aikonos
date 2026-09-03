import { describe, it, expect } from "vitest";
import {
  deriveGroups,
  deriveUser,
  effectiveSkills,
  effectiveSkillBundles,
  tenantRef,
  deriveAgent,
} from "./access-derive.js";

// ── fixtures ──────────────────────────────────────────────────────────────────

const DEV_TUPLES = [
  // tenant
  { user: "user:admin@example.com",  relation: "admin",  object: "tenant:aikonos-dev" },
  { user: "user:alice@example.com",  relation: "member", object: "tenant:aikonos-dev" },
  { user: "user:bob@example.com",    relation: "member", object: "tenant:aikonos-dev" },
  // group members
  { user: "user:alice@example.com",  relation: "member",  object: "group:security-team" },
  { user: "user:bob@example.com",    relation: "member",  object: "group:security-team" },
  { user: "user:alice@example.com",  relation: "manager", object: "group:security-team" },
  // tools (skill:) via group
  { user: "group:security-team#member", relation: "permitted_group", object: "skill:web.fetch" },
  { user: "group:security-team#member", relation: "permitted_group", object: "skill:doc.write" },
  // direct user→tool (as in dev-seed email.draft)
  { user: "user:alice@example.com",  relation: "permitted_group", object: "skill:email.draft" },
  // skill bundles (agentskill:) granted to the group via can_use
  { user: "group:security-team#member", relation: "can_use", object: "agentskill:bundle-research" },
  // agents
  { user: "user:alice@example.com",  relation: "owner_user", object: "agent:alice-agent" },
  { user: "user:bob@example.com",    relation: "owner_user", object: "agent:bob-agent" },
  // scheduler group
  { user: "user:alice@example.com",  relation: "member",         object: "group:schedulers" },
  { user: "group:schedulers#member", relation: "permitted_group", object: "skill:scheduler" },
];

// ── deriveGroups ──────────────────────────────────────────────────────────────

describe("deriveGroups", () => {
  it("returns empty array for empty input", () => {
    expect(deriveGroups([])).toEqual([]);
  });

  it("discovers groups from object refs", () => {
    const tuples = [
      { user: "user:alice@example.com", relation: "member", object: "group:alpha" },
    ];
    const groups = deriveGroups(tuples);
    expect(groups.map((g) => g.name)).toContain("alpha");
  });

  it("discovers groups from group#member subject refs", () => {
    // group:beta#member appears only as a subject — must still be listed
    const tuples = [
      { user: "group:beta#member", relation: "permitted_group", object: "skill:web.fetch" },
    ];
    const groups = deriveGroups(tuples);
    expect(groups.map((g) => g.name)).toContain("beta");
  });

  it("populates members from member relation", () => {
    const groups = deriveGroups(DEV_TUPLES);
    const sec = groups.find((g) => g.name === "security-team");
    expect(sec).toBeDefined();
    expect(sec.members).toContain("user:alice@example.com");
    expect(sec.members).toContain("user:bob@example.com");
  });

  it("populates managers", () => {
    const groups = deriveGroups(DEV_TUPLES);
    const sec = groups.find((g) => g.name === "security-team");
    expect(sec.managers).toEqual(["user:alice@example.com"]);
  });

  it("populates skills (tools) granted via group#member", () => {
    const groups = deriveGroups(DEV_TUPLES);
    const sec = groups.find((g) => g.name === "security-team");
    expect(sec.skills).toEqual(["doc.write", "web.fetch"]);
  });

  it("populates skillBundles granted via can_use on group#member", () => {
    const groups = deriveGroups(DEV_TUPLES);
    const sec = groups.find((g) => g.name === "security-team");
    expect(sec.skillBundles).toEqual(["bundle-research"]);
  });

  it("does not classify a can_use bundle grant as a tool (skills)", () => {
    const groups = deriveGroups(DEV_TUPLES);
    const sec = groups.find((g) => g.name === "security-team");
    expect(sec.skills).not.toContain("bundle-research");
  });

  it("output is sorted by name", () => {
    const groups = deriveGroups(DEV_TUPLES);
    const names = groups.map((g) => g.name);
    expect(names).toEqual([...names].sort());
  });

  it("members are sorted", () => {
    const groups = deriveGroups(DEV_TUPLES);
    const sec = groups.find((g) => g.name === "security-team");
    expect(sec.members).toEqual([...sec.members].sort());
  });

  it("agentsUsable populated", () => {
    const tuples = [
      { user: "group:ops#member", relation: "usable_by", object: "agent:some-agent" },
    ];
    const groups = deriveGroups(tuples);
    const ops = groups.find((g) => g.name === "ops");
    expect(ops.agentsUsable).toEqual(["agent:some-agent"]);
  });

  it("delegatable is false when no marker tuple exists", () => {
    const groups = deriveGroups(DEV_TUPLES);
    const sec = groups.find((g) => g.name === "security-team");
    expect(sec.delegatable).toBe(false);
  });

  it("delegatable is true when marker tuple exists (t.object=group:<name>, t.relation=delegatable)", () => {
    const tuples = [
      ...DEV_TUPLES,
      { user: "group:security-team#member", relation: "delegatable", object: "group:security-team" },
    ];
    const groups = deriveGroups(tuples);
    const sec = groups.find((g) => g.name === "security-team");
    expect(sec.delegatable).toBe(true);
  });

  it("delegatable is scoped to the correct group — other groups remain false", () => {
    const tuples = [
      ...DEV_TUPLES,
      { user: "group:security-team#member", relation: "delegatable", object: "group:security-team" },
    ];
    const groups = deriveGroups(tuples);
    const schedulers = groups.find((g) => g.name === "schedulers");
    expect(schedulers.delegatable).toBe(false);
  });

  it("delegatable is false for group discovered only from subject refs", () => {
    const tuples = [
      { user: "group:beta#member", relation: "permitted_group", object: "skill:web.fetch" },
    ];
    const groups = deriveGroups(tuples);
    const beta = groups.find((g) => g.name === "beta");
    expect(beta.delegatable).toBe(false);
  });

  it("delegatable is false when subject is not the canonical group#member (wrong subject)", () => {
    // tuple has correct object + relation but user is a plain user ref, not group:<name>#member
    const tuples = [
      ...DEV_TUPLES,
      { user: "user:alice@example.com", relation: "delegatable", object: "group:security-team" },
    ];
    const groups = deriveGroups(tuples);
    const sec = groups.find((g) => g.name === "security-team");
    expect(sec.delegatable).toBe(false);
  });
});

// ── deriveUser ────────────────────────────────────────────────────────────────

describe("deriveUser", () => {
  it("returns empty collections for unknown user", () => {
    const u = deriveUser("user:nobody@example.com", DEV_TUPLES);
    expect(u.groups).toEqual([]);
    expect(u.tenantRoles).toEqual([]);
    expect(u.directSkills).toEqual([]);
    expect(u.agentsOwned).toEqual([]);
    expect(u.agentsUsable).toEqual([]);
  });

  it("returns groups user belongs to", () => {
    const u = deriveUser("user:alice@example.com", DEV_TUPLES);
    expect(u.groups).toContain("schedulers");
    expect(u.groups).toContain("security-team");
  });

  it("returns tenant roles", () => {
    const u = deriveUser("user:alice@example.com", DEV_TUPLES);
    expect(u.tenantRoles.some((r) => r.relation === "member" && r.object === "tenant:aikonos-dev")).toBe(true);
  });

  it("returns directSkills for direct permitted_group grant", () => {
    const u = deriveUser("user:alice@example.com", DEV_TUPLES);
    expect(u.directSkills).toContain("email.draft");
  });

  it("does not include group-granted skills in directSkills", () => {
    const u = deriveUser("user:alice@example.com", DEV_TUPLES);
    // web.fetch is only via group, not direct
    expect(u.directSkills).not.toContain("web.fetch");
  });

  it("returns agentsOwned", () => {
    const u = deriveUser("user:alice@example.com", DEV_TUPLES);
    expect(u.agentsOwned).toContain("agent:alice-agent");
  });

  it("groups are sorted", () => {
    const u = deriveUser("user:alice@example.com", DEV_TUPLES);
    expect(u.groups).toEqual([...u.groups].sort());
  });

  it("returns empty agentsUsable when none assigned", () => {
    const u = deriveUser("user:bob@example.com", DEV_TUPLES);
    expect(u.agentsUsable).toEqual([]);
  });
});

// ── effectiveSkills ───────────────────────────────────────────────────────────

describe("effectiveSkills", () => {
  it("returns empty for user with no grants", () => {
    expect(effectiveSkills("user:nobody@example.com", DEV_TUPLES)).toEqual([]);
  });

  it("includes skills via group membership", () => {
    const skills = effectiveSkills("user:alice@example.com", DEV_TUPLES);
    const ids = skills.map((s) => s.skillId);
    expect(ids).toContain("web.fetch");
    expect(ids).toContain("doc.write");
  });

  it("marks via-group provenance correctly", () => {
    const skills = effectiveSkills("user:alice@example.com", DEV_TUPLES);
    const wf = skills.find((s) => s.skillId === "web.fetch");
    expect(wf).toBeDefined();
    expect(wf.provenance.some((p) => p.kind === "group" && p.group === "security-team")).toBe(true);
  });

  it("includes direct grants with kind=direct", () => {
    const skills = effectiveSkills("user:alice@example.com", DEV_TUPLES);
    const ed = skills.find((s) => s.skillId === "email.draft");
    expect(ed).toBeDefined();
    expect(ed.provenance.some((p) => p.kind === "direct")).toBe(true);
  });

  it("does not include group provenance for direct-only grant", () => {
    // email.draft is only directly on alice, not through a group
    const skills = effectiveSkills("user:alice@example.com", DEV_TUPLES);
    const ed = skills.find((s) => s.skillId === "email.draft");
    expect(ed.provenance.every((p) => p.kind === "direct")).toBe(true);
  });

  it("output sorted by skillId", () => {
    const skills = effectiveSkills("user:alice@example.com", DEV_TUPLES);
    const ids = skills.map((s) => s.skillId);
    expect(ids).toEqual([...ids].sort());
  });

  it("deduplicates when user in multiple groups both granting same skill", () => {
    const tuples = [
      { user: "user:u@x.com", relation: "member", object: "group:a" },
      { user: "user:u@x.com", relation: "member", object: "group:b" },
      { user: "group:a#member", relation: "permitted_group", object: "skill:web.fetch" },
      { user: "group:b#member", relation: "permitted_group", object: "skill:web.fetch" },
    ];
    const skills = effectiveSkills("user:u@x.com", tuples);
    expect(skills.filter((s) => s.skillId === "web.fetch")).toHaveLength(1);
    const wf = skills.find((s) => s.skillId === "web.fetch");
    expect(wf.provenance).toHaveLength(2);
  });

  it("bob gets via-group web.fetch", () => {
    const skills = effectiveSkills("user:bob@example.com", DEV_TUPLES);
    const wf = skills.find((s) => s.skillId === "web.fetch");
    expect(wf).toBeDefined();
    expect(wf.provenance[0]).toMatchObject({ kind: "group", group: "security-team" });
  });

  it("excludes agentskill bundle grants from effective tools", () => {
    const skills = effectiveSkills("user:alice@example.com", DEV_TUPLES);
    expect(skills.map((s) => s.skillId)).not.toContain("bundle-research");
  });
});

// ── effectiveSkillBundles ───────────────────────────────────────────────────────

describe("effectiveSkillBundles", () => {
  it("returns empty for user with no bundle grants", () => {
    expect(effectiveSkillBundles("user:nobody@example.com", DEV_TUPLES)).toEqual([]);
  });

  it("includes bundles via group membership with group provenance", () => {
    const bundles = effectiveSkillBundles("user:alice@example.com", DEV_TUPLES);
    const br = bundles.find((b) => b.bundleId === "bundle-research");
    expect(br).toBeDefined();
    expect(br.provenance.some((p) => p.kind === "group" && p.group === "security-team")).toBe(true);
  });

  it("bob also gets the via-group bundle", () => {
    const bundles = effectiveSkillBundles("user:bob@example.com", DEV_TUPLES);
    expect(bundles.map((b) => b.bundleId)).toContain("bundle-research");
  });

  it("does not classify a tool (permitted_group skill) as a bundle", () => {
    const bundles = effectiveSkillBundles("user:alice@example.com", DEV_TUPLES);
    const ids = bundles.map((b) => b.bundleId);
    expect(ids).not.toContain("web.fetch");
    expect(ids).not.toContain("email.draft");
  });

  it("includes direct can_use grants with kind=direct", () => {
    const tuples = [
      { user: "user:c@x.com", relation: "can_use", object: "agentskill:bundle-direct" },
    ];
    const bundles = effectiveSkillBundles("user:c@x.com", tuples);
    const bd = bundles.find((b) => b.bundleId === "bundle-direct");
    expect(bd).toBeDefined();
    expect(bd.provenance.some((p) => p.kind === "direct")).toBe(true);
  });

  it("deduplicates a bundle granted via two groups", () => {
    const tuples = [
      { user: "user:u@x.com", relation: "member", object: "group:a" },
      { user: "user:u@x.com", relation: "member", object: "group:b" },
      { user: "group:a#member", relation: "can_use", object: "agentskill:b1" },
      { user: "group:b#member", relation: "can_use", object: "agentskill:b1" },
    ];
    const bundles = effectiveSkillBundles("user:u@x.com", tuples);
    expect(bundles.filter((b) => b.bundleId === "b1")).toHaveLength(1);
    expect(bundles.find((b) => b.bundleId === "b1").provenance).toHaveLength(2);
  });

  it("output sorted by bundleId", () => {
    const tuples = [
      { user: "user:u@x.com", relation: "can_use", object: "agentskill:zeta" },
      { user: "user:u@x.com", relation: "can_use", object: "agentskill:alpha" },
    ];
    const ids = effectiveSkillBundles("user:u@x.com", tuples).map((b) => b.bundleId);
    expect(ids).toEqual([...ids].sort());
  });
});

// ── deriveAgent ───────────────────────────────────────────────────────────────

describe("deriveAgent", () => {
  const AGENT_TUPLES = [
    // owner
    { user: "user:alice@example.com", relation: "owner_user",    object: "agent:alice-agent" },
    // usable_by — direct user
    { user: "user:bob@example.com",   relation: "usable_by",     object: "agent:alice-agent" },
    // usable_by — group member-set
    { user: "group:ops#member",      relation: "usable_by",     object: "agent:alice-agent" },
    // mcp connector permission — relation is permitted_agent, subject=agent, object=mcp_connector
    { user: "agent:alice-agent",     relation: "permitted_agent", object: "mcp_connector:gdrive-1" },
    { user: "agent:alice-agent",     relation: "permitted_agent", object: "mcp_connector:onedrive-2" },
    // unrelated agent
    { user: "user:bob@example.com",   relation: "owner_user",    object: "agent:bob-agent" },
  ];

  it("returns null owner when no owner_user tuple for agent", () => {
    const d = deriveAgent("agent:ghost", AGENT_TUPLES);
    expect(d.owner).toBeNull();
  });

  it("returns owner userRef", () => {
    const d = deriveAgent("agent:alice-agent", AGENT_TUPLES);
    expect(d.owner).toBe("user:alice@example.com");
  });

  it("returns usableBy sorted", () => {
    const d = deriveAgent("agent:alice-agent", AGENT_TUPLES);
    expect(d.usableBy).toContain("user:bob@example.com");
    expect(d.usableBy).toContain("group:ops#member");
    expect(d.usableBy).toEqual([...d.usableBy].sort());
  });

  it("returns empty usableBy when none", () => {
    const d = deriveAgent("agent:ghost", AGENT_TUPLES);
    expect(d.usableBy).toEqual([]);
  });

  it("returns mcpConnectors sorted", () => {
    const d = deriveAgent("agent:alice-agent", AGENT_TUPLES);
    expect(d.mcpConnectors).toEqual(["gdrive-1", "onedrive-2"]);
  });

  it("returns empty mcpConnectors when none", () => {
    const d = deriveAgent("agent:bob-agent", AGENT_TUPLES);
    expect(d.mcpConnectors).toEqual([]);
  });

  it("does not include other agent tuples", () => {
    const d = deriveAgent("agent:bob-agent", AGENT_TUPLES);
    expect(d.owner).toBe("user:bob@example.com");
    expect(d.usableBy).toEqual([]);
  });

  it("deduplicates usableBy", () => {
    const dupes = [
      { user: "user:x@x.com", relation: "usable_by", object: "agent:a" },
      { user: "user:x@x.com", relation: "usable_by", object: "agent:a" },
    ];
    const d = deriveAgent("agent:a", dupes);
    expect(d.usableBy).toHaveLength(1);
  });
});

// ── tenantRef ─────────────────────────────────────────────────────────────────

describe("tenantRef", () => {
  it("returns null for empty tuples", () => {
    expect(tenantRef([])).toBeNull();
  });

  it("returns null when no tenant tuple exists", () => {
    const tuples = [
      { user: "user:a@b.com", relation: "member", object: "group:x" },
    ];
    expect(tenantRef(tuples)).toBeNull();
  });

  it("returns the tenant object string", () => {
    expect(tenantRef(DEV_TUPLES)).toBe("tenant:aikonos-dev");
  });

  it("returns first tenant found when multiple exist", () => {
    const tuples = [
      { user: "user:a@b.com", relation: "member", object: "tenant:foo" },
      { user: "user:b@b.com", relation: "admin",  object: "tenant:bar" },
    ];
    expect(tenantRef(tuples)).toBe("tenant:foo");
  });
});
