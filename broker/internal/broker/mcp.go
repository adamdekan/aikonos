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

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/ids"
	"github.com/adamdekan/aikonos/broker/internal/secrets"
	"github.com/adamdekan/aikonos/broker/internal/toolproxy"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// mcpConnectionRepo is the minimal interface the broker layer needs from the DB
// layer. Satisfied by *db.McpConnectionRepo; extracted here so unit tests can
// substitute a fake without Postgres.
type mcpConnectionRepo interface {
	List(ctx context.Context, tenantID string) ([]*db.McpConnection, error)
	Get(ctx context.Context, tenantID, id string) (*db.McpConnection, error)
	Add(ctx context.Context, conn *db.McpConnection) error
	Update(ctx context.Context, tenantID string, conn *db.McpConnection) error
	Delete(ctx context.Context, tenantID, id string) error
}

func (s *BrokerService) mcpEnabled() bool { return s.deps.Mcp.Connections != nil }

func toProtoMcpConnection(c *db.McpConnection) *brokerv1.McpConnection {
	p := &brokerv1.McpConnection{
		Id:            c.ID.String(),
		Name:          c.Name,
		Url:           c.URL,
		Transport:     c.Transport,
		AuthType:      c.AuthType,
		AuthSecretRef: c.AuthSecretRef,
		CreatedBy:     c.CreatedBy,
		CreatedAt:     timestamppb.New(c.CreatedAt),
	}
	return p
}

// ListMcpConnections returns all admin-managed MCP server entries for the
// tenant; requires tenant-admin and returns FailedPrecondition when MCP is not configured.
func (s *BrokerService) ListMcpConnections(ctx context.Context, req *brokerv1.ListMcpConnectionsRequest) (*brokerv1.ListMcpConnectionsResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.mcp.list")
	defer span.End()

	if !s.mcpEnabled() {
		return nil, status.Error(codes.FailedPrecondition, "MCP connections not configured")
	}
	tenant, user, err := s.callerIdentity(ctx, req.TenantId, req.UserId)
	if err != nil {
		return nil, err
	}
	if err := s.requireTenantAdmin(ctx, user); err != nil {
		return nil, err
	}
	rows, err := s.deps.Mcp.Connections.List(ctx, tenant)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list mcp connections: %v", err)
	}
	out := make([]*brokerv1.McpConnection, 0, len(rows))
	for _, r := range rows {
		out = append(out, toProtoMcpConnection(r))
	}
	s.emitMcpAudit(ctx, span.SpanContext().TraceID().String(), tenant, user,
		"aikonos.broker.mcp.connections.listed", "", auditv1.PolicyDecision_ALLOW)
	return &brokerv1.ListMcpConnectionsResponse{Connections: out}, nil
}

func (s *BrokerService) AddMcpConnection(ctx context.Context, req *brokerv1.AddMcpConnectionRequest) (*brokerv1.AddMcpConnectionResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.mcp.add")
	defer span.End()

	if !s.mcpEnabled() {
		return nil, status.Error(codes.FailedPrecondition, "MCP connections not configured")
	}
	tenant, user, err := s.callerIdentity(ctx, req.TenantId, req.UserId)
	if err != nil {
		return nil, err
	}
	if err := s.requireTenantAdmin(ctx, user); err != nil {
		return nil, err
	}
	if req.Connection == nil {
		return nil, status.Error(codes.InvalidArgument, "connection is required")
	}
	if err := validateMcpConnection(req.Connection); err != nil {
		return nil, err
	}

	tenantUUID, err := uuid.Parse(tenant)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid tenant_id: %v", err)
	}

	connID := uuid.New()
	rec := &db.McpConnection{
		ID:        connID,
		TenantID:  tenantUUID,
		Name:      strings.TrimSpace(req.Connection.Name),
		URL:       strings.TrimSpace(req.Connection.Url),
		Transport: strings.TrimSpace(req.Connection.Transport),
		AuthType:  strings.TrimSpace(req.Connection.AuthType),
		CreatedBy: user,
	}

	// When auth_type is bearer and a token is provided, write to Vault first;
	// only persist the Vault path reference in Postgres (never the token itself).
	if rec.AuthType == "bearer" && req.BearerToken != "" {
		if s.deps.Mcp.Bearer == nil {
			return nil, status.Error(codes.FailedPrecondition, "Vault not configured — bearer token storage unavailable")
		}
		path, perr := secrets.McpBearerPath(tenant, connID.String())
		if perr != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid vault path: %v", perr)
		}
		if werr := s.deps.Mcp.Bearer.WriteMcpBearer(ctx, path, req.BearerToken); werr != nil {
			s.deps.Logger.Error("AddMcpConnection: vault write failed", zap.Error(werr))
			return nil, status.Error(codes.Internal, "failed to store bearer token")
		}
		rec.AuthSecretRef = path
	}

	if err := s.deps.Mcp.Connections.Add(ctx, rec); err != nil {
		return nil, status.Errorf(codes.Internal, "add mcp connection: %v", err)
	}

	s.emitMcpAudit(ctx, span.SpanContext().TraceID().String(), tenant, user,
		"aikonos.broker.mcp.connection.added", connID.String(), auditv1.PolicyDecision_ALLOW)
	return &brokerv1.AddMcpConnectionResponse{Connection: toProtoMcpConnection(rec)}, nil
}

