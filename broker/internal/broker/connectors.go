package broker

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/connector"
	"github.com/adamdekan/aikonos/broker/internal/connectorstore"
	"github.com/adamdekan/aikonos/broker/internal/ids"
	"github.com/adamdekan/aikonos/broker/internal/secrets"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// dirUpsertTTL is the minimum interval between passive directory upserts for
// the same subject. Throttles DB writes on hot RPCs without stale data risk —
// the entry's last_seen is informational; name/email are idempotent on re-write.
const dirUpsertTTL = 10 * time.Minute

// oboThrottleTTL is the minimum interval between OneDrive OBO bootstrap
// attempts for the same (tenant, user) after a failure
//. Deliberately much shorter
// than dirUpsertTTL — a stale Entra token exchange should retry far sooner
// than a passive directory upsert.
const oboThrottleTTL = 2 * time.Minute

// dirThrottleMaxEntries is the hard cap on the number of live entries in an
// upsertThrottle. When inserting would exceed it, expired entries are evicted
// first; if the map is still over the cap after eviction, it is cleared. This
// is correct for a TTL cache: every entry re-adds itself on the next call.
const dirThrottleMaxEntries = 4096

type upsertThrottle struct {
	mu   sync.Mutex
	seen map[string]time.Time // key: "tenantID/subject"
	ttl  time.Duration
}

func newUpsertThrottle() *upsertThrottle {
	return newUpsertThrottleWithTTL(dirUpsertTTL)
}

// newUpsertThrottleWithTTL builds an upsertThrottle with a caller-chosen TTL
// so distinct throttles (e.g. dirThrottle vs oboThrottle) can run their own
// suppression windows over the same eviction/cap mechanics.
func newUpsertThrottleWithTTL(ttl time.Duration) *upsertThrottle {
	return &upsertThrottle{seen: make(map[string]time.Time), ttl: ttl}
}

// allow returns true when the subject has not been upserted within the TTL,
// and records the current time so the next call within TTL is suppressed.
func (u *upsertThrottle) allow(tenantID, subject string) bool {
	key := tenantID + "/" + subject
	now := time.Now()
	u.mu.Lock()
	defer u.mu.Unlock()
	if t, ok := u.seen[key]; ok && now.Sub(t) < u.ttl {
		return false
	}
	// Cap enforcement: evict expired entries before inserting; clear if still over.
	if len(u.seen) >= dirThrottleMaxEntries {
		for k, t := range u.seen {
			if now.Sub(t) >= u.ttl {
				delete(u.seen, k)
			}
		}
		if len(u.seen) >= dirThrottleMaxEntries {
			u.seen = make(map[string]time.Time)
		}
	}
	u.seen[key] = now
	return true
}

const connectorStateTTL = 10 * time.Minute

// connectorsEnabled reports whether Vault + an OAuth registry are wired. When
// false every connector RPC fails closed with FailedPrecondition.
func (s *BrokerService) connectorsEnabled() bool {
	return s.deps.Secrets != nil && s.deps.OAuth != nil && s.deps.ConnAuth != nil && s.deps.Connectors != nil
}

func providerFromProto(p brokerv1.ConnectorProvider) (connector.Provider, bool) {
	switch p {
	case brokerv1.ConnectorProvider_GOOGLE_DRIVE:
		return connector.ProviderGoogleDrive, true
	case brokerv1.ConnectorProvider_ONEDRIVE:
		return connector.ProviderOneDrive, true
	default:
		return "", false
	}
}

func providerToProto(p connector.Provider) brokerv1.ConnectorProvider {
	switch p {
	case connector.ProviderGoogleDrive:
		return brokerv1.ConnectorProvider_GOOGLE_DRIVE
	case connector.ProviderOneDrive:
		return brokerv1.ConnectorProvider_ONEDRIVE
	default:
		return brokerv1.ConnectorProvider_CONNECTOR_PROVIDER_UNSPECIFIED
	}
}

