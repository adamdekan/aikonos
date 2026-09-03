// broker/internal/broker/api_keys.go
// RPC handlers for per-agent API key management.
//
// North-bound (BrokerService): MintAgentApiKey, ListAgentApiKeys, RevokeAgentApiKey
//
//	— tenant-admin gated, callerIdentity bound.
//
// South-bound (SandboxService): ResolveAgentApiKey
//
//	— SPIFFE gateway-peer gated; returns valid=false on miss/revoke/cross-tenant.
//
// Pepper invariant: both mint and resolve return FailedPrecondition when the
// pepper is empty — there is no unpeppered fallback.
package broker

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/adamdekan/aikonos/broker/internal/apikey"
	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/gatewaygrant"
	"github.com/adamdekan/aikonos/broker/internal/ids"
	"github.com/adamdekan/aikonos/broker/internal/policy"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// ApiKeyStore is the persistence seam for API key operations. *db.AgentApiKeyRepo
// satisfies this interface; tests inject fakeApiKeyStore.
type ApiKeyStore interface {
	Create(ctx context.Context, k *db.StoredApiKey) error
	ListByAgent(ctx context.Context, tenantID, agentID string) ([]*db.StoredApiKey, error)
	Revoke(ctx context.Context, tenantID, agentID, keyID string) error
	Resolve(ctx context.Context, keyHash string) (*db.StoredApiKey, error)
}

func (s *BrokerService) apiKeysEnabled() bool { return s.deps.ApiKeys != nil }

// ── BrokerService (north-bound) ───────────────────────────────────────────────

func (s *BrokerService) MintAgentApiKey(ctx context.Context, req *brokerv1.MintAgentApiKeyRequest) (*brokerv1.MintAgentApiKeyResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.apikeys.mint")
	defer span.End()

	if !s.apiKeysEnabled() {
		return nil, status.Error(codes.FailedPrecondition, "api keys disabled")
	}
	if s.deps.ApiKeyPepper == "" {
		return nil, status.Error(codes.FailedPrecondition, "api key pepper not configured")
	}

	tenant, user, err := s.callerIdentity(ctx, req.TenantId, req.UserId)
	if err != nil {
		return nil, err
	}
	if err := s.requireTenantAdmin(ctx, user); err != nil {
		return nil, err
	}
	if req.AgentId == "" {
		return nil, status.Error(codes.InvalidArgument, "agent_id required")
	}
	// agent_id backs a uuid DB column; validate at the boundary before the
	// store read, which would otherwise 22P02 into codes.Internal on a
	// syntactically invalid id.
	agentUUID, err := uuid.Parse(req.AgentId)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid agent_id: %v", err)
	}

	// Verify the agent exists in the tenant.
	if _, err := s.deps.Agents.Get(ctx, tenant, req.AgentId); err != nil {
		if errors.Is(err, db.ErrAgentNotFound) {
			return nil, status.Errorf(codes.NotFound, "agent %s not found", req.AgentId)
		}
		return nil, status.Errorf(codes.Internal, "get agent: %v", err)
	}

	raw, err := apikey.Generate()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "generate key: %v", err)
	}
	hash, err := apikey.PepperedHash(s.deps.ApiKeyPepper, raw)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "hash key: %v", err)
	}

	tenantUUID, err := uuid.Parse(tenant)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid tenant_id: %v", err)
	}

	k := &db.StoredApiKey{
		ID:        uuid.New(),
		TenantID:  tenantUUID,
		AgentID:   agentUUID,
		KeyHash:   hash,
		KeyPrefix: apikey.Prefix(raw),
		Label:     strings.TrimSpace(req.Label),
		CreatedBy: user,
		CreatedAt: time.Now().UTC(),
	}
	// Provision agent:<id> usable_by user:svc-<id> in FGA BEFORE persisting the
	// key: the key lookup is DB-side and FGA-independent, so a missing tuple on a
	// persisted key would silently mint an unusable key. Idempotent — a duplicate
	// write is benign; a stale tuple after revoke-last is an intentional
	// documented non-guarantee.
	principal := "svc-" + req.AgentId
	if err := s.deps.Policy.WriteRelations(ctx,
		policy.Relation{
			User:     "user:" + principal,
			Relation: "usable_by",
			Object:   "agent:" + req.AgentId,
		},
	); err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "provision api key authorization: %v", err)
	}

	if err := s.deps.ApiKeys.Create(ctx, k); err != nil {
		return nil, status.Errorf(codes.Internal, "store key: %v", err)
	}

	s.deps.Logger.Info("agent api key minted",
		zap.String("key_id", k.ID.String()),
		zap.String("agent_id", req.AgentId),
		zap.String("actor", user),
		// raw key deliberately not logged
	)
	if err := s.deps.Audit.Emit(ctx, &auditv1.AuditEvent{
		EventId:     ids.EventID(),
		TraceId:     span.SpanContext().TraceID().String(),
		TenantId:    tenant,
		OccurredAt:  timestampNow(),
		ActorUserId: user,
		EventType:   "aikonos.broker.apikey.minted",
		ResourceRef: "agent:" + req.AgentId,
		Decision:    auditv1.PolicyDecision_ALLOW,
	}); err != nil {
		audit.RecordEmitFailure(ctx, s.deps.Logger, err, "aikonos.broker.apikey.minted")
	}

	return &brokerv1.MintAgentApiKeyResponse{
		Key: toProtoApiKey(k),
		// RawKey is returned exactly once; never persisted or logged above.
		RawKey: raw,
	}, nil
}

