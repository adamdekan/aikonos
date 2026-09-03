package broker

import (
	"context"
	"maps"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/adamdekan/aikonos/broker/internal/auth"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/gatewaygrant"
	"github.com/adamdekan/aikonos/broker/internal/ids"
	"github.com/adamdekan/aikonos/broker/internal/toolregistry"
	"github.com/adamdekan/aikonos/broker/internal/workflowsvc"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

const (
	maxSchedulePromptLen = 8000
	maxApprovedTools     = 32
	defaultClaimLimit    = 20
	// listAllSentinel is the owner_filter value the admin all-runs view sends;
	// any other non-empty value is a filter by a specific username.
	listAllSentinel = "*"
	// cronFloorSamples is how many upcoming fires enforceCronFloor inspects.
	cronFloorSamples = 6
)

// requireSchedulerSkill gates schedule creation on the skill:scheduler capability
// (granted through the admin "Skill access" tab). Mirrors requireTenantAdmin:
// fails closed on a transport error. FGA-disabled dev ⇒ allow-all, as elsewhere.
func (s *BrokerService) requireSchedulerSkill(ctx context.Context, user string) error {
	ok, err := s.deps.Policy.CheckFGA(ctx, "user:"+user, "can_invoke", "skill:scheduler")
	if err != nil {
		return status.Error(codes.Internal, "authorization check failed")
	}
	if !ok {
		return status.Error(codes.PermissionDenied, "scheduler skill required")
	}
	return nil
}

func kindFromProto(k brokerv1.ScheduleKind) (db.ScheduleKind, bool) {
	switch k {
	case brokerv1.ScheduleKind_SCHEDULE_KIND_CRON:
		return db.ScheduleKindCron, true
	case brokerv1.ScheduleKind_SCHEDULE_KIND_ONCE:
		return db.ScheduleKindOnce, true
	default:
		return "", false
	}
}

func kindToProto(k db.ScheduleKind) brokerv1.ScheduleKind {
	if k == db.ScheduleKindCron {
		return brokerv1.ScheduleKind_SCHEDULE_KIND_CRON
	}
	return brokerv1.ScheduleKind_SCHEDULE_KIND_ONCE
}

func stateToProto(st db.ScheduledRunState) brokerv1.ScheduledRunState {
	switch st {
	case db.ScheduledRunActive:
		return brokerv1.ScheduledRunState_SCHEDULED_RUN_ACTIVE
	case db.ScheduledRunPaused:
		return brokerv1.ScheduledRunState_SCHEDULED_RUN_PAUSED
	case db.ScheduledRunCompleted:
		return brokerv1.ScheduledRunState_SCHEDULED_RUN_COMPLETED
	case db.ScheduledRunFailed:
		return brokerv1.ScheduledRunState_SCHEDULED_RUN_FAILED
	default:
		return brokerv1.ScheduledRunState_SCHEDULED_RUN_STATE_UNSPECIFIED
	}
}

func toProtoScheduledRun(s *db.ScheduledRun) *brokerv1.ScheduledRun {
	out := &brokerv1.ScheduledRun{
		Id:                  s.ScheduledRunID.String(),
		OwnerUserId:         s.OwnerUserID,
		Prompt:              s.Prompt,
		Kind:                kindToProto(s.Kind),
		CronExpr:            s.CronExpr,
		ApprovedTools:       s.ApprovedTools,
		State:               stateToProto(s.State),
		LastStatus:          s.LastStatus,
		LastSummary:         s.LastSummary,
		RunCount:            s.RunCount,
		CreatedBy:           s.CreatedBy,
		CreatedAt:           timestamppb.New(s.CreatedAt),
		WorkflowInputs:      s.WorkflowInputs,
		WorkflowDisplayName: s.WorkflowDisplayName,
	}
	if s.WorkflowLineageID != nil {
		out.WorkflowLineageId = s.WorkflowLineageID.String()
	}
	if s.NextFireAt != nil {
		out.NextFireAt = timestamppb.New(*s.NextFireAt)
	}
	if s.LastFireAt != nil {
		out.LastFireAt = timestamppb.New(*s.LastFireAt)
	}
	return out
}

// enforceCronFloor rejects a cron that fires more often than minInterval.
// Unattended recurring runs cost money per fire and `workflow_schedule` is a Pi
// tool an agent can invoke, so the floor is a spend guard, not an ergonomics
// one. minInterval == 0 disables it. Write-time only: stored rows are never
// re-validated, so raising the floor later cannot break existing schedules.
//
// ponytail: samples the next few fires and takes the smallest gap instead of
// solving the cron algebraically — catches "* * * * *" and burst specs like
// "0,1 9 * * *" whose first gap alone looks innocent. Raise cronFloorSamples
// if a wider burst window ever matters.
func enforceCronFloor(cronExpr string, first time.Time, minInterval time.Duration) error {
	if minInterval <= 0 {
		return nil
	}
	prev := first
	for i := 0; i < cronFloorSamples; i++ {
		next, err := db.NextCronFire(cronExpr, prev)
		if err != nil {
			return status.Errorf(codes.InvalidArgument, "%v", err)
		}
		if next.Sub(prev) < minInterval {
			return status.Errorf(codes.InvalidArgument,
				"cron fires more often than the configured minimum interval of %s", minInterval)
		}
		prev = next
	}
	return nil
}

// validateScheduleDef checks a prompt-mode create/edit definition and returns
// the resolved kind, normalized cron, and the first next_fire_at.
func validateScheduleDef(reg *toolregistry.Registry, prompt string, k brokerv1.ScheduleKind, cronExpr string, runAt *timestamppb.Timestamp, approvedTools []string, now time.Time, minInterval time.Duration) (db.ScheduleKind, string, time.Time, error) {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" || len(prompt) > maxSchedulePromptLen {
		return "", "", time.Time{}, status.Errorf(codes.InvalidArgument, "prompt must be non-empty and <= %d chars", maxSchedulePromptLen)
	}
	if len(approvedTools) > maxApprovedTools {
		return "", "", time.Time{}, status.Errorf(codes.InvalidArgument, "at most %d approved tools", maxApprovedTools)
	}
	for _, t := range approvedTools {
		if _, ok := reg.RequiredScope(t); !ok {
			return "", "", time.Time{}, status.Errorf(codes.InvalidArgument, "unknown tool %q in approved_tools", t)
		}
	}
	return validateScheduleTiming(k, cronExpr, runAt, now, minInterval)
}

