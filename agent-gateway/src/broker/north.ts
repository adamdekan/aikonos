// North-bound broker client (BrokerService): CreateTask / ApproveTask /
// SendEnvelope / inbox over mTLS + a per-user OIDC bearer token. The gateway
// forwards the user's token (it does not validate it — the broker does).
import { Metadata, connectivityState } from "@grpc/grpc-js";
import type { Config } from "../config";
import { mtlsCredentials, channelOptions } from "./creds";
import { unary } from "./unary";
import { BrokerServiceClient } from "../../gen/ts/proto/broker";
import type {
  CreateTaskRequest,
  TaskHandle,
  ApproveTaskRequest,
  ApproveTaskResponse,
  SendEnvelopeRequest,
  EnvelopeHandle,
  ListInboxEnvelopesRequest,
  ListInboxEnvelopesResponse,
  ListDelegatableUsersRequest,
  ListDelegatableUsersResponse,
  RespondToEnvelopeRequest,
  RespondToEnvelopeResponse,
  DismissEnvelopeRequest,
  DismissEnvelopeResponse,
  GetTaskStateRequest,
  TaskState,
  BeginConnectorAuthRequest,
  BeginConnectorAuthResponse,
  CompleteConnectorAuthRequest,
  CompleteConnectorAuthResponse,
  ListConnectorsRequest,
  ListConnectorsResponse,
  ListConnectorProvidersRequest,
  ListConnectorProvidersResponse,
  RevokeConnectorRequest,
  RevokeConnectorResponse,
  ListAssignmentsRequest,
  ListAssignmentsResponse,
  ListSkillsRequest,
  ListSkillsResponse,
  UpsertSkillRequest,
  UpsertSkillResponse,
  DeleteSkillRequest,
  DeleteSkillResponse,
  AssignRoleRequest,
  AssignRoleResponse,
  RevokeRoleRequest,
  RevokeRoleResponse,
  MintAgentApiKeyRequest,
  MintAgentApiKeyResponse,
  ListAgentApiKeysRequest,
  ListAgentApiKeysResponse,
  RevokeAgentApiKeyRequest,
  RevokeAgentApiKeyResponse,
  CreateScheduledRunRequest,
  CreateScheduledRunResponse,
  ListScheduledRunsRequest,
  ListScheduledRunsResponse,
  UpdateScheduledRunRequest,
  UpdateScheduledRunResponse,
  DeleteScheduledRunRequest,
  DeleteScheduledRunResponse,
  ListNetworkRulesRequest,
  ListNetworkRulesResponse,
  AddNetworkRuleRequest,
  AddNetworkRuleResponse,
  DeleteNetworkRuleRequest,
  DeleteNetworkRuleResponse,
  ListProvisioningRulesRequest,
  ListProvisioningRulesResponse,
  AddProvisioningRuleRequest,
  AddProvisioningRuleResponse,
  DeleteProvisioningRuleRequest,
  DeleteProvisioningRuleResponse,
  ListWorkspaceFilesRequest,
  ListWorkspaceFilesResponse,
  ReadWorkspaceFileRequest,
  ReadWorkspaceFileResponse,
  UploadWorkspaceFileRequest,
  UploadWorkspaceFileResponse,
  DeleteWorkspaceFileRequest,
  DeleteWorkspaceFileResponse,
  MoveWorkspaceFileRequest,
  MoveWorkspaceFileResponse,
  CreateWorkspaceDirRequest,
  CreateWorkspaceDirResponse,
  ListMcpConnectionsRequest,
  ListMcpConnectionsResponse,
  AddMcpConnectionRequest,
  AddMcpConnectionResponse,
  UpdateMcpConnectionRequest,
  UpdateMcpConnectionResponse,
  DeleteMcpConnectionRequest,
  DeleteMcpConnectionResponse,
  ListAccessibleMcpServersRequest,
  ListAccessibleMcpServersResponse,
  QueryAuditRequest,
  QueryAuditResponse,
  VerifyAuditChainRequest,
  VerifyAuditChainResponse,
  GetDecisionTraceRequest,
  GetDecisionTraceResponse,
  SimulatePolicyRequest,
  SimulatePolicyResponse,
  GetPlatformConfigRequest,
  GetPlatformConfigResponse,
  GetObservabilityInfoRequest,
  GetObservabilityInfoResponse,
  SetPlatformConfigRequest,
  SetPlatformConfigResponse,
  ListAlertsRequest,
  ListAlertsResponse,
  ListMyAgentsRequest,
  ListMyAgentsResponse,
  ListAgentsRequest,
  ListAgentsResponse,
  CreateAgentRequest,
  CreateAgentResponse,
  UpdateAgentRequest,
  UpdateAgentResponse,
  DeleteAgentRequest,
  DeleteAgentResponse,
  GetAgentSoulRequest,
  GetAgentSoulResponse,
  SetAgentSoulRequest,
  SetAgentSoulResponse,
  ListLlmProvidersRequest,
  ListLlmProvidersResponse,
  UpsertLlmProviderRequest,
  UpsertLlmProviderResponse,
  DeleteLlmProviderRequest,
  DeleteLlmProviderResponse,
  TestLlmProviderRequest,
  TestLlmProviderResponse,
  SetDefaultProviderRequest,
  SetDefaultProviderResponse,
  SetDefaultVisionProviderRequest,
  SetDefaultVisionProviderResponse,
  SetFallbackProviderRequest,
  SetFallbackProviderResponse,
  SetDefaultProviderForRequest,
  SetDefaultProviderForResponse,
  UpsertAgentSkillRequest,
  UpsertAgentSkillResponse,
  ListAgentSkillsRequest,
  ListAgentSkillsResponse,
  DeleteAgentSkillRequest,
  DeleteAgentSkillResponse,
  ListRateLimitPoliciesRequest,
  ListRateLimitPoliciesResponse,
  SetRateLimitPolicyRequest,
  SetRateLimitPolicyResponse,
  DeleteRateLimitPolicyRequest,
  DeleteRateLimitPolicyResponse,
  ListSpendCapsRequest,
  ListSpendCapsResponse,
  SetSpendCapRequest,
  SetSpendCapResponse,
  DeleteSpendCapRequest,
  DeleteSpendCapResponse,
  GetSpendSummaryRequest,
  GetSpendSummaryResponse,
  GetSessionUsageRequest,
  GetSessionUsageResponse,
  SaveWorkflowRequest,
  SaveWorkflowResponse,
  GetWorkflowRequest,
  GetWorkflowResponse,
  ListWorkflowsRequest,
  ListWorkflowsResponse,
  RateWorkflowRunRequest,
  RateWorkflowRunResponse,
  ProposeWorkflowVersionRequest,
  ProposeWorkflowVersionResponse,
  DecideWorkflowVersionRequest,
  DecideWorkflowVersionResponse,
  PublishWorkflowRequest,
  PublishWorkflowResponse,
  ForkWorkflowRequest,
  ForkWorkflowResponse,
  SetWorkflowVersionPinRequest,
  SetWorkflowVersionPinResponse,
  ClearWorkflowVersionPinRequest,
  ClearWorkflowVersionPinResponse,
  ListWorkflowVersionsRequest,
  ListWorkflowVersionsResponse,
  DeleteWorkflowRequest,
  DeleteWorkflowResponse,
  BeginWorkflowRunRequest,
  BeginWorkflowRunResponse,
  GetOrgSettingsRequest,
  GetOrgSettingsResponse,
  UpdateOrgSettingsRequest,
  UpdateOrgSettingsResponse,
  ListMembersRequest,
  ListMembersResponse,
  GetWorkspaceBackendRequest,
  GetWorkspaceBackendResponse,
  SetWorkspaceBackendRequest,
  SetWorkspaceBackendResponse,
  ListOneDriveFoldersRequest,
  ListOneDriveFoldersResponse,
  GetM365ConnectionRequest,
  GetM365ConnectionResponse,
  UpsertM365ConnectionRequest,
  UpsertM365ConnectionResponse,
  DeleteM365ConnectionRequest,
  DeleteM365ConnectionResponse,
  TestM365ConnectionRequest,
  TestM365ConnectionResponse,
  GetWebSearchConfigRequest,
  GetWebSearchConfigResponse,
  UpsertWebSearchConfigRequest,
  UpsertWebSearchConfigResponse,
  DeleteWebSearchConfigRequest,
  DeleteWebSearchConfigResponse,
  TestWebSearchConfigRequest,
  TestWebSearchConfigResponse,
  ListMemoryGroupsRequest,
  ListMemoryGroupsResponse,
  ListMemoryConceptsRequest,
  ListMemoryConceptsResponse,
  GetMemoryConceptRequest,
  GetMemoryConceptResponse,
  VerifyMemoryConceptRequest,
  VerifyMemoryConceptResponse,
  DeprecateMemoryConceptRequest,
  DeprecateMemoryConceptResponse,
  DeleteMemoryConceptRequest,
  DeleteMemoryConceptResponse,
  ListPersonalSkillsRequest,
  ListPersonalSkillsResponse,
  DeletePersonalSkillRequest,
  DeletePersonalSkillResponse,
  SendSkillTransferRequest,
  SendSkillTransferResponse,
  GetSkillTransferPreviewRequest,
  GetSkillTransferPreviewResponse,
  AcceptSkillTransferRequest,
  AcceptSkillTransferResponse,
} from "../../gen/ts/proto/broker";

