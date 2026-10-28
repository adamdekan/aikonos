package broker

import (
	"context"
	"slices"
	"strings"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/ids"
	"github.com/adamdekan/aikonos/broker/internal/toolproxy"
	"github.com/adamdekan/aikonos/broker/internal/toolregistry"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// ListAccessibleMcpServersForAgent is the SPIFFE-south variant of
// ListAccessibleMcpServers. The gateway calls this during session build for a
// non-admin user's agent — no OIDC tenant-admin token is available. The
// SPIFFE gateway-peer gate replaces the tenant-admin OIDC gate; the broker
// still enforces the agent's FGA can_access grant per connector.
func (s *SandboxService) ListAccessibleMcpServersForAgent(ctx context.Context, req *brokerv1.ListAccessibleMcpServersRequest) (*brokerv1.ListAccessibleMcpServersResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.mcp.south.list_accessible")
	defer span.End()

	if err := s.requireGatewayPeer(ctx); err != nil {
		return nil, err
	}
	if s.deps.Mcp.Connections == nil {
		return nil, status.Error(codes.FailedPrecondition, "MCP connections not configured")
	}
	if req.TenantId == "" {
		return nil, status.Error(codes.InvalidArgument, "tenant_id required")
	}
	if req.AgentId == "" {
		return nil, status.Error(codes.InvalidArgument, "agent_id required")
	}

	// AG-01: the session's bound agent, a registered agent UUID. The retired
	// "<user>-agent" pseudo-principal is refused, not checked against FGA.
	bare := strings.TrimPrefix(req.AgentId, "agent:")
	if _, err := uuid.Parse(bare); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid agent_id: %v", err)
	}
	agentRef := "agent:" + bare

	rows, err := s.deps.Mcp.Connections.List(ctx, req.TenantId)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list mcp connections: %v", err)
	}

	out := make([]*brokerv1.McpConnection, 0, len(rows))
	for _, r := range rows {
		object := "mcp_connector:" + r.ID.String()
		allowed, cerr := s.deps.Policy.CheckFGA(ctx, agentRef, "can_access", object)
		if cerr != nil {
			s.deps.Logger.Warn("ListAccessibleMcpServersForAgent: fga check failed",
				zap.String("agent", agentRef), zap.String("object", object), zap.Error(cerr))
			return nil, status.Error(codes.Internal, "authorization check failed")
		}
		if allowed {
			out = append(out, toProtoMcpConnection(r))
		}
	}

	s.emitSouthMcpAudit(ctx, span.SpanContext().TraceID().String(), req.TenantId, agentRef,
		"aikonos.broker.mcp.south.accessible.listed", agentRef, auditv1.PolicyDecision_ALLOW)
	return &brokerv1.ListAccessibleMcpServersResponse{Connections: out}, nil
}

// ListMcpServerToolsSouth is the SPIFFE-south variant of ListMcpServerTools.
// The gateway calls this during session build to register dynamic Pi tools for
// a non-admin user's agent. Same work as the north variant but gated by the
// SPIFFE gateway-peer identity rather than a tenant-admin OIDC token.
func (s *SandboxService) ListMcpServerToolsSouth(ctx context.Context, req *brokerv1.ListMcpServerToolsRequest) (*brokerv1.ListMcpServerToolsResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.mcp.south.list_server_tools")
	defer span.End()

	if err := s.requireGatewayPeer(ctx); err != nil {
		return nil, err
	}
	if s.deps.Mcp.Connections == nil {
		return nil, status.Error(codes.FailedPrecondition, "MCP connections not configured")
	}
	if req.TenantId == "" {
		return nil, status.Error(codes.InvalidArgument, "tenant_id required")
	}
	if req.ConnectorId == "" {
		return nil, status.Error(codes.InvalidArgument, "connector_id required")
	}

	conn, err := s.deps.Mcp.Connections.Get(ctx, req.TenantId, req.ConnectorId)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "mcp connection %q not found: %v", req.ConnectorId, err)
	}

	// AG-08: the model sees only the tools the session's agent may call.
	// No allowlisted tool on this connector lists nothing, without reaching
	// the server.
	allowed, err := s.southAllowedMcpTools(ctx, req, conn.ID.String())
	if err != nil {
		return nil, err
	}
	if len(allowed) == 0 {
		return &brokerv1.ListMcpServerToolsResponse{}, nil
	}

	// An oauth connection lists with the session user's linked account; a
	// user who has not linked it gets FailedPrecondition and the gateway
	// leaves the server out of the session.
	bearer, err := s.deps.mcpToken(ctx, conn, req.TenantId, req.UserId)
	if err != nil {
		return nil, err
	}

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
		if !allowed["mcp:"+conn.ID.String()+":"+t.Name] {
			continue
		}
		out = append(out, &brokerv1.McpToolSchema{
			Name:            t.Name,
			Description:     t.Description,
			InputSchemaJson: t.InputSchemaJSON,
			ReadOnlyHint:    t.ReadOnlyHint,
		})
	}

	s.emitSouthMcpAudit(ctx, span.SpanContext().TraceID().String(), req.TenantId, "agent:"+strings.TrimPrefix(req.AgentId, "agent:"),
		"aikonos.broker.mcp.south.server.tools.listed", req.ConnectorId, auditv1.PolicyDecision_ALLOW)
	return &brokerv1.ListMcpServerToolsResponse{Tools: out}, nil
}

// southAllowedMcpTools returns the "mcp:<conn>:<tool>" ids of connID the
// session may see: the bound agent (req.AgentId) must have the connector
// attached and the tool in its mcp_tools allowlist (AG-08), and a human
// subject (req.UserId) needs its own can_access on the connector when the
// tenant enforces the AG-02 user bound. Audit mode keeps listing; the call
// itself is still checked at InvokeTool. Empty means list nothing.
func (s *SandboxService) southAllowedMcpTools(ctx context.Context, req *brokerv1.ListMcpServerToolsRequest, connID string) (map[string]bool, error) {
	bare := strings.TrimPrefix(req.AgentId, "agent:")
	if _, err := uuid.Parse(bare); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "agent_id: %v", err)
	}
	if req.UserId == "" {
		return nil, status.Error(codes.InvalidArgument, "user_id (the session subject) required")
	}
	if s.deps.Agents == nil {
		return nil, status.Error(codes.FailedPrecondition, "agents store not configured")
	}
	agent, err := s.deps.Agents.Get(ctx, req.TenantId, bare)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "agent %q not found", bare)
	}
	if !slices.Contains(agent.McpServers, connID) {
		return nil, nil
	}
	allowed := map[string]bool{}
	for _, id := range agent.McpTools {
		if c, _, ok := toolregistry.ParseMcpToolID(id); ok && c == connID {
			allowed[id] = true
		}
	}
	if len(allowed) == 0 || strings.HasPrefix(req.UserId, "svc-") || agent.ServiceAgent {
		return allowed, nil
	}
	if d, _ := s.userFGADecision(ctx, &fgaRecord{User: "user:" + req.UserId, Relation: "can_access", Object: "mcp_connector:" + connID}); d != "allow" &&
		s.userBoundMode(ctx, req.TenantId) == "enforce" {
		return nil, nil
	}
	return allowed, nil
}

// emitSouthMcpAudit emits an audit event for a south-bound MCP RPC.
func (s *SandboxService) emitSouthMcpAudit(ctx context.Context, traceID, tenant, actor, eventType, resourceID string, decision auditv1.PolicyDecision) {
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