// validateScheduleTiming validates and resolves the kind/cron/run_at timing
// fields shared by both prompt-mode and workflow-mode schedules. It is the
// single write-time cron chokepoint — every north create/update path reaches
// the frequency floor through here (there is no south create/update twin).
func validateScheduleTiming(k brokerv1.ScheduleKind, cronExpr string, runAt *timestamppb.Timestamp, now time.Time, minInterval time.Duration) (db.ScheduleKind, string, time.Time, error) {
	kind, ok := kindFromProto(k)
	if !ok {
		return "", "", time.Time{}, status.Error(codes.InvalidArgument, "schedule kind required (CRON or ONCE)")
	}
	switch kind {
	case db.ScheduleKindCron:
		if !db.ValidCronExpr(cronExpr) {
			return "", "", time.Time{}, status.Error(codes.InvalidArgument, "invalid cron expression (expected 5-field crontab)")
		}
		next, err := db.NextCronFire(cronExpr, now)
		if err != nil {
			return "", "", time.Time{}, status.Errorf(codes.InvalidArgument, "%v", err)
		}
		if ferr := enforceCronFloor(cronExpr, next, minInterval); ferr != nil {
			return "", "", time.Time{}, ferr
		}
		return kind, cronExpr, next, nil
	default: // ONCE fires exactly once — no frequency floor applies.
		if runAt == nil {
			return "", "", time.Time{}, status.Error(codes.InvalidArgument, "run_at required for a one-off schedule")
		}
		at := runAt.AsTime()
		if !at.After(now) {
			return "", "", time.Time{}, status.Error(codes.InvalidArgument, "run_at must be in the future")
		}
		return kind, "", at, nil
	}
}

