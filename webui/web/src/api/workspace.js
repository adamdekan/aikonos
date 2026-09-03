// Workspace-backend preference: which storage backend (local disk vs the
// tenant's OneDrive OBO connection) the caller's Files explorer/composer/agent
// tools currently route to. Field names
// mirror agent-gateway/src/routes/workspace-prefs.ts exactly.
import { get, put } from "./client.js";

export function getWorkspaceBackend() {
  return get("/workspace/backend");
}

export function setWorkspaceBackend({ backend, onedriveFolderPath } = {}) {
  return put("/workspace/backend", { body: { backend, onedriveFolderPath } });
}

// dir "" lists the drive root — the gateway route defaults an absent query
// param to "" server-side too, so an explicit empty string is equivalent.
export function listOneDriveFolders(dir = "") {
  return get(`/workspace/onedrive/folders?dir=${encodeURIComponent(dir)}`);
}
