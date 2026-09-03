package broker

import (
	"context"
	"strings"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/ids"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

var validNetworkScopes = map[string]db.NetworkScopeKind{
	"TENANT": db.NetworkScopeTenant,
	"GROUP":  db.NetworkScopeGroup,
	"USER":   db.NetworkScopeUser,
}

var validNetworkActions = map[string]db.NetworkAction{
	"ALLOW": db.NetworkAllow,
	"DENY":  db.NetworkDeny,
	"ASK":   db.NetworkAsk,
}

func (s *BrokerService) networkEnabled() bool { return s.deps.Network != nil }

func toProtoNetworkRule(r *db.NetworkRule) *brokerv1.NetworkRule {
	return &brokerv1.NetworkRule{
		Id:          r.ID.String(),
		ScopeKind:   string(r.ScopeKind),
		ScopeValue:  r.ScopeValue,
		Action:      string(r.Action),
		HostPattern: r.HostPattern,
		Note:        r.Note,
		CreatedBy:   r.CreatedBy,
		CreatedAt:   timestamppb.New(r.CreatedAt),
	}
}

// validateNetworkRule enforces the enums, a sane host pattern, and no whitespace
// (the host is matched verbatim). A GROUP/USER rule must name its subject.
func validateNetworkRule(t *brokerv1.NetworkRule) (db.NetworkScopeKind, db.NetworkAction, error) {
	if t == nil {
		return "", "", status.Error(codes.InvalidArgument, "rule is required")
	}
	scope, ok := validNetworkScopes[t.ScopeKind]
	if !ok {
		return "", "", status.Errorf(codes.InvalidArgument, "scope_kind must be TENANT|GROUP|USER, got %q", t.ScopeKind)
	}
	action, ok := validNetworkActions[t.Action]
	if !ok {
		return "", "", status.Errorf(codes.InvalidArgument, "action must be ALLOW|DENY|ASK, got %q", t.Action)
	}
	if scope != db.NetworkScopeTenant && strings.TrimSpace(t.ScopeValue) == "" {
		return "", "", status.Error(codes.InvalidArgument, "scope_value required for GROUP/USER rules")
	}
	host := strings.TrimSpace(t.HostPattern)
	if host == "" || len(host) > 253 || strings.ContainsAny(host, " \t\r\n/") {
		return "", "", status.Error(codes.InvalidArgument, "host_pattern must be a bare host or wildcard (no scheme/path/space)")
	}
	if strings.Contains(host, "*") && host != "*" && !strings.HasPrefix(host, "*.") {
		return "", "", status.Error(codes.InvalidArgument, `wildcards must be "*" or "*.suffix"`)
	}
	return scope, action, nil
}

func (s *BrokerService) ListNetworkRules(ctx context.Context, req *brokerv1.ListNetworkRulesRequest) (*brokerv1.ListNetworkRulesResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.network.list")
	defer span.End()

	if !s.networkEnabled() {
		return nil, status.Error(codes.FailedPrecondition, "network rules disabled")
	}
	tenant, user, err := s.callerIdentity(ctx, req.TenantId, req.UserId)
	if err != nil {
		return nil, err
	}
	if err := s.requireTenantAdmin(ctx, user); err != nil {
		return nil, err
	}
	rows, err := s.deps.Network.List(ctx, tenant)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list network rules: %v", err)
	}
	out := make([]*brokerv1.NetworkRule, 0, len(rows))
	for _, r := range rows {
		out = append(out, toProtoNetworkRule(r))
	}
	s.emitNetworkRuleAudit(ctx, span.SpanContext().TraceID().String(), tenant, user,
		"aikonos.broker.network.rules.listed", nil, auditv1.PolicyDecision_ALLOW)
	return &brokerv1.ListNetworkRulesResponse{Rules: out}, nil
}

// AddNetworkRule persists a new web-egress access-list rule for the tenant;
// requires tenant-admin and returns InvalidArgument if the host pattern or
// enum fields are invalid.
func (s *BrokerService) AddNetworkRule(ctx context.Context, req *brokerv1.AddNetworkRuleRequest) (*brokerv1.AddNetworkRuleResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.network.add")
	defer span.End()

	if !s.networkEnabled() {
		return nil, status.Error(codes.FailedPrecondition, "network rules disabled")
	}
	tenant, user, err := s.callerIdentity(ctx, req.TenantId, req.UserId)
	if err != nil {
		return nil, err
	}
	if err := s.requireTenantAdmin(ctx, user); err != nil {
		return nil, err
	}
	scope, action, err := validateNetworkRule(req.Rule)
	if err != nil {
		return nil, err
	}
	tenantUUID, err := uuid.Parse(tenant)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid tenant_id: %v", err)
	}
	rule := &db.NetworkRule{
		ID:          uuid.New(),
		TenantID:    tenantUUID,
		ScopeKind:   scope,
		ScopeValue:  strings.TrimSpace(req.Rule.ScopeValue),
		Action:      action,
		HostPattern: strings.ToLower(strings.TrimSpace(req.Rule.HostPattern)),
		Note:        req.Rule.Note,
		CreatedBy:   user,
	}
	if err := s.deps.Network.Add(ctx, rule); err != nil {
		return nil, status.Errorf(codes.Internal, "add network rule: %v", err)
	}
	stored, err := s.deps.Network.List(ctx, tenant)
	if err == nil {
		for _, r := range stored {
			if r.ID == rule.ID {
				rule = r
				break
			}
		}
	}
	s.emitNetworkRuleAudit(ctx, span.SpanContext().TraceID().String(), tenant, user,
		"aikonos.broker.network.rule.added", rule, auditv1.PolicyDecision_ALLOW)
	return &brokerv1.AddNetworkRuleResponse{Rule: toProtoNetworkRule(rule)}, nil
}

func (s *BrokerService) DeleteNetworkRule(ctx context.Context, req *brokerv1.DeleteNetworkRuleRequest) (*brokerv1.DeleteNetworkRuleResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.network.delete")
	defer span.End()

	if !s.networkEnabled() {
		return nil, status.Error(codes.FailedPrecondition, "network rules disabled")
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
	if err := s.deps.Network.Delete(ctx, tenant, req.Id); err != nil {
		return nil, status.Errorf(codes.Internal, "delete network rule: %v", err)
	}
	s.emitNetworkRuleAudit(ctx, span.SpanContext().TraceID().String(), tenant, user,
		"aikonos.broker.network.rule.deleted", &db.NetworkRule{HostPattern: req.Id}, auditv1.PolicyDecision_ALLOW)
	return &brokerv1.DeleteNetworkRuleResponse{Success: true}, nil
}

func (s *BrokerService) emitNetworkRuleAudit(ctx context.Context, traceID, tenant, actor, eventType string, rule *db.NetworkRule, decision auditv1.PolicyDecision) {
	ev := &auditv1.AuditEvent{
		EventId:     ids.EventID(),
		TraceId:     traceID,
		TenantId:    tenant,
		OccurredAt:  timestampNow(),
		ActorUserId: actor,
		EventType:   eventType,
		Decision:    decision,
	}
	if rule != nil {
		ev.ResourceRef = "network_rule:" + rule.HostPattern
		if c, err := structpb.NewStruct(map[string]any{
			"scope_kind": string(rule.ScopeKind), "scope_value": rule.ScopeValue,
			"action": string(rule.Action), "host_pattern": rule.HostPattern,
		}); err == nil {
			ev.Context = c
		}
	}
	if err := s.deps.Audit.Emit(ctx, ev); err != nil {
		s.deps.Logger.Warn("network rule audit emit failed", zap.Error(err))
	}
}
