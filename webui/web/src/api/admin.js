import { get, post, put, patch, del, upload } from "./client.js";

// ── Role/assignment management ────────────────────────────────────────────────

export function listAssignments() {
  return get("/admin/assignments");
}

export function assignRole(tuple) {
  return post("/admin/assignments", { body: { tuple } });
}

export function revokeRole(tuple) {
  return post("/admin/assignments/revoke", { body: { tuple } });
}

// ── Network access-list ───────────────────────────────────────────────────────

export function listNetworkRules() {
  return get("/admin/network");
}

export function addNetworkRule({ scopeKind, scopeValue, action, hostPattern, note }) {
  return post("/admin/network", { body: { scopeKind, scopeValue, action, hostPattern, note } });
}

export function deleteNetworkRule(id) {
  return post("/admin/network/delete", { body: { id } });
}

// ── Provisioning rules ───────────────────────────────────────────────────────

export function listProvisioningRules() {
  return get("/admin/provisioning");
}

export function addProvisioningRule(matcher, groups) {
  return post("/admin/provisioning", { body: { matcher, groups } });
}

export function deleteProvisioningRule(id) {
  return post("/admin/provisioning/delete", { body: { id } });
}

// ── MCP connections ───────────────────────────────────────────────────────────

export function listMcpConnections() {
  return get("/admin/mcp");
}

export function addMcpConnection(body) {
  return post("/admin/mcp", { body });
}

export function updateMcpConnection(id, body) {
  return patch(`/admin/mcp/${id}`, { body });
}

export function deleteMcpConnection(id) {
  return del(`/admin/mcp/${id}`);
}

// ── Admin scheduled-run oversight ─────────────────────────────────────────────

export function listAdminRuns(owner) {
  const qs = owner ? `?owner=${encodeURIComponent(owner)}` : "";
  return get(`/admin/scheduled-runs${qs}`);
}

// ── Agents ───────────────────────────────────────────────────────────────────

export function listAgents() {
  return get("/admin/agents");
}

export function createAgent(body) {
  return post("/admin/agents", { body });
}

export function updateAgent(id, body) {
  return patch(`/admin/agents/${id}`, { body });
}

export function deleteAgent(id) {
  return del(`/admin/agents/${id}`);
}

// ── Per-agent API keys ────────────────────────────────────────────────────────

export function mintAgentApiKey(agentId, label = "") {
  return post(`/admin/agents/${agentId}/keys`, { body: { label } });
}

export function listAgentApiKeys(agentId) {
  return get(`/admin/agents/${agentId}/keys`);
}

export function revokeAgentApiKey(agentId, keyId) {
  return del(`/admin/agents/${agentId}/keys/${keyId}`);
}

// ── Skills vocabulary ─────────────────────────────────────────────────────────

export function listSkills() {
  return get("/admin/skills");
}

export function upsertSkill(toolId, { effectClass, displayName, description, enabled } = {}) {
  // scope is derived by the broker, never sent from the UI
  return post("/admin/skills", { body: { toolId, effectClass, displayName, description, enabled } });
}

export function deleteSkill(toolId) {
  return del(`/admin/skills/${encodeURIComponent(toolId)}`);
}

// ── Agent skill bundles ───────────────────────────────────────────────────────

// listSkillBundles: admin view — all tenant bundles (requires tenant-admin).
export function listSkillBundles() {
  return get("/admin/skill-bundles");
}

// listUserSkillBundles: user-facing — only the calling user's FGA-granted bundles.
// Used by the /command palette in Composer.vue (discovery only; server re-checks at submit).
export function listUserSkillBundles() {
  return get("/user/skill-bundles");
}

// listUserSkills: user-facing — the calling user's FGA-granted skill ids
// (tool ids + capability skills like "scheduler"/"workflows"). Discovery only;
// the sidebar uses it to hide skill-gated feature nav, the broker re-checks
// every RPC server-side.
export function listUserSkills() {
  return get("/user/skills");
}

export function uploadSkillBundle(content, contentType = "text/markdown") {
  return upload("/admin/skills/upload", { body: content, contentType });
}

// keywords, when provided (including an empty array to clear), is sent as a
// comma-separated ?keywords= query param — the broker normalizes it server-side.
export function updateSkillBundle(id, content, contentType = "text/markdown", keywords) {
  let path = `/admin/skill-bundles/${encodeURIComponent(id)}`;
  if (keywords !== undefined) {
    path += `?keywords=${keywords.map(encodeURIComponent).join(",")}`;
  }
  return upload(path, { body: content, contentType, method: "PUT" });
}

export function deleteSkillBundle(id) {
  return del(`/admin/skill-bundles/${encodeURIComponent(id)}`);
}

