// South-bound broker client (SandboxService): SubmitPlan / InvokeTool /
// EmitStatus over SPIFFE mTLS. This is how the gateway drives faithful,
// broker-governed tool execution.
import { Metadata } from "@grpc/grpc-js";
import type { Config } from "../config";
import { mtlsCredentials, channelOptions } from "./creds";
import { unary } from "./unary";
import { SandboxServiceClient } from "../../gen/ts/proto/broker";
import type {
  SubmitPlanRequest,
  InvokeToolRequest,
  InvokeToolResponse,
  EmitStatusRequest,
  ClaimDueScheduledRunsRequest,
  ClaimDueScheduledRunsResponse,
  ReportScheduledRunResultRequest,
  ListAccessibleMcpServersRequest,
  ListAccessibleMcpServersResponse,
  ListMcpServerToolsRequest,
  ListMcpServerToolsResponse,
  GetTenantModelRequest,
  GetTenantModelResponse,
  GetAgentSpecRequest,
  GetAgentSpecResponse,
  CreateGatewayTaskRequest,
  TaskHandle,
  ApproveGatewayTaskRequest,
  ApproveTaskResponse,
  ResolveAgentApiKeyRequest,
  ResolveAgentApiKeyResponse,
  GetLlmProvidersRequest,
  GetLlmProvidersResponse,
  EmitLlmUsageRequest,
  ListUserSkillsRequest,
  ListUserSkillsResponse,
  ListUserAgentSkillsRequest,
  ListUserAgentSkillsResponse,
  CheckRateLimitRequest,
  CheckRateLimitResponse,
  GetOrgSettingsRequest,
  GetOrgSettingsResponse,
  SaveWorkflowRequest,
  SaveWorkflowResponse,
  GetWorkflowRequest,
  GetWorkflowResponse,
  ListWorkflowsRequest,
  ListWorkflowsResponse,
  PublishWorkflowRequest,
  PublishWorkflowResponse,
  ProposeWorkflowVersionRequest,
  ProposeWorkflowVersionResponse,
  ListMemoryConceptsRequest,
  ListMemoryConceptsResponse,
  ListPersonalSkillsSouthRequest,
  ListPersonalSkillsSouthResponse,
  GetPersonalSkillSouthRequest,
  GetPersonalSkillSouthResponse,
  GetAgentSkillFileSouthRequest,
  GetAgentSkillFileSouthResponse,
  GetPersonalSkillFileSouthRequest,
  GetPersonalSkillFileSouthResponse,
} from "../../gen/ts/proto/broker";
import type { PlanValidationResult } from "../../gen/ts/proto/plan";

export class SouthClient {
  private readonly client: SandboxServiceClient;

  constructor(cfg: Config) {
    this.client = new SandboxServiceClient(
      cfg.brokerSouthAddr,
      mtlsCredentials(cfg),
      channelOptions(cfg),
    );
  }

  submitPlan(req: SubmitPlanRequest): Promise<PlanValidationResult> {
    return unary((r, m, o, cb) => this.client.submitPlan(r, m, o, cb), req, new Metadata());
  }

  invokeTool(req: InvokeToolRequest): Promise<InvokeToolResponse> {
    return unary((r, m, o, cb) => this.client.invokeTool(r, m, o, cb), req, new Metadata());
  }

  emitStatus(req: EmitStatusRequest): Promise<void> {
    return unary((r, m, o, cb) => this.client.emitStatus(r, m, o, cb), req, new Metadata()).then(
      () => undefined,
    );
  }

  claimDueScheduledRuns(
    req: ClaimDueScheduledRunsRequest,
  ): Promise<ClaimDueScheduledRunsResponse> {
    return unary((r, m, o, cb) => this.client.claimDueScheduledRuns(r, m, o, cb), req, new Metadata());
  }

  reportScheduledRunResult(req: ReportScheduledRunResultRequest): Promise<void> {
    return unary(
      (r, m, o, cb) => this.client.reportScheduledRunResult(r, m, o, cb),
      req,
      new Metadata(),
    ).then(() => undefined);
  }

