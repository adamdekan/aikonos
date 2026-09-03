package broker

// m365_admin.go — north-bound M365 tenant connection admin RPCs (CP1 config
// plane: ). Metadata lives in org_settings,
// the client secret in Vault; TestM365Connection wires connector.ExchangeOBO
// with the caller's own bearer as the OBO assertion. Gate: callerIdentity →
// requireTenantAdmin, matching providers_admin.go exactly.

import (
	"context"
	"net/http"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/auth"
	"github.com/adamdekan/aikonos/broker/internal/connector"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/ids"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

const m365TestTimeout = 12 * time.Second

func (s *BrokerService) GetM365Connection(ctx context.Context, _ *brokerv1.GetM365ConnectionRequest) (*brokerv1.GetM365ConnectionResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.admin.m365.get")
	defer span.End()

	tenant, user, err := s.callerIdentity(ctx, "", "")
	if err != nil {
		return nil, err
	}
	if err := s.requireTenantAdmin(ctx, user); err != nil {
		return nil, err
	}

	settings := db.OrgSettings{}
	if s.deps.OrgSettings != nil {
		var gerr error
		settings, gerr = s.deps.OrgSettings.Get(ctx, tenant)
		if gerr != nil {
			return nil, status.Errorf(codes.Internal, "get m365 connection: %v", gerr)
		}
	}
	return &brokerv1.GetM365ConnectionResponse{Connection: m365ToProto(settings, s.m365HasSecret(ctx, tenant))}, nil
}

func (s *BrokerService) UpsertM365Connection(ctx context.Context, req *brokerv1.UpsertM365ConnectionRequest) (*brokerv1.UpsertM365ConnectionResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.admin.m365.upsert")
	defer span.End()

	tenant, user, err := s.callerIdentity(ctx, "", "")
	if err != nil {
		return nil, err
	}
	if err := s.requireTenantAdmin(ctx, user); err != nil {
		return nil, err
	}

	conn := req.GetConnection()
	if conn == nil || conn.GetEntraTenantId() == "" || conn.GetClientId() == "" {
		return nil, status.Error(codes.InvalidArgument, "connection.entra_tenant_id and connection.client_id are required")
	}

	// Secret handling mirrors UpsertLlmProvider: write to Vault when supplied,
	// otherwise preserve the stored has_secret value — a blank secret on an
	// edit must never silently wipe what's already there.
	hasSecret := false
	if req.ClientSecret != "" {
		if s.deps.Secrets != nil {
			if werr := s.deps.Secrets.WriteM365App(ctx, tenant, req.ClientSecret); werr != nil {
				s.deps.Logger.Warn("UpsertM365Connection: write secret failed", zap.Error(werr))
				return nil, status.Errorf(codes.Internal, "failed to store client secret")
			}
		}
		hasSecret = true
	} else {
		hasSecret = s.m365HasSecret(ctx, tenant)
	}

	if s.deps.OrgSettings == nil {
		return nil, status.Error(codes.FailedPrecondition, "org settings not configured")
	}
	patch := map[string]any{
		"m365_tenant_id": conn.GetEntraTenantId(),
		"m365_client_id": conn.GetClientId(),
		"m365_enabled":   conn.GetEnabled(),
	}
	settings, merr := s.deps.OrgSettings.Merge(ctx, tenant, patch, user)
	if merr != nil {
		return nil, status.Errorf(codes.Internal, "upsert m365 connection: %v", merr)
	}
	s.invalidateM365Cache(tenant)

	changed := []string{"client_id", "enabled", "entra_tenant_id"}
	if req.ClientSecret != "" {
		changed = append(changed, "client_secret")
	}
	s.emitM365Audit(ctx, span.SpanContext().TraceID().String(), tenant, user, "m365.connection.upsert", changed)

	out := m365ToProto(settings, hasSecret)
	return &brokerv1.UpsertM365ConnectionResponse{Connection: out}, nil
}

func (s *BrokerService) DeleteM365Connection(ctx context.Context, _ *brokerv1.DeleteM365ConnectionRequest) (*brokerv1.DeleteM365ConnectionResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.admin.m365.delete")
	defer span.End()

	tenant, user, err := s.callerIdentity(ctx, "", "")
	if err != nil {
		return nil, err
	}
	if err := s.requireTenantAdmin(ctx, user); err != nil {
		return nil, err
	}

	if s.deps.OrgSettings != nil {
		patch := map[string]any{
			"m365_tenant_id": "",
			"m365_client_id": "",
			"m365_enabled":   false,
		}
		if _, merr := s.deps.OrgSettings.Merge(ctx, tenant, patch, user); merr != nil {
			s.deps.Logger.Warn("DeleteM365Connection: repo error", zap.Error(merr))
			return nil, status.Errorf(codes.Internal, "failed to clear m365 connection")
		}
	}
	s.invalidateM365Cache(tenant)
	// Best-effort: delete the secret even if the metadata row was already absent.
	if s.deps.Secrets != nil {
		if derr := s.deps.Secrets.DeleteM365App(ctx, tenant); derr != nil {
			s.deps.Logger.Debug("DeleteM365Connection: delete secret best-effort failed", zap.Error(derr))
		}
	}

	s.emitM365Audit(ctx, span.SpanContext().TraceID().String(), tenant, user,
		"m365.connection.delete", []string{"client_id", "client_secret", "enabled", "entra_tenant_id"})
	return &brokerv1.DeleteM365ConnectionResponse{}, nil
}

// TestM365Connection runs a real OBO exchange using the caller's own bearer as
// the assertion, against the supplied (or stored) connection config. It never
// writes anything — a pure probe, like TestLlmProvider.
func (s *BrokerService) TestM365Connection(ctx context.Context, req *brokerv1.TestM365ConnectionRequest) (*brokerv1.TestM365ConnectionResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.admin.m365.test")
	defer span.End()

	tenant, user, err := s.callerIdentity(ctx, "", "")
	if err != nil {
		return nil, err
	}
	if err := s.requireTenantAdmin(ctx, user); err != nil {
		return nil, err
	}

	app, rerr := s.resolveM365TestApp(ctx, tenant, req)
	if rerr != nil {
		return nil, rerr
	}

	assertion, berr := auth.BearerFromContext(ctx)
	if berr != nil {
		return nil, status.Error(codes.Unauthenticated, "no bearer token on the calling context")
	}

	client := &http.Client{Timeout: m365TestTimeout}
	reqCtx, cancel := context.WithTimeout(ctx, m365TestTimeout)
	defer cancel()
	_, exerr := connector.ExchangeOBO(reqCtx, client, app, assertion)

	s.emitM365Audit(ctx, span.SpanContext().TraceID().String(), tenant, user, "m365.connection.test", nil)
	return m365TestResult(exerr), nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

// invalidateM365Cache clears OrgSettingsM365Source's cached entry for a
// tenant (so a config change takes effect on the very next ensureOneDriveOBO
// call rather than waiting out the TTL) and WorkspacePrefResolver's cached
// entries for every user in the tenant (so GetWorkspaceBackend/the Router's
// routing decision also picks up the change immediately — see
//  CP3 item 2; previously only the
// OBO source was invalidated, leaving Router.Prefs stale for up to
// effectivePrefCacheTTL). Both are no-ops when unwired.
func (s *BrokerService) invalidateM365Cache(tenant string) {
	if s.deps.Workspace.Prefs != nil {
		s.deps.Workspace.Prefs.InvalidateTenant(tenant)
	}
	real, ok := s.deps.OBOBroker.(*connector.OBOBroker)
	if !ok || real == nil {
		return
	}
	if src, ok := real.Apps.(interface{ Invalidate(string) }); ok {
		src.Invalidate(tenant)
	}
}

// m365HasSecret reports whether a client secret is stored for the tenant.
// Any Vault error is treated as "no secret" (fail closed on the UI hint, not
// on the RPC itself — Get/Upsert must still succeed).
func (s *BrokerService) m365HasSecret(ctx context.Context, tenant string) bool {
	if s.deps.Secrets == nil {
		return false
	}
	_, ok, err := s.deps.Secrets.ReadM365App(ctx, tenant)
	return err == nil && ok
}

// resolveM365TestApp resolves the M365App to probe: an unsaved connection
// from the request takes priority (test-before-save), falling back field-by-
// field to the tenant's stored connection + Vault secret — mirroring the
// TestLlmProvider blank-reuses-stored-key rule.
func (s *BrokerService) resolveM365TestApp(ctx context.Context, tenant string, req *brokerv1.TestM365ConnectionRequest) (connector.M365App, error) {
	conn := req.GetConnection()
	tenantID := conn.GetEntraTenantId()
	clientID := conn.GetClientId()
	secret := req.GetClientSecret()

	if (tenantID == "" || clientID == "") && s.deps.OrgSettings != nil {
		if settings, gerr := s.deps.OrgSettings.Get(ctx, tenant); gerr == nil {
			if tenantID == "" {
				tenantID = settings.M365TenantID
			}
			if clientID == "" {
				clientID = settings.M365ClientID
			}
		}
	}
	if secret == "" && s.deps.Secrets != nil {
		if stored, ok, rerr := s.deps.Secrets.ReadM365App(ctx, tenant); rerr == nil && ok {
			secret = stored
		}
	}
	if tenantID == "" || clientID == "" || secret == "" {
		return connector.M365App{}, status.Error(codes.FailedPrecondition,
			"m365 connection not configured: entra_tenant_id, client_id, and a client secret are all required")
	}
	return connector.M365App{TenantID: tenantID, ClientID: clientID, ClientSecret: secret}, nil
}

// m365TestResult builds the {ok, detail} response from the OBO exchange
// outcome. Extracted so the response-shaping is testable without a network call.
func m365TestResult(err error) *brokerv1.TestM365ConnectionResponse {
	if err != nil {
		return &brokerv1.TestM365ConnectionResponse{Ok: false, Detail: err.Error()}
	}
	return &brokerv1.TestM365ConnectionResponse{Ok: true, Detail: "OBO exchange succeeded"}
}

// m365ToProto maps the stored settings + resolved has_secret to the wire message.
func m365ToProto(s db.OrgSettings, hasSecret bool) *brokerv1.M365Connection {
	out := &brokerv1.M365Connection{
		EntraTenantId: s.M365TenantID,
		ClientId:      s.M365ClientID,
		HasSecret:     hasSecret,
		Enabled:       s.M365Enabled != nil && *s.M365Enabled,
		UpdatedBy:     s.UpdatedBy,
	}
	if !s.UpdatedAt.IsZero() {
		out.UpdatedAt = s.UpdatedAt.UTC().Format(time.RFC3339)
	}
	return out
}

// emitM365Audit fires an ALLOW audit event for an M365 connection mutation or
// probe. Only field names travel in the event, never values — the tenant id,
// client id, and (obviously) the secret are never persisted to the WORM trail.
func (s *BrokerService) emitM365Audit(ctx context.Context, traceID, tenant, actor, eventType string, changed []string) {
	if s.deps.Audit == nil {
		return
	}
	ctxStruct, _ := structpb.NewStruct(map[string]any{"actor": actor})
	if len(changed) > 0 {
		fields := make([]any, len(changed))
		for i, k := range changed {
			fields[i] = k
		}
		if changedList, lerr := structpb.NewList(fields); lerr == nil && ctxStruct != nil {
			ctxStruct.Fields["changed_keys"] = structpb.NewListValue(changedList)
		}
	}
	if err := s.deps.Audit.Emit(ctx, &auditv1.AuditEvent{
		EventId:     ids.EventID(),
		TraceId:     traceID,
		TenantId:    tenant,
		OccurredAt:  timestampNow(),
		ActorUserId: actor,
		EventType:   eventType,
		ResourceRef: "aikonos:m365connection:" + tenant,
		Decision:    auditv1.PolicyDecision_ALLOW,
		Context:     ctxStruct,
	}); err != nil {
		audit.RecordEmitFailure(ctx, s.deps.Logger, err, eventType)
	}
}