// validateWorkflowScheduleDef checks a workflow-mode create (or the timing-only
// full-edit of an existing workflow-mode row) definition: no prompt, no
// approved_tools (approval derives from the workflow itself at fire time —
// see ), and the lineage must resolve for the
// caller via the same store path GetWorkflow uses (ResolveVersionForUser +
// GetVersion), rejecting a lineage bound to an agent.
func validateWorkflowScheduleDef(
	ctx context.Context,
	store workflowsvc.Store,
	tenant, user string,
	prompt string,
	approvedTools []string,
	lineageIDStr string,
	inputs map[string]string,
	k brokerv1.ScheduleKind, cronExpr string, runAt *timestamppb.Timestamp,
	now time.Time, minInterval time.Duration,
) (db.ScheduleKind, string, time.Time, uuid.UUID, map[string]string, error) {
	var zero uuid.UUID
	if strings.TrimSpace(prompt) != "" {
		return "", "", time.Time{}, zero, nil, status.Error(codes.InvalidArgument, "prompt not allowed on a workflow schedule")
	}
	if len(approvedTools) > 0 {
		return "", "", time.Time{}, zero, nil, status.Error(codes.InvalidArgument, "approved_tools not allowed on a workflow schedule")
	}
	lineageID, err := uuid.Parse(lineageIDStr)
	if err != nil {
		return "", "", time.Time{}, zero, nil, status.Errorf(codes.InvalidArgument, "invalid workflow_lineage_id: %v", err)
	}
	if store == nil {
		return "", "", time.Time{}, zero, nil, status.Error(codes.FailedPrecondition, "workflow storage not configured")
	}
	version, err := store.ResolveVersionForUser(ctx, tenant, user, lineageID)
	if err != nil {
		return "", "", time.Time{}, zero, nil, status.Errorf(codes.InvalidArgument, "workflow not found: %v", err)
	}
	row, err := store.GetVersion(ctx, tenant, lineageID, version)
	if err != nil {
		return "", "", time.Time{}, zero, nil, status.Errorf(codes.InvalidArgument, "workflow not found: %v", err)
	}
	if row.BoundAgentID != nil {
		return "", "", time.Time{}, zero, nil, status.Error(codes.InvalidArgument, "cannot schedule an agent-bound workflow")
	}
	kind, resolvedCron, next, err := validateScheduleTiming(k, cronExpr, runAt, now, minInterval)
	if err != nil {
		return "", "", time.Time{}, zero, nil, err
	}
	return kind, resolvedCron, next, lineageID, inputs, nil
}

// authorizeRunMutation allows the run's owner, otherwise requires tenant-admin.
func (s *BrokerService) authorizeRunMutation(ctx context.Context, run *db.ScheduledRun, user string) error {
	if run.OwnerUserID == user {
		return nil
	}
	return s.requireTenantAdmin(ctx, user)
}