function bearer(token?: string): Metadata {
  const md = new Metadata();
  if (token) md.set("authorization", `Bearer ${token}`);
  return md;
}

export class NorthClient {
  private readonly client: BrokerServiceClient;

  constructor(cfg: Config) {
    this.client = new BrokerServiceClient(
      cfg.brokerNorthAddr,
      mtlsCredentials(cfg),
      channelOptions(cfg),
    );
  }

  createTask(req: CreateTaskRequest, token?: string): Promise<TaskHandle> {
    return unary((r, m, o, cb) => this.client.createTask(r, m, o, cb), req, bearer(token));
  }

  getTaskState(req: GetTaskStateRequest, token?: string): Promise<TaskState> {
    return unary((r, m, o, cb) => this.client.getTaskState(r, m, o, cb), req, bearer(token));
  }

  approveTask(req: ApproveTaskRequest, token?: string): Promise<ApproveTaskResponse> {
    return unary((r, m, o, cb) => this.client.approveTask(r, m, o, cb), req, bearer(token));
  }

  sendEnvelope(req: SendEnvelopeRequest, token?: string): Promise<EnvelopeHandle> {
    return unary((r, m, o, cb) => this.client.sendEnvelope(r, m, o, cb), req, bearer(token));
  }

  listInboxEnvelopes(
    req: ListInboxEnvelopesRequest,
    token?: string,
  ): Promise<ListInboxEnvelopesResponse> {
    return unary((r, m, o, cb) => this.client.listInboxEnvelopes(r, m, o, cb), req, bearer(token));
  }

