// GovernanceBridge — reconciles Pi's interactive tool-call loop with aikonos's
// plan-then-execute governance using the JIT single-step-plan model.
//
// For each Pi tool call:
//   gate(): map tool → create a gateway-managed broker task → SubmitPlan(1 step)
//           → APPROVED: stash minted token, allow
//           → DENIED:   block with the policy reason
//           → NEEDS_HUMAN/STEP_UP: ask the injected approver; on yes, ApproveTask
//             (which mints the token), stash, allow; on no, block
//   execute(): InvokeTool with the stashed token (faithful: runs through the
//              broker capability gate + Tool Proxy + audit), then EmitStatus.
import type { Config } from "../config";
import type { BrokerClients } from "./clients";
import { mapTool, knownWorkflowStepSkills, unknownSkills, type ToolMapping } from "./mapping";
import { oneStepPlan, GATEWAY_EXECUTION_HINT } from "./planshim";
import { ValidationOutcome } from "../../gen/ts/proto/plan";
import { TaskStatus, ScheduleKind } from "../../gen/ts/proto/broker";
import type { ScheduleRecurrence } from "../ipc/protocol.js";
import type { Logger } from "../log";
import { runWorkflow as runWorkflowDriver, type RunResult, type StepOutcome } from "../workflow/run.js";
import { workflowDefFromToolParams, type WorkflowDef } from "../workflow/author.js";
import { callVisionProvider, VisionProviderError, type VisionCallResult } from "../llm/vision.js";
import {
  callReasonProvider,
  parseReasonOutput,
  ReasonOutputParseError,
  ReasonProviderError,
  type ReasonCallResult,
} from "../llm/reason.js";
import { chatCandidates, visionCandidates, shouldFailover } from "../llm/provider-fallback.js";
import { decisionOnlyGatedTools, gateSkippedTools } from "../pi/gating-manifest.js";
import { PERSONAL_SKILL_PREFIX } from "../pi/load-skill.js";
import type { RateLimitChecker } from "../llm/egress-proxy.js";
// CP5: a runtime import of runSubagents plus a
// type-only import of BranchSupervisor/SubagentBranch/BranchResult. There is no
// runtime cycle back to this file: subagent/run.ts's own import of this module
// (for Approver/Identity) is `import type`, erased at compile time.
import { runSubagents, type BranchSupervisor, type SubagentBranch, type BranchResult, type SubagentEventSink } from "../subagent/run.js";

// noopRateLimitChecker is the default when no checker is injected (existing
// callers/tests that don't exercise reason/vision rate-limiting) — always
// allows, matching "no pre-gate configured" rather than failing closed on a
// dependency that predates this checkpoint.
export const noopRateLimitChecker: RateLimitChecker = async () => {};

// providerFailoverEligible classifies a failed parent-side provider call (reason
// or vision) for retry on the next candidate. shouldFailover owns the trigger
// set — never re-derive it. A status-less provider error is transport-class (see
// the `status` note on ReasonProviderError/VisionProviderError); anything that is
// not a provider error is not a provider failure and never retries — notably
// ReasonOutputParseError, which a different provider would not fix.
function providerFailoverEligible(err: unknown): boolean {
  if (!(err instanceof ReasonProviderError) && !(err instanceof VisionProviderError)) return false;
  return err.status === undefined
    ? shouldFailover({ transportError: true })
    : shouldFailover({ status: err.status });
}

// boundAgentUuid strips an optional "agent:" prefix and returns the bare id iff
// it is a canonical UUID; otherwise "". WHY (F9): only a real agent UUID may bind
// a workflow to an agent. Identity.agentId takes three forms — "agent:<uuid>"
// (interactive named agent), "<userId>-agent" (synthetic personal/scheduled),
// and a bare uuid (external invoke). The synthetic personal-session id must not
// bind, so anything that is not a UUID resolves to "" (unbound/personal).
const AGENT_UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
export function boundAgentUuid(agentId: string): string {
  const bare = agentId.startsWith("agent:") ? agentId.slice("agent:".length) : agentId;
  return AGENT_UUID_RE.test(bare) ? bare : "";
}

export interface Identity {
  token?: string; // OIDC bearer (north). Omitted in no-OIDC dev brokers.
  tenantId: string;
  userId: string;
  agentId: string; // agent acting on behalf of this user (e.g. alice-agent)
  ownerGrant?: string; // broker-minted HMAC grant (south-path only; absent on the north/bearer path)
  runId?: string; // scheduled-run id (scheduler-path only); forwarded as CreateGatewayTask's client_request_id for idempotent replay
}

export interface ApprovalInfo {
  toolCallId: string;
  toolName: string;
  toolId: string;
  effectClass: number;
  reason: string;
  args: Record<string, unknown>;
  stepUp: boolean;
}

// Injected decision for NEEDS_HUMAN/STEP_UP. CLI auto-approves; the AG-UI server
// surfaces an approval card to the browser and resolves this from the response.
export type Approver = (info: ApprovalInfo) => Promise<boolean>;

export interface GateDecision {
  allow: boolean;
  reason?: string;
}

interface PendingCall {
  taskId: string;
  capToken: string;
  mapping: ToolMapping;
  args: Record<string, unknown>;
}

/** Parent-side broker RPC bridge: gates each Pi tool call through create-task → SubmitPlan → optional HITL, then executes via InvokeTool with the minted capability token. */
export class GovernanceBridge {
  private readonly pending = new Map<string, PendingCall>();

