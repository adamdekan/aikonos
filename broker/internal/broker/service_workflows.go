package broker

// service_workflows.go — workflow RPCs (CP3/CP4/CP5a + CP5b north path).
//
// South handlers (SandboxService): owner identity bound from owner_grant HMAC
// token — same pattern as CreateGatewayTask. Used by the scheduled/unattended
// path where no OIDC bearer is available.
//
// North handlers (BrokerService): owner identity resolved via callerIdentity
// (OIDC bearer). Used by interactive chat sessions. Shares the same
// workflowsvc core helpers (workflowsvc.Save / workflowsvc.Get /
// workflowsvc.List / …) so the FGA check + repo logic is never duplicated.
//
// CP5a: every handler checks CheckFGA(user, can_invoke, skill:workflows)
// immediately after owner identity is established. Fails closed on FGA error.
//
// workflowsvc-extraction CP1: the feature core (the *Core helpers, the
// workflowStore/workflowAccessPolicy interfaces, and the access-policy logic)
// moved to broker/internal/workflowsvc — see that package's doc comment for
// the old↔new symbol mapping and  for the
// extraction contract. CP2 dissolved the compat type aliases and the six
// pre-extraction *Core-name shims that CP1 kept for test compatibility; the
// direct-call tests that exercised them now live in workflowsvc (see that
// package's *_test.go files). This file keeps: the two caller preambles, the
// two FGA gate methods, sandboxEnvelopesFor, and the 23 gRPC wrappers.