func (s *BrokerService) ListAgentApiKeys(ctx context.Context, req *brokerv1.ListAgentApiKeysRequest) (*brokerv1.ListAgentApiKeysResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.apikeys.list")
	defer span.End()

	if !s.apiKeysEnabled() {
		return nil, status.Error(codes.FailedPrecondition, "api keys disabled")
	}
	tenant, user, err := s.callerIdentity(ctx, req.TenantId, req.UserId)
	if err != nil {
		return nil, err
	}
	if err := s.requireTenantAdmin(ctx, user); err != nil {
		return nil, err
	}
	if req.AgentId == "" {
		return nil, status.Error(codes.InvalidArgument, "agent_id required")
	}
	// agent_id backs a uuid DB column; validate at the boundary before the
	// store read, which would otherwise 22P02 into codes.Internal on a
	// syntactically invalid id.
	if _, err := uuid.Parse(req.AgentId); err != nil {
		return nil, status.Error(codes.NotFound, "agent not found")
	}

	rows, err := s.deps.ApiKeys.ListByAgent(ctx, tenant, req.AgentId)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list keys: %v", err)
	}
	out := make([]*brokerv1.AgentApiKey, 0, len(rows))
	for _, k := range rows {
		out = append(out, toProtoApiKey(k))
	}
	return &brokerv1.ListAgentApiKeysResponse{Keys: out}, nil
}

func (s *BrokerService) RevokeAgentApiKey(ctx context.Context, req *brokerv1.RevokeAgentApiKeyRequest) (*brokerv1.RevokeAgentApiKeyResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.apikeys.revoke")
	defer span.End()

	if !s.apiKeysEnabled() {
		return nil, status.Error(codes.FailedPrecondition, "api keys disabled")
	}
	tenant, user, err := s.callerIdentity(ctx, req.TenantId, req.UserId)
	if err != nil {
		return nil, err
	}
	if err := s.requireTenantAdmin(ctx, user); err != nil {
		return nil, err
	}
	if req.AgentId == "" {
		return nil, status.Error(codes.InvalidArgument, "agent_id required")
	}
	if req.KeyId == "" {
		return nil, status.Error(codes.InvalidArgument, "key_id required")
	}
	// agent_id backs a uuid DB column; validate at the boundary before the
	// store read, which would otherwise 22P02 into codes.Internal on a
	// syntactically invalid id.
	if _, err := uuid.Parse(req.AgentId); err != nil {
		return nil, status.Error(codes.NotFound, "agent not found")
	}

	if err := s.deps.ApiKeys.Revoke(ctx, tenant, req.AgentId, req.KeyId); err != nil {
		if errors.Is(err, db.ErrApiKeyNotFound) {
			return nil, status.Errorf(codes.NotFound, "key %s not found", req.KeyId)
		}
		return nil, status.Errorf(codes.Internal, "revoke key: %v", err)
	}

	s.deps.Logger.Info("agent api key revoked",
		zap.String("key_id", req.KeyId),
		zap.String("agent_id", req.AgentId),
		zap.String("actor", user),
	)
	if err := s.deps.Audit.Emit(ctx, &auditv1.AuditEvent{
		EventId:     ids.EventID(),
		TraceId:     span.SpanContext().TraceID().String(),
		TenantId:    tenant,
		OccurredAt:  timestampNow(),
		ActorUserId: user,
		EventType:   "aikonos.broker.apikey.revoked",
		ResourceRef: "agent:" + req.AgentId,
		Decision:    auditv1.PolicyDecision_ALLOW,
	}); err != nil {
		audit.RecordEmitFailure(ctx, s.deps.Logger, err, "aikonos.broker.apikey.revoked")
	}
	return &brokerv1.RevokeAgentApiKeyResponse{Success: true}, nil
}

