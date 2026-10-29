// Pending human-in-the-loop approvals. The governance bridge's approver (server
// mode) registers a deferred promise per tool call; the user answers it from
// the approval dialog with POST /approve/:id. Every approval belongs to the user
// whose run asked for it: only that user can list it (/approvals) or answer it.
//
// The id is the tool-call id the model chose, not one the gateway mints, so it
// is neither secret nor unique: it shows in the run's stream and the saved
// session, and two users' runs can produce the same one (some local model
// runtimes number their calls). Approvals are therefore keyed by user and id.
import type { ApprovalInfo } from "../broker/governance";

interface Pending {
  info: ApprovalInfo;
  user: string;
  runId: string;
  resolve: (ok: boolean) => void;
  // Fires the fail-closed timeout; cleared on every resolution path.
  timer?: NodeJS.Timeout;
}

/** In-memory HITL approval store: the governance bridge registers a deferred promise per tool call; the user whose run asked answers it via POST /approve/:id. */
export class ApprovalRegistry {
  // Keyed by approvalKey(user, toolCallId).
  private readonly pending = new Map<string, Pending>();

  // timeoutMs bounds how long an approval waits for an answer; 0 disables it.
  // WHY it exists: without it the only exits are a user POST, shutdown drain, or
  // SSE close — so an approval card nobody ever answers holds its child busy for
  // the process's lifetime (Config.approvalTimeoutMs).
  constructor(private readonly timeoutMs = 0) {}

  // decided (AG-07 slice 2) aborts when the broker already holds the decision
  // (another approver voted) or the run ended: the entry is then withdrawn.
  await_(info: ApprovalInfo, user: string, runId: string, decided?: AbortSignal): Promise<boolean> {
    return new Promise<boolean>((resolve) => {
      const key = approvalKey(user, info.toolCallId);
      // Another of this user's runs already waits under the same id. Deny it
      // rather than leave it waiting for an answer that can no longer reach it.
      const earlier = this.pending.get(key);
      if (earlier) this.settle(key, earlier, false);
      const entry: Pending = { info, user, runId, resolve };
      this.pending.set(key, entry);
      if (this.timeoutMs > 0) {
        // Deny on timeout: an unanswered elevation request is not consent.
        // Settles this entry only, by the same path a manual deny takes, so the
        // entry is removed and the timer cleared.
        entry.timer = setTimeout(() => this.settle(key, entry, false), this.timeoutMs);
        // A pending approval must never be the reason the process stays alive.
        entry.timer.unref();
      }
      if (decided?.aborted) this.settle(key, entry, false);
      else decided?.addEventListener("abort", () => this.settle(key, entry, false), { once: true });
    });
  }

  /**
   * Answers one of the user's own pending approvals. "unknown" when the user has none under this id: answered already, timed out, its run ended, or it belongs to someone else.
   * An approval must echo the argsDigest the dialog was shown (AG-07, M-28); a missing or different digest is "mismatch" and the approval keeps waiting. A denial needs no digest.
   */
  resolve(toolCallId: string, user: string, ok: boolean, argsDigest?: string): "resolved" | "unknown" | "mismatch" {
    const key = approvalKey(user, toolCallId);
    const p = this.pending.get(key);
    if (!p) return "unknown";
    if (ok && argsDigest !== p.info.argsDigest) return "mismatch";
    this.settle(key, p, ok);
    return "resolved";
  }

  // threadId narrows the list to one chat's run (the chat's poll), so a card
  // from the same user's other tab never shows up in this chat.
  listForUser(user: string, threadId?: string): ApprovalInfo[] {
    const out: ApprovalInfo[] = [];
    for (const [, p] of this.pending) {
      if (p.user === user && (!threadId || p.info.threadId === threadId)) out.push(p.info);
    }
    return out;
  }

  // Reject all outstanding approvals (e.g. server shutdown).
  drain(ok = false): void {
    for (const [key, p] of this.pending) this.settle(key, p, ok);
  }

  // Resolve and remove only the approvals belonging to one run (e.g. its
  // /agui connection closed). Other runs' pending approvals are untouched.
  drainForRun(runId: string, ok = false): void {
    for (const [key, p] of this.pending) {
      if (p.runId === runId) this.settle(key, p, ok);
    }
  }

  // Every exit (answer, timeout, drain) ends here: remove the entry if it is
  // still the one under its key, stop its timer, deliver the decision.
  private settle(key: string, p: Pending, ok: boolean): void {
    if (this.pending.get(key) === p) this.pending.delete(key);
    if (p.timer) clearTimeout(p.timer);
    p.resolve(ok);
  }
}

// JSON keeps the pair unambiguous whatever characters either part holds.
function approvalKey(user: string, toolCallId: string): string {
  return JSON.stringify([user, toolCallId]);
}