  listDelegatableUsers(
    req: ListDelegatableUsersRequest,
    token?: string,
  ): Promise<ListDelegatableUsersResponse> {
    return unary((r, m, o, cb) => this.client.listDelegatableUsers(r, m, o, cb), req, bearer(token));
  }

  respondToEnvelope(
    req: RespondToEnvelopeRequest,
    token?: string,
  ): Promise<RespondToEnvelopeResponse> {
    return unary((r, m, o, cb) => this.client.respondToEnvelope(r, m, o, cb), req, bearer(token));
  }

  dismissEnvelope(
    req: DismissEnvelopeRequest,
    token?: string,
  ): Promise<DismissEnvelopeResponse> {
    return unary((r, m, o, cb) => this.client.dismissEnvelope(r, m, o, cb), req, bearer(token));
  }

  beginConnectorAuth(
    req: BeginConnectorAuthRequest,
    token?: string,
  ): Promise<BeginConnectorAuthResponse> {
    return unary((r, m, o, cb) => this.client.beginConnectorAuth(r, m, o, cb), req, bearer(token));
  }

  completeConnectorAuth(
    req: CompleteConnectorAuthRequest,
    token?: string,
  ): Promise<CompleteConnectorAuthResponse> {
    return unary((r, m, o, cb) => this.client.completeConnectorAuth(r, m, o, cb), req, bearer(token));
  }

  listConnectors(
    req: ListConnectorsRequest,
    token?: string,
  ): Promise<ListConnectorsResponse> {
    return unary((r, m, o, cb) => this.client.listConnectors(r, m, o, cb), req, bearer(token));
  }

  listConnectorProviders(
    req: ListConnectorProvidersRequest,
    token?: string,
  ): Promise<ListConnectorProvidersResponse> {
    return unary((r, m, o, cb) => this.client.listConnectorProviders(r, m, o, cb), req, bearer(token));
  }

  revokeConnector(
    req: RevokeConnectorRequest,
    token?: string,
  ): Promise<RevokeConnectorResponse> {
    return unary((r, m, o, cb) => this.client.revokeConnector(r, m, o, cb), req, bearer(token));
  }

  listAssignments(
    req: ListAssignmentsRequest,
    token?: string,
  ): Promise<ListAssignmentsResponse> {
    return unary((r, m, o, cb) => this.client.listAssignments(r, m, o, cb), req, bearer(token));
  }

  listSkills(req: ListSkillsRequest, token?: string): Promise<ListSkillsResponse> {
    return unary((r, m, o, cb) => this.client.listSkills(r, m, o, cb), req, bearer(token));
  }

  upsertSkill(req: UpsertSkillRequest, token?: string): Promise<UpsertSkillResponse> {
    return unary((r, m, o, cb) => this.client.upsertSkill(r, m, o, cb), req, bearer(token));
  }

  deleteSkill(req: DeleteSkillRequest, token?: string): Promise<DeleteSkillResponse> {
    return unary((r, m, o, cb) => this.client.deleteSkill(r, m, o, cb), req, bearer(token));
  }

  assignRole(req: AssignRoleRequest, token?: string): Promise<AssignRoleResponse> {
    return unary((r, m, o, cb) => this.client.assignRole(r, m, o, cb), req, bearer(token));
  }

  revokeRole(req: RevokeRoleRequest, token?: string): Promise<RevokeRoleResponse> {
    return unary((r, m, o, cb) => this.client.revokeRole(r, m, o, cb), req, bearer(token));
  }

  createScheduledRun(
    req: CreateScheduledRunRequest,
    token?: string,
  ): Promise<CreateScheduledRunResponse> {
    return unary((r, m, o, cb) => this.client.createScheduledRun(r, m, o, cb), req, bearer(token));
  }

  listScheduledRuns(
    req: ListScheduledRunsRequest,
    token?: string,
  ): Promise<ListScheduledRunsResponse> {
    return unary((r, m, o, cb) => this.client.listScheduledRuns(r, m, o, cb), req, bearer(token));
  }

  updateScheduledRun(
    req: UpdateScheduledRunRequest,
    token?: string,
  ): Promise<UpdateScheduledRunResponse> {
    return unary((r, m, o, cb) => this.client.updateScheduledRun(r, m, o, cb), req, bearer(token));
  }