// CreateScheduledRun creates a cron or one-off scheduled agent run for the
// authenticated caller; requires the `skill:scheduler` FGA grant and returns
// InvalidArgument if the cron expression, run_at, or approved_tools list is invalid.
func (s *BrokerService) CreateScheduledRun(ctx context.Context, req *brokerv1.CreateScheduledRunRequest) (*brokerv1.CreateScheduledRunResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.scheduler.create")
	defer span.End()

	tenant, user, err := s.callerIdentity(ctx, req.TenantId, req.UserId)
	if err != nil {
		return nil, err
	}
	if err := s.requireSchedulerSkill(ctx, user); err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	isWorkflow := strings.TrimSpace(req.WorkflowLineageId) != ""

	var kind db.ScheduleKind
	var cronExpr string
	var next time.Time
	var lineageID *uuid.UUID
	var workflowInputs map[string]string
	if isWorkflow {
		// Workflow resolution re-checks skill:workflows + visibility (the same
		// GetWorkflow path) in addition to, not instead of, skill:scheduler above.
		if err := s.checkWorkflowSkillNorth(ctx, user); err != nil {
			return nil, err
		}
		var lid uuid.UUID
		kind, cronExpr, next, lid, workflowInputs, err = validateWorkflowScheduleDef(
			ctx, s.deps.Workflows, tenant, user, req.Prompt, req.ApprovedTools,
			req.WorkflowLineageId, req.WorkflowInputs, req.Kind, req.CronExpr, req.RunAt, now,
			s.deps.SchedulerMinInterval,
		)
		if err != nil {
			return nil, err
		}
		lineageID = &lid
	} else {
		kind, cronExpr, next, err = validateScheduleDef(s.toolReg(), req.Prompt, req.Kind, req.CronExpr, req.RunAt, req.ApprovedTools, now, s.deps.SchedulerMinInterval)
		if err != nil {
			return nil, err
		}
	}
	tenantUUID, err := uuid.Parse(tenant)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid tenant_id: %v", err)
	}

	id := uuid.New()
	run := &db.ScheduledRun{
		ScheduledRunID:    id,
		TenantID:          tenantUUID,
		OwnerUserID:       user,
		Prompt:            strings.TrimSpace(req.Prompt),
		Kind:              kind,
		CronExpr:          cronExpr,
		NextFireAt:        &next,
		ApprovedTools:     req.ApprovedTools,
		CreatedBy:         user,
		WorkflowLineageID: lineageID,
		WorkflowInputs:    workflowInputs,
	}
	if run.ApprovedTools == nil {
		run.ApprovedTools = []string{}
	}
	if err := s.deps.Scheduled.Create(ctx, run); err != nil {
		return nil, status.Errorf(codes.Internal, "create scheduled run: %v", err)
	}
	stored, err := s.deps.Scheduled.Get(ctx, tenant, id.String())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load created run: %v", err)
	}
	schedulerAudit(s.deps, ctx, span.SpanContext().TraceID().String(), tenant, user,
		"aikonos.scheduler.run.created", id.String(), auditv1.PolicyDecision_ALLOW)
	return &brokerv1.CreateScheduledRunResponse{Run: toProtoScheduledRun(stored)}, nil
}

func (s *BrokerService) ListScheduledRuns(ctx context.Context, req *brokerv1.ListScheduledRunsRequest) (*brokerv1.ListScheduledRunsResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.scheduler.list")
	defer span.End()

	tenant, user, err := s.callerIdentity(ctx, req.TenantId, req.UserId)
	if err != nil {
		return nil, err
	}

	owner := user
	if req.OwnerFilter != "" {
		// Listing another user's (or every user's) runs is an admin action.
		if err := s.requireTenantAdmin(ctx, user); err != nil {
			return nil, err
		}
		if req.OwnerFilter == listAllSentinel {
			owner = ""
		} else {
			owner = req.OwnerFilter
		}
	}

	runs, err := s.deps.Scheduled.List(ctx, tenant, owner)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list scheduled runs: %v", err)
	}
	out := make([]*brokerv1.ScheduledRun, 0, len(runs))
	for _, r := range runs {
		out = append(out, toProtoScheduledRun(r))
	}
	schedulerAudit(s.deps, ctx, span.SpanContext().TraceID().String(), tenant, user,
		"aikonos.scheduler.runs.listed", "", auditv1.PolicyDecision_ALLOW)
	return &brokerv1.ListScheduledRunsResponse{Runs: out, FgaEnabled: s.deps.Policy.FGAEnabled()}, nil
}

