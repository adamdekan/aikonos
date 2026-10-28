// broker/internal/broker/agent_public_keys.go
// AG-05: key-pair authentication for external agents.
//
// North-bound (BrokerService): RegisterAgentPublicKey, ListAgentPublicKeys,
// RetireAgentPublicKey — tenant-admin gated, like the tk_ key RPCs.
//
// South-bound: ResolveAgentApiKey with client_assertion set verifies a signed
// JWT client assertion (RFC 7523) instead of a tk_ key hash and then runs the
// same agent checks (AG-06 revocation, AG-03 status, AG-17 user assertion and
// service-agent rule) and mints the same owner grant.
package broker

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/ids"
	"github.com/adamdekan/aikonos/broker/internal/policy"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// PublicKeyStore is the persistence seam for agent public keys;
// *db.AgentPublicKeyRepo satisfies it.
type PublicKeyStore interface {
	Create(ctx context.Context, k *db.StoredPublicKey) error
	ListByAgent(ctx context.Context, tenantID, agentID string) ([]*db.StoredPublicKey, error)
	Retire(ctx context.Context, tenantID, agentID, keyID string) error
	Get(ctx context.Context, tenantID, agentID, kid string) (*db.StoredPublicKey, error)
	TouchLastUsed(tenantID, keyID string)
}

const (
	// assertionMaxAhead bounds exp: an assertion lives at most five minutes.
	assertionMaxAhead = 5 * time.Minute
	// assertionLeeway absorbs clock skew between the agent and the broker.
	assertionLeeway = 30 * time.Second
)

// ── BrokerService (north-bound) ───────────────────────────────────────────────

// publicKeyAdmin runs the checks every public key admin RPC shares and returns
// the caller's tenant and user.
func (s *BrokerService) publicKeyAdmin(ctx context.Context, tenantID, userID, agentID string) (string, string, error) {
	if s.deps.PublicKeys == nil {
		return "", "", status.Error(codes.FailedPrecondition, "agent public keys disabled")
	}
	tenant, user, err := s.callerIdentity(ctx, tenantID, userID)
	if err != nil {
		return "", "", err
	}
	if err := s.requireTenantAdmin(ctx, user); err != nil {
		return "", "", err
	}
	if _, err := uuid.Parse(agentID); err != nil {
		return "", "", status.Error(codes.InvalidArgument, "agent_id must be a UUID")
	}
	return tenant, user, nil
}

func (s *BrokerService) RegisterAgentPublicKey(ctx context.Context, req *brokerv1.RegisterAgentPublicKeyRequest) (*brokerv1.RegisterAgentPublicKeyResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.publickeys.register")
	defer span.End()

	tenant, user, err := s.publicKeyAdmin(ctx, req.TenantId, req.UserId, req.AgentId)
	if err != nil {
		return nil, err
	}
	// Same lifetime ceiling as tk_ keys (AG-15): one setting for agent credentials.
	maxTTL, err := s.configFor().GetInt(ctx, tenant, "api_keys.max_ttl_days")
	if err != nil {
		return nil, status.Error(codes.Unavailable, "tenant settings could not be read")
	}
	if req.TtlDays < 1 || int(req.TtlDays) > maxTTL {
		return nil, status.Errorf(codes.InvalidArgument, "ttl_days must be between 1 and %d", maxTTL)
	}
	pemKey, alg, kid, err := parseAgentPublicKey(req.PublicKey, req.Kid)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "public_key: %v", err)
	}
	if _, err := s.deps.Agents.Get(ctx, tenant, req.AgentId); err != nil {
		if errors.Is(err, db.ErrAgentNotFound) {
			return nil, status.Errorf(codes.NotFound, "agent %s not found", req.AgentId)
		}
		return nil, status.Errorf(codes.Internal, "get agent: %v", err)
	}
	// The svc- principal needs usable_by on the agent, as for a tk_ key.
	if err := s.deps.Policy.WriteRelations(ctx, policy.Relation{
		User: "user:svc-" + req.AgentId, Relation: "usable_by", Object: "agent:" + req.AgentId,
	}); err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "provision key authorization: %v", err)
	}

	now := time.Now().UTC()
	k := &db.StoredPublicKey{
		ID:        uuid.New(),
		TenantID:  uuid.MustParse(tenant),
		AgentID:   uuid.MustParse(req.AgentId),
		Kid:       kid,
		Alg:       alg,
		PublicKey: pemKey,
		Status:    "active",
		CreatedBy: user,
		CreatedAt: now,
		ExpiresAt: now.AddDate(0, 0, int(req.TtlDays)),
	}
	if err := s.deps.PublicKeys.Create(ctx, k); err != nil {
		if errors.Is(err, db.ErrPublicKeyExists) {
			return nil, status.Errorf(codes.AlreadyExists, "kid %q already registered for this agent", kid)
		}
		return nil, status.Errorf(codes.Internal, "store key: %v", err)
	}
	s.emitPublicKeyAdmin(ctx, span.SpanContext().TraceID().String(), tenant, user, "aikonos.broker.agentkey.registered", k)
	return &brokerv1.RegisterAgentPublicKeyResponse{Key: toProtoPublicKey(k)}, nil
}