  constructor(
    private readonly cfg: Config,
    private readonly clients: BrokerClients,
    private readonly identity: Identity,
    private approver: Approver,
    private readonly log: Logger,
    // Spend-caps CP3: the same breaker-wrapped rate-limit checker the egress
    // proxy runs before every interactive-chat LLM call — injected here so
    // reason()/analyzeImage() run the identical pre-gate instead of a second,
    // independently-drifting rate-limit call site. Defaults to a permissive
    // no-op so every existing construction site (tests, and any future caller
    // that doesn't care about rate-limiting) is unaffected.
    private readonly rateLimitChecker: RateLimitChecker = noopRateLimitChecker,
    // Books one parent-side LLM call against the run's egress-proxy budget
    // (EgressProxy property 9), returning false when it is spent. reason() and
    // analyzeImage() bypass the proxy entirely, so without this seam they would
    // be an unbounded side channel around the per-run call cap.
    //
    // Absent = no budget: the scheduler's runViaWorkflow builds a bridge with no
    // child behind it, and that path is already bounded by a workflow's finite
    // step count plus schedulerRunTimeoutMs — there is no unbounded loop to cap.
    private readonly consumeLlmBudget?: () => boolean,
    // The run whose parent-side LLM calls (reason/vision) this bridge bills, for
    // usage attribution only. Empty when
    // the caller has no run context (agent-cli, the HTTP workflow-run route).
    private readonly usageRunId: string = "",
    // The chat session those same parent-side calls belong to, so a workflow's
    // reason steps and a chat turn's analyze_image land on the session the user
    // is looking at rather than on no session at all. Empty when the caller has
    // no session context (agent-cli, external invoke).
    private readonly usageSessionId: string = "",
    // CP5: the narrow structural interface a
    // subagent branch spawns through. Satisfied by ChildSupervisor itself —
    // injected here (server.ts wires the real supervisor via a closure that
    // resolves once construction finishes, avoiding a governance.ts ->
    // ipc/supervisor.ts import) rather than importing ipc/supervisor.ts
    // directly. Absent for contexts that never spawn subagents (agent-cli,
    // scheduled workflow runs) — spawnSubagents fails closed with a clear
    // error rather than throwing on a missing dependency.
    private readonly branchSupervisor?: BranchSupervisor,
    // CP7: notified once per branch at spawn
    // and once at resolution, for the chat timeline. Threaded the same way as
    // usageRunId/usageSessionId above — server.ts's BridgeFactory closure
    // wires the route's per-run SSE closure through here. Absent for every
    // context that never spawns subagents.
    private readonly onSubagentEvent?: SubagentEventSink,
  ) {}

  private get north() { return this.clients.north; }
  private get south() { return this.clients.south; }

  // Dispatch to the north (OIDC bearer) or south (broker-minted ownerGrant)
  // twin RPC depending on whether an interactive token is present. Each call
  // site's closures build their own request objects (north's `ownerGrant: ""`
  // + token arg vs south's `ownerGrant: this.identity.ownerGrant ?? ""`) —
  // this helper only picks the branch, so the request-shape difference per
  // RPC never has to be unified into a shared type.
  private viaTokenOrGrant<TResp>(
    north: (token: string) => Promise<TResp>,
    south: () => Promise<TResp>,
  ): Promise<TResp> {
    return this.identity.token ? north(this.identity.token) : south();
  }

  // The pi session is persistent per conversation thread, but the OIDC token and
  // the approval surface (which streams to the current request) change per run.
  setApprover(a: Approver): void { this.approver = a; }
  setToken(token?: string): void { this.identity.token = token; }

  // Stable who-is-running snapshot for usage attribution (no token). tenantId/
  // userId/agentId are fixed for the life of a session; only the token rotates.
  usageIdentity(): { tenantId: string; userId: string; agentId: string } {
    return {
      tenantId: this.identity.tenantId,
      userId: this.identity.userId,
      agentId: this.identity.agentId,
    };
  }