func (s *BrokerService) UpdateMcpConnection(ctx context.Context, req *brokerv1.UpdateMcpConnectionRequest) (*brokerv1.UpdateMcpConnectionResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.mcp.update")
	defer span.End()

	if !s.mcpEnabled() {
		return nil, status.Error(codes.FailedPrecondition, "MCP connections not configured")
	}
	tenant, user, err := s.callerIdentity(ctx, req.TenantId, req.UserId)
	if err != nil {
		return nil, err
	}
	if err := s.requireTenantAdmin(ctx, user); err != nil {
		return nil, err
	}
	if req.Connection == nil {
		return nil, status.Error(codes.InvalidArgument, "connection is required")
	}
	if req.Connection.Id == "" {
		return nil, status.Error(codes.InvalidArgument, "connection.id is required")
	}
	if err := validateMcpConnection(req.Connection); err != nil {
		return nil, err
	}

	connID, err := uuid.Parse(req.Connection.Id)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid connection id: %v", err)
	}
	tenantUUID, err := uuid.Parse(tenant)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid tenant_id: %v", err)
	}

	rec := &db.McpConnection{
		ID:        connID,
		TenantID:  tenantUUID,
		Name:      strings.TrimSpace(req.Connection.Name),
		URL:       strings.TrimSpace(req.Connection.Url),
		Transport: strings.TrimSpace(req.Connection.Transport),
		AuthType:  strings.TrimSpace(req.Connection.AuthType),
		// AuthSecretRef is never taken from the client; derived server-side only.
	}

	switch {
	case rec.AuthType == "bearer" && req.BearerToken != "":
		// New bearer token supplied: write to Vault, derive the canonical ref.
		if s.deps.Mcp.Bearer == nil {
			return nil, status.Error(codes.FailedPrecondition, "Vault not configured — bearer token storage unavailable")
		}
		path, perr := secrets.McpBearerPath(tenant, connID.String())
		if perr != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid vault path: %v", perr)
		}
		if werr := s.deps.Mcp.Bearer.WriteMcpBearer(ctx, path, req.BearerToken); werr != nil {
			s.deps.Logger.Error("UpdateMcpConnection: vault write failed", zap.Error(werr))
			return nil, status.Error(codes.Internal, "failed to store bearer token")
		}
		rec.AuthSecretRef = path
	case rec.AuthType == "bearer":
		// No new token: preserve the existing ref from the DB (re-read to avoid
		// a client-supplied value ever reaching the store).
		existing, gerr := s.deps.Mcp.Connections.Get(ctx, tenant, connID.String())
		if gerr != nil {
			return nil, status.Errorf(codes.Internal, "fetch existing mcp connection: %v", gerr)
		}
		rec.AuthSecretRef = existing.AuthSecretRef
		// auth_type != "bearer": ref must be empty (zero value is correct).
	}

	if err := s.deps.Mcp.Connections.Update(ctx, tenant, rec); err != nil {
		return nil, status.Errorf(codes.Internal, "update mcp connection: %v", err)
	}

	s.emitMcpAudit(ctx, span.SpanContext().TraceID().String(), tenant, user,
		"aikonos.broker.mcp.connection.updated", connID.String(), auditv1.PolicyDecision_ALLOW)
	return &brokerv1.UpdateMcpConnectionResponse{Connection: toProtoMcpConnection(rec)}, nil
}