// ── SandboxService (south-bound) ──────────────────────────────────────────────

func (s *SandboxService) ResolveAgentApiKey(ctx context.Context, req *brokerv1.ResolveAgentApiKeyRequest) (*brokerv1.ResolveAgentApiKeyResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.apikeys.resolve")
	defer span.End()

	if err := s.requireGatewayPeer(ctx); err != nil {
		return nil, err
	}
	if s.deps.ApiKeys == nil {
		return nil, status.Error(codes.FailedPrecondition, "api keys disabled")
	}
	if s.deps.ApiKeyPepper == "" {
		return nil, status.Error(codes.FailedPrecondition, "api key pepper not configured")
	}

	// Compute the peppered hash from the sha256 the gateway sent.
	storedHash, err := apikey.PepperedHashFromResolveHash(s.deps.ApiKeyPepper, req.KeyHashHex)
	if err != nil {
		// Malformed hash hex → treat as not-found (not an internal error).
		return &brokerv1.ResolveAgentApiKeyResponse{Valid: false}, nil
	}

	k, err := s.deps.ApiKeys.Resolve(ctx, storedHash)
	if err != nil {
		if errors.Is(err, db.ErrApiKeyNotFound) {
			return &brokerv1.ResolveAgentApiKeyResponse{Valid: false}, nil
		}
		return nil, status.Errorf(codes.Internal, "resolve key: %v", err)
	}

	// Cross-tenant guard: the resolved key's tenant must match the request.
	if req.TenantId != "" && k.TenantID.String() != req.TenantId {
		return &brokerv1.ResolveAgentApiKeyResponse{Valid: false}, nil
	}

	ownerGrant, mintErr := gatewaygrant.Mint(s.deps.GatewayGrantKey, k.TenantID.String(), "svc-"+k.AgentID.String(), s.deps.GatewayGrantTTL)
	if mintErr != nil {
		// Cannot hand the gateway a usable resolution without a grant — fail closed.
		s.deps.Logger.Warn("ResolveAgentApiKey: mint owner grant failed",
			zap.String("agent_id", k.AgentID.String()),
			zap.Error(mintErr),
		)
		return &brokerv1.ResolveAgentApiKeyResponse{Valid: false}, nil
	}

	return &brokerv1.ResolveAgentApiKeyResponse{
		Valid:      true,
		AgentId:    k.AgentID.String(),
		TenantId:   k.TenantID.String(),
		Principal:  "svc-" + k.AgentID.String(),
		OwnerGrant: ownerGrant,
	}, nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

func toProtoApiKey(k *db.StoredApiKey) *brokerv1.AgentApiKey {
	pk := &brokerv1.AgentApiKey{
		Id:        k.ID.String(),
		KeyPrefix: k.KeyPrefix,
		Label:     k.Label,
		CreatedBy: k.CreatedBy,
		CreatedAt: timestamppb.New(k.CreatedAt),
	}
	if k.LastUsedAt != nil {
		pk.LastUsedAt = timestamppb.New(*k.LastUsedAt)
	}
	return pk
}
