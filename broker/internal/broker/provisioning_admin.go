// broker/internal/broker/provisioning_admin.go
// North admin RPCs for email-based JIT provisioning rules.
// Pattern: network.go (callerIdentity + requireTenantAdmin + audit).
package broker

import (
	"context"
	"strings"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/ids"
	"github.com/adamdekan/aikonos/broker/internal/policy"
	"github.com/adamdekan/aikonos/broker/internal/provisioning"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// provisioningAdminStore is the DB seam the admin RPCs need. Satisfied by
// *db.ProvisioningRepo; injectable in tests via the interface.
type provisioningAdminStore interface {
	ListRules(ctx context.Context, tenantID string) ([]db.ProvisioningRule, error)
	AddRule(ctx context.Context, tenantID string, rule db.ProvisioningRule) error
	DeleteRule(ctx context.Context, tenantID, id string) error
	SubjectsByEmail(ctx context.Context, tenantID, email string) ([]string, error)
}

// provisioningEnabled reports whether the provisioning repo is wired. List
// works regardless of FGA state; Add/Delete require FGA too.
func (s *BrokerService) provisioningEnabled() bool { return s.deps.Provisioning != nil }

func toProtoProvisioningRule(r db.ProvisioningRule) *brokerv1.ProvisioningRule {
	return &brokerv1.ProvisioningRule{
		Id:        r.ID.String(),
		Matcher:   r.Matcher,
		Groups:    r.Groups,
		CreatedBy: r.CreatedBy,
		CreatedAt: r.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
}

func (s *BrokerService) ListProvisioningRules(ctx context.Context, req *brokerv1.ListProvisioningRulesRequest) (*brokerv1.ListProvisioningRulesResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.provisioning.list")
	defer span.End()

	if !s.provisioningEnabled() {
		return nil, status.Error(codes.FailedPrecondition, "provisioning disabled")
	}
	tenant, user, err := s.callerIdentity(ctx, req.TenantId, req.UserId)
	if err != nil {
		return nil, err
	}
	if err := s.requireTenantAdmin(ctx, user); err != nil {
		return nil, err
	}
	rows, err := s.deps.Provisioning.ListRules(ctx, tenant)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list provisioning rules: %v", err)
	}
	out := make([]*brokerv1.ProvisioningRule, 0, len(rows))
	for _, r := range rows {
		out = append(out, toProtoProvisioningRule(r))
	}
	s.emitProvisioningAudit(ctx, span.SpanContext().TraceID().String(), tenant, user,
		"aikonos.broker.provisioning.rules.listed", "", auditv1.PolicyDecision_ALLOW)
	return &brokerv1.ListProvisioningRulesResponse{Rules: out}, nil
}

// AddProvisioningRule persists an email→groups JIT provisioning rule and
// immediately applies it to already-seen users matching an exact-email matcher;
// requires tenant-admin and an active OpenFGA store.
func (s *BrokerService) AddProvisioningRule(ctx context.Context, req *brokerv1.AddProvisioningRuleRequest) (*brokerv1.AddProvisioningRuleResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.provisioning.add")
	defer span.End()

	if !s.provisioningEnabled() {
		return nil, status.Error(codes.FailedPrecondition, "provisioning disabled")
	}
	if !s.deps.Policy.FGAEnabled() {
		return nil, status.Error(codes.FailedPrecondition, "OpenFGA is disabled")
	}
	tenant, user, err := s.callerIdentity(ctx, req.TenantId, req.UserId)
	if err != nil {
		return nil, err
	}
	if err := s.requireTenantAdmin(ctx, user); err != nil {
		return nil, err
	}

	matcher, err := provisioning.NormalizeMatcher(req.Matcher)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid matcher: %v", err)
	}
	if err := provisioning.ValidateGroups(req.Groups); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid groups: %v", err)
	}

	rule := db.ProvisioningRule{
		ID:        uuid.New(),
		Matcher:   matcher,
		Groups:    req.Groups,
		CreatedBy: user,
	}
	if err := s.deps.Provisioning.AddRule(ctx, tenant, rule); err != nil {
		if strings.Contains(err.Error(), "already exists") {
			return nil, status.Errorf(codes.AlreadyExists, "%v", err)
		}
		return nil, status.Errorf(codes.Internal, "add provisioning rule: %v", err)
	}

	// For exact matchers, immediately apply the rule to already-seen users.
	appliedCount := int32(0)
	if !strings.HasPrefix(matcher, "*@") {
		subjects, sErr := s.deps.Provisioning.SubjectsByEmail(ctx, tenant, matcher)
		if sErr != nil {
			s.deps.Logger.Warn("provisioning: SubjectsByEmail failed on add",
				zap.String("matcher", matcher), zap.Error(sErr))
		} else {
			for _, subject := range subjects {
				userRef := "user:" + subject
				subjectWritten := false
				for _, g := range req.Groups {
					wErr := s.deps.Policy.WriteRelations(ctx,
						policy.Relation{User: userRef, Relation: "member", Object: "group:" + g})
					if wErr != nil && !provisioning.IsDuplicateTuple(wErr) {
						s.deps.Logger.Warn("provisioning: WriteRelations failed",
							zap.String("subject", subject), zap.String("group", g), zap.Error(wErr))
					} else {
						// nil (new write) or duplicate (tolerated) both count as success.
						subjectWritten = true
					}
				}
				if subjectWritten {
					appliedCount++
				}
			}
		}
	}

	s.emitProvisioningAudit(ctx, span.SpanContext().TraceID().String(), tenant, user,
		"aikonos.broker.provisioning.rule.added", rule.ID.String(), auditv1.PolicyDecision_ALLOW)
	return &brokerv1.AddProvisioningRuleResponse{
		Rule:         toProtoProvisioningRule(rule),
		AppliedCount: appliedCount,
	}, nil
}

