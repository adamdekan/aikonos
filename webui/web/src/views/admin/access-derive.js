/**
 * access-derive.js — pure client-side derivation over ListAssignments data.
 *
 * All functions accept pre-svc-filtered tuples (filterTuples/filterPrincipals
 * already applied by the caller). No Vue imports, no side effects.
 *
 * Tuple shape: { user: string, relation: string, object: string }
 * Principal shape: { id: string, kind: string, displayName?: string, email?: string }
 */

// ── helpers ───────────────────────────────────────────────────────────────────

/** Extract the value after the first colon: "group:foo" → "foo". */
function afterColon(s) {
  const i = s.indexOf(":");
  return i < 0 ? s : s.slice(i + 1);
}

/**
 * Strip the OpenFGA member-set suffix: "group:foo#member" → "group:foo",
 * "group:foo" → "group:foo".
 */
function stripMember(s) {
  const i = s.indexOf("#");
  return i < 0 ? s : s.slice(0, i);
}

/** Bare group name from any group ref: "group:foo#member" / "group:foo" → "foo". */
function groupName(s) {
  return afterColon(stripMember(s));
}

function isGroupRef(s) {
  return s.startsWith("group:");
}

function isUserRef(s) {
  return s.startsWith("user:");
}

function isAgentRef(s) {
  return s.startsWith("agent:");
}

function isSkillObj(s) {
  return s.startsWith("skill:");
}

// agentskill: = an agent-skill *bundle* (SKILL.md), surfaced in the UI as a
// "Skill". Distinct from skill: which is an atomic *tool* grant.
function isAgentSkillObj(s) {
  return s.startsWith("agentskill:");
}

function isTenantObj(s) {
  return s.startsWith("tenant:");
}

function skillId(s) {
  return afterColon(s);
}

function agentSkillId(s) {
  return afterColon(s);
}

function sorted(arr) {
  return [...arr].sort();
}

function sortedBy(arr, key) {
  return [...arr].sort((a, b) => (a[key] < b[key] ? -1 : a[key] > b[key] ? 1 : 0));
}

function uniq(arr) {
  return [...new Set(arr)];
}

// ── deriveGroups ──────────────────────────────────────────────────────────────

/**
 * Derive all groups visible in the tuple set.
 *
 * A group exists if it appears as:
 *   - object "group:<name>" in any tuple, OR
 *   - subject "group:<name>#member" in any tuple (member-set reference).
 *
 * Returns [{name, members, managers, skills, skillBundles, agentsUsable, delegatable}] sorted by name.
 *   members: userRef strings that have `member` relation on the group
 *   managers: userRef strings that have `manager` relation on the group
 *   skills: skill (tool) ids granted via `permitted_group` to this group's member-set
 *   skillBundles: agentskill (bundle) ids granted via `can_use` to this group's member-set
 *   agentsUsable: agentRef strings granted `usable_by` to this group's member-set
 *   delegatable: true iff the self-referential marker tuple exists (group#member, delegatable, group)
 */
export function deriveGroups(tuples) {
  // Collect all group names
  const names = new Set();
  for (const t of tuples) {
    if (isGroupRef(t.object)) names.add(groupName(t.object));
    if (isGroupRef(t.user))   names.add(groupName(t.user));
  }

  const groups = [];
  for (const name of names) {
    const groupObj   = `group:${name}`;
    const memberUser = `group:${name}#member`;

    const members = tuples
      .filter((t) => t.object === groupObj && t.relation === "member" && isUserRef(t.user))
      .map((t) => t.user);

    const managers = tuples
      .filter((t) => t.object === groupObj && t.relation === "manager" && isUserRef(t.user))
      .map((t) => t.user);

    // tool grants to this group's member-set (subject = "group:<name>#member")
    const skills = tuples
      .filter((t) => t.user === memberUser && t.relation === "permitted_group" && isSkillObj(t.object))
      .map((t) => skillId(t.object));

    // skill-bundle (agentskill) grants to this group's member-set via can_use
    const skillBundles = tuples
      .filter((t) => t.user === memberUser && t.relation === "can_use" && isAgentSkillObj(t.object))
      .map((t) => agentSkillId(t.object));

    // agents where this group's member-set has usable_by
    const agentsUsable = tuples
      .filter((t) => t.user === memberUser && t.relation === "usable_by" && isAgentRef(t.object))
      .map((t) => t.object);

    // self-referential delegation marker: (group:<name>#member, delegatable, group:<name>)
    const delegatable = tuples.some(
      (t) => t.user === memberUser && t.relation === "delegatable" && t.object === groupObj,
    );

    groups.push({
      name,
      members:      sorted(uniq(members)),
      managers:     sorted(uniq(managers)),
      skills:       sorted(uniq(skills)),
      skillBundles: sorted(uniq(skillBundles)),
      agentsUsable: sorted(uniq(agentsUsable)),
      delegatable,
    });
  }

  return sortedBy(groups, "name");
}