  deleteScheduledRun(
    req: DeleteScheduledRunRequest,
    token?: string,
  ): Promise<DeleteScheduledRunResponse> {
    return unary((r, m, o, cb) => this.client.deleteScheduledRun(r, m, o, cb), req, bearer(token));
  }

  listNetworkRules(
    req: ListNetworkRulesRequest,
    token?: string,
  ): Promise<ListNetworkRulesResponse> {
    return unary((r, m, o, cb) => this.client.listNetworkRules(r, m, o, cb), req, bearer(token));
  }

  listAlerts(req: ListAlertsRequest, token?: string): Promise<ListAlertsResponse> {
    return unary((r, m, o, cb) => this.client.listAlerts(r, m, o, cb), req, bearer(token));
  }

  addNetworkRule(req: AddNetworkRuleRequest, token?: string): Promise<AddNetworkRuleResponse> {
    return unary((r, m, o, cb) => this.client.addNetworkRule(r, m, o, cb), req, bearer(token));
  }

  deleteNetworkRule(
    req: DeleteNetworkRuleRequest,
    token?: string,
  ): Promise<DeleteNetworkRuleResponse> {
    return unary((r, m, o, cb) => this.client.deleteNetworkRule(r, m, o, cb), req, bearer(token));
  }

  listProvisioningRules(
    req: ListProvisioningRulesRequest,
    token?: string,
  ): Promise<ListProvisioningRulesResponse> {
    return unary((r, m, o, cb) => this.client.listProvisioningRules(r, m, o, cb), req, bearer(token));
  }

  addProvisioningRule(
    req: AddProvisioningRuleRequest,
    token?: string,
  ): Promise<AddProvisioningRuleResponse> {
    return unary((r, m, o, cb) => this.client.addProvisioningRule(r, m, o, cb), req, bearer(token));
  }

  deleteProvisioningRule(
    req: DeleteProvisioningRuleRequest,
    token?: string,
  ): Promise<DeleteProvisioningRuleResponse> {
    return unary((r, m, o, cb) => this.client.deleteProvisioningRule(r, m, o, cb), req, bearer(token));
  }

  listWorkspaceFiles(
    req: ListWorkspaceFilesRequest,
    token?: string,
  ): Promise<ListWorkspaceFilesResponse> {
    return unary((r, m, o, cb) => this.client.listWorkspaceFiles(r, m, o, cb), req, bearer(token));
  }

  readWorkspaceFile(
    req: ReadWorkspaceFileRequest,
    token?: string,
  ): Promise<ReadWorkspaceFileResponse> {
    return unary((r, m, o, cb) => this.client.readWorkspaceFile(r, m, o, cb), req, bearer(token));
  }

  uploadWorkspaceFile(
    req: UploadWorkspaceFileRequest,
    token?: string,
  ): Promise<UploadWorkspaceFileResponse> {
    return unary((r, m, o, cb) => this.client.uploadWorkspaceFile(r, m, o, cb), req, bearer(token));
  }

  deleteWorkspaceFile(
    req: DeleteWorkspaceFileRequest,
    token?: string,
  ): Promise<DeleteWorkspaceFileResponse> {
    return unary((r, m, o, cb) => this.client.deleteWorkspaceFile(r, m, o, cb), req, bearer(token));
  }

  moveWorkspaceFile(
    req: MoveWorkspaceFileRequest,
    token?: string,
  ): Promise<MoveWorkspaceFileResponse> {
    return unary((r, m, o, cb) => this.client.moveWorkspaceFile(r, m, o, cb), req, bearer(token));
  }

  createWorkspaceDir(
    req: CreateWorkspaceDirRequest,
    token?: string,
  ): Promise<CreateWorkspaceDirResponse> {
    return unary((r, m, o, cb) => this.client.createWorkspaceDir(r, m, o, cb), req, bearer(token));
  }

  listMcpConnections(
    req: ListMcpConnectionsRequest,
    token?: string,
  ): Promise<ListMcpConnectionsResponse> {
    return unary((r, m, o, cb) => this.client.listMcpConnections(r, m, o, cb), req, bearer(token));
  }

  addMcpConnection(
    req: AddMcpConnectionRequest,
    token?: string,
  ): Promise<AddMcpConnectionResponse> {
    return unary((r, m, o, cb) => this.client.addMcpConnection(r, m, o, cb), req, bearer(token));
  }

  updateMcpConnection(
    req: UpdateMcpConnectionRequest,
    token?: string,
  ): Promise<UpdateMcpConnectionResponse> {
    return unary((r, m, o, cb) => this.client.updateMcpConnection(r, m, o, cb), req, bearer(token));
  }

  deleteMcpConnection(
    req: DeleteMcpConnectionRequest,
    token?: string,
  ): Promise<DeleteMcpConnectionResponse> {
    return unary((r, m, o, cb) => this.client.deleteMcpConnection(r, m, o, cb), req, bearer(token));
  }

