// Scheduler ticker: periodically claims due scheduled runs from the broker and
// fires each unattended through the child supervisor. The broker owns the
// schedule (claim advances next_fire_at so a slow run can't be re-claimed);
// the gateway executes and reports the outcome.
//
// Unattended approvals: a run carries an approved_tools allowlist (standing
// human-consent). The pre-auth approver grants a NEEDS_HUMAN tool iff its aikonos
// tool id is in that list, else denies. It never overrides an OPA DENY or a
// capability-scope failure — those still come back as a blocked tool call.
//
// CP8: both the scheduler ticker and the external :8090 surface route through
// the ChildSupervisor rather than building a GovernanceBridge directly. Owner
// identity (ownerGrant) is bound from the child's spawn record, never carried
// in an IPC message body.
import { randomUUID } from "node:crypto";
import type { Config } from "../config";
import type { Logger } from "../log";
import type { BrokerClients } from "../broker/clients";
import { GovernanceBridge, type Approver, type Identity } from "../broker/governance.js";
import type { RateLimitChecker } from "../llm/egress-proxy.js";
import type { RunResult, StepOutcome } from "../workflow/run.js";
import type { WorkflowDef } from "../workflow/author.js";
import type { RunIdentity } from "../ipc/bridge-server.js";
import { ChildSupervisor } from "../ipc/supervisor.js";
import { agentForUser } from "../broker/agent-identity.js";
import { buildScheduledSessionRecord } from "./session-record.js";
import type { SessionEvent } from "./session-record.js";

const MAX_SUMMARY = 1200;

// F30: cap on how many claimed runs tick() executes concurrently. A serial
// for...of loop lets one slow run (up to schedulerRunTimeoutMs) head-of-line
// block every other run in the same batch — live under the production
// per-user child-keying default, where different owners get different
// children and have no reason to serialize. 4 is a conservative slice of the
// child-pool cap (default 32) — plenty of headroom for interactive /agui and
// external-API children sharing the same pool, while still bounding the
// scheduler's own fan-out per tick.
const SCHEDULER_MAX_CONCURRENT_RUNS = 4;

// Tools always pre-approved for unattended scheduler runs. Without a baseline
// a run with no explicit approvedTools list cannot use any tools, making
// scheduled tasks useless in practice. These three are the minimum viable set;
// all still pass FGA + OPA at plan time — pre-auth only substitutes for HITL.
const SCHEDULER_BASELINE_TOOLS = ["web.fetch", "doc.read", "doc.write"];

interface RunOutcome {
  ok: boolean;
  summary: string;
  sessionRecord: Uint8Array;
  runId: string;
}

// preAuthApprover is the unattended-run approval policy: a tool requiring human
// approval is granted iff its aikonos tool id is in the schedule's standing
// allowlist. Exposed for testing — the governance semantics are the crux of the
// feature.
export function preAuthApprover(approvedTools: string[], log?: Logger): Approver {
  const allow = new Set(approvedTools);
  return async (info) => {
    const ok = allow.has(info.toolId);
    log?.info({ tool: info.toolId, allowed: ok }, "scheduled run: pre-authorized approval decision");
    return ok;
  };
}

// SupervisorDeps is the injection seam for the supervisor used by tick().
// Production callers (startScheduler via server.ts) pass the real supervisor.
// Tests inject a fake supervisor to avoid real forks.
export type { ChildSupervisor as SupervisorDeps };

