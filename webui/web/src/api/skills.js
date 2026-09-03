// Personal skills client.
// Request bodies are snake_case per the gateway's wire contract (user_id /
// group_id / mode / name_override); response bodies are the gateway's native
// camelCase. GET /skills 403s (no skill:personal-skills grant) resolve to
// { forbidden: true } via client.js, same convention as every other gated view.
import { get, post, del, upload } from "./client.js";

export function listSkills() {
  return get("/skills");
}

// content is a string (text/markdown SKILL.md) or an ArrayBuffer (application/zip
// bundle) — mirrors api/admin.js's uploadSkillBundle. A 409 (name already taken)
// throws an Error whose `.data.suggested_name` (attached by client.js) carries
// the broker's free-name hint.
export function importSkill(content, contentType) {
  return upload("/skills/import", { body: content, contentType });
}

export function deleteSkill(name) {
  return del(`/skills/${encodeURIComponent(name)}`);
}

// recipient is { user_id } or { group_id } — the gateway's snake_case oneof.
export function shareSkill(name, recipient) {
  return post(`/skills/${encodeURIComponent(name)}/share`, { body: recipient });
}

export function getSkillTransferPreview(envelopeId) {
  return get(`/skills/transfers/${encodeURIComponent(envelopeId)}`);
}

export function acceptSkillTransfer(envelopeId, body) {
  return post(`/skills/transfers/${encodeURIComponent(envelopeId)}/accept`, { body });
}