  async gate(
    toolCallId: string,
    toolName: string,
    input: Record<string, unknown>,
    opts?: { readOnlyHint?: boolean },
  ): Promise<GateDecision> {
    // `delegate` and the workflow_* tools call broker RPCs directly (SendEnvelope's
    // OPA envelope_send + OpenFGA can_delegate_to_user + Biscuit attenuation for
    // delegate; FGA-gated workflow RPCs for workflow_*). They are not Tool Proxy
    // invocations, so per the gating manifest (bridge-direct model) they skip the
    // JIT plan/InvokeTool path.
    if (gateSkippedTools().has(toolName)) return { allow: true };

    const mapping = mapTool(toolName, opts?.readOnlyHint);
    if (!mapping) {
      return { allow: false, reason: `tool '${toolName}' is not permitted by aikonos policy` };
    }

    // One gateway-managed broker task per tool call (the broker won't run its
    // own executor for these — the gateway calls InvokeTool itself).
    // When a token is present (interactive) use the north surface so the broker
    // can bind the verified OIDC subject. When absent (scheduled runs) use the
    // SPIFFE-gated south twin — the gateway SVID is the trust anchor.
    const handle = this.identity.token
      ? await this.north.createTask(
          {
            tenantId: this.identity.tenantId,
            userId: this.identity.userId,
            prompt: `agent tool call: ${mapping.toolId}`,
            // 0 = take the broker's default (1000). Cost units are KiB of tool
            // output, so the former hardcoded 100 capped a single call at 100 KiB
            // and most web.fetch calls blew the whole task budget on their first
            // (and only) call. The broker owns this default; don't restate it.
            costBudget: 0,
            skillHints: [GATEWAY_EXECUTION_HINT],
            parentTaskId: "",
            clientRequestId: "",
          },
          this.identity.token,
        )
      : await this.south.createGatewayTask({
          tenantId: this.identity.tenantId,
          ownerUserId: this.identity.userId,
          prompt: `agent tool call: ${mapping.toolId}`,
          costBudget: 0, // broker default (1000) — see the north branch above
          agentId: this.identity.agentId,
          ownerGrant: this.identity.ownerGrant ?? "",
          // Scoped to the individual tool call, not just the run: identity.runId
          // is constant for every gate() call across a scheduled run's lifetime
          // (bound once by setRunContext), so using it bare would make every
          // tool call after the first resolve-to-existing against the FIRST
          // call's task. Combining with toolCallId keeps per-call uniqueness.
          // NOTE: this does not dedup anything today — south.createGatewayTask
          // (unary()) has no retry policy, and a genuine LLM/scheduler-level
          // retry mints a fresh toolCallId, so no request actually replays this
          // exact key. It's forward-looking: it makes a *future* same-request
          // retry wrapper, or a transport-level gRPC retry, dedupe correctly
          // once one exists. Wiring an actual retry is out of scope here.
          clientRequestId: this.identity.runId ? `${this.identity.runId}:${toolCallId}` : "",
        });
    const taskId = handle.taskId;

    // A gate-then-bridge-direct tool (analyze_image, spawn_subagents) is gated
    // for the DECISION only — OPA + CheckFGA(skill:<capability>) + the
    // NEEDS_HUMAN/NEEDS_STEP_UP outcomes. Its toolId is a bare capability-skill
    // name with no toolregistry scope, so the broker mints no Biscuit, and it
    // executes via a direct parent-side bridge call that never calls InvokeTool
    // — there is nothing for a token to authorize. Requiring one blocked these
    // tools unconditionally. No pending entry is registered either: execute()
    // must never be able to reach InvokeTool with a blank capability token, so
    // it fails loudly on the missing entry instead.
    const decisionOnly = decisionOnlyGatedTools().has(toolName);

    const plan = oneStepPlan({ taskId, tenantId: this.identity.tenantId, toolCallId, mapping, args: input });
    const result = await this.south.submitPlan({
      taskId,
      sandboxSpiffeId: this.cfg.gatewaySpiffeId,
      plan,
    });

    switch (result.outcome) {
      case ValidationOutcome.APPROVED: {
        if (decisionOnly) return { allow: true };
        const tok = result.capabilityTokenIds[1];
        if (!tok) return { allow: false, reason: "internal: no capability token minted" };
        this.pending.set(toolCallId, { taskId, capToken: tok, mapping, args: input });
        return { allow: true };
      }
      case ValidationOutcome.DENIED:
        return { allow: false, reason: result.violations.join("; ") || "denied by policy" };
      case ValidationOutcome.NEEDS_HUMAN:
      case ValidationOutcome.NEEDS_STEP_UP: {
        const stepUp = result.outcome === ValidationOutcome.NEEDS_STEP_UP;
        const ok = await this.approver({
          toolCallId,
          toolName,
          toolId: mapping.toolId,
          effectClass: mapping.effectClass,
          reason: stepUp ? "elevated (step-up) approval required" : "human approval required",
          args: input,
          stepUp,
        });
        if (!ok) return { allow: false, reason: "approval declined" };

        const approval = this.identity.token
          ? await this.north.approveTask(
              {
                taskId,
                tenantId: this.identity.tenantId,
                approved: true,
                reason: "approved via agent-gateway",
                approverUserId: this.identity.userId,
              },
              this.identity.token,
            )
          : await this.south.approveGatewayTask({
              taskId,
              tenantId: this.identity.tenantId,
              ownerUserId: this.identity.userId,
              approved: true,
              reason: "approved via agent-gateway",
            });
        if (decisionOnly) return { allow: true };
        const tok = approval.capabilityTokenIds[1];
        if (!tok) return { allow: false, reason: "internal: no token after approval" };
        this.pending.set(toolCallId, { taskId, capToken: tok, mapping, args: input });
        return { allow: true };
      }
      default:
        return { allow: false, reason: `unexpected validation outcome ${result.outcome}` };
    }
  }

  // Delegate a task to another user (north SendEnvelope — Biscuit-attenuated,
  // OPA + OpenFGA governed by the broker). Used by the `delegate` agent tool.
  async delegate(
    to: string,
    intent: string,
    scopes: string[] = ["siem:read"],
    maxCost = 50,
  ): Promise<{ ok: boolean; envelopeId?: string; error?: string }> {
    try {
      const h = await this.north.sendEnvelope(
        {
          fromUserId: this.identity.userId,
          tenantId: this.identity.tenantId,
          recipient: { userId: to },
          task: { intent, payloadRef: "", requiredSkills: [], priority: "normal", kind: "" },
          delegation: { capabilityToken: "", attenuatedScopes: scopes, maxCostUnits: maxCost },
        },
        this.identity.token,
      );
      return { ok: true, envelopeId: h.envelopeId };
    } catch (err) {
      this.log.warn({ err: String(err) }, "delegate failed");
      return { ok: false, error: String(err) };
    }
  }