// runViaChild executes one scheduled prompt via the supervisor. The child's
// IPC events are drained into a RunOutcome (text_delta → summary, done → ok).
async function runViaChild(
  cfg: Config,
  supervisor: ChildSupervisor,
  log: Logger,
  run: { id: string; ownerUserId: string; prompt: string; approvedTools: string[]; ownerGrant: string; runId: string; runAt: string },
  timeoutMs: number,
): Promise<RunOutcome> {
  // Derive the acting agent id exactly as the interactive /agui path does
  // (agentForUser). For a personal run this is "<userId>-agent" — deliberately
  // NOT a UUID — so the broker's CreateGatewayTask stores tasks.agent_id = NULL
  // (an unbound personal task). Passing the bare ownerUserId here (a valid UUID)
  // made the broker try to FK it to a non-existent agents row, failing
  // tasks_agent_id_fkey (SQLSTATE 23503) and blocking every tool call in a
  // scheduled run with "13 INTERNAL: failed to create task". Using agentForUser
  // also makes the scheduler share the user's interactive child (same supervisor
  // key) — the sharing the old bare-ownerUserId comment intended but never achieved.
  const sessionAgentId = agentForUser(run.ownerUserId, cfg.agentForUserOverrides);

  const identity: Identity = {
    tenantId: cfg.defaultTenantId,
    userId: run.ownerUserId,
    agentId: sessionAgentId,
    ownerGrant: run.ownerGrant,
  };

  const key = supervisor.keyFor(identity);
  const handle = await supervisor.getOrSpawn(key, identity);

  const { runId, runAt } = run;
  const threadId = `sched-${run.id}`;

  const runIdentity: RunIdentity = {
    tenantId: cfg.defaultTenantId,
    userId: run.ownerUserId,
    agentId: sessionAgentId,
    token: undefined,
    // WHY ownerGrant is set here and not in any IPC message: the grant must
    // reach GovernanceBridge.createGatewayTask through the parent-side identity
    // record, never through the child's message body. The child is untrusted —
    // anything in an IPC message body could be forged by a compromised child.
    ownerGrant: run.ownerGrant,
    // F22: forwarded as CreateGatewayTask's client_request_id (combined with the
    // per-call toolCallId in GovernanceBridge.gate) so a retried scheduled run
    // doesn't spawn duplicate gateway tasks for the same tool call.
    runId,
  };

  handle.setRunContext(runId, runIdentity, preAuthApprover([...SCHEDULER_BASELINE_TOOLS, ...run.approvedTools], log), runId);

  let text = "";
  const summaryTools: string[] = [];
  const sessionEvents: SessionEvent[] = [];

  const runWithTimeout = new Promise<void>((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error("run timed out")), timeoutMs);
    // sessionId is runId: a scheduled run's session record is filed under its
    // run id (reportScheduledRunResult below sends sessionId: outcome.runId), so
    // this is what makes the resulting usage events joinable to the session the
    // user opens from the Schedules view. Without it the child's usage relay
    // emits session_id="" and a scheduled session shows zero cost.
    supervisor.run(handle, { runId, threadId, sessionId: runId, text: run.prompt }, (evt) => {
      if (evt.kind === "text_delta") {
        text += evt.delta;
        sessionEvents.push({ kind: "text_delta", delta: evt.delta });
      } else if (evt.kind === "tool_start") {
        summaryTools.push(evt.toolName);
        sessionEvents.push({
          kind: "tool_start",
          toolCallId: evt.toolCallId,
          toolName: evt.toolName,
          input: evt.input,
        });
      } else if (evt.kind === "tool_end") {
        sessionEvents.push({
          kind: "tool_end",
          toolCallId: evt.toolCallId,
          ok: evt.ok,
          result: evt.result,
        });
      } else if (evt.kind === "error") {
        sessionEvents.push({ kind: "error", message: evt.message });
      }
    }).then(() => {
      clearTimeout(timer);
      resolve();
    }, (err: unknown) => {
      clearTimeout(timer);
      reject(err instanceof Error ? err : new Error(String(err)));
    });
  });

  try {
    await runWithTimeout;
    // "done" marks lifecycle boundary only — buildScheduledSessionRecord ignores it.
    sessionEvents.push({ kind: "done" });
  } finally {
    // WHY abortRun before clearRunContext: on the timeout branch (and any other
    // early exit) the child is still mid-run — without the abort directive it
    // never settles, so the supervisor's busy flag never clears and reapIdle()
    // can't evict the child, burning tokens indefinitely. Mirrors the pattern in
    // src/external/core.ts's finally.
    handle.abortRun(runId);
    handle.clearRunContext(runId);
  }

  const finishedAt = new Date().toISOString();
  const summary = `tools: [${summaryTools.join(", ")}] · ${text.trim()}`.slice(0, MAX_SUMMARY);

  const record = buildScheduledSessionRecord(
    { runId, scheduleId: run.id, prompt: run.prompt, runAt, finishedAt },
    sessionEvents,
  );
  const sessionRecord = Buffer.from(JSON.stringify(record), "utf8");

  return { ok: true, summary, sessionRecord, runId };
}

