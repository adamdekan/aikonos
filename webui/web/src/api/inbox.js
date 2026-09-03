import { get, post } from "./client.js";

export function listInbox() {
  return get("/inbox");
}

export function dismiss(id) {
  return post(`/inbox/${id}/dismiss`, {});
}

export function delegate({ to, group, intent, scopes, maxCost }) {
  if (group != null) {
    return post("/delegate", { body: { group, intent, scopes, maxCost } });
  }
  return post("/delegate", { body: { to, intent, scopes, maxCost } });
}