  // Persist a gateway-authored workflow definition (FGA-gated server-side by CP5a).
  // Uses the north (OIDC bearer) path for interactive sessions, south (ownerGrant)
  // for scheduled/unattended runs — mirrors the gate() createTask split above.
  async saveWorkflow(
    def: Record<string, unknown>,
  ): Promise<{ ok: boolean; workflowId?: string; lineageId?: string; version?: number; error?: string }> {
    try {
      // Wrap the flat tool params into a canonical broker-valid WorkflowDef.
      // WHY: the broker validates apiVersion/kind/metadata in definitionJson;
      // the Pi tool passes flat params (no envelope) so we build it here.
      const canonical = workflowDefFromToolParams(def);
      const skillErr = invalidSkillError(canonical.steps);
      if (skillErr) return { ok: false, error: skillErr };
      const definitionJson = JSON.stringify(canonical);
      const name = canonical.metadata.name;
      const description = canonical.metadata.description ?? "";

      const resp = await this.viaTokenOrGrant(
        (token) =>
          this.north.saveWorkflow(
            {
              tenantId: this.identity.tenantId,
              ownerUserId: this.identity.userId,
              ownerGrant: "",
              name,
              description,
              definitionJson,
              visibilityKind: "private",
              lineageId: "",
              // F9: bind the new lineage to the session's agent (only a real
              // agent UUID binds; synthetic personal-session ids resolve to "").
              agentId: boundAgentUuid(this.identity.agentId),
            },
            token,
          ),
        () =>
          this.south.saveWorkflow({
            tenantId: this.identity.tenantId,
            ownerUserId: this.identity.userId,
            ownerGrant: this.identity.ownerGrant ?? "",
            name,
            description,
            definitionJson,
            visibilityKind: "private",
            lineageId: "",
            // F9: bind the new lineage to the session's agent (only a real
            // agent UUID binds; synthetic personal-session ids resolve to "").
            agentId: boundAgentUuid(this.identity.agentId),
          }),
      );
      return { ok: true, workflowId: resp.workflowId, lineageId: resp.lineageId, version: resp.version };
    } catch (err) {
      this.log.warn({ err: String(err) }, "saveWorkflow failed");
      return { ok: false, error: String(err) };
    }
  }

  // Fetch and run a workflow by lineageId under the current run's identity.
  // Drives each step through this bridge's gate→execute path so every step is
  // re-validated by the broker under the runner's own OPA + FGA grants.
  // Uses north path for interactive (OIDC bearer) sessions, south for scheduled.
  async runWorkflow(
    lineageId: string,
    inputs: Record<string, string>,
    // Optional live per-step callback — the SSE run route wires it to stream
    // each step as it settles. Forwarded verbatim to the run driver.
    onStep?: (outcome: StepOutcome) => void,
  ): Promise<{ ok: true; result: RunResult } | { ok: false; error: string }> {
    try {
      const resp = await this.viaTokenOrGrant(
        (token) =>
          this.north.getWorkflow(
            {
              tenantId: this.identity.tenantId,
              ownerGrant: "",
              userId: this.identity.userId,
              lineageId,
            },
            token,
          ),
        () =>
          this.south.getWorkflow({
            tenantId: this.identity.tenantId,
            ownerGrant: this.identity.ownerGrant ?? "",
            userId: this.identity.userId,
            lineageId,
          }),
      );
      const parsed: unknown = JSON.parse(resp.definitionJson);
      if (
        parsed === null ||
        typeof parsed !== "object" ||
        !Array.isArray((parsed as Record<string, unknown>).steps)
      ) {
        throw new Error("workflow definition_json missing required 'steps' array");
      }
      const readOnlyHints = await this.resolveMcpReadOnlyHints(parsed as WorkflowDef);
      const result = await runWorkflowDriver(this, parsed as WorkflowDef, inputs, this.log, readOnlyHints, onStep);
      return { ok: true, result };
    } catch (err) {
      this.log.warn({ err: String(err) }, "runWorkflow failed");
      return { ok: false, error: String(err) };
    }
  }

  // resolveMcpReadOnlyHints looks up the effective read-only annotation for every
  // mcp: step in the workflow, keyed by full skill id (mcp:<connectorId>:<tool>).
  // The run driver uses these so an MCP tool the server advertises as read-only
  // is classified READ_ONLY (no HITL) even when its name doesn't start with a
  // read verb — matching what the interactive Pi path already does via the
  // per-tool readOnlyHint. Best-effort: any per-connector failure is logged and
  // that connector's tools fall back to the driver's name heuristic. Always uses
  // the south twin (gateway SVID), so it works for interactive and scheduled runs.
  private async resolveMcpReadOnlyHints(def: WorkflowDef): Promise<Map<string, boolean>> {
    const hints = new Map<string, boolean>();
    const connectorIds = new Set<string>();
    for (const step of def.steps ?? []) {
      if (step.kind === "reason" || !step.skill.startsWith("mcp:")) continue;
      const rest = step.skill.slice("mcp:".length);
      const sep = rest.indexOf(":");
      if (sep > 0) connectorIds.add(rest.slice(0, sep));
    }
    for (const connectorId of connectorIds) {
      try {
        const resp = await this.south.listMcpServerToolsSouth({
          tenantId: this.identity.tenantId,
          userId: this.identity.userId,
          connectorId,
        });
        for (const t of resp.tools ?? []) {
          if (typeof t.readOnlyHint === "boolean") {
            hints.set(`mcp:${connectorId}:${t.name}`, t.readOnlyHint);
          }
        }
      } catch (err) {
        this.log.warn(
          { connectorId, err: String(err) },
          "workflow: MCP read-only hint resolution failed; falling back to tool-name heuristic",
        );
      }
    }
    return hints;
  }

  // List workflows owned by the current run's user (FGA-gated server-side).
  // Uses north path for interactive (OIDC bearer) sessions, south for scheduled.
  async listWorkflows(): Promise<{ ok: boolean; items?: unknown[]; error?: string }> {
    try {
      const resp = await this.viaTokenOrGrant(
        (token) =>
          this.north.listWorkflows(
            {
              tenantId: this.identity.tenantId,
              ownerGrant: "",
              userId: this.identity.userId,
              limit: 0,
              cursor: "",
            },
            token,
          ),
        () =>
          this.south.listWorkflows({
            tenantId: this.identity.tenantId,
            ownerGrant: this.identity.ownerGrant ?? "",
            userId: this.identity.userId,
            limit: 0,
            cursor: "",
          }),
      );
      return { ok: true, items: resp.items };
    } catch (err) {
      this.log.warn({ err: String(err) }, "listWorkflows failed");
      return { ok: false, error: String(err) };
    }
  }