// raceTimeout rejects with a "run timed out" error after ms — separate from
// runViaChild's inline timeout (which also drives the child abort sequence via
// its own promise chain) because the workflow path has nothing to abort: a
// GovernanceBridge call has no cancellation handle, so on timeout we simply
// stop waiting and let the in-flight broker RPC settle on its own in the
// background. This is what frees the pool slot without wedging the ticker.
function raceTimeout<T>(promise: Promise<T>, ms: number): Promise<T> {
  return new Promise<T>((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error("run timed out")), ms);
    promise.then(
      (v) => {
        clearTimeout(timer);
        resolve(v);
      },
      (err: unknown) => {
        clearTimeout(timer);
        reject(err instanceof Error ? err : new Error(String(err)));
      },
    );
  });
}

// workflowRunSummary maps a RunResult to the last_summary format (SC7):
// "N/M steps ok" (M = the workflow's own step count, N = steps that executed
// without a governance denial or a tool-exec error), plus the halted clause
// when the run stopped early.
function workflowRunSummary(result: RunResult, totalSteps: number): string {
  const okCount = result.steps.filter((s) => s.allowed && !s.error).length;
  let summary = `${okCount}/${totalSteps} steps ok`;
  if (result.halted) {
    summary += `; halted at step ${result.haltedAtStep}: ${result.haltReason}`;
  }
  return summary;
}

// workflowStepsToSessionEvents renders each StepOutcome as a tool_start/
// tool_end pair so the existing buildScheduledSessionRecord builder (designed
// around a Pi child's IPC event stream) can produce a faithful session record
// for a workflow run too, without a second record-building implementation.
function workflowStepsToSessionEvents(steps: StepOutcome[]): SessionEvent[] {
  const events: SessionEvent[] = [];
  for (const step of steps) {
    const toolCallId = `step-${step.stepIndex}`;
    events.push({
      kind: "tool_start",
      toolCallId,
      toolName: step.kind === "reason" ? "reason" : step.skill,
      input: step.resolvedArgs,
    });
    events.push({
      kind: "tool_end",
      toolCallId,
      ok: step.allowed && !step.error,
      result: step.allowed ? step.output : { denied: step.denyReason },
    });
  }
  return events;
}