  listAccessibleMcpServers(
    req: ListAccessibleMcpServersRequest,
    token?: string,
  ): Promise<ListAccessibleMcpServersResponse> {
    return unary((r, m, o, cb) => this.client.listAccessibleMcpServers(r, m, o, cb), req, bearer(token));
  }

  queryAudit(req: QueryAuditRequest, token?: string): Promise<QueryAuditResponse> {
    return unary((r, m, o, cb) => this.client.queryAudit(r, m, o, cb), req, bearer(token));
  }

  verifyAuditChain(
    req: VerifyAuditChainRequest,
    token?: string,
  ): Promise<VerifyAuditChainResponse> {
    return unary((r, m, o, cb) => this.client.verifyAuditChain(r, m, o, cb), req, bearer(token));
  }

  getDecisionTrace(
    req: GetDecisionTraceRequest,
    token?: string,
  ): Promise<GetDecisionTraceResponse> {
    return unary((r, m, o, cb) => this.client.getDecisionTrace(r, m, o, cb), req, bearer(token));
  }

  simulatePolicy(
    req: SimulatePolicyRequest,
    token?: string,
  ): Promise<SimulatePolicyResponse> {
    return unary((r, m, o, cb) => this.client.simulatePolicy(r, m, o, cb), req, bearer(token));
  }

  getPlatformConfig(
    req: GetPlatformConfigRequest,
    token?: string,
  ): Promise<GetPlatformConfigResponse> {
    return unary((r, m, o, cb) => this.client.getPlatformConfig(r, m, o, cb), req, bearer(token));
  }

  getObservabilityInfo(
    req: GetObservabilityInfoRequest,
    token?: string,
  ): Promise<GetObservabilityInfoResponse> {
    return unary((r, m, o, cb) => this.client.getObservabilityInfo(r, m, o, cb), req, bearer(token));
  }

  setPlatformConfig(
    req: SetPlatformConfigRequest,
    token?: string,
  ): Promise<SetPlatformConfigResponse> {
    return unary((r, m, o, cb) => this.client.setPlatformConfig(r, m, o, cb), req, bearer(token));
  }

  listLlmProviders(
    req: ListLlmProvidersRequest,
    token?: string,
  ): Promise<ListLlmProvidersResponse> {
    return unary((r, m, o, cb) => this.client.listLlmProviders(r, m, o, cb), req, bearer(token));
  }

  upsertLlmProvider(
    req: UpsertLlmProviderRequest,
    token?: string,
  ): Promise<UpsertLlmProviderResponse> {
    return unary((r, m, o, cb) => this.client.upsertLlmProvider(r, m, o, cb), req, bearer(token));
  }

  deleteLlmProvider(
    req: DeleteLlmProviderRequest,
    token?: string,
  ): Promise<DeleteLlmProviderResponse> {
    return unary((r, m, o, cb) => this.client.deleteLlmProvider(r, m, o, cb), req, bearer(token));
  }

  setDefaultProvider(
    req: SetDefaultProviderRequest,
    token?: string,
  ): Promise<SetDefaultProviderResponse> {
    return unary((r, m, o, cb) => this.client.setDefaultProvider(r, m, o, cb), req, bearer(token));
  }

  setDefaultVisionProvider(
    req: SetDefaultVisionProviderRequest,
    token?: string,
  ): Promise<SetDefaultVisionProviderResponse> {
    return unary((r, m, o, cb) => this.client.setDefaultVisionProvider(r, m, o, cb), req, bearer(token));
  }

  setFallbackProvider(
    req: SetFallbackProviderRequest,
    token?: string,
  ): Promise<SetFallbackProviderResponse> {
    return unary((r, m, o, cb) => this.client.setFallbackProvider(r, m, o, cb), req, bearer(token));
  }

  setDefaultProviderFor(
    req: SetDefaultProviderForRequest,
    token?: string,
  ): Promise<SetDefaultProviderForResponse> {
    return unary((r, m, o, cb) => this.client.setDefaultProviderFor(r, m, o, cb), req, bearer(token));
  }

  testLlmProvider(
    req: TestLlmProviderRequest,
    token?: string,
  ): Promise<TestLlmProviderResponse> {
    return unary((r, m, o, cb) => this.client.testLlmProvider(r, m, o, cb), req, bearer(token));
  }

  listMyAgents(req: ListMyAgentsRequest, token?: string): Promise<ListMyAgentsResponse> {
    return unary((r, m, o, cb) => this.client.listMyAgents(r, m, o, cb), req, bearer(token));
  }

  listAgents(req: ListAgentsRequest, token?: string): Promise<ListAgentsResponse> {
    return unary((r, m, o, cb) => this.client.listAgents(r, m, o, cb), req, bearer(token));
  }

  createAgent(req: CreateAgentRequest, token?: string): Promise<CreateAgentResponse> {
    return unary((r, m, o, cb) => this.client.createAgent(r, m, o, cb), req, bearer(token));
  }

  updateAgent(req: UpdateAgentRequest, token?: string): Promise<UpdateAgentResponse> {
    return unary((r, m, o, cb) => this.client.updateAgent(r, m, o, cb), req, bearer(token));
  }