  // Propose a new version of an existing workflow lineage through the owner-gated
  // propose/decide loop. Creates a proposed version (status: proposed) awaiting
  // the owner's approval. Uses north path for interactive (OIDC bearer) sessions,
  // south for scheduled/unattended runs.
  async proposeWorkflow(
    lineageId: string,
    def: Record<string, unknown>,
  ): Promise<{ ok: boolean; version?: number; error?: string }> {
    try {
      // Wrap the flat tool params into a canonical broker-valid WorkflowDef.
      // WHY: the broker validates apiVersion/kind/metadata in definitionJson;
      // the Pi tool passes flat params (no envelope) so we build it here — same
      // as saveWorkflow.
      const canonical = workflowDefFromToolParams(def);
      const skillErr = invalidSkillError(canonical.steps);
      if (skillErr) return { ok: false, error: skillErr };
      const definitionJson = JSON.stringify(canonical);

      const resp = await this.viaTokenOrGrant(
        (token) =>
          this.north.proposeWorkflowVersion(
            {
              tenantId: this.identity.tenantId,
              ownerUserId: this.identity.userId,
              ownerGrant: "",
              lineageId,
              definitionJson,
            },
            token,
          ),
        () =>
          this.south.proposeWorkflowVersion({
            tenantId: this.identity.tenantId,
            ownerUserId: this.identity.userId,
            ownerGrant: this.identity.ownerGrant ?? "",
            lineageId,
            definitionJson,
          }),
      );
      return { ok: true, version: resp.version };
    } catch (err) {
      this.log.warn({ err: String(err) }, "proposeWorkflow failed");
      return { ok: false, error: String(err) };
    }
  }

  // Publish a success-rated workflow version to one or more groups the owner belongs to.
  // Uses north path for interactive (OIDC bearer) sessions, south for scheduled.
  async publishWorkflow(
    lineageId: string,
    groupIds: string[],
    version = 0,
  ): Promise<{ ok: boolean; visibilityKind?: string; groups?: string[]; error?: string }> {
    try {
      const resp = await this.viaTokenOrGrant(
        (token) =>
          this.north.publishWorkflow(
            {
              tenantId: this.identity.tenantId,
              ownerUserId: this.identity.userId,
              ownerGrant: "",
              lineageId,
              version,
              groupIds,
            },
            token,
          ),
        () =>
          this.south.publishWorkflow({
            tenantId: this.identity.tenantId,
            ownerUserId: this.identity.userId,
            ownerGrant: this.identity.ownerGrant ?? "",
            lineageId,
            version,
            groupIds,
          }),
      );
      return { ok: true, visibilityKind: resp.visibilityKind, groups: resp.groups };
    } catch (err) {
      this.log.warn({ err: String(err) }, "publishWorkflow failed");
      return { ok: false, error: String(err) };
    }
  }

  // Create a schedule bound to a saved workflow lineage. North-only (CreateScheduledRun has no south twin) — scheduled/unattended
  // runs never create schedules, so a token-less identity fails cleanly instead
  // of silently trying (and failing) a south call that doesn't exist.
  //
  // Never a hard failure when the supplied inputs don't cover the workflow's
  // required (no-default) inputs: the create still proceeds and the missing
  // names are returned so the Pi tool can surface a warning in its result text.
  async scheduleWorkflow(
    lineageId: string,
    inputs: Record<string, string>,
    recurrence: ScheduleRecurrence,
  ): Promise<{ ok: boolean; scheduleId?: string; missingInputs?: string[]; error?: string }> {
    if (!this.identity.token) {
      return {
        ok: false,
        error: "workflow_schedule requires an interactive chat session (no OIDC token) — scheduled runs cannot create schedules",
      };
    }
    try {
      const resp = await this.north.getWorkflow(
        {
          tenantId: this.identity.tenantId,
          ownerGrant: "",
          userId: this.identity.userId,
          lineageId,
        },
        this.identity.token,
      );
      const parsed: unknown = JSON.parse(resp.definitionJson);
      const defInputs =
        parsed !== null && typeof parsed === "object" && Array.isArray((parsed as WorkflowDef).inputs)
          ? (parsed as WorkflowDef).inputs ?? []
          : [];
      const missingInputs = defInputs
        .filter((input) => !("default" in input && input.default !== undefined))
        .map((input) => input.name)
        .filter((name) => !(name in inputs));

      const created = await this.north.createScheduledRun(
        {
          tenantId: this.identity.tenantId,
          userId: this.identity.userId,
          prompt: "",
          kind: recurrence.kind === "once" ? ScheduleKind.SCHEDULE_KIND_ONCE : ScheduleKind.SCHEDULE_KIND_CRON,
          cronExpr: recurrence.cronExpr ?? "",
          runAt: recurrence.runAt ? new Date(recurrence.runAt) : undefined,
          approvedTools: [],
          workflowLineageId: lineageId,
          workflowInputs: inputs,
        },
        this.identity.token,
      );
      return { ok: true, scheduleId: created.run?.id, missingInputs };
    } catch (err) {
      this.log.warn({ err: String(err) }, "scheduleWorkflow failed");
      return { ok: false, error: String(err) };
    }
  }

  // Pre-gate one candidate provider through the same rate-limit (and spend-cap)
  // checker the egress proxy runs before every interactive-chat LLM call. Keyed
  // by hostname (not provider.id) to match the proxy's convention
  // (egress-proxy.ts: `new URL(target.upstreamBaseUrl).hostname`) — a
  // per-provider policy must match on the same key regardless of which call site
  // pre-gates it. Returns the denial message, or undefined when allowed.
  private async preGateProvider(endpoint: string): Promise<string | undefined> {
    try {
      await this.rateLimitChecker(
        this.identity.tenantId,
        this.identity.agentId,
        new URL(endpoint).hostname,
        this.identity.userId,
      );
      return undefined;
    } catch (err) {
      return String(err);
    }
  }