// ── deriveUser ────────────────────────────────────────────────────────────────

/**
 * Derive a user's direct relationships from the tuple set.
 *
 * Returns {groups, tenantRoles, directSkills, agentsOwned, agentsUsable}.
 *   groups:      group names the user is a member of
 *   tenantRoles: [{relation, object}] for tenant objects
 *   directSkills: skill ids where the user is directly the subject of permitted_group
 *   agentsOwned: agentRef strings where user has owner_user
 *   agentsUsable: agentRef strings where user has usable_by
 */
export function deriveUser(userRef, tuples) {
  const groups = tuples
    .filter((t) => t.user === userRef && t.relation === "member" && isGroupRef(t.object))
    .map((t) => groupName(t.object));

  const tenantRoles = tuples
    .filter((t) => t.user === userRef && isTenantObj(t.object))
    .map((t) => ({ relation: t.relation, object: t.object }));

  // Direct user→skill: the FGA model types skill.permitted_group as [group#member],
  // so direct user grants are NOT writable from the UI (manageableRules in roles.go
  // is groupOnly). This branch surfaces only pre-existing store tuples — revoke works,
  // create does not.
  const directSkills = tuples
    .filter((t) => t.user === userRef && t.relation === "permitted_group" && isSkillObj(t.object))
    .map((t) => skillId(t.object));

  const agentsOwned = tuples
    .filter((t) => t.user === userRef && t.relation === "owner_user" && isAgentRef(t.object))
    .map((t) => t.object);

  const agentsUsable = tuples
    .filter((t) => t.user === userRef && t.relation === "usable_by" && isAgentRef(t.object))
    .map((t) => t.object);

  return {
    groups:       sorted(uniq(groups)),
    tenantRoles:  sortedBy(tenantRoles, "relation"),
    directSkills: sorted(uniq(directSkills)),
    agentsOwned:  sorted(uniq(agentsOwned)),
    agentsUsable: sorted(uniq(agentsUsable)),
  };
}

// ── effectiveSkills ───────────────────────────────────────────────────────────

/**
 * Compute the effective skill set for a user: union of direct grants and
 * skills inherited via group membership.
 *
 * Returns [{skillId, provenance: [{kind: "direct"} | {kind: "group", group: name}]}]
 * sorted by skillId; provenance entries sorted by kind then group.
 *
 * "direct" appears only when the user is literally the subject on a
 * permitted_group tuple for a skill (e.g. alice→email.draft in dev-seed).
 */
export function effectiveSkills(userRef, tuples) {
  const bySkill = new Map(); // skillId → Set of provenance JSON strings (dedup)

  // Direct grants
  for (const t of tuples) {
    if (t.user === userRef && t.relation === "permitted_group" && isSkillObj(t.object)) {
      const sid = skillId(t.object);
      if (!bySkill.has(sid)) bySkill.set(sid, []);
      bySkill.get(sid).push({ kind: "direct" });
    }
  }

  // Via-group grants: find user's groups, then skills of those groups
  const userGroups = tuples
    .filter((t) => t.user === userRef && t.relation === "member" && isGroupRef(t.object))
    .map((t) => groupName(t.object));

  for (const gname of uniq(userGroups)) {
    const memberUser = `group:${gname}#member`;
    for (const t of tuples) {
      if (t.user === memberUser && t.relation === "permitted_group" && isSkillObj(t.object)) {
        const sid = skillId(t.object);
        if (!bySkill.has(sid)) bySkill.set(sid, []);
        bySkill.get(sid).push({ kind: "group", group: gname });
      }
    }
  }

  const result = [];
  for (const [sid, provList] of bySkill) {
    // Deduplicate and sort provenance
    const seen = new Set();
    const prov = [];
    for (const p of provList) {
      const key = p.kind === "direct" ? "direct" : `group:${p.group}`;
      if (!seen.has(key)) { seen.add(key); prov.push(p); }
    }
    prov.sort((a, b) => {
      if (a.kind !== b.kind) return a.kind < b.kind ? -1 : 1;
      if (a.group && b.group) return a.group < b.group ? -1 : a.group > b.group ? 1 : 0;
      return 0;
    });
    result.push({ skillId: sid, provenance: prov });
  }

  return sortedBy(result, "skillId");
}

