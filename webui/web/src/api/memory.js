// OKF memory bundle management. Query/body keys are snake_case to match the gateway's
// routes/memory.ts scopeRequest() mapping; tenant and user come from the
// verified principal there, never from here.
import { get, post } from "./client.js";

export function listMemoryGroups() {
  return get("/memory/groups");
}

// scope is "user" | "group" | "agent"; groupId/agentId apply to their own scope.
function scopeQuery({ scope, groupId, agentId } = {}) {
  const qs = new URLSearchParams({ scope: scope ?? "" });
  if (groupId) qs.set("group_id", groupId);
  if (agentId) qs.set("agent_id", agentId);
  return qs;
}

function scopeBody({ scope, groupId, agentId, id }) {
  return {
    scope,
    ...(groupId ? { group_id: groupId } : {}),
    ...(agentId ? { agent_id: agentId } : {}),
    id,
  };
}

export function listMemoryConcepts(args = {}) {
  return get(`/memory?${scopeQuery(args)}`);
}

export function getMemoryConcept({ scope, groupId, agentId, id }) {
  const qs = scopeQuery({ scope, groupId, agentId });
  qs.set("id", id);
  return get(`/memory/concept?${qs}`);
}

// contentSha256: the hash getMemoryConcept returned for the content the
// reviewer saw (G19); the broker refuses a verify of anything since changed.
export function verifyMemoryConcept({ contentSha256, ...args }) {
  return post("/memory/verify", { body: { ...scopeBody(args), content_sha256: contentSha256 } });
}

export function deprecateMemoryConcept(args) {
  return post("/memory/deprecate", { body: scopeBody(args) });
}

export function deleteMemoryConcept(args) {
  return post("/memory/delete", { body: scopeBody(args) });
}