  // Book one parent-side LLM call against the run's per-run egress budget.
  // Returns the denial message when the budget is spent, undefined otherwise.
  // Booked ONCE per logical call (before the candidate loop), the same rule the
  // egress proxy applies — a provider failover must not multiply the count.
  private budgetDenial(): string | undefined {
    if (!this.consumeLlmBudget || this.consumeLlmBudget()) return undefined;
    return "llm call budget exceeded for this run";
  }

  // Fire-and-forget usage emit for a parent-side LLM call (reason/vision),
  // mirroring the deleted pi/usage.ts's posture: an emit failure must never fail
  // a call that otherwise succeeded. No cost from the response — the broker
  // computes it from tokens × provider rate.
  //
  // source is the emit site ("reason"/"vision"); sessionId is the session the
  // bridge was built for, so these calls are billed to the same session as the
  // child's own — a workflow run's reason steps are the whole of its LLM cost.
  private emitParentLlmUsage(
    source: "reason" | "vision",
    provider: string,
    model: string,
    tokensIn: number,
    tokensOut: number,
  ): void {
    void this.south
      .emitLlmUsage({
        tenantId: this.identity.tenantId,
        userId: this.identity.userId,
        agentId: this.identity.agentId,
        provider,
        model,
        tokensIn,
        tokensOut,
        cacheRead: 0,
        cacheWrite: 0,
        cost: 0,
        runId: this.usageRunId,
        sessionId: this.usageSessionId,
        source,
        quantity: 0,
        unit: "",
      })
      .catch((err: unknown) => {
        this.log.warn({ err: String(err) }, "emitLlmUsage failed");
      });
  }

  // Analyze an image via the tenant's vision provider chain (default-vision →
  // tenant fallback). Provider selection is routing, not authorization — whether
  // analyze_image may be invoked at all is gated upstream (skill:vision, CP6).
  // Still fail closed: no vision-capable provider at all, or a resolved file that
  // is not image/* → a clear error, never a silent fallback to the chat provider
  // or a best-effort call with non-image bytes. visionCandidates already filters
  // to vision_capable providers, so a fallback that cannot take an image is never
  // attempted.
  //
  // Image bytes are fetched via the existing north ReadWorkspaceFile RPC under
  // the calling session's own bearer token — no new file-read path, no south
  // twin (ReadWorkspaceFile is north-only in the proto), so this only works for
  // interactive (token-bearing) sessions.
  async analyzeImage(
    path: string,
    prompt?: string,
  ): Promise<{ ok: boolean; text?: string; error?: string }> {
    try {
      // Checked before anything else so a spent budget costs no RPC and no
      // workspace read, exactly like the head pre-gate below.
      const budget = this.budgetDenial();
      if (budget) return { ok: false, error: budget };

      const { providers } = await this.south.getLlmProviders({ tenantId: this.identity.tenantId });
      const candidates = visionCandidates(providers);
      if (candidates.length === 0) {
        return { ok: false, error: "no vision provider assigned" };
      }

      // The head is pre-gated before anything else happens, so a denial costs no
      // workspace read. Each SUBSEQUENT candidate is pre-gated inside the loop
      // instead — a different provider is a different rate-limit/spend-cap
      // subject and must not inherit the head's clearance.
      const headDenial = await this.preGateProvider(candidates[0].provider.endpoint);
      if (headDenial) {
        return { ok: false, error: headDenial };
      }

      if (!this.identity.token) {
        return { ok: false, error: "analyze_image requires an interactive session (no bearer token)" };
      }
      const file = await this.north.readWorkspaceFile(
        { tenantId: this.identity.tenantId, userId: this.identity.userId, path },
        this.identity.token,
      );

      if (!file.mimeType.startsWith("image/")) {
        return { ok: false, error: `not an image: ${path} (mime type ${file.mimeType})` };
      }

      const imageBase64 = Buffer.from(file.content).toString("base64");
      for (const [index, picked] of candidates.entries()) {
        if (index > 0) {
          const denial = await this.preGateProvider(picked.provider.endpoint);
          if (denial) return { ok: false, error: denial };
        }
        let result: VisionCallResult;
        try {
          result = await callVisionProvider({
            provider: picked.provider,
            modelId: picked.modelId,
            imageBase64,
            mimeType: file.mimeType,
            prompt,
            // Per attempt, not per chain: each candidate gets its own full budget.
            timeoutMs: this.cfg.egressTimeoutMs,
          });
        } catch (err) {
          // Exhausted chain → rethrow the last error, so the surfaced error type
          // and shape are exactly what a single-provider failure produced before.
          if (index === candidates.length - 1 || !providerFailoverEligible(err)) throw err;
          this.log.warn(
            { provider: picked.provider.id, err: String(err) },
            "vision provider failed — failing over to the next provider",
          );
          continue;
        }
        this.emitParentLlmUsage("vision", picked.provider.id, picked.modelId, result.tokensIn, result.tokensOut);
        return { ok: true, text: result.text };
      }
      // Unreachable: the last candidate either returns or rethrows above.
      throw new Error("analyze_image: vision provider chain exhausted without an outcome");
    } catch (err) {
      this.log.warn({ err: String(err) }, "analyzeImage failed");
      return { ok: false, error: String(err) };
    }
  }

