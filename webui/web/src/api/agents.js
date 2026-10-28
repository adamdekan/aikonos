import { get, put } from "./client.js";

export function listMyAgents() {
  return get("/agents");
}

export function getAgentSoul(id) {
  return get(`/agents/${id}/soul`);
}

export function setAgentSoul(id, soul) {
  return put(`/agents/${id}/soul`, { body: { soul } });
}

// AG-01: the agent a chat or schedule runs as when none is picked.
// agentId "" = the organisation's default agent.
export function getDefaultAgent() {
  return get("/agents/default");
}

export function setDefaultAgent(agentId) {
  return put("/agents/default", { body: { agentId } });
}