// runViaWorkflow fires one workflow-mode scheduled run: build the run Identity
// from the claim-minted ownerGrant (same shape as the prompt path's Identity,
// just no Pi child), derive the fire-time approver from the CURRENT version's
// own tool steps, and drive the run through GovernanceBridge.runWorkflow —
// each step still re-validated live via SubmitPlan/OPA/FGA/Biscuit.
async function runViaWorkflow(
  cfg: Config,
  clients: BrokerClients,
  log: Logger,
  run: {
    id: string;
    ownerUserId: string;
    ownerGrant: string;
    workflowLineageId: string;
    workflowInputs: Record<string, string>;
    runId: string;
    runAt: string;
  },
  timeoutMs: number,
  // Spend-caps CP3 finding 1: threaded through so a scheduled workflow's
  // reason steps run the same pre-gate check as every other GovernanceBridge
  // construction site (server.ts's ChildSupervisor factory, routes/workflows.ts).
  // Without it, this construction site silently used the permissive no-op
  // default and reason steps in a scheduled run were never rate-limited.
  rateLimitChecker?: RateLimitChecker,
): Promise<RunOutcome> {
  // The whole fire — including the definition fetch used to derive the
  // approver, not just bridge.runWorkflow() — is bounded by timeoutMs. A hang
  // anywhere in this chain (e.g. a wedged getWorkflow call) must still free
  // the pool slot, the same guarantee the prompt path gets from its own timer.
  const { def, outcome } = await raceTimeout(
    (async () => {
      const sessionAgentId = agentForUser(run.ownerUserId, cfg.agentForUserOverrides);
      const identity: Identity = {
        tenantId: cfg.defaultTenantId,
        userId: run.ownerUserId,
        agentId: sessionAgentId,
        ownerGrant: run.ownerGrant,
        runId: run.runId,
      };

      // Fetched once here (not just inside bridge.runWorkflow) because the
      // Approver is bound at bridge construction time, not late-bindable per
      // call — deriving the pre-authorized tool-step set requires the
      // definition before `new GovernanceBridge(...)`. bridge.runWorkflow
      // re-fetches the same grant-scoped read internally; a second read, not
      // a second authority check.
      const workflowResp = await clients.south.getWorkflow({
        tenantId: cfg.defaultTenantId,
        ownerGrant: run.ownerGrant,
        userId: run.ownerUserId,
        lineageId: run.workflowLineageId,
      });
      const def = JSON.parse(workflowResp.definitionJson) as WorkflowDef;
      const toolStepSkills = (def.steps ?? [])
        .filter((s) => s.kind !== "reason")
        .map((s) => s.skill);
      // Scheduling the workflow IS the standing approval
      //: no SCHEDULER_BASELINE_TOOLS
      // here — only this version's own tool steps are pre-authorized, derived
      // live every fire, never a stored list. OPA DENY and FGA denials still
      // halt the run; the approver only ever answers the NEEDS_HUMAN/
      // NEEDS_STEP_UP question for those exact steps.
      const approver = preAuthApprover(toolStepSkills, log);
      // run.runId doubles as the session id (reportScheduledRunResult files the
      // session record under it), so the reason steps that make up a workflow
      // run's entire LLM cost are billed to the session the user can open.
      const bridge = new GovernanceBridge(
        cfg, clients, identity, approver, log, rateLimitChecker, undefined, run.runId, run.runId,
      );

      const outcome = await bridge.runWorkflow(run.workflowLineageId, run.workflowInputs);
      return { def, outcome };
    })(),
    timeoutMs,
  );
  if (!outcome.ok) throw new Error(outcome.error);

  const finishedAt = new Date().toISOString();
  const summary = workflowRunSummary(outcome.result, def.steps?.length ?? 0);
  const label = def.metadata?.name || run.workflowLineageId;
  const events: SessionEvent[] = [
    { kind: "text_delta", delta: summary },
    ...workflowStepsToSessionEvents(outcome.result.steps),
    { kind: "done" },
  ];
  const record = buildScheduledSessionRecord(
    { runId: run.runId, scheduleId: run.id, prompt: label, runAt: run.runAt, finishedAt },
    events,
  );

  return {
    ok: !outcome.result.halted,
    summary,
    sessionRecord: Buffer.from(JSON.stringify(record), "utf8"),
    runId: run.runId,
  };
}

// tick is exported so tests can drive one claim→run cycle with an injected
// supervisor, without starting the timer loop. The supervisor defaults to
// undefined — production callers (startScheduler) always supply it from
// server.ts where the real supervisor lives.
export async function tick(
  cfg: Config,
  clients: BrokerClients,
  log: Logger,
  supervisor: ChildSupervisor,
  rateLimitChecker?: RateLimitChecker,
): Promise<void> {
  let claimed;
  try {
    claimed = await clients.south.claimDueScheduledRuns({
      tenantId: cfg.defaultTenantId,
      now: undefined, // let the broker use its own clock (avoids gateway↔broker skew)
      limit: cfg.schedulerClaimLimit,
    });
  } catch (err) {
    log.warn({ err: String(err) }, "scheduler: claim failed");
    return;
  }
  const runs = claimed.runs ?? [];
  if (runs.length === 0) return;
  log.info({ count: runs.length }, "scheduler: firing due runs");

  // Bounded-concurrency worker pool: a fixed number of workers pull runs off
  // the shared queue and execute them independently, so one slow run no
  // longer blocks the others in the batch. Claiming has already advanced
  // each run's next_fire_at, so executing out of claim order is a pure
  // latency win with no ordering hazard. tick() itself still resolves only
  // once every worker (and therefore every run) has finished, so the
  // self-rescheduling timer in startScheduler continues to see ticks as
  // non-overlapping units of work.
  let cursor = 0;
  const worker = async (): Promise<void> => {
    while (cursor < runs.length) {
      const run = runs[cursor++];
      if (!run) continue;
      await runOneScheduledRun(cfg, clients, log, supervisor, run, rateLimitChecker);
    }
  };
  const poolSize = Math.min(SCHEDULER_MAX_CONCURRENT_RUNS, runs.length);
  await Promise.allSettled(Array.from({ length: poolSize }, () => worker()));
}