func (s *BrokerService) UpdateScheduledRun(ctx context.Context, req *brokerv1.UpdateScheduledRunRequest) (*brokerv1.UpdateScheduledRunResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.scheduler.update")
	defer span.End()

	tenant, user, err := s.callerIdentity(ctx, req.TenantId, req.UserId)
	if err != nil {
		return nil, err
	}
	if req.Id == "" {
		return nil, status.Error(codes.InvalidArgument, "id required")
	}
	run, err := s.deps.Scheduled.Get(ctx, tenant, req.Id)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "scheduled run not found")
	}
	if err := s.authorizeRunMutation(ctx, run, user); err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	switch req.DesiredState {
	case brokerv1.ScheduledRunState_SCHEDULED_RUN_PAUSED:
		// Keep next_fire_at; ClaimDue ignores non-ACTIVE rows.
		if err := s.deps.Scheduled.SetState(ctx, tenant, req.Id, db.ScheduledRunPaused, run.NextFireAt); err != nil {
			return nil, status.Errorf(codes.Internal, "pause: %v", err)
		}
	case brokerv1.ScheduledRunState_SCHEDULED_RUN_ACTIVE:
		// Resume: re-anchor a recurring run to its next future slot so a long
		// pause doesn't fire a backlog; a one-off keeps its original time.
		next := run.NextFireAt
		if run.Kind == db.ScheduleKindCron {
			n, cerr := db.NextCronFire(run.CronExpr, now)
			if cerr != nil {
				return nil, status.Errorf(codes.FailedPrecondition, "%v", cerr)
			}
			next = &n
		}
		if err := s.deps.Scheduled.SetState(ctx, tenant, req.Id, db.ScheduledRunActive, next); err != nil {
			return nil, status.Errorf(codes.Internal, "resume: %v", err)
		}
	default: // full edit
		if run.WorkflowLineageID != nil {
			// Workflow-mode row: timing (kind/cron/run_at) is the only editable
			// field. Any attempt to touch the prompt, approved_tools, or the
			// workflow binding/inputs is rejected outright (design: "workflow
			// schedules are frozen except timing").
			if strings.TrimSpace(req.Prompt) != "" {
				return nil, status.Error(codes.InvalidArgument, "cannot set a prompt on a workflow schedule")
			}
			if len(req.ApprovedTools) > 0 {
				return nil, status.Error(codes.InvalidArgument, "cannot set approved_tools on a workflow schedule")
			}
			if req.WorkflowLineageId != "" && req.WorkflowLineageId != run.WorkflowLineageID.String() {
				return nil, status.Error(codes.InvalidArgument, "workflow binding is immutable")
			}
			if len(req.WorkflowInputs) > 0 && !maps.Equal(req.WorkflowInputs, run.WorkflowInputs) {
				return nil, status.Error(codes.InvalidArgument, "workflow inputs are immutable")
			}
			kind, cronExpr, next, verr := validateScheduleTiming(req.Kind, req.CronExpr, req.RunAt, now, s.deps.SchedulerMinInterval)
			if verr != nil {
				return nil, verr
			}
			edit := &db.ScheduledRun{
				Prompt:            "",
				Kind:              kind,
				CronExpr:          cronExpr,
				NextFireAt:        &next,
				ApprovedTools:     []string{},
				WorkflowLineageID: run.WorkflowLineageID,
				WorkflowInputs:    run.WorkflowInputs,
			}
			if err := s.deps.Scheduled.Update(ctx, tenant, req.Id, edit); err != nil {
				return nil, status.Errorf(codes.Internal, "update: %v", err)
			}
			break
		}
		if req.WorkflowLineageId != "" {
			return nil, status.Error(codes.InvalidArgument, "cannot bind an existing prompt schedule to a workflow")
		}
		kind, cronExpr, next, verr := validateScheduleDef(s.toolReg(), req.Prompt, req.Kind, req.CronExpr, req.RunAt, req.ApprovedTools, now, s.deps.SchedulerMinInterval)
		if verr != nil {
			return nil, verr
		}
		tools := req.ApprovedTools
		if tools == nil {
			tools = []string{}
		}
		edit := &db.ScheduledRun{
			Prompt:        strings.TrimSpace(req.Prompt),
			Kind:          kind,
			CronExpr:      cronExpr,
			NextFireAt:    &next,
			ApprovedTools: tools,
		}
		if err := s.deps.Scheduled.Update(ctx, tenant, req.Id, edit); err != nil {
			return nil, status.Errorf(codes.Internal, "update: %v", err)
		}
	}

	updated, err := s.deps.Scheduled.Get(ctx, tenant, req.Id)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load updated run: %v", err)
	}
	schedulerAudit(s.deps, ctx, span.SpanContext().TraceID().String(), tenant, user,
		"aikonos.scheduler.run.updated", req.Id, auditv1.PolicyDecision_ALLOW)
	return &brokerv1.UpdateScheduledRunResponse{Run: toProtoScheduledRun(updated)}, nil
}