  listAccessibleMcpServersForAgent(
    req: ListAccessibleMcpServersRequest,
  ): Promise<ListAccessibleMcpServersResponse> {
    return unary((r, m, o, cb) => this.client.listAccessibleMcpServersForAgent(r, m, o, cb), req, new Metadata());
  }

  listMcpServerToolsSouth(
    req: ListMcpServerToolsRequest,
  ): Promise<ListMcpServerToolsResponse> {
    return unary((r, m, o, cb) => this.client.listMcpServerToolsSouth(r, m, o, cb), req, new Metadata());
  }

  getTenantModel(req: GetTenantModelRequest): Promise<GetTenantModelResponse> {
    return unary((r, m, o, cb) => this.client.getTenantModel(r, m, o, cb), req, new Metadata());
  }

  getAgentSpec(req: GetAgentSpecRequest): Promise<GetAgentSpecResponse> {
    return unary((r, m, o, cb) => this.client.getAgentSpec(r, m, o, cb), req, new Metadata());
  }

  // Org governance settings (A-series). Read-only; SPIFFE-gated. Read at session
  // resolution time for the instruction preamble and unattended-mode master.
  getOrgSettings(req: GetOrgSettingsRequest): Promise<GetOrgSettingsResponse> {
    return unary((r, m, o, cb) => this.client.getOrgSettings(r, m, o, cb), req, new Metadata());
  }

  // South twins for gateway-managed tasks (SPIFFE-gated; no bearer needed).
  createGatewayTask(req: CreateGatewayTaskRequest): Promise<TaskHandle> {
    return unary((r, m, o, cb) => this.client.createGatewayTask(r, m, o, cb), req, new Metadata());
  }

  approveGatewayTask(req: ApproveGatewayTaskRequest): Promise<ApproveTaskResponse> {
    return unary((r, m, o, cb) => this.client.approveGatewayTask(r, m, o, cb), req, new Metadata());
  }

  // South RPC for resolving a per-agent API key by its sha256 hex digest.
  // Called by the external surface before any session is built.
  resolveAgentApiKey(req: ResolveAgentApiKeyRequest): Promise<ResolveAgentApiKeyResponse> {
    return unary((r, m, o, cb) => this.client.resolveAgentApiKey(r, m, o, cb), req, new Metadata());
  }

  // Per-tenant LLM provider configs (incl. api keys) for session building.
  getLlmProviders(req: GetLlmProvidersRequest): Promise<GetLlmProvidersResponse> {
    return unary((r, m, o, cb) => this.client.getLlmProviders(r, m, o, cb), req, new Metadata());
  }

  // Fetch the FGA-granted skills for a personal (non-agent) user session.
  // The caller handles errors: on RPC failure pass undefined to buildSession so
  // all tools are registered — SubmitPlan remains the enforcement point.
  listUserSkills(req: ListUserSkillsRequest): Promise<ListUserSkillsResponse> {
    return unary((r, m, o, cb) => this.client.listUserSkills(r, m, o, cb), req, new Metadata());
  }

  // Fetch the FGA-granted skill bundles for a personal user session.
  // The caller handles errors: on RPC failure pass [] so the session builds
  // without skill bundles — SubmitPlan remains the enforcement point.
  listUserAgentSkills(req: ListUserAgentSkillsRequest): Promise<ListUserAgentSkillsResponse> {
    return unary((r, m, o, cb) => this.client.listUserAgentSkills(r, m, o, cb), req, new Metadata());
  }

  checkRateLimit(req: CheckRateLimitRequest): Promise<CheckRateLimitResponse> {
    return unary((r, m, o, cb) => this.client.checkRateLimit(r, m, o, cb), req, new Metadata());
  }

  // Fire-and-forget LLM usage report. A failed emit must never break a run, so
  // this resolves (logging nothing here — the caller owns logging) on error.
  emitLlmUsage(req: EmitLlmUsageRequest): Promise<void> {
    return unary((r, m, o, cb) => this.client.emitLlmUsage(r, m, o, cb), req, new Metadata()).then(
      () => undefined,
      () => undefined,
    );
  }

