package broker

// m365_obo.go — lazy OneDrive OBO bootstrap on the connector read path
//. ensureOneDriveOBO is called from
// ListConnectors before listing: a no-op when the tenant isn't M365-managed
// or the connection is already healthy, otherwise a throttled Bootstrap
// attempt using the caller's own bearer as the OBO assertion. Always
// tolerant of failure — the RPC it's called from must keep working
// regardless of the outcome here.

import (
	"context"
	"time"

	"go.uber.org/zap"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/auth"
	"github.com/adamdekan/aikonos/broker/internal/connector"
	"github.com/adamdekan/aikonos/broker/internal/connectorstore"
	"github.com/adamdekan/aikonos/broker/internal/ids"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
)

// oboBroker is the seam ensureOneDriveOBO depends on — satisfied by
// *connector.OBOBroker in production (rpc-twins-tails precedent: Deps fields
// are typed as a narrow interface over their concrete production type so
// tests inject a fake rather than standing up a live Entra-shaped server).
type oboBroker interface {
	Configured(ctx context.Context, tenant string) bool
	Bootstrap(ctx context.Context, tenant, user, assertion string) error
}

// oboBootstrapEventType is the audit event emitted on every lazy bootstrap
// attempt, success or failure — never carries a token or secret value.
const oboBootstrapEventType = "aikonos.broker.connector.obo_bootstrap"

// ensureOneDriveOBO lazily bootstraps the caller's OneDrive OBO connection
// when the tenant is M365-managed and the connection isn't already healthy.
// No-op (never blocks or fails the caller's RPC) when: OBOBroker is unwired;
// the tenant has no M365 connection configured; the connection is already
// connected in Vault + connectorstore; a recent attempt for this (tenant,
// user) already failed within the throttle window; or no bearer is on the
// context (south-bound / scheduled calls never have one to exchange).
func (s *BrokerService) ensureOneDriveOBO(ctx context.Context, tenant, user string) {
	if s.deps.OBOBroker == nil || !s.deps.OBOBroker.Configured(ctx, tenant) {
		return
	}
	connectorID := connector.ProviderOneDrive.ConnectorID()
	if s.oboAlreadyHealthy(ctx, tenant, user, connectorID) {
		return
	}
	if !s.oboThrottle.allow(tenant, user) {
		return
	}
	assertion, berr := auth.BearerFromContext(ctx)
	if berr != nil {
		return
	}
	err := s.deps.OBOBroker.Bootstrap(ctx, tenant, user, assertion)
	s.recordOBOBootstrapEntry(tenant, user, connectorID, err)
	s.emitOBOBootstrapAudit(ctx, tenant, user, err)
}

// oneDriveManaged reports whether OneDrive is under the tenant-wide OBO
// connection for this tenant, rather than the legacy per-user OAuth path.
// Nil-safe and empty-tenant-safe so call sites need not guard separately.
func (s *BrokerService) oneDriveManaged(ctx context.Context, tenant string) bool {
	if s.deps.OBOBroker == nil || tenant == "" {
		return false
	}
	return s.deps.OBOBroker.Configured(ctx, tenant)
}

// tenantFromContext best-effort reads the authenticated tenant without
// erroring when absent. ListConnectorProviders takes no tenant_id request
// field (it predates per-tenant behavior) and must keep working for
// unauthenticated/dev-mode callers — callerIdentity's hard failure on a
// missing/invalid tenant would break that.
func tenantFromContext(ctx context.Context) string {
	if id, ok := auth.IdentityFromContext(ctx); ok {
		return id.TenantID
	}
	return ""
}

// oboAlreadyHealthy reports whether the connection needs no bootstrap
// attempt: a token is stored in Vault and the connectorstore entry already
// reads "connected".
func (s *BrokerService) oboAlreadyHealthy(ctx context.Context, tenant, user, connectorID string) bool {
	if s.deps.Connectors == nil || s.deps.Secrets == nil {
		return false
	}
	entry, ok, err := s.deps.Connectors.Resolve(tenant, user, connectorID)
	if err != nil || !ok || entry.Status != connector.StatusConnected {
		return false
	}
	_, terr := s.deps.Secrets.ReadConnector(ctx, tenant, user, connectorID)
	return terr == nil
}

// recordOBOBootstrapEntry upserts the connectorstore entry reflecting the
// bootstrap outcome so the ListConnectors read that triggered it (and every
// read after) sees an honest status without a second round trip. Bootstrap
// itself may also write via its own StatusUpdater on success — that write
// no-ops here because it targets an entry that doesn't exist yet on first
// connect, so this Upsert (which creates it) and Bootstrap's write are both
// harmless and intentionally redundant, not a double-write bug.
func (s *BrokerService) recordOBOBootstrapEntry(tenant, user, connectorID string, bootstrapErr error) {
	if s.deps.Connectors == nil {
		return
	}
	status := connector.StatusConnected
	connectedAt := time.Now().UTC()
	if bootstrapErr != nil {
		status = connector.StatusReconnectNeeded
		// Preserve the original connected_at across a transient failure rather
		// than stamping "now" on a connection that was never (or not newly)
		// established.
		connectedAt = time.Time{}
		if prev, ok, _ := s.deps.Connectors.Resolve(tenant, user, connectorID); ok {
			connectedAt = prev.ConnectedAt
		}
	}
	entry := connectorstore.Entry{
		ConnectorID: connectorID,
		Provider:    string(connector.ProviderOneDrive),
		DisplayName: connector.ProviderOneDrive.DisplayName(),
		Scopes:      connector.OBOScopes,
		VaultPath:   vaultPath(tenant, user, connectorID),
		Status:      status,
		ConnectedAt: connectedAt,
	}
	if uerr := s.deps.Connectors.Upsert(tenant, user, entry); uerr != nil && s.deps.Logger != nil {
		s.deps.Logger.Warn("obo bootstrap: connectorstore upsert failed", zap.Error(uerr))
	}
}

// emitOBOBootstrapAudit fires on every lazy bootstrap attempt, both outcomes —
// only the classified error text (never a token/secret value) travels in
// context, matching emitM365Audit's changed-keys-only discipline.
func (s *BrokerService) emitOBOBootstrapAudit(ctx context.Context, tenant, user string, bootstrapErr error) {
	if s.deps.Audit == nil {
		return
	}
	decision := auditv1.PolicyDecision_ALLOW
	fields := map[string]any{}
	if bootstrapErr != nil {
		decision = auditv1.PolicyDecision_DENY
		fields["reason"] = bootstrapErr.Error()
	}
	ctxStruct, _ := structpb.NewStruct(fields)
	if err := s.deps.Audit.Emit(ctx, &auditv1.AuditEvent{
		EventId:     ids.EventID(),
		TenantId:    tenant,
		OccurredAt:  timestampNow(),
		ActorUserId: user,
		EventType:   oboBootstrapEventType,
		Decision:    decision,
		Context:     ctxStruct,
	}); err != nil {
		audit.RecordEmitFailure(ctx, s.deps.Logger, err, oboBootstrapEventType)
	}
}