func (s *BrokerService) DeleteScheduledRun(ctx context.Context, req *brokerv1.DeleteScheduledRunRequest) (*brokerv1.DeleteScheduledRunResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.scheduler.delete")
	defer span.End()

	tenant, user, err := s.callerIdentity(ctx, req.TenantId, req.UserId)
	if err != nil {
		return nil, err
	}
	if req.Id == "" {
		return nil, status.Error(codes.InvalidArgument, "id required")
	}
	run, err := s.deps.Scheduled.Get(ctx, tenant, req.Id)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "scheduled run not found")
	}
	if err := s.authorizeRunMutation(ctx, run, user); err != nil {
		return nil, err
	}
	if err := s.deps.Scheduled.Delete(ctx, tenant, req.Id); err != nil {
		return nil, status.Errorf(codes.Internal, "delete: %v", err)
	}
	schedulerAudit(s.deps, ctx, span.SpanContext().TraceID().String(), tenant, user,
		"aikonos.scheduler.run.deleted", req.Id, auditv1.PolicyDecision_ALLOW)
	return &brokerv1.DeleteScheduledRunResponse{Success: true}, nil
}

// ── South: gateway ticker (SPIFFE-gated to the agent-gateway) ─────────────────

// requireGatewayPeer restricts the scheduler infra RPCs to the agent-gateway's
// verified SVID — these advance durable state, so no arbitrary sandbox may call
// them. When GatewaySpiffeID is unset (dev), any trust-domain peer is accepted.
func (s *SandboxService) requireGatewayPeer(ctx context.Context) error {
	id, ok := auth.IdentityFromContext(ctx)
	if !ok || id.SpiffeID == "" {
		return status.Error(codes.PermissionDenied, "peer identity required")
	}
	if s.deps.GatewaySpiffeID != "" && id.SpiffeID != s.deps.GatewaySpiffeID {
		return status.Error(codes.PermissionDenied, "not the agent-gateway identity")
	}
	return nil
}

// ClaimDueScheduledRuns atomically claims up to limit due runs for the tenant
// and returns each with a broker-minted HMAC owner grant; restricted to the
// agent-gateway SPIFFE identity (any trust-domain peer in dev when GatewaySpiffeID is unset).
func (s *SandboxService) ClaimDueScheduledRuns(ctx context.Context, req *brokerv1.ClaimDueScheduledRunsRequest) (*brokerv1.ClaimDueScheduledRunsResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.scheduler.claim")
	defer span.End()

	if err := s.requireGatewayPeer(ctx); err != nil {
		return nil, err
	}
	if _, err := uuid.Parse(req.TenantId); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid tenant_id: %v", err)
	}
	now := time.Now().UTC()
	if req.Now != nil {
		now = req.Now.AsTime()
	}
	limit := int(req.Limit)
	if limit <= 0 || limit > 100 {
		limit = defaultClaimLimit
	}

	due, err := s.deps.Scheduled.ClaimDue(ctx, req.TenantId, now, limit)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "claim due: %v", err)
	}
	out := make([]*brokerv1.DueRun, 0, len(due))
	traceID := span.SpanContext().TraceID().String()
	for _, r := range due {
		ownerGrant, mintErr := gatewaygrant.Mint(s.deps.GatewayGrantKey, req.TenantId, r.OwnerUserID, s.deps.GatewayGrantTTL)
		if mintErr != nil {
			// A mint failure means we cannot hand the gateway a usable, non-impersonable
			// run — skip rather than return a run with an empty grant that the broker
			// would then reject at CreateGatewayTask.
			s.deps.Logger.Warn("ClaimDueScheduledRuns: mint owner grant failed, skipping run",
				zap.String("run_id", r.ScheduledRunID.String()),
				zap.Error(mintErr),
			)
			continue
		}
		due := &brokerv1.DueRun{
			Id:             r.ScheduledRunID.String(),
			OwnerUserId:    r.OwnerUserID,
			Prompt:         r.Prompt,
			ApprovedTools:  r.ApprovedTools,
			OwnerGrant:     ownerGrant,
			WorkflowInputs: r.WorkflowInputs,
		}
		if r.WorkflowLineageID != nil {
			due.WorkflowLineageId = r.WorkflowLineageID.String()
		}
		out = append(out, due)
		schedulerAudit(s.deps, ctx, traceID, req.TenantId, r.OwnerUserID,
			"aikonos.scheduler.run.fired", r.ScheduledRunID.String(), auditv1.PolicyDecision_ALLOW)
	}
	return &brokerv1.ClaimDueScheduledRunsResponse{Runs: out}, nil
}

