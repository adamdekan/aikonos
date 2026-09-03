import { get, post } from "./client.js";

export function listConnectors() {
  return get("/connectors");
}

export function listConnectorProviders() {
  return get("/connectors/providers");
}

export function beginConnectorAuth(provider) {
  return post("/connectors/begin", { body: { provider } });
}

export function completeConnectorAuth(code, state) {
  return post("/connectors/complete", { body: { code, state } });
}

export function revokeConnector(id) {
  return post(`/connectors/${id}/revoke`);
}
