package broker

import (
	"context"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/ids"
	"github.com/adamdekan/aikonos/broker/internal/ratelimit"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// North admin RPCs ─────────────────────────────────────────────────────────────

func (s *BrokerService) ListRateLimitPolicies(ctx context.Context, req *brokerv1.ListRateLimitPoliciesRequest) (*brokerv1.ListRateLimitPoliciesResponse, error) {
	tenant, user, err := s.callerIdentity(ctx, req.TenantId, "")
	if err != nil {
		return nil, err
	}
	if err := s.requireTenantAdmin(ctx, user); err != nil {
		return nil, err
	}
	if s.deps.RateLimitPolicies == nil {
		return &brokerv1.ListRateLimitPoliciesResponse{}, nil
	}
	rows, err := s.deps.RateLimitPolicies.List(ctx, tenant)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list rate limit policies: %v", err)
	}
	out := make([]*brokerv1.RateLimitPolicy, 0, len(rows))
	for _, r := range rows {
		out = append(out, rateLimitPolicyToProto(r))
	}
	return &brokerv1.ListRateLimitPoliciesResponse{Policies: out}, nil
}

func (s *BrokerService) SetRateLimitPolicy(ctx context.Context, req *brokerv1.SetRateLimitPolicyRequest) (*brokerv1.SetRateLimitPolicyResponse, error) {
	tenant, user, err := s.callerIdentity(ctx, req.TenantId, "")
	if err != nil {
		return nil, err
	}
	if err := s.requireTenantAdmin(ctx, user); err != nil {
		return nil, err
	}
	if s.deps.RateLimitPolicies == nil || s.deps.RateLimiter == nil {
		return nil, status.Error(codes.FailedPrecondition, "rate limiting not configured")
	}

	p := db.RateLimitPolicy{
		Provider:  req.Provider,
		CreatedBy: user,
	}
	if req.AgentId != "" {
		id, perr := uuid.Parse(req.AgentId)
		if perr != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid agent_id: %v", perr)
		}
		p.AgentID = &id
	}
	// 0 = deny-all, negative = unconstrained (stored as NULL)
	if req.RpmLimit >= 0 {
		v := req.RpmLimit
		p.RPMLimit = &v
	}
	if req.TpmLimit >= 0 {
		v := req.TpmLimit
		p.TPMLimit = &v
	}

	saved, err := s.deps.RateLimitPolicies.Upsert(ctx, tenant, p)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "upsert rate limit policy: %v", err)
	}

	// Hot-reload the in-process limiter with the updated policy set.
	// Non-fatal: policy is persisted even if the reload fails.
	if err := s.reloadRateLimitPolicies(ctx, tenant); err != nil {
		s.deps.Logger.Warn("rate-limit: failed to reload limiter after policy change; limiter may be stale", zap.Error(err))
	}

	return &brokerv1.SetRateLimitPolicyResponse{Id: saved.ID.String()}, nil
}

func (s *BrokerService) DeleteRateLimitPolicy(ctx context.Context, req *brokerv1.DeleteRateLimitPolicyRequest) (*brokerv1.DeleteRateLimitPolicyResponse, error) {
	tenant, user, err := s.callerIdentity(ctx, req.TenantId, "")
	if err != nil {
		return nil, err
	}
	if err := s.requireTenantAdmin(ctx, user); err != nil {
		return nil, err
	}
	if s.deps.RateLimitPolicies == nil {
		return nil, status.Error(codes.FailedPrecondition, "rate limiting not configured")
	}
	if err := s.deps.RateLimitPolicies.Delete(ctx, tenant, req.Id); err != nil {
		return nil, status.Errorf(codes.Internal, "delete rate limit policy: %v", err)
	}
	if s.deps.RateLimiter != nil {
		if err := s.reloadRateLimitPolicies(ctx, tenant); err != nil {
			s.deps.Logger.Warn("rate-limit: failed to reload limiter after policy change; limiter may be stale", zap.Error(err))
		}
	}
	return &brokerv1.DeleteRateLimitPolicyResponse{}, nil
}

// reloadRateLimitPolicies fetches all policies from DB and hot-reloads the limiter.
func (s *BrokerService) reloadRateLimitPolicies(ctx context.Context, tenant string) error {
	rows, err := s.deps.RateLimitPolicies.List(ctx, tenant)
	if err != nil {
		return err
	}
	s.deps.RateLimiter.SetPolicies(DBPoliciesToRatelimit(rows))
	return nil
}

