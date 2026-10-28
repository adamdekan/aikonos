// broker/internal/broker/scim.go
// AG-16 SCIM 2.0 provisioning: the tenant-admin token RPCs (north) and the
// SCIM call the gateway forwards from its external surface (south). The SCIM
// protocol itself lives in internal/scim.
package broker

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/adamdekan/aikonos/broker/internal/apikey"
	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/ids"
	"github.com/adamdekan/aikonos/broker/internal/scim"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// scimStore is the SCIM state; *db.ScimRepo satisfies it.
type scimStore interface {
	scim.Store
	CreateToken(ctx context.Context, tenantID, hash, prefix, by string, replaces *uuid.UUID) (db.ScimToken, error)
	ListTokens(ctx context.Context, tenantID string) ([]db.ScimToken, error)
	RevokeToken(ctx context.Context, tenantID string, id uuid.UUID, by string) error
	ResolveToken(ctx context.Context, tenantID, hash string) (uuid.UUID, error)
}

// scimTokenPrefix marks SCIM bearer tokens apart from agent tk_ keys.
const scimTokenPrefix = "scim_"

func toProtoScimToken(t db.ScimToken) *brokerv1.ScimToken {
	out := &brokerv1.ScimToken{Id: t.ID.String(), Prefix: t.Prefix, CreatedBy: t.CreatedBy, CreatedAt: timestamppb.New(t.CreatedAt)}
	if t.LastUsedAt != nil {
		out.LastUsedAt = timestamppb.New(*t.LastUsedAt)
	}
	return out
}

// scimAdmin is the shared prologue of the token RPCs.
func (s *BrokerService) scimAdmin(ctx context.Context, reqTenant, reqUser string) (string, string, error) {
	if s.deps.Scim == nil {
		return "", "", status.Error(codes.FailedPrecondition, "scim provisioning not configured")
	}
	tenant, user, err := s.callerIdentity(ctx, reqTenant, reqUser)
	if err != nil {
		return "", "", err
	}
	if err := s.requireTenantAdmin(ctx, user); err != nil {
		return "", "", err
	}
	return tenant, user, nil
}

func (s *BrokerService) ListScimTokens(ctx context.Context, req *brokerv1.ListScimTokensRequest) (*brokerv1.ListScimTokensResponse, error) {
	tenant, _, err := s.scimAdmin(ctx, req.TenantId, req.UserId)
	if err != nil {
		return nil, err
	}
	rows, err := s.deps.Scim.ListTokens(ctx, tenant)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list scim tokens: %v", err)
	}
	mode, err := s.configFor().GetString(ctx, tenant, "provisioning.mode")
	if err != nil {
		return nil, status.Error(codes.Unavailable, "tenant settings could not be read")
	}
	out := &brokerv1.ListScimTokensResponse{Mode: mode}
	for _, t := range rows {
		out.Tokens = append(out.Tokens, toProtoScimToken(t))
	}
	return out, nil
}

// CreateScimToken mints a token and returns it once; only its peppered hash
// is stored. With replaces_id it rotates: the old token dies in the same step.
func (s *BrokerService) CreateScimToken(ctx context.Context, req *brokerv1.CreateScimTokenRequest) (*brokerv1.CreateScimTokenResponse, error) {
	tenant, user, err := s.scimAdmin(ctx, req.TenantId, req.UserId)
	if err != nil {
		return nil, err
	}
	if s.deps.ApiKeyPepper == "" {
		return nil, status.Error(codes.FailedPrecondition, "api key pepper not configured")
	}
	var replaces *uuid.UUID
	if req.ReplacesId != "" {
		id, err := uuid.Parse(req.ReplacesId)
		if err != nil {
			return nil, status.Error(codes.NotFound, "token not found")
		}
		replaces = &id
	}
	raw, err := apikey.Generate()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "generate token: %v", err)
	}
	raw = scimTokenPrefix + strings.TrimPrefix(raw, "tk_")
	hash, _ := apikey.PepperedHash(s.deps.ApiKeyPepper, raw)
	t, err := s.deps.Scim.CreateToken(ctx, tenant, hash, apikey.Prefix(raw), user, replaces)
	if errors.Is(err, db.ErrNotFound) {
		return nil, status.Error(codes.NotFound, "token not found")
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "create scim token: %v", err)
	}
	fields := map[string]any{"token_id": t.ID.String(), "prefix": t.Prefix}
	if replaces != nil {
		fields["replaces_id"] = replaces.String()
		s.emitScimTokenAudit(ctx, tenant, user, "aikonos.broker.scim.token.revoked", map[string]any{"token_id": replaces.String(), "rotated_to": t.ID.String()})
	}
	s.emitScimTokenAudit(ctx, tenant, user, "aikonos.broker.scim.token.created", fields)
	return &brokerv1.CreateScimTokenResponse{Token: toProtoScimToken(t), RawToken: raw}, nil
}