  deleteAgent(req: DeleteAgentRequest, token?: string): Promise<DeleteAgentResponse> {
    return unary((r, m, o, cb) => this.client.deleteAgent(r, m, o, cb), req, bearer(token));
  }

  getAgentSoul(req: GetAgentSoulRequest, token?: string): Promise<GetAgentSoulResponse> {
    return unary((r, m, o, cb) => this.client.getAgentSoul(r, m, o, cb), req, bearer(token));
  }

  setAgentSoul(req: SetAgentSoulRequest, token?: string): Promise<SetAgentSoulResponse> {
    return unary((r, m, o, cb) => this.client.setAgentSoul(r, m, o, cb), req, bearer(token));
  }

  mintAgentApiKey(
    req: MintAgentApiKeyRequest,
    token?: string,
  ): Promise<MintAgentApiKeyResponse> {
    return unary((r, m, o, cb) => this.client.mintAgentApiKey(r, m, o, cb), req, bearer(token));
  }

  listAgentApiKeys(
    req: ListAgentApiKeysRequest,
    token?: string,
  ): Promise<ListAgentApiKeysResponse> {
    return unary((r, m, o, cb) => this.client.listAgentApiKeys(r, m, o, cb), req, bearer(token));
  }

  revokeAgentApiKey(
    req: RevokeAgentApiKeyRequest,
    token?: string,
  ): Promise<RevokeAgentApiKeyResponse> {
    return unary((r, m, o, cb) => this.client.revokeAgentApiKey(r, m, o, cb), req, bearer(token));
  }

  upsertAgentSkill(
    req: UpsertAgentSkillRequest,
    token?: string,
  ): Promise<UpsertAgentSkillResponse> {
    return unary((r, m, o, cb) => this.client.upsertAgentSkill(r, m, o, cb), req, bearer(token));
  }

  listAgentSkills(
    req: ListAgentSkillsRequest,
    token?: string,
  ): Promise<ListAgentSkillsResponse> {
    return unary((r, m, o, cb) => this.client.listAgentSkills(r, m, o, cb), req, bearer(token));
  }

  deleteAgentSkill(
    req: DeleteAgentSkillRequest,
    token?: string,
  ): Promise<DeleteAgentSkillResponse> {
    return unary((r, m, o, cb) => this.client.deleteAgentSkill(r, m, o, cb), req, bearer(token));
  }

  listRateLimitPolicies(
    req: ListRateLimitPoliciesRequest,
    token?: string,
  ): Promise<ListRateLimitPoliciesResponse> {
    return unary((r, m, o, cb) => this.client.listRateLimitPolicies(r, m, o, cb), req, bearer(token));
  }

  setRateLimitPolicy(
    req: SetRateLimitPolicyRequest,
    token?: string,
  ): Promise<SetRateLimitPolicyResponse> {
    return unary((r, m, o, cb) => this.client.setRateLimitPolicy(r, m, o, cb), req, bearer(token));
  }

  deleteRateLimitPolicy(
    req: DeleteRateLimitPolicyRequest,
    token?: string,
  ): Promise<DeleteRateLimitPolicyResponse> {
    return unary((r, m, o, cb) => this.client.deleteRateLimitPolicy(r, m, o, cb), req, bearer(token));
  }

  listSpendCaps(req: ListSpendCapsRequest, token?: string): Promise<ListSpendCapsResponse> {
    return unary((r, m, o, cb) => this.client.listSpendCaps(r, m, o, cb), req, bearer(token));
  }

  setSpendCap(req: SetSpendCapRequest, token?: string): Promise<SetSpendCapResponse> {
    return unary((r, m, o, cb) => this.client.setSpendCap(r, m, o, cb), req, bearer(token));
  }

  deleteSpendCap(req: DeleteSpendCapRequest, token?: string): Promise<DeleteSpendCapResponse> {
    return unary((r, m, o, cb) => this.client.deleteSpendCap(r, m, o, cb), req, bearer(token));
  }

  getSpendSummary(req: GetSpendSummaryRequest, token?: string): Promise<GetSpendSummaryResponse> {
    return unary((r, m, o, cb) => this.client.getSpendSummary(r, m, o, cb), req, bearer(token));
  }

  getSessionUsage(req: GetSessionUsageRequest, token?: string): Promise<GetSessionUsageResponse> {
    return unary((r, m, o, cb) => this.client.getSessionUsage(r, m, o, cb), req, bearer(token));
  }

  getOrgSettings(req: GetOrgSettingsRequest, token?: string): Promise<GetOrgSettingsResponse> {
    return unary((r, m, o, cb) => this.client.getOrgSettings(r, m, o, cb), req, bearer(token));
  }

  listMembers(req: ListMembersRequest, token?: string): Promise<ListMembersResponse> {
    return unary((r, m, o, cb) => this.client.listMembers(r, m, o, cb), req, bearer(token));
  }

  updateOrgSettings(req: UpdateOrgSettingsRequest, token?: string): Promise<UpdateOrgSettingsResponse> {
    return unary((r, m, o, cb) => this.client.updateOrgSettings(r, m, o, cb), req, bearer(token));
  }