  // Persist a gateway-authored Workflow as a private version.
  // SPIFFE mTLS via existing credentials — same pattern as createGatewayTask.
  saveWorkflow(req: SaveWorkflowRequest): Promise<SaveWorkflowResponse> {
    return unary((r, m, o, cb) => this.client.saveWorkflow(r, m, o, cb), req, new Metadata());
  }

  // Fetch a workflow definition (pin-or-current version) for execution.
  // SPIFFE mTLS via existing credentials — same pattern as saveWorkflow.
  getWorkflow(req: GetWorkflowRequest): Promise<GetWorkflowResponse> {
    return unary((r, m, o, cb) => this.client.getWorkflow(r, m, o, cb), req, new Metadata());
  }

  // List the workflows owned by the requesting user.
  // SPIFFE mTLS via existing credentials — same pattern as getWorkflow.
  listWorkflows(req: ListWorkflowsRequest): Promise<ListWorkflowsResponse> {
    return unary((r, m, o, cb) => this.client.listWorkflows(r, m, o, cb), req, new Metadata());
  }

  // Publish a success-rated workflow version to one or more groups the owner belongs to.
  // SPIFFE mTLS via existing credentials — same pattern as saveWorkflow.
  publishWorkflow(req: PublishWorkflowRequest): Promise<PublishWorkflowResponse> {
    return unary((r, m, o, cb) => this.client.publishWorkflow(r, m, o, cb), req, new Metadata());
  }

  // Propose a new version of an existing workflow lineage through the owner-gated
  // decide loop. SPIFFE mTLS via existing credentials — same pattern as saveWorkflow.
  proposeWorkflowVersion(req: ProposeWorkflowVersionRequest): Promise<ProposeWorkflowVersionResponse> {
    return unary((r, m, o, cb) => this.client.proposeWorkflowVersion(r, m, o, cb), req, new Metadata());
  }

  // Memory concept metas for auto-recall: the broker resolves the user's own
  // bundle, every member-group bundle, and (when agentId is passed and that
  // agent holds memory.read) the agent bundle. The request's scope/group_id
  // fields are ignored by this south twin — it always fans out.
  listMemoryConceptsSouth(req: ListMemoryConceptsRequest): Promise<ListMemoryConceptsResponse> {
    return unary((r, m, o, cb) => this.client.listMemoryConceptsSouth(r, m, o, cb), req, new Metadata());
  }

  // Frontmatter-only catalog of the caller's own Skills/<name>/ personal
  // skills, unioned into the session's skill-bundle list at plan-resolve time.
  listPersonalSkillsSouth(req: ListPersonalSkillsSouthRequest): Promise<ListPersonalSkillsSouthResponse> {
    return unary((r, m, o, cb) => this.client.listPersonalSkillsSouth(r, m, o, cb), req, new Metadata());
  }

  // One personal skill's body, fetched on demand at load_skill activation
  // (never eagerly listed — see listPersonalSkillsSouth).
  getPersonalSkillSouth(req: GetPersonalSkillSouthRequest): Promise<GetPersonalSkillSouthResponse> {
    return unary((r, m, o, cb) => this.client.getPersonalSkillSouth(r, m, o, cb), req, new Metadata());
  }

  // One admin bundle file's raw bytes, fetched on demand by read_skill_file
  //. Re-checks per-bundle FGA can_use broker-side.
  getAgentSkillFileSouth(req: GetAgentSkillFileSouthRequest): Promise<GetAgentSkillFileSouthResponse> {
    return unary((r, m, o, cb) => this.client.getAgentSkillFileSouth(r, m, o, cb), req, new Metadata());
  }

  // One personal skill file's raw bytes, fetched on demand by read_skill_file
  // — same pattern as getAgentSkillFileSouth.
  getPersonalSkillFileSouth(req: GetPersonalSkillFileSouthRequest): Promise<GetPersonalSkillFileSouthResponse> {
    return unary((r, m, o, cb) => this.client.getPersonalSkillFileSouth(r, m, o, cb), req, new Metadata());
  }

  close(): void {
    this.client.close();
  }
}