// DBPoliciesToRatelimit converts DB policy rows to the in-process ratelimit.Policy slice.
func DBPoliciesToRatelimit(rows []db.RateLimitPolicy) []ratelimit.Policy {
	out := make([]ratelimit.Policy, 0, len(rows))
	for _, r := range rows {
		p := ratelimit.Policy{
			TenantID: r.TenantID.String(),
			Provider: r.Provider,
			RPM:      -1,
			TPM:      -1,
		}
		if r.AgentID != nil {
			p.AgentID = r.AgentID.String()
		}
		if r.RPMLimit != nil {
			p.RPM = *r.RPMLimit
		}
		if r.TPMLimit != nil {
			p.TPM = *r.TPMLimit
		}
		out = append(out, p)
	}
	return out
}

// checkLlmBudget is the shared "may this tenant spend on an LLM call right now"
// gate: RPM/TPM first, then monthly spend caps (a call under its rate limit can
// still be denied for having pushed the tenant/user/agent over budget). Lives on
// Deps so both the south CheckRateLimit RPC and the north TestLlmProvider probe
// enforce the same thing — any billable outbound call must route through here.
func (d Deps) checkLlmBudget(ctx context.Context, tenant, userID, agentID, provider string) (allowed bool, limitType string) {
	if d.RateLimiter != nil {
		if ok, lt := d.RateLimiter.CheckLlmRequest(ctx, tenant, agentID, provider); !ok {
			return false, lt
		}
	}
	return d.checkSpendCaps(ctx, tenant, userID, agentID)
}

// South RPC ────────────────────────────────────────────────────────────────────

func (s *SandboxService) CheckRateLimit(ctx context.Context, req *brokerv1.CheckRateLimitRequest) (*brokerv1.CheckRateLimitResponse, error) {
	if err := s.requireGatewayPeer(ctx); err != nil {
		return nil, err
	}
	if req.TenantId == "" {
		return nil, status.Error(codes.InvalidArgument, "tenant_id required")
	}
	if allowed, limitType := s.deps.checkLlmBudget(ctx, req.TenantId, req.UserId, req.AgentId, req.Provider); !allowed {
		go s.deps.emitCheckRateLimitAudit(context.Background(), req.TenantId, req.AgentId, req.Provider, limitType)
		return &brokerv1.CheckRateLimitResponse{Allowed: false, LimitType: limitType}, nil
	}

	return &brokerv1.CheckRateLimitResponse{Allowed: true}, nil
}

// emitCheckRateLimitAudit fires a rate-limit enforcement audit event for a
// denied billable call. Fire-and-forget; failures are logged, never surfaced.
func (d Deps) emitCheckRateLimitAudit(ctx context.Context, tenantID, agentID, provider, limitType string) {
	if d.Audit == nil {
		return
	}
	ctxStruct, _ := structpb.NewStruct(map[string]any{
		"limit_type": limitType,
		"agent_id":   agentID,
		"provider":   provider,
	})
	if err := d.Audit.Emit(ctx, &auditv1.AuditEvent{
		EventId:    ids.EventID(),
		TenantId:   tenantID,
		OccurredAt: timestampNow(),
		EventType:  "aikonos.broker.rate_limit.enforced",
		Decision:   auditv1.PolicyDecision_DENY,
		Context:    ctxStruct,
	}); err != nil {
		audit.RecordEmitFailure(ctx, d.Logger, err, "aikonos.broker.rate_limit.enforced")
	}
}

// rateLimitPolicyToProto converts a db.RateLimitPolicy to its proto representation.
// Default for rpm_limit / tpm_limit is −1 (unconstrained) so the zero value (deny-all)
// is unambiguous on the wire.
func rateLimitPolicyToProto(p db.RateLimitPolicy) *brokerv1.RateLimitPolicy {
	out := &brokerv1.RateLimitPolicy{
		Id:        p.ID.String(),
		TenantId:  p.TenantID.String(),
		Provider:  p.Provider,
		CreatedBy: p.CreatedBy,
		RpmLimit:  -1,
		TpmLimit:  -1,
	}
	if p.AgentID != nil {
		out.AgentId = p.AgentID.String()
	}
	if p.RPMLimit != nil {
		out.RpmLimit = *p.RPMLimit
	}
	if p.TPMLimit != nil {
		out.TpmLimit = *p.TPMLimit
	}
	return out
}
