// broker/internal/broker/api_keys.go
// RPC handlers for per-agent API key management.
//
// North-bound (BrokerService): MintAgentApiKey, ListAgentApiKeys, RevokeAgentApiKey
//
//	— tenant-admin gated, callerIdentity bound.
//
// South-bound (SandboxService): ResolveAgentApiKey
//
//	— SPIFFE gateway-peer gated; returns valid=false on miss/revoke/expiry/cross-tenant.
//	  With client_assertion set it verifies a key-pair assertion instead
//	  (agent_public_keys.go, AG-05).
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
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/adamdekan/aikonos/broker/internal/apikey"
	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/auth"
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

	// AG-15: every key expires, at most api_keys.max_ttl_days after minting.
	maxTTL, err := s.configFor().GetInt(ctx, tenant, "api_keys.max_ttl_days")
	if err != nil {
		return nil, status.Error(codes.Unavailable, "tenant settings could not be read")
	}
	if req.TtlDays < 1 || int(req.TtlDays) > maxTTL {
		return nil, status.Errorf(codes.InvalidArgument, "ttl_days must be between 1 and %d", maxTTL)
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

	now := time.Now().UTC()
	k := &db.StoredApiKey{
		ID:        uuid.New(),
		TenantID:  tenantUUID,
		AgentID:   agentUUID,
		KeyHash:   hash,
		KeyPrefix: apikey.Prefix(raw),
		Label:     strings.TrimSpace(req.Label),
		CreatedBy: user,
		CreatedAt: now,
		ExpiresAt: now.AddDate(0, 0, int(req.TtlDays)),
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
	mintedCtx, _ := structpb.NewStruct(map[string]any{
		"key_id": k.ID.String(), "expires_at": k.ExpiresAt.Format(time.RFC3339),
	})
	if err := s.deps.Audit.Emit(ctx, &auditv1.AuditEvent{
		EventId:     ids.EventID(),
		TraceId:     span.SpanContext().TraceID().String(),
		TenantId:    tenant,
		OccurredAt:  timestampNow(),
		ActorUserId: user,
		EventType:   "aikonos.broker.apikey.minted",
		ResourceRef: "agent:" + req.AgentId,
		Decision:    auditv1.PolicyDecision_ALLOW,
		Context:     mintedCtx,
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
	maxTTL, err := s.configFor().GetInt(ctx, tenant, "api_keys.max_ttl_days")
	if err != nil {
		return nil, status.Error(codes.Unavailable, "tenant settings could not be read")
	}
	return &brokerv1.ListAgentApiKeysResponse{Keys: out, MaxTtlDays: int32(maxTTL)}, nil
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
	revokedCtx, _ := structpb.NewStruct(map[string]any{"key_id": req.KeyId})
	if err := s.deps.Audit.Emit(ctx, &auditv1.AuditEvent{
		EventId:     ids.EventID(),
		TraceId:     span.SpanContext().TraceID().String(),
		TenantId:    tenant,
		OccurredAt:  timestampNow(),
		ActorUserId: user,
		EventType:   "aikonos.broker.apikey.revoked",
		ResourceRef: "agent:" + req.AgentId,
		Decision:    auditv1.PolicyDecision_ALLOW,
		Context:     revokedCtx,
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
	if req.ClientAssertion != "" { // AG-05: key pair instead of a tk_ key
		return s.resolveClientAssertion(ctx, req)
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

	// AG-15: an expired key is refused exactly like a revoked one. The DB
	// lookup already skips expired rows; this guards a store that does not.
	if !time.Now().Before(k.ExpiresAt) {
		return &brokerv1.ResolveAgentApiKeyResponse{Valid: false}, nil
	}

	// Cross-tenant guard: the resolved key's tenant must match the request.
	if req.TenantId != "" && k.TenantID.String() != req.TenantId {
		return &brokerv1.ResolveAgentApiKeyResponse{Valid: false}, nil
	}
	return s.resolveAgentCredential(ctx, k, req.UserAssertion)
}

// resolveAgentCredential runs the agent checks shared by tk_ keys and client
// assertions (AG-05) on a verified credential and mints the owner grant.
func (s *SandboxService) resolveAgentCredential(ctx context.Context, k *db.StoredApiKey, userAssertion string) (*brokerv1.ResolveAgentApiKeyResponse, error) {
	// AG-06: a key minted before the agent's revoke-all is void, like a
	// revoked one. A state read failure refuses rather than resolves.
	if st, rerr := s.deps.Revocation.Agent(ctx, k.TenantID.String(), k.AgentID.String()); rerr != nil {
		return nil, status.Error(codes.Unavailable, "access state could not be read; retry")
	} else if st.Revoked(k.CreatedAt) {
		s.emitApiKeyDenied(ctx, k, "sessions_revoked", "")
		return &brokerv1.ResolveAgentApiKeyResponse{Valid: false}, nil
	} else if r := st.AgentRefusal(); r != "" {
		// AG-03: the key is valid but its agent may not run; say why.
		s.emitApiKeyDenied(ctx, k, r, "")
		return nil, agentRefusedErr(r)
	}

	// AG-17 S6: an optional user assertion names the person the agent acts
	// for. A bad one refuses the key outright, never falls back to svc-.
	var subject string
	if userAssertion != "" {
		var reason string
		if subject, reason = s.verifyUserAssertion(ctx, k, userAssertion); reason != "" {
			s.emitApiKeyDenied(ctx, k, reason, "")
			if reason == "assertion_state_unavailable" {
				return nil, status.Error(codes.Unavailable, "access state could not be read; retry")
			}
			// The key itself is valid, so say why (unknown keys stay Valid:false).
			return nil, status.Error(codes.PermissionDenied, "user assertion rejected: "+reason)
		}
	}

	// AG-17 S5: a key without a user assertion runs subject-less, so its agent
	// must be a service agent. Every pre-S5 agent defaults to false, so
	// identity.obo=audit logs the gap and proceeds (a DENY row per successful
	// call would flood the audit chain); enforce refuses with a DENY event.
	// ponytail: dedupe the audit-mode log per key if it gets noisy.
	if s.deps.Agents != nil {
		a, aerr := s.deps.Agents.Get(ctx, k.TenantID.String(), k.AgentID.String())
		if errors.Is(aerr, db.ErrAgentNotFound) {
			return &brokerv1.ResolveAgentApiKeyResponse{Valid: false}, nil
		}
		if aerr != nil {
			return nil, status.Errorf(codes.Internal, "get agent: %v", aerr)
		}
		if !a.ServiceAgent && subject == "" {
			mode, cerr := s.cfg().GetString(ctx, k.TenantID.String(), "identity.obo")
			if cerr != nil {
				s.emitApiKeyDenied(ctx, k, "settings_check_failed", "")
				return nil, status.Error(codes.Unavailable, "tenant settings could not be read")
			}
			if mode == "enforce" {
				s.emitApiKeyDenied(ctx, k, "service_agent_required", mode)
				return nil, status.Error(codes.PermissionDenied, "identity.obo=enforce: agent is not a service agent; send a user assertion")
			}
			s.deps.Logger.Warn("ResolveAgentApiKey: key for non-service agent (identity.obo=audit)",
				zap.String("agent_id", k.AgentID.String()), zap.String("key_id", k.ID.String()))
		}
	}

	ownerGrant, mintErr := gatewaygrant.Mint(s.deps.GatewayGrantKey, gatewaygrant.Grant{
		// Empty subject (no assertion): Grant.Owner() yields the svc-<agent> owner.
		TenantID: k.TenantID.String(), Subject: subject, Actor: k.AgentID.String(), Kind: gatewaygrant.KindExternal, GrantID: k.ID.String(),
	}, s.deps.GatewayGrantTTL)
	if mintErr != nil {
		// Cannot hand the gateway a usable resolution without a grant — fail closed.
		s.deps.Logger.Warn("ResolveAgentApiKey: mint owner grant failed",
			zap.String("agent_id", k.AgentID.String()),
			zap.Error(mintErr),
		)
		return &brokerv1.ResolveAgentApiKeyResponse{Valid: false}, nil
	}

	principal := "svc-" + k.AgentID.String()
	if subject != "" {
		principal = subject
	}
	return &brokerv1.ResolveAgentApiKeyResponse{
		Valid:      true,
		AgentId:    k.AgentID.String(),
		TenantId:   k.TenantID.String(),
		Principal:  principal,
		OwnerGrant: ownerGrant,
	}, nil
}

// verifyUserAssertion validates an external caller's user assertion (AG-17 S6)
// and returns its subject, or a denial reason. The north OIDC validator checks
// signature, issuer and audience and refuses svc- subjects; the subject must
// also hold can_use on the key's agent. Every failure path fails closed.
func (s *SandboxService) verifyUserAssertion(ctx context.Context, k *db.StoredApiKey, raw string) (subject, reason string) {
	if s.deps.UserAssertions == nil || s.deps.Policy == nil {
		return "", "assertion_unavailable"
	}
	id, err := s.deps.UserAssertions.Validate(ctx, raw)
	var svcErr *auth.SvcSubjectError
	if errors.As(err, &svcErr) {
		return "", "assertion_svc_subject"
	}
	if err != nil {
		return "", "assertion_invalid"
	}
	subject = id.PrincipalID() // same principal the north API uses for FGA
	if subject == "" || strings.HasPrefix(subject, "svc-") {
		return "", "assertion_svc_subject"
	}
	if id.TenantID != k.TenantID.String() { // no tenant claim refused too, as north does
		return "", "assertion_tenant_mismatch"
	}
	// AG-06: the north interceptor's check. The grant minted below is stamped
	// now, so the assertion's own iat is the last place a revoke-all can bite.
	st, err := s.deps.Revocation.User(ctx, k.TenantID.String(), subject)
	if err != nil {
		return "", "assertion_state_unavailable"
	}
	if st.Revoked(id.IssuedAt) {
		return "", "assertion_revoked"
	}
	ok, err := s.deps.Policy.CheckFGA(ctx, "user:"+subject, "can_use", "agent:"+k.AgentID.String())
	if err != nil || !ok {
		return "", "assertion_no_can_use"
	}
	return subject, ""
}

// ── helpers ───────────────────────────────────────────────────────────────────

// emitApiKeyDenied records a DENY for a refused key resolution (fire-and-forget).
func (s *SandboxService) emitApiKeyDenied(ctx context.Context, k *db.StoredApiKey, reason, mode string) {
	if s.deps.Audit == nil {
		return
	}
	c, _ := structpb.NewStruct(map[string]any{
		"reason": reason, "mode": mode, "agent_id": k.AgentID.String(), "key_id": k.ID.String(),
	})
	if err := s.deps.Audit.Emit(ctx, &auditv1.AuditEvent{
		EventId:      ids.EventID(),
		TenantId:     k.TenantID.String(),
		OccurredAt:   timestampNow(),
		ActorUserId:  "svc-" + k.AgentID.String(),
		ActorAgentId: k.AgentID.String(),
		EventType:    "aikonos.broker.apikey.denied",
		ResourceRef:  "agent:" + k.AgentID.String(),
		Decision:     auditv1.PolicyDecision_DENY,
		Context:      c,
	}); err != nil {
		audit.RecordEmitFailure(ctx, s.deps.Logger, err, "aikonos.broker.apikey.denied")
	}
}

func toProtoApiKey(k *db.StoredApiKey) *brokerv1.AgentApiKey {
	pk := &brokerv1.AgentApiKey{
		Id:        k.ID.String(),
		KeyPrefix: k.KeyPrefix,
		Label:     k.Label,
		CreatedBy: k.CreatedBy,
		CreatedAt: timestamppb.New(k.CreatedAt),
		ExpiresAt: timestamppb.New(k.ExpiresAt),
	}
	if k.LastUsedAt != nil {
		pk.LastUsedAt = timestamppb.New(*k.LastUsedAt)
	}
	return pk
}