// ── effectiveSkillBundles ───────────────────────────────────────────────────────

/**
 * Compute the effective skill-bundle (agentskill) set for a user: union of
 * direct grants and bundles inherited via group membership.
 *
 * Returns [{bundleId, provenance: [{kind: "direct"} | {kind: "group", group: name}]}]
 * sorted by bundleId; provenance sorted by kind then group.
 *
 * The FGA model types agentskill.can_use as [group#member], so direct user
 * grants are not writable from the UI — "direct" appears only if a pre-existing
 * store tuple grants the user can_use literally.
 */
export function effectiveSkillBundles(userRef, tuples) {
  const byBundle = new Map(); // bundleId → provenance list

  // Direct grants
  for (const t of tuples) {
    if (t.user === userRef && t.relation === "can_use" && isAgentSkillObj(t.object)) {
      const bid = agentSkillId(t.object);
      if (!byBundle.has(bid)) byBundle.set(bid, []);
      byBundle.get(bid).push({ kind: "direct" });
    }
  }

  // Via-group grants
  const userGroups = tuples
    .filter((t) => t.user === userRef && t.relation === "member" && isGroupRef(t.object))
    .map((t) => groupName(t.object));

  for (const gname of uniq(userGroups)) {
    const memberUser = `group:${gname}#member`;
    for (const t of tuples) {
      if (t.user === memberUser && t.relation === "can_use" && isAgentSkillObj(t.object)) {
        const bid = agentSkillId(t.object);
        if (!byBundle.has(bid)) byBundle.set(bid, []);
        byBundle.get(bid).push({ kind: "group", group: gname });
      }
    }
  }

  const result = [];
  for (const [bid, provList] of byBundle) {
    const seen = new Set();
    const prov = [];
    for (const p of provList) {
      const key = p.kind === "direct" ? "direct" : `group:${p.group}`;
      if (!seen.has(key)) { seen.add(key); prov.push(p); }
    }
    prov.sort((a, b) => {
      if (a.kind !== b.kind) return a.kind < b.kind ? -1 : 1;
      if (a.group && b.group) return a.group < b.group ? -1 : a.group > b.group ? 1 : 0;
      return 0;
    });
    result.push({ bundleId: bid, provenance: prov });
  }

  return sortedBy(result, "bundleId");
}

// ── deriveAgent ───────────────────────────────────────────────────────────────

/**
 * Derive relationships for one agent from the tuple set.
 *
 * Returns {owner, usableBy, mcpConnectors}
 *   owner: userRef string (owner_user relation) or null
 *   usableBy: sorted array of user/group subjects that have usable_by on this agent
 *   mcpConnectors: sorted array of connector id strings from mcp_connector tuples
 *     where the subject is "agent:<id>"
 */
export function deriveAgent(agentRef, tuples) {
  const ownerTuple = tuples.find(
    (t) => t.object === agentRef && t.relation === "owner_user" && isUserRef(t.user),
  );
  const owner = ownerTuple ? ownerTuple.user : null;

  const usableBy = sorted(uniq(
    tuples
      .filter((t) => t.object === agentRef && t.relation === "usable_by")
      .map((t) => t.user),
  ));

  // permitted_agent tuples: subject = agent:<id>, relation = permitted_agent, object = mcp_connector:<connId>
  const mcpConnectors = sorted(uniq(
    tuples
      .filter(
        (t) =>
          t.user === agentRef &&
          t.relation === "permitted_agent" &&
          t.object.startsWith("mcp_connector:"),
      )
      .map((t) => afterColon(t.object)),
  ));

  return { owner, usableBy, mcpConnectors };
}

// ── tenantRef ────────────────────────────────────────────────────────────────

/**
 * Return the first "tenant:<name>" object found in tuples whose object starts
 * with "tenant:", or null if no tenant tuple exists yet.
 *
 * Tenant-role controls in the UI are disabled with a hint when this is null.
 */
export function tenantRef(tuples) {
  for (const t of tuples) {
    if (isTenantObj(t.object)) return t.object;
  }
  return null;
}