  // reason handles a workflow `reason` step:
  // a bounded parent-side LLM call, no broker gate, no authority. Mirrors
  // analyzeImage's fail-closed provider resolution but against the tenant's chat
  // provider chain (assigned → default → fallback, not vision) and text-only.
  async reason(
    instruction: string,
    outputSchema?: Record<string, unknown>,
  ): Promise<{ ok: boolean; output?: unknown; error?: string }> {
    try {
      const budget = this.budgetDenial();
      if (budget) return { ok: false, error: budget };

      const { providers } = await this.south.getLlmProviders({ tenantId: this.identity.tenantId });
      const candidates = chatCandidates(providers);
      if (candidates.length === 0) {
        return { ok: false, error: "no default LLM provider configured" };
      }

      for (const [index, picked] of candidates.entries()) {
        // Per-candidate pre-gate: a denial fails the step before this provider is
        // ever called — no fetch, no usage to emit.
        const denial = await this.preGateProvider(picked.provider.endpoint);
        if (denial) return { ok: false, error: denial };

        let result: ReasonCallResult;
        try {
          result = await callReasonProvider({
            provider: picked.provider,
            modelId: picked.modelId,
            instruction,
            outputSchema,
            // The model's own budget wins when the operator has set one, so a
            // reason step that needs more room is a Providers-panel edit rather
            // than a rebuild — workflowReasonMaxTokens is not compose-substituted,
            // so it cannot be tuned on a running deployment. 0 = unset.
            maxTokens: picked.maxTokens || this.cfg.workflowReasonMaxTokens,
            timeoutMs: this.cfg.egressTimeoutMs,
          });
        } catch (err) {
          // Exhausted chain → rethrow the last error, so the surfaced error type
          // and shape are exactly what a single-provider failure produced before.
          if (index === candidates.length - 1 || !providerFailoverEligible(err)) throw err;
          this.log.warn(
            { provider: picked.provider.id, err: String(err) },
            "reason provider failed — failing over to the next provider",
          );
          continue;
        }
        this.emitParentLlmUsage("reason", picked.provider.id, picked.modelId, result.tokensIn, result.tokensOut);
        return { ok: true, output: parseReasonOutput(result.text, outputSchema !== undefined, outputSchema) };
      }
      // Unreachable: the last candidate either returns or rethrows above.
      throw new Error("reason step: chat provider chain exhausted without an outcome");
    } catch (err) {
      if (err instanceof ReasonOutputParseError) {
        return { ok: false, error: `reason step output did not match output_schema: ${err.message}` };
      }
      this.log.warn({ err: String(err) }, "workflow reason step failed");
      return { ok: false, error: String(err) };
    }
  }

  // getSkillBody: on-demand personal-
  // skill body fetch for a child-side load_skill activation. Unlike gate/
  // execute this is a plain metadata read, not a governed tool call — no
  // broker gate/task/capability token, just the SPIFFE-gated south RPC the
  // broker itself chokepoints on skill:personal-skills. name is the bare
  // Skills/<name>/ directory name (the "personal:" activation-key prefix is
  // stripped by the caller before this is invoked).
  async getSkillBody(
    name: string,
  ): Promise<{ ok: boolean; body?: string; allowedTools?: string[]; filePaths?: string[]; error?: string }> {
    try {
      const resp = await this.south.getPersonalSkillSouth({
        tenantId: this.identity.tenantId,
        userId: this.identity.userId,
        name,
      });
      return { ok: true, body: resp.body, allowedTools: resp.allowedTools, filePaths: resp.filePaths };
    } catch (err) {
      this.log.warn({ err: String(err), name }, "getSkillBody failed");
      return { ok: false, error: String(err) };
    }
  }

  // getSkillFile: on-demand single-file
  // read backing read_skill_file. Like getSkillBody this is a plain read, not
  // a governed tool call — no broker gate/task/capability token. ref routes to
  // one of two south RPCs: a "personal:"-prefixed ref reads the caller's own
  // Skills/<name>/ tree; anything else is treated as an admin bundle UUID,
  // which the broker re-checks CheckFGA(user, can_use, agentskill:<id>) on.
  //
  // contentB64 (base64), not raw bytes: the real child is forked with
  // serialization:"json" (supervisor.ts), which flattens a Uint8Array into a
  // numeric-keyed plain object in transit — silently corrupting binary/text
  // content alike. Encoding here, at the point the south response's bytes
  // first land in this result object, makes the IPC wire shape JSON-safe by
  // construction; read-skill-file.ts decodes it back to bytes before applying
  // the content gate.
  async getSkillFile(
    ref: string,
    path: string,
  ): Promise<{ ok: boolean; contentB64?: string; error?: string }> {
    try {
      if (ref.startsWith(PERSONAL_SKILL_PREFIX)) {
        const resp = await this.south.getPersonalSkillFileSouth({
          tenantId: this.identity.tenantId,
          userId: this.identity.userId,
          name: ref.slice(PERSONAL_SKILL_PREFIX.length),
          path,
        });
        return { ok: true, contentB64: Buffer.from(resp.content).toString("base64") };
      }
      const resp = await this.south.getAgentSkillFileSouth({
        tenantId: this.identity.tenantId,
        userId: this.identity.userId,
        id: ref,
        path,
      });
      return { ok: true, contentB64: Buffer.from(resp.content).toString("base64") };
    } catch (err) {
      this.log.warn({ err: String(err), ref, path }, "getSkillFile failed");
      return { ok: false, error: String(err) };
    }
  }

