// Shared history[] validator for the two surfaces that accept client-held
// conversation history (external :8090 invoke API and the internal /agui run
// endpoint) — extracted so both enforce identical caps instead of drifting
//.
import type { ConvMessage } from "./ipc/protocol.js";

export const HISTORY_MAX_TURNS = 100;
export const HISTORY_MAX_BYTES = 200 * 1024; // 200 KiB

export type HistoryValidation =
  | { ok: true; history: ConvMessage[] }
  | { ok: false; error: string };

function isConvMessage(item: unknown): item is ConvMessage {
  if (typeof item !== "object" || item === null) return false;
  if (!("role" in item) || !("content" in item)) return false;
  return (item.role === "user" || item.role === "assistant") && typeof item.content === "string";
}

// validateHistory shape-checks the optional history field. Absent → empty
// history (today's no-history behavior). Anything malformed → a one-line
// error for the 400 response.
export function validateHistory(raw: unknown): HistoryValidation {
  if (raw === undefined) return { ok: true, history: [] };
  if (!Array.isArray(raw)) return { ok: false, error: "history must be an array" };
  if (raw.length > HISTORY_MAX_TURNS) {
    return { ok: false, error: `history exceeds ${HISTORY_MAX_TURNS} turns` };
  }

  const history: ConvMessage[] = [];
  let totalBytes = 0;
  for (const item of raw) {
    if (!isConvMessage(item)) {
      return { ok: false, error: "each history entry must have role \"user\"|\"assistant\" and string content" };
    }
    totalBytes += Buffer.byteLength(item.content, "utf8");
    history.push(item);
  }
  if (totalBytes > HISTORY_MAX_BYTES) {
    return { ok: false, error: `history content exceeds ${HISTORY_MAX_BYTES} bytes` };
  }
  return { ok: true, history };
}