import (
	"context"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/gatewaygrant"
	"github.com/adamdekan/aikonos/broker/internal/ids"
	"github.com/adamdekan/aikonos/broker/internal/workflowsvc"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// sandboxEnvelopesFor returns the workflowsvc.EnvelopeStore for SandboxService
// — Deps.Tasks, which is the sole production envelope-persist path (CP3
// collapsed the former Deps.createEnvelopes test-only override onto it; see
// service_envelopes.go). Returns nil (a nil taskStore interface converts to a
// nil workflowsvc.EnvelopeStore) when Tasks isn't wired — envelope notify is
// non-fatal, so workflowsvc.Propose tolerates a nil envStore.
func (s *SandboxService) sandboxEnvelopesFor() workflowsvc.EnvelopeStore {
	return s.deps.Tasks
}

// ── FGA skill gates ───────────────────────────────────────────────────────────

// checkWorkflowSkill enforces the skill:workflows FGA gate for a known owner
// on the SandboxService (south) path. Uses the injected skillPolicy override
// when present (test seam), otherwise deps.Policy. Fails closed on error.
func (s *SandboxService) checkWorkflowSkill(ctx context.Context, ownerUserID string) error {
	sp := s.skillPolicyFor()
	if sp == nil || !sp.FGAEnabled() {
		return nil
	}
	granted, err := sp.CheckFGA(ctx, "user:"+ownerUserID, "can_invoke", "skill:workflows")
	if err != nil {
		s.deps.Logger.Warn("checkWorkflowSkill: CheckFGA error — failing closed",
			zap.String("user", ownerUserID),
			zap.Error(err))
		return status.Error(codes.PermissionDenied, "skill:workflows check failed")
	}
	if !granted {
		return status.Error(codes.PermissionDenied, "skill:workflows not granted")
	}
	return nil
}

// checkWorkflowSkillNorth enforces the skill:workflows FGA gate on the
// BrokerService (north/OIDC) path. Uses deps.Policy directly (BrokerService
// has no skillPolicy test-override seam; tests wire deps.Policy instead).
func (s *BrokerService) checkWorkflowSkillNorth(ctx context.Context, ownerUserID string) error {
	p := s.deps.Policy
	if p == nil || !p.FGAEnabled() {
		return nil
	}
	granted, err := p.CheckFGA(ctx, "user:"+ownerUserID, "can_invoke", "skill:workflows")
	if err != nil {
		s.deps.Logger.Warn("checkWorkflowSkillNorth: CheckFGA error — failing closed",
			zap.String("user", ownerUserID),
			zap.Error(err))
		return status.Error(codes.PermissionDenied, "skill:workflows check failed")
	}
	if !granted {
		return status.Error(codes.PermissionDenied, "skill:workflows not granted")
	}
	return nil
}

// ── RPC-twin preambles (fable-rpc-twins CP2) ──────────────────────────────────
//
// Every north workflow wrapper repeats callerIdentity → checkWorkflowSkillNorth;
// every south wrapper repeats requireGatewayPeer → owner-grant verify →
// tenant-match → checkWorkflowSkill. workflowCallerNorth / workflowCallerSouth
// collapse each repeated preamble into one helper returning exactly the errors
// the inline code returned before this collapse — same codes, same message
// strings. Span start/name stay in each wrapper (names differ per RPC).

// workflowCallerNorth resolves and skill-gates the caller on the BrokerService
// (north/OIDC) path: callerIdentity(ctx, tenantID, ownerUserID) then
// checkWorkflowSkillNorth(ctx, user).
func (s *BrokerService) workflowCallerNorth(ctx context.Context, tenantID, ownerUserID string) (tenant, user string, err error) {
	tenant, user, err = s.callerIdentity(ctx, tenantID, ownerUserID)
	if err != nil {
		return "", "", err
	}
	if err := s.checkWorkflowSkillNorth(ctx, user); err != nil {
		return "", "", err
	}
	return tenant, user, nil
}

// workflowCallerSouth resolves and skill-gates the caller on the SandboxService
// (south/SPIFFE) path: requireGatewayPeer, owner-grant presence, gatewaygrant.Verify,
// grant-tenant match, then checkWorkflowSkill(ctx, grantOwner).
func (s *SandboxService) workflowCallerSouth(ctx context.Context, tenantID, ownerGrant string) (tenant, user string, err error) {
	if err := s.requireGatewayPeer(ctx); err != nil {
		return "", "", err
	}
	if ownerGrant == "" {
		return "", "", status.Error(codes.PermissionDenied, "owner grant required")
	}
	grantTenant, grantOwner, grantErr := gatewaygrant.Verify(s.deps.GatewayGrantKey, ownerGrant)
	if grantErr != nil {
		return "", "", status.Errorf(codes.PermissionDenied, "invalid owner grant: %v", grantErr)
	}
	if grantTenant != tenantID {
		return "", "", status.Error(codes.PermissionDenied, "owner grant tenant mismatch")
	}
	if err := s.checkWorkflowSkill(ctx, grantOwner); err != nil {
		return "", "", err
	}
	return grantTenant, grantOwner, nil
}

// ── South handlers (SandboxService — SPIFFE-gated, owner from grant) ─────────

// SaveWorkflow persists a gateway-authored Workflow as a private version.
// SPIFFE-gated to the gateway SVID. The owner is bound from owner_grant, not
// from the asserted owner_user_id field (which is kept for logging only).
func (s *SandboxService) SaveWorkflow(ctx context.Context, req *brokerv1.SaveWorkflowRequest) (*brokerv1.SaveWorkflowResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.workflow.save")
	defer span.End()

	tenant, user, err := s.workflowCallerSouth(ctx, req.TenantId, req.OwnerGrant)
	if err != nil {
		return nil, err
	}

	return workflowsvc.Save(ctx, tenant, user, req, s.deps.Workflows, s.deps.ToolRegistry, s.deps.Audit, s.deps.Logger)
}

// GetWorkflow fetches the definition for a workflow lineage.
// SPIFFE-gated to the gateway SVID. Owner identity is verified from owner_grant.
func (s *SandboxService) GetWorkflow(ctx context.Context, req *brokerv1.GetWorkflowRequest) (*brokerv1.GetWorkflowResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.workflow.get")
	defer span.End()

	tenant, user, err := s.workflowCallerSouth(ctx, req.TenantId, req.OwnerGrant)
	if err != nil {
		return nil, err
	}

	return workflowsvc.Get(ctx, tenant, user, req, s.deps.Workflows, s.deps.Logger)
}

// ListWorkflows returns the current version of every workflow owned by the
// requesting user. SPIFFE-gated to the gateway SVID. Owner identity is
// verified from owner_grant. Requires skill:workflows (deny-by-default).
func (s *SandboxService) ListWorkflows(ctx context.Context, req *brokerv1.ListWorkflowsRequest) (*brokerv1.ListWorkflowsResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.workflow.list")
	defer span.End()

	tenant, user, err := s.workflowCallerSouth(ctx, req.TenantId, req.OwnerGrant)
	if err != nil {
		return nil, err
	}

	// Use deps.Policy as workflowsvc.AccessPolicy for group lookup + per-skill
	// CheckFGA. nil when FGA is not configured; workflowsvc.List treats nil as
	// "FGA disabled → all runnable" (allow-all posture matches rest of codebase).
	var ap workflowsvc.AccessPolicy
	if s.deps.Policy != nil {
		ap = s.deps.Policy
	}
	return workflowsvc.List(ctx, tenant, user, s.deps.Workflows, ap, s.fgaTenantObject(), s.deps.Logger, req.Limit, req.Cursor)
}

// ── North handlers (BrokerService — OIDC bearer, owner from callerIdentity) ──

// SaveWorkflow (north) persists a gateway-authored Workflow as a private
// version for an interactive OIDC-bearer user. owner_grant is ignored; owner
// identity comes from the verified OIDC bearer via callerIdentity.
func (s *BrokerService) SaveWorkflow(ctx context.Context, req *brokerv1.SaveWorkflowRequest) (*brokerv1.SaveWorkflowResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.workflow.save.north")
	defer span.End()

	tenant, user, err := s.workflowCallerNorth(ctx, req.TenantId, req.OwnerUserId)
	if err != nil {
		return nil, err
	}

	return workflowsvc.Save(ctx, tenant, user, req, s.deps.Workflows, s.deps.ToolRegistry, s.deps.Audit, s.deps.Logger)
}

// GetWorkflow (north) fetches a workflow definition for an interactive OIDC-
// bearer user. owner_grant is ignored; identity from callerIdentity.
func (s *BrokerService) GetWorkflow(ctx context.Context, req *brokerv1.GetWorkflowRequest) (*brokerv1.GetWorkflowResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.workflow.get.north")
	defer span.End()

	tenant, user, err := s.workflowCallerNorth(ctx, req.TenantId, req.UserId)
	if err != nil {
		return nil, err
	}

	return workflowsvc.Get(ctx, tenant, user, req, s.deps.Workflows, s.deps.Logger)
}

// ListWorkflows (north) lists workflows owned by an interactive OIDC-bearer
// user. owner_grant is ignored; identity from callerIdentity.
func (s *BrokerService) ListWorkflows(ctx context.Context, req *brokerv1.ListWorkflowsRequest) (*brokerv1.ListWorkflowsResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.workflow.list.north")
	defer span.End()

	tenant, user, err := s.workflowCallerNorth(ctx, req.TenantId, req.UserId)
	if err != nil {
		return nil, err
	}

	// deps.Policy implements workflowsvc.AccessPolicy; nil when not wired (FGA off).
	var ap workflowsvc.AccessPolicy
	if s.deps.Policy != nil {
		ap = s.deps.Policy
	}
	return workflowsvc.List(ctx, tenant, user, s.deps.Workflows, ap, s.fgaTenantObject(), s.deps.Logger, req.Limit, req.Cursor)
}

// ── South handlers: RateWorkflowRun / ProposeWorkflowVersion / DecideWorkflowVersion ──

// RateWorkflowRun (south) records a run quality signal. Audit-only — no
// proposal is created. SPIFFE-gated; owner bound from owner_grant.
func (s *SandboxService) RateWorkflowRun(ctx context.Context, req *brokerv1.RateWorkflowRunRequest) (*brokerv1.RateWorkflowRunResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.workflow.rate")
	defer span.End()

	tenant, user, err := s.workflowCallerSouth(ctx, req.TenantId, req.OwnerGrant)
	if err != nil {
		return nil, err
	}

	return workflowsvc.Rate(ctx, tenant, user, req, s.deps.Workflows, s.deps.Audit, s.deps.Logger)
}

// ProposeWorkflowVersion (south) inserts a proposed version and notifies the
// owner. Caller must be the lineage owner. SPIFFE-gated; owner from grant.
func (s *SandboxService) ProposeWorkflowVersion(ctx context.Context, req *brokerv1.ProposeWorkflowVersionRequest) (*brokerv1.ProposeWorkflowVersionResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.workflow.propose")
	defer span.End()

	tenant, user, err := s.workflowCallerSouth(ctx, req.TenantId, req.OwnerGrant)
	if err != nil {
		return nil, err
	}

	return workflowsvc.Propose(ctx, tenant, user, req,
		s.deps.Workflows, s.deps.Workflows, s.sandboxEnvelopesFor(),
		s.deps.ToolRegistry, s.deps.Audit, s.deps.Logger)
}

// DecideWorkflowVersion (south) approves or rejects a proposed version. Caller
// must be the lineage owner. SPIFFE-gated; owner from grant.
func (s *SandboxService) DecideWorkflowVersion(ctx context.Context, req *brokerv1.DecideWorkflowVersionRequest) (*brokerv1.DecideWorkflowVersionResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.workflow.decide")
	defer span.End()

	tenant, user, err := s.workflowCallerSouth(ctx, req.TenantId, req.OwnerGrant)
	if err != nil {
		return nil, err
	}

	return workflowsvc.Decide(ctx, tenant, user, req,
		s.deps.Workflows, s.deps.Workflows,
		s.deps.Audit, s.deps.Logger)
}

// ── North handlers: RateWorkflowRun / ProposeWorkflowVersion / DecideWorkflowVersion ──

// RateWorkflowRun (north) records an audit-only run quality signal. Owner
// identity from OIDC bearer via callerIdentity.
func (s *BrokerService) RateWorkflowRun(ctx context.Context, req *brokerv1.RateWorkflowRunRequest) (*brokerv1.RateWorkflowRunResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.workflow.rate.north")
	defer span.End()

	tenant, user, err := s.workflowCallerNorth(ctx, req.TenantId, req.OwnerUserId)
	if err != nil {
		return nil, err
	}

	return workflowsvc.Rate(ctx, tenant, user, req, s.deps.Workflows, s.deps.Audit, s.deps.Logger)
}

// ProposeWorkflowVersion (north) inserts a proposed version and notifies the
// owner. Owner identity from OIDC bearer. Caller must be the lineage owner.
func (s *BrokerService) ProposeWorkflowVersion(ctx context.Context, req *brokerv1.ProposeWorkflowVersionRequest) (*brokerv1.ProposeWorkflowVersionResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.workflow.propose.north")
	defer span.End()

	tenant, user, err := s.workflowCallerNorth(ctx, req.TenantId, req.OwnerUserId)
	if err != nil {
		return nil, err
	}

	return workflowsvc.Propose(ctx, tenant, user, req,
		s.deps.Workflows, s.deps.Workflows, s.deps.Tasks,
		s.deps.ToolRegistry, s.deps.Audit, s.deps.Logger)
}

// DecideWorkflowVersion (north) approves or rejects a proposed version. Owner
// identity from OIDC bearer. Caller must be the lineage owner.
func (s *BrokerService) DecideWorkflowVersion(ctx context.Context, req *brokerv1.DecideWorkflowVersionRequest) (*brokerv1.DecideWorkflowVersionResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.workflow.decide.north")
	defer span.End()

	tenant, user, err := s.workflowCallerNorth(ctx, req.TenantId, req.OwnerUserId)
	if err != nil {
		return nil, err
	}

	return workflowsvc.Decide(ctx, tenant, user, req,
		s.deps.Workflows, s.deps.Workflows,
		s.deps.Audit, s.deps.Logger)
}

// ── South handler: PublishWorkflow ────────────────────────────────────────────

// PublishWorkflow (south) publishes a success-rated workflow version to the
// owner's groups. SPIFFE-gated; owner bound from owner_grant.
func (s *SandboxService) PublishWorkflow(ctx context.Context, req *brokerv1.PublishWorkflowRequest) (*brokerv1.PublishWorkflowResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.workflow.publish")
	defer span.End()

	tenant, user, err := s.workflowCallerSouth(ctx, req.TenantId, req.OwnerGrant)
	if err != nil {
		return nil, err
	}

	// A6: org master over workflow sharing (checked before FGA gates).
	if err := requireWorkflowSharingAllowed(ctx, s.deps.OrgSettings, tenant); err != nil {
		return nil, err
	}

	var checkFGA func(ctx context.Context, user, relation, object string) (bool, error)
	sp := s.skillPolicyFor()
	if sp != nil && sp.FGAEnabled() {
		checkFGA = sp.CheckFGA
	}

	return workflowsvc.Publish(ctx, tenant, user, req,
		s.deps.Workflows, s.deps.Workflows, s.deps.ToolRegistry,
		checkFGA, s.deps.Audit, s.deps.Logger)
}

// ── North handler: PublishWorkflow ────────────────────────────────────────────

// PublishWorkflow (north) publishes a success-rated workflow version to the
// owner's groups. Owner identity from OIDC bearer via callerIdentity.
func (s *BrokerService) PublishWorkflow(ctx context.Context, req *brokerv1.PublishWorkflowRequest) (*brokerv1.PublishWorkflowResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.workflow.publish.north")
	defer span.End()

	tenant, user, err := s.workflowCallerNorth(ctx, req.TenantId, req.OwnerUserId)
	if err != nil {
		return nil, err
	}

	// A6: org master over workflow sharing (checked before FGA gates).
	if err := requireWorkflowSharingAllowed(ctx, s.deps.OrgSettings, tenant); err != nil {
		return nil, err
	}

	var checkFGA func(ctx context.Context, user, relation, object string) (bool, error)
	p := s.deps.Policy
	if p != nil && p.FGAEnabled() {
		checkFGA = p.CheckFGA
	}

	return workflowsvc.Publish(ctx, tenant, user, req,
		s.deps.Workflows, s.deps.Workflows, s.deps.ToolRegistry,
		checkFGA, s.deps.Audit, s.deps.Logger)
}

// ── South handlers: ForkWorkflow / SetWorkflowVersionPin / ClearWorkflowVersionPin ──

// ForkWorkflow (south) creates a forker-owned private copy of the source
// workflow's current definition. SPIFFE-gated; owner bound from owner_grant.
func (s *SandboxService) ForkWorkflow(ctx context.Context, req *brokerv1.ForkWorkflowRequest) (*brokerv1.ForkWorkflowResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.workflow.fork")
	defer span.End()

	tenant, user, err := s.workflowCallerSouth(ctx, req.TenantId, req.OwnerGrant)
	if err != nil {
		return nil, err
	}

	var checkFGA func(ctx context.Context, user, relation, object string) (bool, error)
	sp := s.skillPolicyFor()
	if sp != nil && sp.FGAEnabled() {
		checkFGA = sp.CheckFGA
	}

	return workflowsvc.Fork(ctx, tenant, user, req, s.deps.Workflows, checkFGA, s.deps.Audit, s.deps.Logger)
}

// SetWorkflowVersionPin (south) pins the caller's view of a lineage to a
// specific approved version. SPIFFE-gated; owner bound from owner_grant.
func (s *SandboxService) SetWorkflowVersionPin(ctx context.Context, req *brokerv1.SetWorkflowVersionPinRequest) (*brokerv1.SetWorkflowVersionPinResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.workflow.pin")
	defer span.End()

	tenant, user, err := s.workflowCallerSouth(ctx, req.TenantId, req.OwnerGrant)
	if err != nil {
		return nil, err
	}

	return workflowsvc.Pin(ctx, tenant, user, req, s.deps.Workflows, s.deps.Audit, s.deps.Logger)
}

// ClearWorkflowVersionPin (south) removes the caller's pin for a lineage.
// SPIFFE-gated; owner bound from owner_grant.
func (s *SandboxService) ClearWorkflowVersionPin(ctx context.Context, req *brokerv1.ClearWorkflowVersionPinRequest) (*brokerv1.ClearWorkflowVersionPinResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.workflow.clearpin")
	defer span.End()

	tenant, user, err := s.workflowCallerSouth(ctx, req.TenantId, req.OwnerGrant)
	if err != nil {
		return nil, err
	}

	return workflowsvc.ClearPin(ctx, tenant, user, req, s.deps.Workflows, s.deps.Audit, s.deps.Logger)
}

// ── North handlers: ForkWorkflow / SetWorkflowVersionPin / ClearWorkflowVersionPin ──

// ForkWorkflow (north) creates a forker-owned private copy. Owner identity from
// OIDC bearer via callerIdentity.
func (s *BrokerService) ForkWorkflow(ctx context.Context, req *brokerv1.ForkWorkflowRequest) (*brokerv1.ForkWorkflowResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.workflow.fork.north")
	defer span.End()

	tenant, user, err := s.workflowCallerNorth(ctx, req.TenantId, req.OwnerUserId)
	if err != nil {
		return nil, err
	}

	var checkFGA func(ctx context.Context, user, relation, object string) (bool, error)
	p := s.deps.Policy
	if p != nil && p.FGAEnabled() {
		checkFGA = p.CheckFGA
	}

	return workflowsvc.Fork(ctx, tenant, user, req, s.deps.Workflows, checkFGA, s.deps.Audit, s.deps.Logger)
}

// SetWorkflowVersionPin (north) pins the caller's view to an approved version.
// Owner identity from OIDC bearer via callerIdentity.
func (s *BrokerService) SetWorkflowVersionPin(ctx context.Context, req *brokerv1.SetWorkflowVersionPinRequest) (*brokerv1.SetWorkflowVersionPinResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.workflow.pin.north")
	defer span.End()

	tenant, user, err := s.workflowCallerNorth(ctx, req.TenantId, req.OwnerUserId)
	if err != nil {
		return nil, err
	}

	return workflowsvc.Pin(ctx, tenant, user, req, s.deps.Workflows, s.deps.Audit, s.deps.Logger)
}

// ClearWorkflowVersionPin (north) removes the caller's pin. Owner identity from
// OIDC bearer via callerIdentity.
func (s *BrokerService) ClearWorkflowVersionPin(ctx context.Context, req *brokerv1.ClearWorkflowVersionPinRequest) (*brokerv1.ClearWorkflowVersionPinResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.workflow.clearpin.north")
	defer span.End()

	tenant, user, err := s.workflowCallerNorth(ctx, req.TenantId, req.OwnerUserId)
	if err != nil {
		return nil, err
	}

	return workflowsvc.ClearPin(ctx, tenant, user, req, s.deps.Workflows, s.deps.Audit, s.deps.Logger)
}

// ── ListWorkflowVersions ──────────────────────────────────────────────────────

// ListWorkflowVersions (south) lists all versions of a lineage.
// SPIFFE-gated; owner identity bound from owner_grant.
func (s *SandboxService) ListWorkflowVersions(ctx context.Context, req *brokerv1.ListWorkflowVersionsRequest) (*brokerv1.ListWorkflowVersionsResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.workflow.listversions")
	defer span.End()

	tenant, _, err := s.workflowCallerSouth(ctx, req.TenantId, req.OwnerGrant)
	if err != nil {
		return nil, err
	}

	return workflowsvc.ListVersions(ctx, tenant, req, s.deps.Workflows, s.deps.Logger)
}

// ListWorkflowVersions (north) lists all versions of a lineage.
// OIDC-bearer; owner identity from callerIdentity.
func (s *BrokerService) ListWorkflowVersions(ctx context.Context, req *brokerv1.ListWorkflowVersionsRequest) (*brokerv1.ListWorkflowVersionsResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.workflow.listversions.north")
	defer span.End()

	tenant, _, err := s.workflowCallerNorth(ctx, req.TenantId, req.OwnerUserId)
	if err != nil {
		return nil, err
	}

	return workflowsvc.ListVersions(ctx, tenant, req, s.deps.Workflows, s.deps.Logger)
}

// ── DeleteWorkflow ────────────────────────────────────────────────────────────
//
// Removes an entire lineage (all versions + per-user pins). Owner-only. Deleting
// a published lineage also withdraws it from every group it was shared with — the
// webui confirms this consequence before calling. North-only: there is no Pi tool
// that deletes a workflow, so no south (SandboxService) handler exists.

// DeleteWorkflow (north) removes an entire lineage owned by the caller. Owner
// identity from OIDC bearer via callerIdentity.
func (s *BrokerService) DeleteWorkflow(ctx context.Context, req *brokerv1.DeleteWorkflowRequest) (*brokerv1.DeleteWorkflowResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.workflow.delete.north")
	defer span.End()

	tenant, user, err := s.workflowCallerNorth(ctx, req.TenantId, req.OwnerUserId)
	if err != nil {
		return nil, err
	}

	return workflowsvc.Delete(ctx, tenant, user, req, s.deps.Workflows, s.deps.Audit, s.deps.Logger)
}

// ── BeginWorkflowRun (F9) ──────────────────────────────────────────────────────
//
// Authorizes running a lineage's current workflow under its bound agent. North-
// only (OIDC bearer): the webui calls it before RunWorkflowModal drives the run.
//
// Unbound (personal) workflow → empty response; the caller keeps the legacy
// personal run path. Bound workflow → the may-operate-agent gate (can_use =
// usable_by or owner_user, tenant-admin fallback) decides. On allow the broker
// mints an owner grant whose owner is the REQUESTING USER: task ownership (and
// audit attribution) stays with the human; the agent identity travels separately
// as bound_agent_id → CreateGatewayTask.agent_id.
func (s *BrokerService) BeginWorkflowRun(ctx context.Context, req *brokerv1.BeginWorkflowRunRequest) (*brokerv1.BeginWorkflowRunResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.workflow.begin_run.north")
	defer span.End()

	tenant, user, err := s.workflowCallerNorth(ctx, req.TenantId, req.OwnerUserId)
	if err != nil {
		return nil, err
	}

	lineageID, err := uuid.Parse(req.LineageId)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid lineage_id: %v", err)
	}

	row, err := s.deps.Workflows.GetCurrent(ctx, tenant, lineageID)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "workflow lineage not found: %v", err)
	}

	traceID := span.SpanContext().TraceID().String()

	// deps.Policy implements workflowsvc.AccessPolicy; nil when not wired (FGA
	// off). Passing a typed-nil *policy.Engine would not compare == nil inside
	// the workflowsvc gates, so pass a genuinely nil interface in that case.
	var ap workflowsvc.AccessPolicy
	if s.deps.Policy != nil {
		ap = s.deps.Policy
	}

	// Workflow-dimension gate (SEC): MayOperateAgent below authorizes only the
	// AGENT dimension — it says nothing about the caller's access to the workflow
	// itself. Without this a non-owner who may operate the bound agent could run
	// (and disclose) another user's private bound workflow. Runs BEFORE the
	// unbound branch so an unbound private workflow of another user is denied
	// here rather than falling through to the empty personal-path response.
	if !workflowsvc.MayAccessWorkflow(ctx, ap, user, row) {
		s.emitAgentRunAudit(ctx, traceID, tenant, user, req.LineageId, auditv1.PolicyDecision_DENY)
		return nil, status.Error(codes.PermissionDenied, "you do not have access to this workflow")
	}

	// Unbound lineage: no agent gate, no grant. Empty response signals the caller
	// to run it on the legacy personal path.
	if row.BoundAgentID == nil {
		return &brokerv1.BeginWorkflowRunResponse{}, nil
	}

	agentID := row.BoundAgentID.String()

	if !workflowsvc.MayOperateAgent(ctx, ap, user, agentID, s.fgaTenantObject()) {
		s.emitAgentRunAudit(ctx, traceID, tenant, user, req.LineageId, auditv1.PolicyDecision_DENY)
		return nil, status.Errorf(codes.PermissionDenied, "you do not have access to agent %s", agentID)
	}

	grant, err := gatewaygrant.Mint(s.deps.GatewayGrantKey, tenant, user, s.deps.GatewayGrantTTL)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "mint owner grant: %v", err)
	}
	s.emitAgentRunAudit(ctx, traceID, tenant, user, req.LineageId, auditv1.PolicyDecision_ALLOW)
	return &brokerv1.BeginWorkflowRunResponse{
		OwnerGrant:   grant,
		BoundAgentId: agentID,
	}, nil
}

// emitAgentRunAudit records the ALLOW/DENY decision of the F9 agent-bound run
// gate. Fire-and-forget; nil-guards deps.Audit.
func (s *BrokerService) emitAgentRunAudit(ctx context.Context, traceID, tenant, user, lineageID string, decision auditv1.PolicyDecision) {
	if s.deps.Audit == nil {
		return
	}
	if err := s.deps.Audit.Emit(ctx, &auditv1.AuditEvent{
		EventId:     ids.EventID(),
		TraceId:     traceID,
		TenantId:    tenant,
		OccurredAt:  timestampNow(),
		ActorUserId: user,
		EventType:   "aikonos.workflow.agent_run",
		ResourceRef: "aikonos:workflow:" + lineageID,
		Decision:    decision,
	}); err != nil {
		s.deps.Logger.Warn("agent-run audit emit failed", zap.Error(err))
	}
}
