// Maps a session user to the agent identity that acts on their behalf.
// Default: <localpart>-agent (e.g. alice@example.com → alice-agent).
// Overridable via a config map (AIKONOS_AGENT_FOR_USER env, parsed as JSON).

export function agentForUser(
  userId: string,
  overrides: Record<string, string>,
): string {
  if (overrides[userId]) return overrides[userId];
  const localpart = userId.includes("@") ? userId.split("@")[0] : userId;
  return `${localpart}-agent`;
}

// Parse AIKONOS_AGENT_FOR_USER env var (JSON object) into an override map.
// Returns an empty map if unset, or if valid JSON that isn't an object.
// Malformed JSON fails loud at startup (F26) instead of silently degrading to
// an empty map — a typo'd env var should surface immediately, not as a
// mysteriously-missing override discovered later in production.
export function agentOverridesFromEnv(): Record<string, string> {
  const raw = process.env["AIKONOS_AGENT_FOR_USER"];
  if (!raw) return {};
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch (err) {
    const reason = err instanceof Error ? err.message : String(err);
    throw new Error(`invalid AIKONOS_AGENT_FOR_USER: malformed JSON (${reason})`);
  }
  if (typeof parsed === "object" && parsed !== null && !Array.isArray(parsed)) {
    const result: Record<string, string> = {};
    for (const [k, v] of Object.entries(parsed)) {
      if (typeof v === "string") result[k] = v;
    }
    return result;
  }
  return {};
}
