// Client-side mirror of the broker's manageable-relation allowlist
// (broker/internal/broker/roles.go). The broker re-validates everything; this
// only drives the UI (tabs + the assign dialog's relation/subject choices).
//
// Subject kinds: "user" → user:<email>, "group" → group:<name>#member,
// "agent" → agent:<id>.

export const SECTIONS = [
  {
    id: 1,
    key: "groups",
    label: "Groups",
    objectType: "group",
    blurb: "Who belongs to which group. Group membership drives skill access.",
    relations: [
      { name: "member", subjects: ["user", "group"] },
      { name: "manager", subjects: ["user"] },
      { name: "delegatable", subjects: ["group"] },
    ],
  },
  {
    id: 2,
    key: "tenant",
    label: "Tenant roles",
    objectType: "tenant",
    blurb: "Tenant-wide admin / member. Admins can manage assignments here.",
    relations: [
      { name: "admin", subjects: ["user"] },
      { name: "member", subjects: ["user", "group"] },
    ],
  },
  {
    id: 3,
    key: "skills",
    label: "Skill access",
    objectType: "skill",
    blurb: "Which groups may invoke which skills (permitted_group).",
    relations: [{ name: "permitted_group", subjects: ["group"] }],
  },
  {
    id: 4,
    key: "agents",
    label: "Agents",
    objectType: "agent",
    objectColumnLabel: "Agent",
    blurb: "Agent ownership and provenance.",
    relations: [
      { name: "owner_user", subjects: ["user"] },
      { name: "spawned_by", subjects: ["user", "agent"] },
    ],
  },
  {
    id: 5,
    key: "mcp",
    label: "MCP access",
    objectType: "mcp_connector",
    blurb: "Which agents may access which MCP servers (permitted_agent).",
    relations: [{ name: "permitted_agent", subjects: ["agent"] }],
  },
];

export function sectionById(id) {
  return SECTIONS.find((s) => s.id === id);
}

// Canonicalize a chosen principal id into the FGA user string. A group is
// referenced by its member-set (group:<name>#member); users/agents are used as-is.
export function subjectRef(id) {
  if (id.startsWith("group:")) return id.endsWith("#member") ? id : `${id}#member`;
  return id;
}

export function principalKind(id) {
  if (id.startsWith("user:")) return "user";
  if (id.startsWith("group:")) return "group";
  if (id.startsWith("agent:")) return "agent";
  return "";
}