func (s *BrokerService) DeleteMcpConnection(ctx context.Context, req *brokerv1.DeleteMcpConnectionRequest) (*brokerv1.DeleteMcpConnectionResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.mcp.delete")
	defer span.End()

	if !s.mcpEnabled() {
		return nil, status.Error(codes.FailedPrecondition, "MCP connections not configured")
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

	// Best-effort Vault cleanup before DB delete. Failure never blocks the delete.
	if s.deps.Mcp.Bearer != nil {
		path, perr := secrets.McpBearerPath(tenant, req.Id)
		if perr == nil {
			if derr := s.deps.Mcp.Bearer.DeleteMcpBearer(ctx, path); derr != nil {
				s.deps.Logger.Warn("DeleteMcpConnection: vault delete failed (continuing)", zap.Error(derr))
			}
		}
	}

	if err := s.deps.Mcp.Connections.Delete(ctx, tenant, req.Id); err != nil {
		return nil, status.Errorf(codes.Internal, "delete mcp connection: %v", err)
	}

	s.emitMcpAudit(ctx, span.SpanContext().TraceID().String(), tenant, user,
		"aikonos.broker.mcp.connection.deleted", req.Id, auditv1.PolicyDecision_ALLOW)
	return &brokerv1.DeleteMcpConnectionResponse{Success: true}, nil
}

// ListAccessibleMcpServers lists the MCP connections an agent may access.
// It lists all tenant connections then filters by OpenFGA Check(agent, can_access,
// mcp_connector:<id>).
//
// Gating: tenant-admin only (v1). The acting caller is the gateway or admin console,
// not the agent itself; gating to tenant-admin matches every other admin list RPC and
// prevents unauthenticated callers from enumerating MCP servers by probing agent IDs.
// A future SPIFFE-south variant will let the gateway query on the agent's own behalf
// without requiring a tenant-admin OIDC token.
func (s *BrokerService) ListAccessibleMcpServers(ctx context.Context, req *brokerv1.ListAccessibleMcpServersRequest) (*brokerv1.ListAccessibleMcpServersResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.mcp.list_accessible")
	defer span.End()

	if !s.mcpEnabled() {
		return nil, status.Error(codes.FailedPrecondition, "MCP connections not configured")
	}
	// ListAccessibleMcpServers has no user_id field on the request; the acting
	// principal comes from the OIDC context only (or the dev-mode empty string
	// path — callerIdentity handles both).
	tenant, user, err := s.callerIdentity(ctx, req.TenantId, "")
	if err != nil {
		return nil, err
	}
	if err := s.requireTenantAdmin(ctx, user); err != nil {
		return nil, err
	}
	if req.AgentId == "" {
		return nil, status.Error(codes.InvalidArgument, "agent_id required")
	}

	// Normalise: accept bare "alice-agent" or "agent:alice-agent".
	agentRef := req.AgentId
	if !strings.HasPrefix(agentRef, "agent:") {
		agentRef = "agent:" + agentRef
	}

	rows, err := s.deps.Mcp.Connections.List(ctx, tenant)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list mcp connections: %v", err)
	}

	out := make([]*brokerv1.McpConnection, 0, len(rows))
	for _, r := range rows {
		object := "mcp_connector:" + r.ID.String()
		allowed, cerr := s.deps.Policy.CheckFGA(ctx, agentRef, "can_access", object)
		if cerr != nil {
			s.deps.Logger.Warn("ListAccessibleMcpServers: fga check failed",
				zap.String("agent", agentRef), zap.String("object", object), zap.Error(cerr))
			// Fail closed: a transport error must not widen access.
			return nil, status.Error(codes.Internal, "authorization check failed")
		}
		if allowed {
			out = append(out, toProtoMcpConnection(r))
		}
	}

	s.emitMcpAudit(ctx, span.SpanContext().TraceID().String(), tenant, user,
		"aikonos.broker.mcp.accessible.listed", agentRef, auditv1.PolicyDecision_ALLOW)
	return &brokerv1.ListAccessibleMcpServersResponse{Connections: out}, nil
}