func providerFromConnectorID(id string) (connector.Provider, bool) {
	switch id {
	case connector.ProviderGoogleDrive.ConnectorID():
		return connector.ProviderGoogleDrive, true
	case connector.ProviderOneDrive.ConnectorID():
		return connector.ProviderOneDrive, true
	default:
		return "", false
	}
}

// vaultPath is the logical reference recorded in mcp_connections.json. The
// segments were already validated by the preceding WriteConnector, so a path
// error here cannot occur in practice; an empty string is a safe display value.
func vaultPath(tenant, user, connectorID string) string {
	p, _ := secrets.ConnectorPath(tenant, user, connectorID)
	return p
}

func (s *BrokerService) BeginConnectorAuth(ctx context.Context, req *brokerv1.BeginConnectorAuthRequest) (*brokerv1.BeginConnectorAuthResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.connector.begin")
	defer span.End()

	if !s.connectorsEnabled() {
		return nil, status.Error(codes.FailedPrecondition, "connectors disabled (Vault not configured)")
	}
	tenant, user, err := s.callerIdentity(ctx, req.TenantId, req.UserId)
	if err != nil {
		return nil, err
	}
	prov, ok := providerFromProto(req.Provider)
	if !ok {
		return nil, status.Error(codes.InvalidArgument, "unknown connector provider")
	}
	if prov == connector.ProviderOneDrive && s.oneDriveManaged(ctx, tenant) {
		return nil, status.Error(codes.FailedPrecondition, "OneDrive is managed by your organization")
	}
	// A7: org connector-install allowlist. Fail closed on a settings read error;
	// no-op when the repo is unconfigured or the allowlist is disabled.
	if s.deps.OrgSettings != nil {
		settings, serr := s.deps.OrgSettings.Get(ctx, tenant)
		if serr != nil {
			return nil, status.Errorf(codes.Internal, "org settings read: %v", serr)
		}
		if !settings.ConnectorAllowed(string(prov)) {
			return nil, status.Errorf(codes.PermissionDenied, "connector %q is not permitted by your organization", prov)
		}
	}
	if !s.deps.OAuth.Configured(prov) {
		return nil, status.Errorf(codes.FailedPrecondition, "provider %q not configured", prov)
	}

	state, err := connector.GenerateState()
	if err != nil {
		return nil, status.Error(codes.Internal, "state generation failed")
	}
	verifier := connector.GenerateVerifier()
	scopes := connector.EffectiveScopes(prov, req.Scopes)
	authURL, err := s.deps.OAuth.AuthCodeURL(prov, scopes, state, verifier)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "authorize url: %v", err)
	}

	if err := s.deps.ConnAuth.Put(ctx, state, connector.PendingAuth{
		Provider: prov,
		Tenant:   tenant,
		User:     user,
		Verifier: verifier,
		Scopes:   scopes,
	}); err != nil {
		s.deps.Logger.Warn("BeginConnectorAuth: persist state failed", zap.Error(err))
		return nil, status.Error(codes.Internal, "could not persist auth state")
	}
	s.emitConnectorAudit(ctx, span.SpanContext().TraceID().String(), tenant, user,
		"aikonos.broker.connector.begin", auditv1.PolicyDecision_ALLOW)

	return &brokerv1.BeginConnectorAuthResponse{AuthorizeUrl: authURL, State: state}, nil
}