func (s *SandboxService) ReportScheduledRunResult(ctx context.Context, req *brokerv1.ReportScheduledRunResultRequest) (*brokerv1.ReportScheduledRunResultResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.scheduler.report")
	defer span.End()

	if err := s.requireGatewayPeer(ctx); err != nil {
		return nil, err
	}
	if _, err := uuid.Parse(req.TenantId); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid tenant_id: %v", err)
	}
	if req.Id == "" {
		return nil, status.Error(codes.InvalidArgument, "id required")
	}
	if err := s.deps.Scheduled.ReportResult(ctx, req.TenantId, req.Id, req.Success, req.Summary); err != nil {
		return nil, status.Errorf(codes.Internal, "report result: %v", err)
	}
	event := "aikonos.scheduler.run.completed"
	decision := auditv1.PolicyDecision_ALLOW
	if !req.Success {
		event = "aikonos.scheduler.run.failed"
		decision = auditv1.PolicyDecision_DENY
	}
	traceID := span.SpanContext().TraceID().String()
	schedulerAudit(s.deps, ctx, traceID, req.TenantId, "", event, req.Id, decision)

	// Best-effort: write the session file when the gateway sends one.
	// Any failure here is logged and swallowed — the result report is the primary job.
	// Workspace nil-check is defensive: Enabled() has a pointer receiver and panics on nil.
	if len(req.SessionRecord) > 0 && s.deps.Workspace.Local != nil && s.deps.Workspace.Local.Enabled() {
		if _, parseErr := uuid.Parse(req.SessionId); parseErr != nil {
			s.deps.Logger.Warn("ReportScheduledRunResult: session_id is not a valid UUID, skipping session file write",
				zap.String("session_id", req.SessionId),
				zap.String("run_id", req.Id),
			)
		} else {
			run, getErr := s.deps.Scheduled.Get(ctx, req.TenantId, req.Id)
			if getErr != nil {
				s.deps.Logger.Warn("ReportScheduledRunResult: failed to look up run for session file write",
					zap.String("run_id", req.Id),
					zap.Error(getErr),
				)
			} else {
				rel := ".agent/Sessions/" + req.SessionId + ".json"
				// CP4.2: Write signs this automatically (the resolved path lands
				// under workspacefs.SessionsDirPrefix) — this session file is read
				// back as chat history to seed later gateway executions.
				if _, writeErr := s.deps.Workspace.Local.Write(ctx, req.TenantId, run.OwnerUserID, rel, req.SessionRecord); writeErr != nil {
					s.deps.Logger.Warn("ReportScheduledRunResult: session file write failed",
						zap.String("run_id", req.Id),
						zap.String("session_id", req.SessionId),
						zap.String("owner", run.OwnerUserID),
						zap.Error(writeErr),
					)
				} else {
					schedulerAudit(s.deps, ctx, traceID, req.TenantId, run.OwnerUserID,
						"aikonos.scheduler.session.persisted", req.SessionId, auditv1.PolicyDecision_ALLOW)
				}
			}
		}
	}

	return &brokerv1.ReportScheduledRunResultResponse{}, nil
}

// schedulerAudit emits a scheduler audit event (shared by the north and south
// surfaces). The run id, when present, is the resource ref.
func schedulerAudit(deps Deps, ctx context.Context, traceID, tenant, actor, eventType, runID string, decision auditv1.PolicyDecision) {
	ev := &auditv1.AuditEvent{
		EventId:     ids.EventID(),
		TraceId:     traceID,
		TenantId:    tenant,
		OccurredAt:  timestampNow(),
		ActorUserId: actor,
		EventType:   eventType,
		Decision:    decision,
	}
	if runID != "" {
		ev.ResourceRef = "scheduled_run:" + runID
		if c, err := structpb.NewStruct(map[string]any{"scheduled_run_id": runID}); err == nil {
			ev.Context = c
		}
	}
	if err := deps.Audit.Emit(ctx, ev); err != nil {
		deps.Logger.Warn("scheduler audit emit failed", zap.Error(err))
	}
}