func (s *BrokerService) RevokeScimToken(ctx context.Context, req *brokerv1.RevokeScimTokenRequest) (*brokerv1.RevokeScimTokenResponse, error) {
	tenant, user, err := s.scimAdmin(ctx, req.TenantId, req.UserId)
	if err != nil {
		return nil, err
	}
	id, err := uuid.Parse(req.Id)
	if err != nil {
		return nil, status.Error(codes.NotFound, "token not found")
	}
	if err := s.deps.Scim.RevokeToken(ctx, tenant, id, user); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, status.Error(codes.NotFound, "token not found")
		}
		return nil, status.Errorf(codes.Internal, "revoke scim token: %v", err)
	}
	s.emitScimTokenAudit(ctx, tenant, user, "aikonos.broker.scim.token.revoked", map[string]any{"token_id": id.String()})
	return &brokerv1.RevokeScimTokenResponse{}, nil
}

func (s *BrokerService) emitScimTokenAudit(ctx context.Context, tenant, actor, eventType string, fields map[string]any) {
	emitScimAudit(ctx, s.deps, tenant, actor, eventType, "aikonos:tenant:"+tenant, auditv1.PolicyDecision_ALLOW, fields)
}

func emitScimAudit(ctx context.Context, d Deps, tenant, actor, eventType, resource string, decision auditv1.PolicyDecision, fields map[string]any) {
	if d.Audit == nil {
		return
	}
	c, _ := structpb.NewStruct(fields)
	if err := d.Audit.Emit(ctx, &auditv1.AuditEvent{
		EventId:     ids.EventID(),
		TenantId:    tenant,
		OccurredAt:  timestampNow(),
		ActorUserId: actor,
		EventType:   eventType,
		ResourceRef: resource,
		Decision:    decision,
		Context:     c,
	}); err != nil {
		audit.RecordEmitFailure(ctx, d.Logger, err, eventType)
	}
}

// ScimCall serves one SCIM request. Transport failures are gRPC errors; every
// SCIM outcome, including a bad token, is a SCIM status and body.
func (s *SandboxService) ScimCall(ctx context.Context, req *brokerv1.ScimCallRequest) (*brokerv1.ScimCallResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.scim.call")
	defer span.End()

	if err := s.requireGatewayPeer(ctx); err != nil {
		return nil, err
	}
	fail := func(code int, detail string) (*brokerv1.ScimCallResponse, error) {
		return &brokerv1.ScimCallResponse{Status: int32(code), Body: scim.ErrorBody(code, "", detail)}, nil
	}
	// Deactivation must land as a block: without the revocation view or the
	// pepper the endpoint is closed rather than half working.
	if s.deps.Scim == nil || s.deps.Revocation == nil || s.deps.ApiKeyPepper == "" {
		return fail(http.StatusServiceUnavailable, "scim provisioning not configured")
	}
	tenant := req.TenantId
	if _, err := uuid.Parse(tenant); err != nil {
		return fail(http.StatusUnauthorized, "invalid token")
	}
	hash, err := apikey.PepperedHashFromResolveHash(s.deps.ApiKeyPepper, req.TokenHashHex)
	if err != nil || req.TokenHashHex == "" {
		return fail(http.StatusUnauthorized, "invalid token")
	}
	tokenID, err := s.deps.Scim.ResolveToken(ctx, tenant, hash)
	if errors.Is(err, db.ErrNotFound) {
		emitScimAudit(ctx, s.deps, tenant, "scim", "aikonos.broker.scim.auth.denied", "aikonos:tenant:"+tenant,
			auditv1.PolicyDecision_DENY, map[string]any{"reason": "invalid_token", "method": req.Method, "path": req.Path})
		return fail(http.StatusUnauthorized, "invalid token")
	}
	if err != nil {
		return fail(http.StatusServiceUnavailable, "token could not be checked")
	}
	actor := "scim-token:" + tokenID.String()
	mode, err := s.cfg().GetString(ctx, tenant, "provisioning.mode")
	if err != nil {
		return fail(http.StatusServiceUnavailable, "tenant settings could not be read")
	}
	if mode != "scim" {
		emitScimAudit(ctx, s.deps, tenant, actor, "aikonos.broker.scim.auth.denied", "aikonos:tenant:"+tenant,
			auditv1.PolicyDecision_DENY, map[string]any{"reason": "mode_" + mode, "method": req.Method, "path": req.Path})
		return fail(http.StatusForbidden, "SCIM is off for this tenant: set provisioning.mode to scim")
	}
	h := &scim.Handler{Store: s.deps.Scim, Tuples: s.deps.Policy, Blocks: s.deps.Revocation, Audit: s.deps.Audit, Logger: s.deps.Logger}
	code, body := h.Serve(ctx, scim.Request{
		Tenant: tenant, Actor: actor,
		Method: req.Method, Path: req.Path, Query: req.Query, Body: req.Body,
	})
	return &brokerv1.ScimCallResponse{Status: int32(code), Body: body}, nil
}