func (s *BrokerService) CompleteConnectorAuth(ctx context.Context, req *brokerv1.CompleteConnectorAuthRequest) (*brokerv1.CompleteConnectorAuthResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.connector.complete")
	defer span.End()
	traceID := span.SpanContext().TraceID().String()

	if !s.connectorsEnabled() {
		return nil, status.Error(codes.FailedPrecondition, "connectors disabled (Vault not configured)")
	}
	tenant, user, err := s.callerIdentity(ctx, req.TenantId, req.UserId)
	if err != nil {
		return nil, err
	}
	if req.Code == "" || req.State == "" {
		return nil, status.Error(codes.InvalidArgument, "code and state required")
	}
	pending, ok, err := s.deps.ConnAuth.Take(ctx, tenant, req.State)
	if err != nil {
		s.deps.Logger.Warn("CompleteConnectorAuth: load state failed", zap.Error(err))
		return nil, status.Error(codes.Internal, "could not load auth state")
	}
	if !ok {
		return nil, status.Error(codes.InvalidArgument, "unknown or expired state")
	}
	// The state was minted for this identity; a mismatch means a crossed flow.
	if pending.Tenant != tenant || pending.User != user {
		return nil, status.Error(codes.PermissionDenied, "state does not match caller")
	}

	tok, err := s.deps.OAuth.Exchange(ctx, pending.Provider, pending.Scopes, req.Code, pending.Verifier)
	if err != nil {
		s.deps.Logger.Warn("CompleteConnectorAuth: token exchange failed", zap.Error(err))
		return nil, status.Error(codes.PermissionDenied, "token exchange failed")
	}
	if tok.RefreshToken == "" {
		// Without a refresh token the connection can't survive access-token
		// expiry; surface it so the eventual "reconnect needed" has a breadcrumb.
		s.deps.Logger.Warn("connector linked without a refresh token; will need re-linking after expiry",
			zap.String("provider", string(pending.Provider)), zap.String("user", user))
	}

	connectorID := pending.Provider.ConnectorID()
	ct := &secrets.ConnectorToken{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresAt:    tok.Expiry,
		Scopes:       pending.Scopes,
	}
	if err := s.deps.Secrets.WriteConnector(ctx, tenant, user, connectorID, ct); err != nil {
		s.deps.Logger.Error("CompleteConnectorAuth: vault write failed", zap.Error(err))
		return nil, status.Error(codes.Internal, "failed to store connector token")
	}

	entry := connectorstore.Entry{
		ConnectorID: connectorID,
		Provider:    string(pending.Provider),
		DisplayName: pending.Provider.DisplayName(),
		Scopes:      pending.Scopes,
		VaultPath:   vaultPath(tenant, user, connectorID),
		Status:      "connected",
		ConnectedAt: time.Now().UTC(),
	}
	if err := s.deps.Connectors.Upsert(tenant, user, entry); err != nil {
		s.deps.Logger.Error("CompleteConnectorAuth: reference write failed", zap.Error(err))
		return nil, status.Error(codes.Internal, "failed to record connector")
	}

	s.deps.Logger.Info("connector linked",
		zap.String("provider", string(pending.Provider)),
		zap.String("user", user))
	s.emitConnectorAudit(ctx, traceID, tenant, user,
		"aikonos.broker.connector.connected", auditv1.PolicyDecision_ALLOW)

	return &brokerv1.CompleteConnectorAuthResponse{
		ConnectorId: connectorID,
		Status:      "connected",
		Scopes:      pending.Scopes,
	}, nil
}

func (s *BrokerService) ListConnectors(ctx context.Context, req *brokerv1.ListConnectorsRequest) (*brokerv1.ListConnectorsResponse, error) {
	if !s.connectorsEnabled() {
		return nil, status.Error(codes.FailedPrecondition, "connectors disabled (Vault not configured)")
	}
	tenant, user, err := s.callerIdentity(ctx, req.TenantId, req.UserId)
	if err != nil {
		return nil, err
	}
	// Lazy OBO bootstrap: no-op unless the tenant is M365-managed and the
	// connection isn't already healthy. Tolerant of failure — a bad or
	// missing bearer must never break this listing.
	s.ensureOneDriveOBO(ctx, tenant, user)
	entries, err := s.deps.Connectors.List(tenant, user)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list connectors: %v", err)
	}
	managed := s.oneDriveManaged(ctx, tenant)
	out := make([]*brokerv1.ConnectorStatus, 0, len(entries))
	for _, e := range entries {
		prov := connector.Provider(e.Provider)
		out = append(out, &brokerv1.ConnectorStatus{
			ConnectorId: e.ConnectorID,
			Provider:    providerToProto(prov),
			DisplayName: e.DisplayName,
			Scopes:      e.Scopes,
			Status:      e.Status,
			ConnectedAt: timestamppb.New(e.ConnectedAt),
			Managed:     managed && prov == connector.ProviderOneDrive,
		})
	}
	return &brokerv1.ListConnectorsResponse{Connectors: out}, nil
}