  // spawn_subagents: gate-then-bridge-direct
  // like analyzeImage/reason — the model's tool_call is JIT-plan-gated
  // (CheckFGA skill:subagents) via gate() before this ever runs; this method
  // executes bridge-direct, bypassing InvokeTool entirely (spawn_subagents has
  // no Tool Proxy registration — every branch's own tool call is separately
  // gated through the ordinary gate()->InvokeTool path, under this same
  // caller's identity).
  //
  // callerSkills is resolved HERE, not carried on the bridge: a fresh
  // listUserSkills read under the caller's own identity, mirroring the FGA
  // read every other skill-gated surface in this class already does. Never
  // unions in a role-bound agent's skills (run.ts's own doc explains why: it
  // would grant standing consent the caller does not hold) — that narrowing
  // happens entirely inside runSubagents via the resolved AgentSpec.
  async spawnSubagents(
    branches: SubagentBranch[],
    aggregatorInstruction: string,
  ): Promise<{ ok: boolean; branches?: BranchResult[]; synthesis?: string; error?: string }> {
    if (!this.branchSupervisor) {
      return { ok: false, error: "spawn_subagents is unavailable in this context (no branch supervisor configured)" };
    }
    // Fail closed instead of a randomUUID() fallback: a fan-out's branch pool keys
    // are derived from THIS run id (ephemeralKey), and run-teardown finds
    // them by reconstructing that same id from the chat run's own runId. An
    // id nothing can reconstruct would let the fan-out's branch children
    // outlive the run that spawned them — exactly the leak CP6 exists to
    // close. usageRunId is genuinely "" for a bridge with no run context
    // (agent-cli, the HTTP workflow-run route — see the ctor's own doc
    // comment), so this is a real reachable call shape, not a defensive-only
    // guard.
    if (!this.usageRunId) {
      return {
        ok: false,
        error: "spawn_subagents: no active run id bound to this session — refusing to fan out",
      };
    }
    try {
      const { skills } = await this.south.listUserSkills({
        tenantId: this.identity.tenantId,
        userId: this.identity.userId,
      });
      const result = await runSubagents(branches, {
        supervisor: this.branchSupervisor,
        identity: this.identity,
        north: this.north,
        south: this.south,
        // Reuses the parent chat run's own id as the fan-out's run id — the
        // documented contract (SubagentRunDeps.runId) is "the fan-out's run
        // id, also each branch's IPC run id", and this bridge already has one
        // whenever it is reachable via a real chat tool_call (bound at
        // setRunContext time). Guaranteed non-empty by the guard above.
        runId: this.usageRunId,
        // F-19: the session this bridge was
        // built for, so every branch's usage row attributes to the chat session
        // that caused the fan-out instead of "" — same source as reason()'s own
        // emitParentLlmUsage below.
        sessionId: this.usageSessionId,
        // F-13: read from Config at this production call site, not a
        // hand-passed default — see .
        maxWidth: this.cfg.subagentMaxWidth,
        rateLimitChecker: this.rateLimitChecker,
        branchTimeoutMs: this.cfg.subagentBranchTimeoutMs,
        callerSkills: skills.map((s) => s.toolId),
        reasoner: this,
        aggregatorInstruction,
        onBranchEvent: this.onSubagentEvent,
        log: this.log,
      });
      return { ok: true, branches: result.branches, synthesis: result.synthesis };
    } catch (err) {
      this.log.warn({ err: String(err) }, "spawnSubagents failed");
      return { ok: false, error: String(err) };
    }
  }

  // Run the governed tool call through the broker. Returns the Tool Proxy result.
  async execute(toolCallId: string): Promise<{ ok: boolean; output: unknown; error?: string }> {
    const p = this.pending.get(toolCallId);
    if (!p) return { ok: false, output: null, error: "tool call was not authorized (no gate decision)" };
    this.pending.delete(toolCallId);

    let res;
    try {
      res = await this.south.invokeTool({
        taskId: p.taskId,
        stepId: "1",
        toolId: p.mapping.toolId,
        args: p.args,
        capabilityToken: p.capToken,
        sandboxSpiffeId: this.cfg.gatewaySpiffeId,
        tenantId: this.identity.tenantId,
        userId: this.identity.userId,
        agentId: this.identity.agentId,
      });
    } catch (err) {
      await this.finish(p.taskId, false, String(err));
      return { ok: false, output: null, error: String(err) };
    }
    await this.finish(p.taskId, res.success, res.error, res.costUnitsConsumed);
    return { ok: res.success, output: res.result ?? null, error: res.success ? undefined : res.error };
  }

  private async finish(taskId: string, ok: boolean, desc?: string, cost = 0): Promise<void> {
    try {
      await this.south.emitStatus({
        taskId,
        sandboxSpiffeId: this.cfg.gatewaySpiffeId,
        status: ok ? TaskStatus.COMPLETED : TaskStatus.FAILED,
        description: desc ?? "",
        costUnitsConsumed: cost,
        tenantId: this.identity.tenantId,
      });
    } catch (err) {
      this.log.warn({ err: String(err), taskId }, "EmitStatus failed (best-effort)");
    }
  }
}

// invalidSkillError returns an error message when any workflow step references a
// skill that is not a aikonos tool, or null when all steps are valid. This is the
// authoring-time guard against the model composing a workflow from invented tools
// (e.g. data.transform, template.render, chat.output) that would be denied at run
// time — such a workflow must never be persisted. Uses the same mapTool authority
// that gates execution, so save/propose validity and run-time gating never diverge.
// Also rejects skills like "vision" that resolve via mapTool (for their own
// tool_call gating) but aren't Tool-Proxy-routable — see mapping.ts's
// WORKFLOW_UNRESOLVABLE_SKILLS.
export function invalidSkillError(
  steps: { skill?: string; kind?: string }[],
): string | null {
  // A reason step carries no skill and calls no broker gate — nothing to validate.
  const toolSteps = steps.filter((s) => s.kind !== "reason");
  const bad = unknownSkills(toolSteps.map((s) => s.skill ?? ""));
  if (bad.length === 0) return null;
  return (
    `workflow references unknown skill(s): ${bad.join(", ")}. Every step must use a aikonos tool id — one of: ${knownWorkflowStepSkills().join(", ")} (or an mcp:<connector>:<tool> id you have access to). Do not invent skills. ` +
    `For computation or synthesis between tool calls, use a step with kind: "reason" and an instruction instead of inventing a skill.`
  );
}

export function autoApprover(log: Logger): Approver {
  return async (info) => {
    log.info({ tool: info.toolId, stepUp: info.stepUp }, "auto-approving (CLI mode)");
    return true;
  };
}
