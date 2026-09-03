// External invocation core: drives a named agent as svc-<agentId> through the
// child supervisor, yielding InvocationEvents live as the child emits them.
//
// Identity is always south-mode (no OIDC token): userId = "svc-<agentId>",
// agentId = the resolved agent UUID. The GovernanceBridge detects the absent
// token and routes to createGatewayTask / approveGatewayTask (south twins).
//
// Events are yielded live as they arrive — the SSE client receives output
// incrementally during the run, not buffered until completion.
//
// CP8: wired to the ChildSupervisor. The supervisor binds ownerGrant from
// the spawn identity record — it is never carried in an IPC message body.
import { randomUUID } from "node:crypto";
import type { Config } from "../config.js";
import type { Logger } from "../log.js";
import type { BrokerClients } from "../broker/clients.js";
import type { Identity } from "../broker/governance.js";
import type { RunIdentity } from "../ipc/bridge-server.js";
import type { ConvMessage } from "../ipc/protocol.js";
import type { ChildSupervisor } from "../ipc/supervisor.js";
import type { AgentSpec } from "../pi/session.js";
import { record } from "../audit/stream.js";
import { externalApprover, cachedApprovalMode } from "./approver.js";
import { EventQueue } from "../pi/event-queue.js";
import { trimmedErrorMessage } from "../http-errors.js";

// InvocationEvent is defined here (canonical owner) and re-exported. headless-run.ts
// was its former home; it is now deleted — this is the only definition.
export interface InvocationEvent {
  type: "text_delta" | "tool_start" | "tool_end" | "usage" | "done" | "error";
  delta?: string;
  toolName?: string;
  toolCallId?: string;
  ok?: boolean;
  result?: string;
  inputTokens?: number;
  outputTokens?: number;
  error?: string;
}

export interface InvocationOpts {
  clients: BrokerClients;
  cfg: Config;
  log: Logger;
  supervisor: ChildSupervisor;
  timeoutMs?: number;
}

const DEFAULT_TIMEOUT_MS = 180_000;

// runAgentInvocation drives a prompt against a named agent as its service
// principal via the child supervisor. InvocationEvents are yielded as the
// child emits them — text_delta and tool_start arrive before the run completes.
//
// agentId and ownerUserId come from key resolution — never from agentForUser()
// or any user-supplied field.
export async function* runAgentInvocation(
  agentId: string,
  ownerUserId: string, // "svc-<agentId>"
  tenantId: string,
  approvedTools: string[], // agent's spec.skills
  prompt: string,
  opts: InvocationOpts,
  spec?: AgentSpec, // the agent's config — resolves its preferred provider/model
  ownerGrant?: string, // broker-minted HMAC grant from resolveAgentApiKey response
  history?: ConvMessage[], // prior turns to seed the lazily-created thread session
): AsyncGenerator<InvocationEvent> {
  const { cfg, log, supervisor } = opts;
  const timeoutMs = opts.timeoutMs ?? DEFAULT_TIMEOUT_MS;

  const identity: Identity = {
    tenantId,
    userId: ownerUserId,
    agentId,
    // WHY ownerGrant is set on the spawn identity and not in any IPC message:
    // the child is untrusted — anything in an IPC body could be forged. The
    // grant must reach GovernanceBridge.createGatewayTask through the parent-
    // side spawn record only.
    ownerGrant,
  };

  const key = supervisor.keyFor(identity);
  const handle = await supervisor.getOrSpawn(key, identity, spec);

  const runId = randomUUID();
  const threadId = `ext-${agentId}-${runId}`;

  const runIdentity: RunIdentity = {
    tenantId,
    userId: ownerUserId,
    agentId,
    token: undefined,
    ownerGrant,
  };

  // WHY externalApprover instead of preAuthApprover: the external surface must
  // re-check approvalMode at each gate. A mid-run admin flip to "needs_approval"
  // must deny subsequent NEEDS_HUMAN gates rather than continuing to auto-approve.
  // The scheduler's preAuthApprover keeps its standing-consent semantics unchanged.
  const getApprovalMode = cachedApprovalMode(
    () => opts.clients.south.getAgentSpec({ agentId, tenantId }).then((r) => r.approvalMode),
  );
  handle.setRunContext(runId, runIdentity, externalApprover(approvedTools, getApprovalMode, log));

  const queue = new EventQueue<InvocationEvent>();

  const runWithTimeout = new Promise<void>((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error("run timed out")), timeoutMs);
    supervisor.run(handle, { runId, threadId, text: prompt, history }, (evt) => {
      if (evt.kind === "text_delta") {
        queue.push({ type: "text_delta", delta: evt.delta });
      } else if (evt.kind === "tool_start") {
        queue.push({ type: "tool_start", toolCallId: evt.toolCallId, toolName: evt.toolName });
      } else if (evt.kind === "tool_end") {
        // Stringify the same way /agui does: string passthrough, else JSON.stringify(result ?? null).
        const resultStr = typeof evt.result === "string" ? evt.result : JSON.stringify(evt.result ?? null);
        queue.push({ type: "tool_end", toolCallId: evt.toolCallId, ok: evt.ok, result: resultStr });
      } else if (evt.kind === "usage") {
        queue.push({ type: "usage", inputTokens: evt.inputTokens, outputTokens: evt.outputTokens });
      }
    }).then(() => {
      clearTimeout(timer);
      resolve();
    }, (err: unknown) => {
      clearTimeout(timer);
      reject(err instanceof Error ? err : new Error(String(err)));
    });
  });

  // Settle the queue when the run resolves or rejects. trimmedErrorMessage
  // (not the raw message) — a gRPC rejection from supervisor.run may carry
  // status/detail internals that must not reach the SSE caller.
  void runWithTimeout.then(
    () => queue.push({ type: "done" }),
    (err: unknown) => queue.push({ type: "error", error: trimmedErrorMessage(err) }),
  ).finally(() => handle.clearRunContext(runId));

  // WHY try/finally: an early generator return() (SSE client disconnect) must
  // stop the in-flight run and clear the run context. abortRun() sends the abort
  // IPC directive to the child (which calls session.abort() + closes the queue),
  // preventing a 180s token burn after the client disconnects. clearRunContext
  // removes the stale per-run identity/approver from the bridge server.
  // The settle-promise finally already fires on normal completion; this covers
  // the early-return path where the settle-promise is still pending.
  try {
    for await (const item of queue.drain()) {
      if (item.type === "done") {
        yield item;
        record({ type: "agent.external.invoked", agentId, principal: ownerUserId, tenantId, status: "completed" });
        return;
      }
      if (item.type === "error") {
        yield item;
        record({ type: "agent.external.invoked", agentId, principal: ownerUserId, tenantId, status: "error", error: item.error });
        return;
      }
      yield item;
    }
  } catch (err) {
    // Queue overflowed — yield terminal error event. trimmedErrorMessage
    // keeps this consistent with the settle-promise error path above.
    const message = trimmedErrorMessage(err);
    yield { type: "error", error: message };
    record({ type: "agent.external.invoked", agentId, principal: ownerUserId, tenantId, status: "error", error: message });
  } finally {
    handle.abortRun(runId);
    handle.clearRunContext(runId);
  }
}
