// Pending human-in-the-loop approvals. The governance bridge's approver (server
// mode) registers a deferred promise keyed by toolCallId; the frontend resolves
// it by POSTing to /approve/:id. Approvals are tagged with the acting user so a
// per-user frontend can poll just its own pending approvals (/approvals?user=).
import type { ApprovalInfo } from "../broker/governance";

interface Pending {
  info: ApprovalInfo;
  user: string;
  runId: string;
  resolve: (ok: boolean) => void;
  // Fires the fail-closed timeout; cleared on every resolution path.
  timer?: NodeJS.Timeout;
}

/** In-memory HITL approval store: the governance bridge registers a deferred promise per toolCallId; the frontend resolves it via POST /approve/:id. */
export class ApprovalRegistry {
  private readonly pending = new Map<string, Pending>();

  // timeoutMs bounds how long an approval waits for an answer; 0 disables it.
  // WHY it exists: without it the only exits are a user POST, shutdown drain, or
  // SSE close — so an approval card nobody ever answers holds its child busy for
  // the process's lifetime (Config.approvalTimeoutMs).
  constructor(private readonly timeoutMs = 0) {}

  await_(info: ApprovalInfo, user: string, runId: string): Promise<boolean> {
    return new Promise<boolean>((resolve) => {
      const entry: Pending = { info, user, runId, resolve };
      this.pending.set(info.toolCallId, entry);
      if (this.timeoutMs > 0) {
        // Deny on timeout: an unanswered elevation request is not consent.
        // Routed through resolve() so the entry is removed and the timer cleared
        // by exactly the same code path a manual deny takes.
        entry.timer = setTimeout(() => this.resolve(info.toolCallId, false), this.timeoutMs);
        // A pending approval must never be the reason the process stays alive.
        entry.timer.unref();
      }
    });
  }

  resolve(toolCallId: string, ok: boolean): boolean {
    const p = this.pending.get(toolCallId);
    if (!p) return false;
    this.pending.delete(toolCallId);
    if (p.timer) clearTimeout(p.timer);
    p.resolve(ok);
    return true;
  }

  listForUser(user: string): ApprovalInfo[] {
    const out: ApprovalInfo[] = [];
    for (const [, p] of this.pending) if (p.user === user) out.push(p.info);
    return out;
  }

  // Reject all outstanding approvals (e.g. server shutdown).
  drain(ok = false): void {
    for (const [, p] of this.pending) {
      if (p.timer) clearTimeout(p.timer);
      p.resolve(ok);
    }
    this.pending.clear();
  }

  // Resolve and remove only the approvals belonging to one run (e.g. its
  // /agui connection closed). Other runs' pending approvals are untouched.
  drainForRun(runId: string, ok = false): void {
    for (const [toolCallId, p] of this.pending) {
      if (p.runId !== runId) continue;
      this.pending.delete(toolCallId);
      if (p.timer) clearTimeout(p.timer);
      p.resolve(ok);
    }
  }
}
