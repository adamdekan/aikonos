// broker/internal/broker/revocation_admin.go
// AG-06 admin RPCs: block / unblock a user and revoke all sessions of a user
// or an agent. Tenant-admin gated, callerIdentity bound, audited with the
// acting admin and the reason. Enforcement lives where credentials are
// checked: auth.RevocationInterceptor (north), verifyOwnerGrant, InvokeTool
// and the scheduled-run claim.
package broker

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/ids"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

const maxRevocationReason = 500

// revocationAdmin is the shared prologue: feature wired, caller is a tenant
// admin, reason present and bounded. Returns tenant, admin and the reason.
func (s *BrokerService) revocationAdmin(ctx context.Context, reqTenant, reqUser, reason string) (string, string, string, error) {
	if s.deps.Revocation == nil {
		return "", "", "", status.Error(codes.FailedPrecondition, "revocation not configured")
	}
	tenant, user, err := s.callerIdentity(ctx, reqTenant, reqUser)
	if err != nil {
		return "", "", "", err
	}
	if err := s.requireTenantAdmin(ctx, user); err != nil {
		return "", "", "", err
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return "", "", "", status.Error(codes.InvalidArgument, "reason required")
	}
	if len(reason) > maxRevocationReason {
		return "", "", "", status.Errorf(codes.InvalidArgument, "reason longer than %d bytes", maxRevocationReason)
	}
	return tenant, user, reason, nil
}

// SetUserBlocked blocks or unblocks a user. An admin cannot block themself:
// that is the one block nobody in the tenant could lift from the UI.
func (s *BrokerService) SetUserBlocked(ctx context.Context, req *brokerv1.SetUserBlockedRequest) (*brokerv1.SetUserBlockedResponse, error) {
	tenant, admin, reason, err := s.revocationAdmin(ctx, req.TenantId, req.UserId, req.Reason)
	if err != nil {
		return nil, err
	}
	subject := strings.TrimSpace(req.Subject)
	if subject == "" {
		return nil, status.Error(codes.InvalidArgument, "subject required")
	}
	if req.Blocked && subject == admin {
		return nil, status.Error(codes.InvalidArgument, "you cannot block yourself")
	}
	var changed bool
	event := "aikonos.broker.user.blocked"
	if req.Blocked {
		changed, err = s.deps.Revocation.BlockUser(ctx, tenant, subject, reason, admin)
	} else {
		event = "aikonos.broker.user.unblocked"
		changed, err = s.deps.Revocation.UnblockUser(ctx, tenant, subject, admin)
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "set user block: %v", err)
	}
	s.emitRevocationAudit(ctx, tenant, admin, event, "user:"+subject, map[string]any{"reason": reason, "changed": changed})
	return &brokerv1.SetUserBlockedResponse{Changed: changed}, nil
}

// RevokeUserSessions moves the user's revocation epoch to now.
func (s *BrokerService) RevokeUserSessions(ctx context.Context, req *brokerv1.RevokeUserSessionsRequest) (*brokerv1.RevokeSessionsResponse, error) {
	tenant, admin, reason, err := s.revocationAdmin(ctx, req.TenantId, req.UserId, req.Reason)
	if err != nil {
		return nil, err
	}
	subject := strings.TrimSpace(req.Subject)
	if subject == "" {
		return nil, status.Error(codes.InvalidArgument, "subject required")
	}
	at, err := s.deps.Revocation.RevokeUser(ctx, tenant, subject)
	if errors.Is(err, db.ErrNotFound) {
		return nil, status.Errorf(codes.NotFound, "user %s has not signed in", subject)
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "revoke user sessions: %v", err)
	}
	nb := at.Format(time.RFC3339Nano)
	s.emitRevocationAudit(ctx, tenant, admin, "aikonos.broker.user.sessions_revoked", "user:"+subject, map[string]any{"reason": reason, "not_before": nb})
	return &brokerv1.RevokeSessionsResponse{NotBefore: nb}, nil
}

// RevokeAgentSessions moves the agent's revocation epoch to now: its
// outstanding capability tokens and owner grants die at their next use, its
// API keys minted before now stop resolving, and its schedules created before
// now are parked at their next claim.
func (s *BrokerService) RevokeAgentSessions(ctx context.Context, req *brokerv1.RevokeAgentSessionsRequest) (*brokerv1.RevokeSessionsResponse, error) {
	tenant, admin, reason, err := s.revocationAdmin(ctx, req.TenantId, req.UserId, req.Reason)
	if err != nil {
		return nil, err
	}
	if _, err := uuid.Parse(req.AgentId); err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid agent_id")
	}
	at, err := s.deps.Revocation.RevokeAgent(ctx, tenant, req.AgentId)
	if errors.Is(err, db.ErrAgentNotFound) {
		return nil, status.Errorf(codes.NotFound, "agent %s not found", req.AgentId)
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "revoke agent sessions: %v", err)
	}
	nb := at.Format(time.RFC3339Nano)
	s.emitRevocationAudit(ctx, tenant, admin, "aikonos.broker.agent.sessions_revoked", "agent:"+req.AgentId, map[string]any{"reason": reason, "not_before": nb})
	return &brokerv1.RevokeSessionsResponse{NotBefore: nb}, nil
}

func (s *BrokerService) emitRevocationAudit(ctx context.Context, tenant, actor, eventType, resource string, fields map[string]any) {
	if s.deps.Audit == nil {
		return
	}
	c, _ := structpb.NewStruct(fields)
	if err := s.deps.Audit.Emit(ctx, &auditv1.AuditEvent{
		EventId:     ids.EventID(),
		TraceId:     trace.SpanContextFromContext(ctx).TraceID().String(),
		TenantId:    tenant,
		OccurredAt:  timestampNow(),
		ActorUserId: actor,
		EventType:   eventType,
		ResourceRef: resource,
		Decision:    auditv1.PolicyDecision_ALLOW,
		Context:     c,
	}); err != nil {
		audit.RecordEmitFailure(ctx, s.deps.Logger, err, eventType)
	}
}
