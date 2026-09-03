import { get } from "./client.js";

// Users the caller may delegate to (shared delegatable group). Discovery-only.
export function listDelegatableUsers() {
  return get("/delegatable-users");
}