func (s *BrokerService) ListAgentPublicKeys(ctx context.Context, req *brokerv1.ListAgentPublicKeysRequest) (*brokerv1.ListAgentPublicKeysResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.publickeys.list")
	defer span.End()

	tenant, _, err := s.publicKeyAdmin(ctx, req.TenantId, req.UserId, req.AgentId)
	if err != nil {
		return nil, err
	}
	rows, err := s.deps.PublicKeys.ListByAgent(ctx, tenant, req.AgentId)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list keys: %v", err)
	}
	maxTTL, err := s.configFor().GetInt(ctx, tenant, "api_keys.max_ttl_days")
	if err != nil {
		return nil, status.Error(codes.Unavailable, "tenant settings could not be read")
	}
	out := make([]*brokerv1.AgentPublicKey, 0, len(rows))
	for _, k := range rows {
		out = append(out, toProtoPublicKey(k))
	}
	return &brokerv1.ListAgentPublicKeysResponse{Keys: out, MaxTtlDays: int32(maxTTL)}, nil
}

func (s *BrokerService) RetireAgentPublicKey(ctx context.Context, req *brokerv1.RetireAgentPublicKeyRequest) (*brokerv1.RetireAgentPublicKeyResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.publickeys.retire")
	defer span.End()

	tenant, user, err := s.publicKeyAdmin(ctx, req.TenantId, req.UserId, req.AgentId)
	if err != nil {
		return nil, err
	}
	if _, err := uuid.Parse(req.KeyId); err != nil {
		return nil, status.Error(codes.NotFound, "key not found")
	}
	if err := s.deps.PublicKeys.Retire(ctx, tenant, req.AgentId, req.KeyId); err != nil {
		if errors.Is(err, db.ErrPublicKeyNotFound) {
			return nil, status.Errorf(codes.NotFound, "key %s not found", req.KeyId)
		}
		return nil, status.Errorf(codes.Internal, "retire key: %v", err)
	}
	s.emitPublicKeyAdmin(ctx, span.SpanContext().TraceID().String(), tenant, user, "aikonos.broker.agentkey.retired",
		&db.StoredPublicKey{ID: uuid.MustParse(req.KeyId), AgentID: uuid.MustParse(req.AgentId)})
	return &brokerv1.RetireAgentPublicKeyResponse{Success: true}, nil
}

func (s *BrokerService) emitPublicKeyAdmin(ctx context.Context, traceID, tenant, user, eventType string, k *db.StoredPublicKey) {
	s.deps.Logger.Info("agent public key change", zap.String("event", eventType),
		zap.String("key_id", k.ID.String()), zap.String("agent_id", k.AgentID.String()), zap.String("actor", user))
	fields := map[string]any{"key_id": k.ID.String()}
	if k.Kid != "" {
		fields["kid"], fields["alg"], fields["expires_at"] = k.Kid, k.Alg, k.ExpiresAt.Format(time.RFC3339)
	}
	c, _ := structpb.NewStruct(fields)
	if err := s.deps.Audit.Emit(ctx, &auditv1.AuditEvent{
		EventId:     ids.EventID(),
		TraceId:     traceID,
		TenantId:    tenant,
		OccurredAt:  timestampNow(),
		ActorUserId: user,
		EventType:   eventType,
		ResourceRef: "agent:" + k.AgentID.String(),
		Decision:    auditv1.PolicyDecision_ALLOW,
		Context:     c,
	}); err != nil {
		audit.RecordEmitFailure(ctx, s.deps.Logger, err, eventType)
	}
}