func (s *BrokerService) DeleteProvisioningRule(ctx context.Context, req *brokerv1.DeleteProvisioningRuleRequest) (*brokerv1.DeleteProvisioningRuleResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.provisioning.delete")
	defer span.End()

	if !s.provisioningEnabled() {
		return nil, status.Error(codes.FailedPrecondition, "provisioning disabled")
	}
	if !s.deps.Policy.FGAEnabled() {
		return nil, status.Error(codes.FailedPrecondition, "OpenFGA is disabled")
	}
	tenant, user, err := s.callerIdentity(ctx, req.TenantId, req.UserId)
	if err != nil {
		return nil, err
	}
	if err := s.requireTenantAdmin(ctx, user); err != nil {
		return nil, err
	}
	if req.Id == "" {
		return nil, status.Error(codes.InvalidArgument, "id required")
	}
	if err := s.deps.Provisioning.DeleteRule(ctx, tenant, req.Id); err != nil {
		if strings.Contains(err.Error(), "not found") {
			return nil, status.Errorf(codes.NotFound, "%v", err)
		}
		return nil, status.Errorf(codes.Internal, "delete provisioning rule: %v", err)
	}
	s.emitProvisioningAudit(ctx, span.SpanContext().TraceID().String(), tenant, user,
		"aikonos.broker.provisioning.rule.deleted", req.Id, auditv1.PolicyDecision_ALLOW)
	return &brokerv1.DeleteProvisioningRuleResponse{}, nil
}

func (s *BrokerService) emitProvisioningAudit(ctx context.Context, traceID, tenant, actor, eventType, ruleID string, decision auditv1.PolicyDecision) {
	ev := &auditv1.AuditEvent{
		EventId:     ids.EventID(),
		TraceId:     traceID,
		TenantId:    tenant,
		OccurredAt:  timestampNow(),
		ActorUserId: actor,
		EventType:   eventType,
		Decision:    decision,
	}
	if ruleID != "" {
		ev.ResourceRef = "provisioning_rule:" + ruleID
		if c, err := structpb.NewStruct(map[string]any{"rule_id": ruleID}); err == nil {
			ev.Context = c
		}
	}
	if err := s.deps.Audit.Emit(ctx, ev); err != nil {
		s.deps.Logger.Warn("provisioning rule audit emit failed", zap.Error(err))
	}
}