  saveWorkflow(req: SaveWorkflowRequest, token?: string): Promise<SaveWorkflowResponse> {
    return unary((r, m, o, cb) => this.client.saveWorkflow(r, m, o, cb), req, bearer(token));
  }

  getWorkflow(req: GetWorkflowRequest, token?: string): Promise<GetWorkflowResponse> {
    return unary((r, m, o, cb) => this.client.getWorkflow(r, m, o, cb), req, bearer(token));
  }

  listWorkflows(req: ListWorkflowsRequest, token?: string): Promise<ListWorkflowsResponse> {
    return unary((r, m, o, cb) => this.client.listWorkflows(r, m, o, cb), req, bearer(token));
  }

  rateWorkflowRun(req: RateWorkflowRunRequest, token?: string): Promise<RateWorkflowRunResponse> {
    return unary((r, m, o, cb) => this.client.rateWorkflowRun(r, m, o, cb), req, bearer(token));
  }

  proposeWorkflowVersion(
    req: ProposeWorkflowVersionRequest,
    token?: string,
  ): Promise<ProposeWorkflowVersionResponse> {
    return unary((r, m, o, cb) => this.client.proposeWorkflowVersion(r, m, o, cb), req, bearer(token));
  }

  decideWorkflowVersion(
    req: DecideWorkflowVersionRequest,
    token?: string,
  ): Promise<DecideWorkflowVersionResponse> {
    return unary((r, m, o, cb) => this.client.decideWorkflowVersion(r, m, o, cb), req, bearer(token));
  }

  publishWorkflow(req: PublishWorkflowRequest, token?: string): Promise<PublishWorkflowResponse> {
    return unary((r, m, o, cb) => this.client.publishWorkflow(r, m, o, cb), req, bearer(token));
  }

  forkWorkflow(req: ForkWorkflowRequest, token?: string): Promise<ForkWorkflowResponse> {
    return unary((r, m, o, cb) => this.client.forkWorkflow(r, m, o, cb), req, bearer(token));
  }

  setWorkflowVersionPin(
    req: SetWorkflowVersionPinRequest,
    token?: string,
  ): Promise<SetWorkflowVersionPinResponse> {
    return unary((r, m, o, cb) => this.client.setWorkflowVersionPin(r, m, o, cb), req, bearer(token));
  }

  clearWorkflowVersionPin(
    req: ClearWorkflowVersionPinRequest,
    token?: string,
  ): Promise<ClearWorkflowVersionPinResponse> {
    return unary((r, m, o, cb) => this.client.clearWorkflowVersionPin(r, m, o, cb), req, bearer(token));
  }

  listWorkflowVersions(
    req: ListWorkflowVersionsRequest,
    token?: string,
  ): Promise<ListWorkflowVersionsResponse> {
    return unary((r, m, o, cb) => this.client.listWorkflowVersions(r, m, o, cb), req, bearer(token));
  }

  deleteWorkflow(req: DeleteWorkflowRequest, token?: string): Promise<DeleteWorkflowResponse> {
    return unary((r, m, o, cb) => this.client.deleteWorkflow(r, m, o, cb), req, bearer(token));
  }

  beginWorkflowRun(req: BeginWorkflowRunRequest, token?: string): Promise<BeginWorkflowRunResponse> {
    return unary((r, m, o, cb) => this.client.beginWorkflowRun(r, m, o, cb), req, bearer(token));
  }

  getWorkspaceBackend(
    req: GetWorkspaceBackendRequest,
    token?: string,
  ): Promise<GetWorkspaceBackendResponse> {
    return unary((r, m, o, cb) => this.client.getWorkspaceBackend(r, m, o, cb), req, bearer(token));
  }

  setWorkspaceBackend(
    req: SetWorkspaceBackendRequest,
    token?: string,
  ): Promise<SetWorkspaceBackendResponse> {
    return unary((r, m, o, cb) => this.client.setWorkspaceBackend(r, m, o, cb), req, bearer(token));
  }

  listOneDriveFolders(
    req: ListOneDriveFoldersRequest,
    token?: string,
  ): Promise<ListOneDriveFoldersResponse> {
    return unary((r, m, o, cb) => this.client.listOneDriveFolders(r, m, o, cb), req, bearer(token));
  }

  getM365Connection(
    req: GetM365ConnectionRequest,
    token?: string,
  ): Promise<GetM365ConnectionResponse> {
    return unary((r, m, o, cb) => this.client.getM365Connection(r, m, o, cb), req, bearer(token));
  }

  upsertM365Connection(
    req: UpsertM365ConnectionRequest,
    token?: string,
  ): Promise<UpsertM365ConnectionResponse> {
    return unary((r, m, o, cb) => this.client.upsertM365Connection(r, m, o, cb), req, bearer(token));
  }

  deleteM365Connection(
    req: DeleteM365ConnectionRequest,
    token?: string,
  ): Promise<DeleteM365ConnectionResponse> {
    return unary((r, m, o, cb) => this.client.deleteM365Connection(r, m, o, cb), req, bearer(token));
  }