// runOneScheduledRun executes a single claimed run end-to-end (run → report)
// and swallows its own errors so one run's failure can't take down its
// sibling workers in the bounded pool.
async function runOneScheduledRun(
  cfg: Config,
  clients: BrokerClients,
  log: Logger,
  supervisor: ChildSupervisor,
  run: {
    id: string;
    ownerUserId: string;
    prompt: string;
    approvedTools?: string[];
    ownerGrant?: string;
    workflowLineageId?: string;
    workflowInputs?: Record<string, string>;
  },
  rateLimitChecker?: RateLimitChecker,
): Promise<void> {
  const started = Date.now();
  // Mint runId here so both success and error paths can reference it when
  // building the session record sent to the broker.
  const runId = randomUUID();
  const runAt = new Date().toISOString();
  let outcome: RunOutcome;
  try {
    outcome = run.workflowLineageId
      ? await runViaWorkflow(
          cfg,
          clients,
          log,
          {
            id: run.id,
            ownerUserId: run.ownerUserId,
            ownerGrant: run.ownerGrant ?? "",
            workflowLineageId: run.workflowLineageId,
            workflowInputs: run.workflowInputs ?? {},
            runId,
            runAt,
          },
          cfg.schedulerRunTimeoutMs,
          rateLimitChecker,
        )
      : await runViaChild(cfg, supervisor, log, {
          id: run.id,
          ownerUserId: run.ownerUserId,
          prompt: run.prompt,
          approvedTools: run.approvedTools ?? [],
          ownerGrant: run.ownerGrant ?? "",
          runId,
          runAt,
        }, cfg.schedulerRunTimeoutMs);
  } catch (err) {
    const summary = String(err).slice(0, MAX_SUMMARY);
    const finishedAt = new Date().toISOString();
    const errorRecord = buildScheduledSessionRecord(
      { runId, scheduleId: run.id, prompt: run.prompt || run.workflowLineageId || "", runAt, finishedAt },
      [{ kind: "error", message: summary }],
    );
    outcome = {
      ok: false,
      summary,
      sessionRecord: Buffer.from(JSON.stringify(errorRecord), "utf8"),
      runId,
    };
  }
  log.info(
    { id: run.id, owner: run.ownerUserId, ok: outcome.ok, ms: Date.now() - started },
    "scheduler: run finished",
  );
  try {
    await clients.south.reportScheduledRunResult({
      tenantId: cfg.defaultTenantId,
      id: run.id,
      success: outcome.ok,
      summary: outcome.summary,
      sessionId: outcome.runId,
      sessionRecord: outcome.sessionRecord,
    });
  } catch (err) {
    log.warn({ err: String(err), id: run.id }, "scheduler: report failed");
  }
}

// startScheduler begins the claim loop and returns a stop function. Uses a
// self-rescheduling timer (not setInterval) so a slow batch never overlaps the
// next tick.
export function startScheduler(
  cfg: Config,
  clients: BrokerClients,
  log: Logger,
  supervisor: ChildSupervisor,
  rateLimitChecker?: RateLimitChecker,
): () => void {
  let stopped = false;
  let timer: NodeJS.Timeout | undefined;

  const loop = async () => {
    if (stopped) return;
    try {
      await tick(cfg, clients, log, supervisor, rateLimitChecker);
    } catch (err) {
      log.warn({ err: String(err) }, "scheduler: tick error");
    }
    if (!stopped) timer = setTimeout(loop, cfg.schedulerTickMs);
  };

  log.info({ tickMs: cfg.schedulerTickMs, limit: cfg.schedulerClaimLimit }, "scheduler: started");
  timer = setTimeout(loop, cfg.schedulerTickMs);

  return () => {
    stopped = true;
    if (timer) clearTimeout(timer);
  };
}