// parseAgentPublicKey accepts an SPKI PEM or a public JWK, allows only P-256
// (ES256) and Ed25519 (EdDSA) keys, and returns the key as SPKI PEM with its
// alg and kid (given, else the JWK kid, else the RFC 7638 thumbprint).
func parseAgentPublicKey(in, kid string) (pemKey, alg, outKid string, err error) {
	in = strings.TrimSpace(in)
	var key any
	if strings.HasPrefix(in, "{") {
		var jwk jose.JSONWebKey
		if err := json.Unmarshal([]byte(in), &jwk); err != nil {
			return "", "", "", fmt.Errorf("invalid JWK: %v", err)
		}
		if !jwk.IsPublic() {
			return "", "", "", errors.New("private key material is not accepted; paste the public key only")
		}
		key, alg = jwk.Key, jwk.Algorithm
		if kid == "" {
			kid = jwk.KeyID
		}
	} else {
		block, _ := pem.Decode([]byte(in))
		if block == nil || block.Type != "PUBLIC KEY" {
			return "", "", "", errors.New("want a PEM \"PUBLIC KEY\" block or a JWK")
		}
		if key, err = x509.ParsePKIXPublicKey(block.Bytes); err != nil {
			return "", "", "", fmt.Errorf("invalid public key: %v", err)
		}
	}
	var keyAlg string
	switch k := key.(type) {
	case *ecdsa.PublicKey:
		if k.Curve == elliptic.P256() {
			keyAlg = string(jose.ES256)
		}
	case ed25519.PublicKey:
		keyAlg = string(jose.EdDSA)
	}
	if keyAlg == "" {
		return "", "", "", errors.New("only P-256 (ES256) and Ed25519 (EdDSA) keys are supported")
	}
	if alg != "" && alg != keyAlg {
		return "", "", "", fmt.Errorf("JWK alg %q does not match the %s key", alg, keyAlg)
	}
	der, err := x509.MarshalPKIXPublicKey(key)
	if err != nil {
		return "", "", "", fmt.Errorf("encode key: %v", err)
	}
	if kid == "" {
		tp, err := (&jose.JSONWebKey{Key: key}).Thumbprint(crypto.SHA256)
		if err != nil {
			return "", "", "", fmt.Errorf("thumbprint: %v", err)
		}
		kid = base64.RawURLEncoding.EncodeToString(tp)
	}
	if len(kid) > 128 || strings.ContainsFunc(kid, func(r rune) bool { return r <= ' ' || r > '~' }) {
		return "", "", "", errors.New("kid must be at most 128 printable ASCII characters")
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})), keyAlg, kid, nil
}

func toProtoPublicKey(k *db.StoredPublicKey) *brokerv1.AgentPublicKey {
	pk := &brokerv1.AgentPublicKey{
		Id:        k.ID.String(),
		Kid:       k.Kid,
		Alg:       k.Alg,
		CreatedBy: k.CreatedBy,
		CreatedAt: timestamppb.New(k.CreatedAt),
		ExpiresAt: timestamppb.New(k.ExpiresAt),
	}
	if k.LastUsedAt != nil {
		pk.LastUsedAt = timestamppb.New(*k.LastUsedAt)
	}
	return pk
}

// ── SandboxService (south-bound) ──────────────────────────────────────────────