// ListConnectorProviders returns the providers the deployment can offer — those
// whose OAuth app credentials are present. The UI renders Connect buttons from
// this, so an unconfigured provider is never advertised (avoiding a guaranteed
// FailedPrecondition on BeginConnectorAuth). When connectors are disabled the
// list is empty rather than an error: the UI simply shows nothing.
func (s *BrokerService) ListConnectorProviders(ctx context.Context, req *brokerv1.ListConnectorProvidersRequest) (*brokerv1.ListConnectorProvidersResponse, error) {
	if !s.connectorsEnabled() {
		return &brokerv1.ListConnectorProvidersResponse{}, nil
	}
	managed := s.oneDriveManaged(ctx, tenantFromContext(ctx))
	var out []*brokerv1.ConnectorProviderInfo
	for _, p := range connector.RegisteredProviders() {
		if !s.deps.OAuth.Configured(p) {
			continue
		}
		if managed && p == connector.ProviderOneDrive {
			continue
		}
		out = append(out, &brokerv1.ConnectorProviderInfo{
			Provider:    providerToProto(p),
			Key:         string(p),
			DisplayName: p.DisplayName(),
			ConnectorId: p.ConnectorID(),
		})
	}
	return &brokerv1.ListConnectorProvidersResponse{Providers: out}, nil
}

func (s *BrokerService) RevokeConnector(ctx context.Context, req *brokerv1.RevokeConnectorRequest) (*brokerv1.RevokeConnectorResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.connector.revoke")
	defer span.End()

	if !s.connectorsEnabled() {
		return nil, status.Error(codes.FailedPrecondition, "connectors disabled (Vault not configured)")
	}
	tenant, user, err := s.callerIdentity(ctx, req.TenantId, req.UserId)
	if err != nil {
		return nil, err
	}
	if req.ConnectorId == "" {
		return nil, status.Error(codes.InvalidArgument, "connector_id required")
	}

	// Best-effort provider revoke before deleting the secret.
	if prov, ok := providerFromConnectorID(req.ConnectorId); ok {
		if tok, rerr := s.deps.Secrets.ReadConnector(ctx, tenant, user, req.ConnectorId); rerr == nil {
			if err := s.deps.OAuth.Revoke(ctx, prov, tok.RefreshToken); err != nil {
				s.deps.Logger.Warn("RevokeConnector: provider revoke failed (continuing)", zap.Error(err))
			}
		}
	}

	if err := s.deps.Secrets.DeleteConnector(ctx, tenant, user, req.ConnectorId); err != nil {
		s.deps.Logger.Error("RevokeConnector: vault delete failed", zap.Error(err))
		return nil, status.Error(codes.Internal, "failed to delete connector token")
	}
	if err := s.deps.Connectors.Remove(tenant, user, req.ConnectorId); err != nil {
		s.deps.Logger.Error("RevokeConnector: reference remove failed", zap.Error(err))
		return nil, status.Error(codes.Internal, "failed to remove connector reference")
	}

	s.emitConnectorAudit(ctx, span.SpanContext().TraceID().String(), tenant, user,
		"aikonos.broker.connector.revoked", auditv1.PolicyDecision_ALLOW)
	return &brokerv1.RevokeConnectorResponse{Success: true}, nil
}

func (s *BrokerService) emitConnectorAudit(ctx context.Context, traceID, tenant, actor, eventType string, decision auditv1.PolicyDecision) {
	if err := s.deps.Audit.Emit(ctx, &auditv1.AuditEvent{
		EventId:     ids.EventID(),
		TraceId:     traceID,
		TenantId:    tenant,
		OccurredAt:  timestampNow(),
		ActorUserId: actor,
		EventType:   eventType,
		Decision:    decision,
	}); err != nil {
		audit.RecordEmitFailure(ctx, s.deps.Logger, err, eventType)
	}
}
