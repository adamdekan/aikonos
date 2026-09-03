// Per-session LLM usage (model / tokens / cost) for the chat view's usage strip.
// The gateway derives the caller from the verified principal and the broker
// scopes the read to that user's own sessions, so there is no user id to pass.
import { get } from "./client.js";

export function getSessionUsage(sessionId) {
  return get(`/sessions/${encodeURIComponent(sessionId)}/usage`);
}