// resolveClientAssertion verifies an RFC 7523 client assertion and, when it
// holds, resolves its agent exactly like a tk_ key. Every refusal answers
// Valid:false (REST 401) without saying which check failed; once the issuer is
// a known agent of the tenant, the reason goes to the audit log.
func (s *SandboxService) resolveClientAssertion(ctx context.Context, req *brokerv1.ResolveAgentApiKeyRequest) (*brokerv1.ResolveAgentApiKeyResponse, error) {
	invalid := &brokerv1.ResolveAgentApiKeyResponse{Valid: false}
	if s.deps.PublicKeys == nil || s.deps.Agents == nil {
		return nil, status.Error(codes.FailedPrecondition, "agent public keys disabled")
	}
	// The assertion names no tenant; only the gateway's configured one is
	// searched, and an unconfigured audience accepts nothing.
	if req.TenantId == "" || req.Audience == "" {
		return invalid, nil
	}
	tok, err := jwt.ParseSigned(req.ClientAssertion, []jose.SignatureAlgorithm{jose.ES256, jose.EdDSA})
	if err != nil || len(tok.Headers) != 1 {
		return invalid, nil
	}
	var unverified jwt.Claims
	if err := tok.UnsafeClaimsWithoutVerification(&unverified); err != nil {
		return invalid, nil
	}
	agentID := unverified.Issuer
	if _, err := uuid.Parse(agentID); err != nil {
		return invalid, nil
	}
	if _, err := s.deps.Agents.Get(ctx, req.TenantId, agentID); err != nil {
		if errors.Is(err, db.ErrAgentNotFound) {
			return invalid, nil
		}
		return nil, status.Errorf(codes.Internal, "get agent: %v", err)
	}
	kid := tok.Headers[0].KeyID
	deny := func(reason string) (*brokerv1.ResolveAgentApiKeyResponse, error) {
		s.emitAssertionDenied(ctx, req.TenantId, agentID, kid, reason)
		return invalid, nil
	}

	pk, err := s.deps.PublicKeys.Get(ctx, req.TenantId, agentID, kid)
	switch {
	case errors.Is(err, db.ErrPublicKeyNotFound):
		return deny("unknown_kid")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "get public key: %v", err)
	case pk.Status != "active":
		return deny("key_retired")
	case !time.Now().Before(pk.ExpiresAt):
		return deny("key_expired")
	case tok.Headers[0].Algorithm != pk.Alg:
		return deny("alg_mismatch")
	}
	block, _ := pem.Decode([]byte(pk.PublicKey))
	if block == nil {
		return deny("key_unreadable")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return deny("key_unreadable")
	}
	var c jwt.Claims
	if err := tok.Claims(pub, &c); err != nil {
		return deny("bad_signature")
	}
	now := time.Now()
	if c.Expiry == nil || c.IssuedAt == nil || c.ID == "" {
		return deny("missing_claims") // exp, iat and jti are all required
	}
	switch err := c.ValidateWithLeeway(jwt.Expected{
		Issuer: agentID, Subject: agentID, AnyAudience: jwt.Audience{req.Audience}, Time: now,
	}, assertionLeeway); {
	case errors.Is(err, jwt.ErrInvalidAudience):
		return deny("bad_audience")
	case errors.Is(err, jwt.ErrExpired):
		return deny("expired")
	case err != nil:
		return deny("bad_claims") // sub != iss, nbf or iat in the future
	}
	exp := c.Expiry.Time()
	if exp.Sub(now) > assertionMaxAhead+assertionLeeway {
		return deny("exp_too_far")
	}
	// Checked only after the signature, so nobody can burn another agent's jti.
	if !s.jtis.claim(req.TenantId+"|"+agentID+"|"+c.ID, exp.Add(assertionLeeway), now) {
		return deny("replayed_jti")
	}
	s.deps.PublicKeys.TouchLastUsed(req.TenantId, pk.ID.String())

	// ponytail: the shared tail takes a tk_ key row; the public key's id,
	// tenant, agent and created_at are all it reads (grant id, AG-06 epoch).
	return s.resolveAgentCredential(ctx, &db.StoredApiKey{
		ID: pk.ID, TenantID: pk.TenantID, AgentID: pk.AgentID, CreatedAt: pk.CreatedAt,
	}, req.UserAssertion)
}

// emitAssertionDenied records a refused client assertion (fire-and-forget).
func (s *SandboxService) emitAssertionDenied(ctx context.Context, tenant, agentID, kid, reason string) {
	s.deps.Logger.Info("client assertion refused", zap.String("agent_id", agentID), zap.String("kid", kid), zap.String("reason", reason))
	if s.deps.Audit == nil {
		return
	}
	c, _ := structpb.NewStruct(map[string]any{"reason": reason, "agent_id": agentID, "kid": kid})
	if err := s.deps.Audit.Emit(ctx, &auditv1.AuditEvent{
		EventId:      ids.EventID(),
		TenantId:     tenant,
		OccurredAt:   timestampNow(),
		ActorUserId:  "svc-" + agentID,
		ActorAgentId: agentID,
		EventType:    "aikonos.broker.agentkey.assertion_denied",
		ResourceRef:  "agent:" + agentID,
		Decision:     auditv1.PolicyDecision_DENY,
		Context:      c,
	}); err != nil {
		audit.RecordEmitFailure(ctx, s.deps.Logger, err, "aikonos.broker.agentkey.assertion_denied")
	}
}

// jtiCache remembers assertion ids until their exp, so each assertion is
// accepted once. The zero value is ready to use.
// ponytail: in-process; a restart reopens the (at most five-minute) window and
// several broker replicas would each keep their own. Move to a Postgres
// INSERT … ON CONFLICT table if the broker is ever scaled out.
type jtiCache struct {
	mu   sync.Mutex
	seen map[string]time.Time // key → expiry
}

// claim records key until expires and reports whether it was new.
func (c *jtiCache) claim(key string, expires, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.seen == nil {
		c.seen = map[string]time.Time{}
	}
	if at, ok := c.seen[key]; ok && now.Before(at) {
		return false
	}
	// Sweep on write; the map holds at most five minutes of assertions.
	for k, at := range c.seen {
		if !now.Before(at) {
			delete(c.seen, k)
		}
	}
	c.seen[key] = expires
	return true
}