// ListMcpServerTools connects to the named MCP server, performs the initialize
// handshake, and returns the tool schemas the server advertises.
//
// Gating: tenant-admin (same as all other MCP admin RPCs in v1). The gateway
// is the primary caller; it must hold an admin OIDC token or the dev allow-all
// path must be active. A SPIFFE-south variant (bypassing OIDC) is deferred.
//
// On success the handler emits an audit event. The MCP connection is
// established per-call (no session pooling in v1).
func (s *BrokerService) ListMcpServerTools(ctx context.Context, req *brokerv1.ListMcpServerToolsRequest) (*brokerv1.ListMcpServerToolsResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.mcp.list_server_tools")
	defer span.End()

	if !s.mcpEnabled() {
		return nil, status.Error(codes.FailedPrecondition, "MCP connections not configured")
	}
	tenant, user, err := s.callerIdentity(ctx, req.TenantId, req.UserId)
	if err != nil {
		return nil, err
	}
	if err := s.requireTenantAdmin(ctx, user); err != nil {
		return nil, err
	}
	if req.ConnectorId == "" {
		return nil, status.Error(codes.InvalidArgument, "connector_id required")
	}

	conn, err := s.deps.Mcp.Connections.Get(ctx, tenant, req.ConnectorId)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "mcp connection %q not found: %v", req.ConnectorId, err)
	}

	var bearer string
	if conn.AuthType == "bearer" && s.deps.Mcp.Bearer != nil {
		b, rerr := s.deps.Mcp.Bearer.ReadMcpBearer(ctx, tenant, req.ConnectorId)
		if rerr != nil && rerr != secrets.ErrMcpBearerNotFound {
			s.deps.Logger.Error("ListMcpServerTools: read bearer failed", zap.Error(rerr))
			return nil, status.Error(codes.Internal, "failed to read MCP bearer token")
		}
		bearer = b
	}

	// NewMCPClientPublic has SSRF guard on (allowPrivate=false). Flip for tests
	// via Deps.Mcp.AllowPrivate — never set true in production.
	var client *toolproxy.MCPClient
	if s.deps.Mcp.AllowPrivate {
		client = toolproxy.NewMCPClientAllowPrivate(conn.URL, bearer)
	} else {
		client = toolproxy.NewMCPClientPublic(conn.URL, bearer)
	}
	if cerr := client.Connect(ctx); cerr != nil {
		return nil, status.Errorf(codes.Unavailable, "mcp: connect %q: %v", conn.URL, cerr)
	}
	tools, lerr := client.ListTools(ctx)
	if lerr != nil {
		return nil, status.Errorf(codes.Unavailable, "mcp: list tools %q: %v", conn.URL, lerr)
	}

	out := make([]*brokerv1.McpToolSchema, 0, len(tools))
	for _, t := range tools {
		out = append(out, &brokerv1.McpToolSchema{
			Name:            t.Name,
			Description:     t.Description,
			InputSchemaJson: t.InputSchemaJSON,
			ReadOnlyHint:    t.ReadOnlyHint,
		})
	}

	s.emitMcpAudit(ctx, span.SpanContext().TraceID().String(), tenant, user,
		"aikonos.broker.mcp.server.tools.listed", req.ConnectorId, auditv1.PolicyDecision_ALLOW)
	return &brokerv1.ListMcpServerToolsResponse{Tools: out}, nil
}

// validateMcpConnection checks required fields and allowed enum values.
func validateMcpConnection(c *brokerv1.McpConnection) error {
	if strings.TrimSpace(c.Name) == "" {
		return status.Error(codes.InvalidArgument, "connection.name is required")
	}
	if strings.TrimSpace(c.Url) == "" {
		return status.Error(codes.InvalidArgument, "connection.url is required")
	}
	switch strings.TrimSpace(c.Transport) {
	case "streamable_http", "sse":
	default:
		return status.Errorf(codes.InvalidArgument, "transport must be streamable_http|sse, got %q", c.Transport)
	}
	switch strings.TrimSpace(c.AuthType) {
	case "none", "bearer":
	default:
		return status.Errorf(codes.InvalidArgument, "auth_type must be none|bearer, got %q", c.AuthType)
	}
	return nil
}

func (s *BrokerService) emitMcpAudit(ctx context.Context, traceID, tenant, actor, eventType, resourceID string, decision auditv1.PolicyDecision) {
	ev := &auditv1.AuditEvent{
		EventId:     ids.EventID(),
		TraceId:     traceID,
		TenantId:    tenant,
		OccurredAt:  timestampNow(),
		ActorUserId: actor,
		EventType:   eventType,
		Decision:    decision,
	}
	if resourceID != "" {
		ev.ResourceRef = "mcp_connection:" + resourceID
		if c, err := structpb.NewStruct(map[string]any{"id": resourceID}); err == nil {
			ev.Context = c
		}
	}
	if err := s.deps.Audit.Emit(ctx, ev); err != nil {
		audit.RecordEmitFailure(ctx, s.deps.Logger, err, ev.EventType)
	}
}
