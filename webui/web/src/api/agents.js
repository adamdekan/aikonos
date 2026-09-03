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