  testM365Connection(
    req: TestM365ConnectionRequest,
    token?: string,
  ): Promise<TestM365ConnectionResponse> {
    return unary((r, m, o, cb) => this.client.testM365Connection(r, m, o, cb), req, bearer(token));
  }

  getWebSearchConfig(
    req: GetWebSearchConfigRequest,
    token?: string,
  ): Promise<GetWebSearchConfigResponse> {
    return unary((r, m, o, cb) => this.client.getWebSearchConfig(r, m, o, cb), req, bearer(token));
  }

  upsertWebSearchConfig(
    req: UpsertWebSearchConfigRequest,
    token?: string,
  ): Promise<UpsertWebSearchConfigResponse> {
    return unary((r, m, o, cb) => this.client.upsertWebSearchConfig(r, m, o, cb), req, bearer(token));
  }

  deleteWebSearchConfig(
    req: DeleteWebSearchConfigRequest,
    token?: string,
  ): Promise<DeleteWebSearchConfigResponse> {
    return unary((r, m, o, cb) => this.client.deleteWebSearchConfig(r, m, o, cb), req, bearer(token));
  }

  testWebSearchConfig(
    req: TestWebSearchConfigRequest,
    token?: string,
  ): Promise<TestWebSearchConfigResponse> {
    return unary((r, m, o, cb) => this.client.testWebSearchConfig(r, m, o, cb), req, bearer(token));
  }

  // Memory management surface. The broker
  // owns every gate — self / group member-or-manager / tenant admin — so these
  // are pure forwards of the caller's own bearer.
  listMemoryGroups(req: ListMemoryGroupsRequest, token?: string): Promise<ListMemoryGroupsResponse> {
    return unary((r, m, o, cb) => this.client.listMemoryGroups(r, m, o, cb), req, bearer(token));
  }

  listMemoryConcepts(
    req: ListMemoryConceptsRequest,
    token?: string,
  ): Promise<ListMemoryConceptsResponse> {
    return unary((r, m, o, cb) => this.client.listMemoryConcepts(r, m, o, cb), req, bearer(token));
  }

  getMemoryConcept(req: GetMemoryConceptRequest, token?: string): Promise<GetMemoryConceptResponse> {
    return unary((r, m, o, cb) => this.client.getMemoryConcept(r, m, o, cb), req, bearer(token));
  }

  verifyMemoryConcept(
    req: VerifyMemoryConceptRequest,
    token?: string,
  ): Promise<VerifyMemoryConceptResponse> {
    return unary((r, m, o, cb) => this.client.verifyMemoryConcept(r, m, o, cb), req, bearer(token));
  }

  deprecateMemoryConcept(
    req: DeprecateMemoryConceptRequest,
    token?: string,
  ): Promise<DeprecateMemoryConceptResponse> {
    return unary((r, m, o, cb) => this.client.deprecateMemoryConcept(r, m, o, cb), req, bearer(token));
  }

  deleteMemoryConcept(
    req: DeleteMemoryConceptRequest,
    token?: string,
  ): Promise<DeleteMemoryConceptResponse> {
    return unary((r, m, o, cb) => this.client.deleteMemoryConcept(r, m, o, cb), req, bearer(token));
  }

  // Personal skills — transfer + management. The broker owns every gate (ownership,
  // FGA capability, recipient checks) — these are pure forwards of the
  // caller's own bearer.
  listPersonalSkills(req: ListPersonalSkillsRequest, token?: string): Promise<ListPersonalSkillsResponse> {
    return unary((r, m, o, cb) => this.client.listPersonalSkills(r, m, o, cb), req, bearer(token));
  }

  deletePersonalSkill(
    req: DeletePersonalSkillRequest,
    token?: string,
  ): Promise<DeletePersonalSkillResponse> {
    return unary((r, m, o, cb) => this.client.deletePersonalSkill(r, m, o, cb), req, bearer(token));
  }

  sendSkillTransfer(req: SendSkillTransferRequest, token?: string): Promise<SendSkillTransferResponse> {
    return unary((r, m, o, cb) => this.client.sendSkillTransfer(r, m, o, cb), req, bearer(token));
  }

  getSkillTransferPreview(
    req: GetSkillTransferPreviewRequest,
    token?: string,
  ): Promise<GetSkillTransferPreviewResponse> {
    return unary((r, m, o, cb) => this.client.getSkillTransferPreview(r, m, o, cb), req, bearer(token));
  }

  acceptSkillTransfer(
    req: AcceptSkillTransferRequest,
    token?: string,
  ): Promise<AcceptSkillTransferResponse> {
    return unary((r, m, o, cb) => this.client.acceptSkillTransfer(r, m, o, cb), req, bearer(token));
  }

  close(): void {
    this.client.close();
  }

  // Narrow accessor for /readyz: exposes the underlying channel's connectivity
  // state without leaking the grpc-js client itself. READY or IDLE both count
  // as "connectable" — IDLE means no RPC has been made yet, not that the
  // channel is broken (grpc-js lazily connects on first call).
  getConnectivityState(tryToConnect = false): number {
    return this.client.getChannel().getConnectivityState(tryToConnect);
  }
}

export { connectivityState };