export function grantSkillBundle(id, groupName) {
  return post("/admin/assignments", {
    body: {
      tuple: {
        user: `group:${groupName}#member`,
        relation: "can_use",
        object: `agentskill:${id}`,
      },
    },
  });
}

// ── Alerts ────────────────────────────────────────────────────────────────────

export function listAlerts(limit) {
  const qs = limit ? `?limit=${encodeURIComponent(limit)}` : "";
  return get(`/admin/alerts${qs}`);
}

// ── LLM Providers ─────────────────────────────────────────────────────────────

export function listLlmProviders() {
  return get("/admin/providers");
}

export function upsertLlmProvider(provider, apiKey) {
  return post("/admin/providers", { body: { provider, apiKey } });
}

export function deleteLlmProvider(id) {
  return del(`/admin/providers/${id}`);
}

export function setDefaultProvider(id) {
  return post(`/admin/providers/${id}/default`, { body: {} });
}

export function setDefaultVisionProvider(id) {
  return post(`/admin/providers/${id}/default-vision`, { body: {} });
}

export function setFallbackProvider(id) {
  return post(`/admin/providers/${id}/fallback`, { body: {} });
}

// setDefaultProviderFor supersedes the three setters above: one row per
// capability (chat/vision/fallback + the modality capabilities). clear=true
// unsets the capability — the id in the path only names which row to clear.
export function setDefaultProviderFor(id, capability, clear = false) {
  return post(`/admin/providers/${id}/default-for`, { body: { capability, clear } });
}

export function testLlmProvider(provider, apiKey, mode) {
  return post("/admin/providers/test", { body: { provider, apiKey, ...(mode ? { mode } : {}) } });
}

// ── Rate limit policies ────────────────────────────────────────────────────────

export function listRateLimitPolicies() {
  return get("/admin/rate-limits");
}

export function setRateLimitPolicy({ agentId, provider, rpmLimit, tpmLimit }) {
  return post("/admin/rate-limits", { body: { agentId, provider, rpmLimit, tpmLimit } });
}

export function deleteRateLimitPolicy(id) {
  return del(`/admin/rate-limits/${encodeURIComponent(id)}`);
}

// ── Spend caps ────────────────────────────────────

export function listSpendCaps() {
  return get("/admin/spend-caps");
}

export function setSpendCap({ scope, subjectId, capMicros }) {
  return post("/admin/spend-caps", { body: { scope, subjectId, capMicros } });
}

export function deleteSpendCap(id) {
  return del(`/admin/spend-caps/${encodeURIComponent(id)}`);
}

export function getSpendSummary() {
  return get("/admin/spend-caps/summary");
}

// ── M365 tenant connection (tenant-onedrive-obo CP8) ────────────────────────

export function getM365Connection() {
  return get("/admin/m365");
}

// upsertM365Connection sends "" for clientSecret when left blank — the broker
// preserves the stored secret on an edit (same contract as the LLM provider key).
export function upsertM365Connection({ entraTenantId, clientId, clientSecret, enabled }) {
  return put("/admin/m365", { body: { entraTenantId, clientId, clientSecret, enabled } });
}

export function deleteM365Connection() {
  return del("/admin/m365");
}

export function testM365Connection({ entraTenantId, clientId, clientSecret, enabled }) {
  return post("/admin/m365/test", { body: { entraTenantId, clientId, clientSecret, enabled } });
}

// ── web.search engine config ─────────────────

export function getWebSearchConfig() {
  return get("/admin/websearch");
}

// upsertWebSearchConfig sends "" for apiKey when left blank — the broker
// preserves the stored key on an edit (same contract as the M365 client secret).
export function upsertWebSearchConfig({ engine, maxResults, apiKey }) {
  return put("/admin/websearch", { body: { engine, maxResults, apiKey } });
}

export function deleteWebSearchConfig() {
  return del("/admin/websearch");
}

export function testWebSearchConfig({ engine, maxResults, apiKey }) {
  return post("/admin/websearch/test", { body: { engine, maxResults, apiKey } });
}

// ── Org governance settings (A-series) ──────────────────────────────────────

export function getOrgSettings() {
  return get("/admin/org-settings");
}

// updateOrgSettings sends only the fields present in `patch` — the broker merges
// partial, so each admin page updates only the settings it owns.
export function updateOrgSettings(patch) {
  return put("/admin/org-settings", { body: patch });
}

// ── Members roster (A3) ─────────────────────────────────────────────────────

export function listMembers() {
  return get("/admin/members");
}

// ── Observability info (A9) ──────────────────────────────────────────────────

// getObservability returns read-only telemetry-export state. The OTLP endpoint
// is a broker deploy-time env var (AIKONOS_OTEL_ENDPOINT), so this is display-only.
export function getObservability() {
  return get("/admin/observability");
}
