package broker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/google/uuid"
	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/capability"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/policy"
	"github.com/adamdekan/aikonos/broker/internal/toolproxy"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// ── helpers ──────────────────────────────────────────────────────────────────

// fgaForMcpRouting is a configurable FGA check server.
// It allows can_access for agent→object pairs in the allowed map, and admin for
// any user listed in admins.
type fgaForMcpRouting struct {
	allowed map[string]bool // "agent||can_access||object" → true
	admins  map[string]bool // "user:x" → true (for admin gate in broker RPCs)
}

func (f *fgaForMcpRouting) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case strings.HasSuffix(r.URL.Path, "/check"):
			var req struct {
				TupleKey struct{ User, Relation, Object string } `json:"tuple_key"`
			}
			_ = json.Unmarshal(body, &req)
			allowed := false
			if req.TupleKey.Relation == "admin" {
				allowed = f.admins[req.TupleKey.User]
			} else {
				key := req.TupleKey.User + "||" + req.TupleKey.Relation + "||" + req.TupleKey.Object
				allowed = f.allowed[key]
			}
			_ = json.NewEncoder(w).Encode(map[string]bool{"allowed": allowed})
		case strings.HasSuffix(r.URL.Path, "/read"):
			_ = json.NewEncoder(w).Encode(map[string]any{"tuples": []any{}})
		case strings.HasSuffix(r.URL.Path, "/write"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotImplemented)
		}
	}))
}

func buildMcpSandboxSvc(
	t *testing.T,
	connID string,
	mcpSrvURL string,
	agentCanAccess bool,
) (*SandboxService, *capability.Minter) {
	t.Helper()

	m, err := capability.GenerateMinter()
	if err != nil {
		t.Fatalf("GenerateMinter: %v", err)
	}

	agentRef := "agent:test-agent"
	object := "mcp_connector:" + connID

	fgaSvc := &fgaForMcpRouting{
		allowed: map[string]bool{},
		admins:  map[string]bool{},
	}
	if agentCanAccess {
		fgaSvc.allowed[agentRef+"||can_access||"+object] = true
	}
	fgaSrv := fgaSvc.server()
	t.Cleanup(fgaSrv.Close)

	cfg := policy.Config{OPAEndpoint: "http://unused", OpenFGAEndpoint: fgaSrv.URL, OpenFGAStoreID: "s1"}
	eng, err := policy.NewEngine(context.Background(), cfg)
	if err != nil {
		t.Fatalf("policy.NewEngine: %v", err)
	}

	tenantUUID := uuid.MustParse(testTenantUUID)
	repo := &fakeMcpRepo{
		conns: []*db.McpConnection{{
			ID:       uuid.MustParse(connID),
			TenantID: tenantUUID,
			URL:      mcpSrvURL,
			AuthType: "none",
		}},
	}

	proxy := toolproxy.New(toolproxy.Config{
		McpConnections:  repo,
		AllowMcpPrivate: true,
	}, zap.NewNop())

	em, _ := audit.NewEmitter(context.Background(), audit.Config{})
	svc := NewSandboxService(Deps{
		Logger:     zap.NewNop(),
		Capability: m,
		ToolProxy:  proxy,
		Policy:     eng,
		Audit:      em,
		Mcp:        McpDeps{Connections: repo},
		TenantID:   "aikonos-dev",
	})
	return svc, m
}

// fakeMCPSrvSimple is a minimal MCP httptest server for broker routing tests.
func fakeMCPSrvSimple(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string           `json:"method"`
			ID     *json.RawMessage `json:"id"`
		}
		json.NewDecoder(r.Body).Decode(&req) //nolint:errcheck

		if req.Method == "notifications/initialized" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "sid-routing")
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{
					"protocolVersion": "2025-06-18",
					"capabilities":    map[string]any{},
					"serverInfo":      map[string]any{"name": "test-mcp", "version": "0.1"},
				},
			})
		case "tools/list":
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{"tools": []map[string]any{
					{"name": "echo", "description": "Echo", "inputSchema": map[string]any{"type": "object"}},
				}},
			})
		case "tools/call":
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{
					"content": []map[string]any{{"type": "text", "text": "echo-result"}},
					"isError": false,
				},
			})
		default:
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				"jsonrpc": "2.0", "id": req.ID,
				"error": map[string]any{"code": -32601, "message": "method not found"},
			})
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// mintMcpToken mints a Biscuit scoped to mcp:<connID>.
func mintMcpToken(t *testing.T, m *capability.Minter, connID string) string {
	t.Helper()
	raw, err := m.Mint("spiffe://test/agent", "task-mcp-1", []string{"mcp:" + connID})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

// ── InvokeTool MCP routing tests ─────────────────────────────────────────────

// Agent with FGA can_access + valid capability token routes to the MCP handler.
func TestInvokeTool_MCP_AllowedAgent_RoutesToHandler(t *testing.T) {
	mcpSrv := fakeMCPSrvSimple(t)
	connID := uuid.New().String()

	svc, m := buildMcpSandboxSvc(t, connID, mcpSrv.URL, true /* allowed */)

	tok := mintMcpToken(t, m, connID)
	resp, err := svc.InvokeTool(context.Background(), &brokerv1.InvokeToolRequest{
		TaskId:          "task-mcp-1",
		ToolId:          fmt.Sprintf("mcp:%s:echo", connID),
		CapabilityToken: tok,
		TenantId:        testTenantUUID,
		AgentId:         "test-agent",
	})
	if err != nil {
		t.Fatalf("InvokeTool: %v", err)
	}
	if !resp.Success {
		t.Fatalf("expected success, got error: %s", resp.Error)
	}
}

// Empty agent_id must be denied for mcp: tool calls.
func TestInvokeTool_MCP_EmptyAgentID_Denied(t *testing.T) {
	mcpSrv := fakeMCPSrvSimple(t)
	connID := uuid.New().String()

	svc, m := buildMcpSandboxSvc(t, connID, mcpSrv.URL, true)

	tok := mintMcpToken(t, m, connID)
	_, err := svc.InvokeTool(context.Background(), &brokerv1.InvokeToolRequest{
		TaskId:          "task-mcp-2",
		ToolId:          fmt.Sprintf("mcp:%s:echo", connID),
		CapabilityToken: tok,
		TenantId:        testTenantUUID,
		AgentId:         "", // empty → denied
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("empty agent_id: want PermissionDenied, got %v", err)
	}
}

// TestInvokeTool_MCP_AgentIDMismatchWithBoundTask_Denied verifies CP2 fix 4:
// when the task is loadable and bound to a different agent, a caller-supplied
// req.AgentId that doesn't match it is denied — even though that req.AgentId
// alone would pass the FGA check.
func TestInvokeTool_MCP_AgentIDMismatchWithBoundTask_Denied(t *testing.T) {
	mcpSrv := fakeMCPSrvSimple(t)
	connID := uuid.New().String()
	svc, m := buildMcpSandboxSvc(t, connID, mcpSrv.URL, true /* "test-agent" is FGA-allowed */)

	boundAgent := uuid.New()
	svc.deps.Tasks = &fakeInvokeToolTaskStore{task: &db.Task{AgentID: &boundAgent, CostBudget: 100}}

	tok := mintMcpToken(t, m, connID)
	_, err := svc.InvokeTool(context.Background(), &brokerv1.InvokeToolRequest{
		TaskId:          "task-mcp-mismatch",
		ToolId:          fmt.Sprintf("mcp:%s:echo", connID),
		CapabilityToken: tok,
		TenantId:        testTenantUUID,
		AgentId:         "test-agent", // does not match boundAgent
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("agent_id mismatch with bound task: want PermissionDenied, got %v", err)
	}
}

// TestInvokeTool_MCP_TaskLoadTransportError_Denied verifies a transport/DB
// error on the task load (as opposed to a genuine not-found) is not silently
// treated like an unbound task: it must deny rather than fall back to
// trusting req.AgentId verbatim.
func TestInvokeTool_MCP_TaskLoadTransportError_Denied(t *testing.T) {
	mcpSrv := fakeMCPSrvSimple(t)
	connID := uuid.New().String()
	svc, m := buildMcpSandboxSvc(t, connID, mcpSrv.URL, true /* "test-agent" is FGA-allowed */)
	svc.deps.Tasks = &fakeInvokeToolTaskStore{getErr: errors.New("connection reset by peer")}

	tok := mintMcpToken(t, m, connID)
	_, err := svc.InvokeTool(context.Background(), &brokerv1.InvokeToolRequest{
		TaskId:          "task-mcp-transport-err",
		ToolId:          fmt.Sprintf("mcp:%s:echo", connID),
		CapabilityToken: tok,
		TenantId:        testTenantUUID,
		AgentId:         "test-agent",
	})
	if status.Code(err) != codes.Internal {
		t.Fatalf("task load transport error: want Internal, got %v", err)
	}
}

// TestInvokeTool_MCP_TaskWithNoBoundAgent_Unaffected verifies a loadable task
// that simply has no agent_id binding (personal task) skips the mismatch
// check entirely — same as a task store that isn't wired at all.
func TestInvokeTool_MCP_TaskWithNoBoundAgent_Unaffected(t *testing.T) {
	mcpSrv := fakeMCPSrvSimple(t)
	connID := uuid.New().String()
	svc, m := buildMcpSandboxSvc(t, connID, mcpSrv.URL, true)
	svc.deps.Tasks = &fakeInvokeToolTaskStore{task: &db.Task{CostBudget: 100}} // no AgentID bound

	tok := mintMcpToken(t, m, connID)
	resp, err := svc.InvokeTool(context.Background(), &brokerv1.InvokeToolRequest{
		TaskId:          "task-mcp-nobinding",
		ToolId:          fmt.Sprintf("mcp:%s:echo", connID),
		CapabilityToken: tok,
		TenantId:        testTenantUUID,
		AgentId:         "test-agent",
	})
	if err != nil {
		t.Fatalf("InvokeTool: %v", err)
	}
	if !resp.Success {
		t.Fatalf("expected success, got error: %s", resp.Error)
	}
}

// TestInvokeTool_MCP_NilPolicy_DeniesInsteadOfPanicking verifies CP2 fix 4's
// nil-Policy guard: an mcp: call with no Policy engine wired must fail closed
// (PermissionDenied) rather than nil-pointer-panic on CheckFGA.
func TestInvokeTool_MCP_NilPolicy_DeniesInsteadOfPanicking(t *testing.T) {
	m, err := capability.GenerateMinter()
	if err != nil {
		t.Fatalf("GenerateMinter: %v", err)
	}
	connID := uuid.New().String()
	svc := &SandboxService{deps: Deps{Logger: zap.NewNop(), Capability: m}} // Policy left nil

	tok := mintMcpToken(t, m, connID)
	_, err = svc.InvokeTool(context.Background(), &brokerv1.InvokeToolRequest{
		TaskId:          "task-mcp-nopolicy",
		ToolId:          fmt.Sprintf("mcp:%s:echo", connID),
		CapabilityToken: tok,
		AgentId:         "test-agent",
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("nil Policy: want PermissionDenied (fail closed), got %v", err)
	}
}

// Agent without FGA can_access must be denied.
func TestInvokeTool_MCP_DeniedByFGA(t *testing.T) {
	mcpSrv := fakeMCPSrvSimple(t)
	connID := uuid.New().String()

	svc, m := buildMcpSandboxSvc(t, connID, mcpSrv.URL, false /* denied */)

	tok := mintMcpToken(t, m, connID)
	_, err := svc.InvokeTool(context.Background(), &brokerv1.InvokeToolRequest{
		TaskId:          "task-mcp-3",
		ToolId:          fmt.Sprintf("mcp:%s:echo", connID),
		CapabilityToken: tok,
		TenantId:        testTenantUUID,
		AgentId:         "test-agent",
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("FGA deny: want PermissionDenied, got %v", err)
	}
}

// ── ListMcpServerTools tests ─────────────────────────────────────────────────

func buildMcpBrokerSvc(t *testing.T, mcpSrvURL, connID string) *BrokerService {
	t.Helper()

	fgaSvc := &fgaForMcpRouting{
		admins: map[string]bool{"user:admin@example.com": true},
	}
	fgaSrv := fgaSvc.server()
	t.Cleanup(fgaSrv.Close)

	em, _ := audit.NewEmitter(context.Background(), audit.Config{})
	cfg := policy.Config{OPAEndpoint: "http://unused", OpenFGAEndpoint: fgaSrv.URL, OpenFGAStoreID: "s1"}
	eng, _ := policy.NewEngine(context.Background(), cfg)

	tenantUUID := uuid.MustParse(testTenantUUID)
	repo := &fakeMcpRepo{
		conns: []*db.McpConnection{{
			ID:       uuid.MustParse(connID),
			TenantID: tenantUUID,
			URL:      mcpSrvURL,
			AuthType: "none",
		}},
	}

	return NewBrokerService(Deps{
		Logger:   zap.NewNop(),
		Audit:    em,
		Policy:   eng,
		Mcp:      McpDeps{Connections: repo, AllowPrivate: true},
		TenantID: "aikonos-dev",
	})
}

// ListMcpServerTools returns schemas from the fake server.
func TestListMcpServerTools_ReturnsTools(t *testing.T) {
	mcpSrv := fakeMCPSrvSimple(t)
	connID := uuid.New().String()

	svc := buildMcpBrokerSvc(t, mcpSrv.URL, connID)

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	resp, err := svc.ListMcpServerTools(ctx, &brokerv1.ListMcpServerToolsRequest{
		ConnectorId: connID,
	})
	if err != nil {
		t.Fatalf("ListMcpServerTools: %v", err)
	}
	if len(resp.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(resp.Tools))
	}
	if resp.Tools[0].Name != "echo" {
		t.Errorf("tool name = %q, want %q", resp.Tools[0].Name, "echo")
	}
}

// Non-admin must get PermissionDenied.
func TestListMcpServerTools_NonAdminDenied(t *testing.T) {
	mcpSrv := fakeMCPSrvSimple(t)
	connID := uuid.New().String()
	svc := buildMcpBrokerSvc(t, mcpSrv.URL, connID)

	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")
	_, err := svc.ListMcpServerTools(ctx, &brokerv1.ListMcpServerToolsRequest{
		ConnectorId: connID,
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-admin: want PermissionDenied, got %v", err)
	}
}

// Missing connector_id must return InvalidArgument.
func TestListMcpServerTools_MissingConnectorID_InvalidArgument(t *testing.T) {
	mcpSrv := fakeMCPSrvSimple(t)
	connID := uuid.New().String()
	svc := buildMcpBrokerSvc(t, mcpSrv.URL, connID)

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	_, err := svc.ListMcpServerTools(ctx, &brokerv1.ListMcpServerToolsRequest{})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("missing connector_id: want InvalidArgument, got %v", err)
	}
}

// ── South RPC tests (SPIFFE-gated) ───────────────────────────────────────────

// buildSouthSvcForMcp builds a SandboxService with MCP + FGA wired, using the
// given gatewaySpiffeID (mirrors buildMcpSandboxSvc but with MCP discovery deps).
func buildSouthSvcForMcp(
	t *testing.T,
	mcpSrvURL string,
	connID string,
	agentCanAccess bool,
	gatewaySpiffeID string,
) *SandboxService {
	t.Helper()

	agentRef := "agent:test-agent"
	object := "mcp_connector:" + connID

	fgaSvc := &fgaForMcpRouting{
		allowed: map[string]bool{},
		admins:  map[string]bool{},
	}
	if agentCanAccess {
		fgaSvc.allowed[agentRef+"||can_access||"+object] = true
	}
	fgaSrv := fgaSvc.server()
	t.Cleanup(fgaSrv.Close)

	cfg := policy.Config{OPAEndpoint: "http://unused", OpenFGAEndpoint: fgaSrv.URL, OpenFGAStoreID: "s1"}
	eng, err := policy.NewEngine(context.Background(), cfg)
	if err != nil {
		t.Fatalf("policy.NewEngine: %v", err)
	}

	tenantUUID := uuid.MustParse(testTenantUUID)
	repo := &fakeMcpRepo{
		conns: []*db.McpConnection{{
			ID:       uuid.MustParse(connID),
			TenantID: tenantUUID,
			URL:      mcpSrvURL,
			AuthType: "none",
		}},
	}

	em, _ := audit.NewEmitter(context.Background(), audit.Config{})
	return NewSandboxService(Deps{
		Logger:          zap.NewNop(),
		Policy:          eng,
		Audit:           em,
		Mcp:             McpDeps{Connections: repo, AllowPrivate: true},
		TenantID:        "aikonos-dev",
		GatewaySpiffeID: gatewaySpiffeID,
	})
}

const testGatewaySpiffeID = "spiffe://aikonos.com/agent-gateway"

// ListAccessibleMcpServersForAgent: gateway SVID + FGA access → returns the allowed connection.
func TestListAccessibleMcpServersForAgent_GatewayAllowed(t *testing.T) {
	mcpSrv := fakeMCPSrvSimple(t)
	connID := uuid.New().String()

	svc := buildSouthSvcForMcp(t, mcpSrv.URL, connID, true /* agentCanAccess */, testGatewaySpiffeID)

	ctx := gatewayCtx(testGatewaySpiffeID)
	resp, err := svc.ListAccessibleMcpServersForAgent(ctx, &brokerv1.ListAccessibleMcpServersRequest{
		TenantId: testTenantUUID,
		AgentId:  "test-agent",
	})
	if err != nil {
		t.Fatalf("ListAccessibleMcpServersForAgent: %v", err)
	}
	if len(resp.Connections) != 1 {
		t.Fatalf("expected 1 connection, got %d", len(resp.Connections))
	}
	if resp.Connections[0].GetId() != connID {
		t.Errorf("expected conn %s, got %s", connID, resp.Connections[0].GetId())
	}
}

// ListAccessibleMcpServersForAgent: FGA denies → empty list (not an error).
func TestListAccessibleMcpServersForAgent_FGADenied_EmptyList(t *testing.T) {
	mcpSrv := fakeMCPSrvSimple(t)
	connID := uuid.New().String()

	svc := buildSouthSvcForMcp(t, mcpSrv.URL, connID, false /* agentCanAccess */, testGatewaySpiffeID)

	ctx := gatewayCtx(testGatewaySpiffeID)
	resp, err := svc.ListAccessibleMcpServersForAgent(ctx, &brokerv1.ListAccessibleMcpServersRequest{
		TenantId: testTenantUUID,
		AgentId:  "test-agent",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.Connections) != 0 {
		t.Fatalf("expected 0 connections (FGA deny), got %d", len(resp.Connections))
	}
}

// ListAccessibleMcpServersForAgent: non-gateway SPIFFE ID → PermissionDenied.
func TestListAccessibleMcpServersForAgent_NonGatewayPeer_Denied(t *testing.T) {
	mcpSrv := fakeMCPSrvSimple(t)
	connID := uuid.New().String()

	svc := buildSouthSvcForMcp(t, mcpSrv.URL, connID, true, testGatewaySpiffeID)

	// A sandbox SVID — not the gateway.
	ctx := gatewayCtx("spiffe://aikonos.com/some-sandbox")
	_, err := svc.ListAccessibleMcpServersForAgent(ctx, &brokerv1.ListAccessibleMcpServersRequest{
		TenantId: testTenantUUID,
		AgentId:  "test-agent",
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-gateway peer: want PermissionDenied, got %v", err)
	}
}

// ListMcpServerToolsSouth: gateway SVID → returns tools from the fake MCP server.
func TestListMcpServerToolsSouth_GatewayAllowed(t *testing.T) {
	mcpSrv := fakeMCPSrvSimple(t)
	connID := uuid.New().String()

	svc := buildSouthSvcForMcp(t, mcpSrv.URL, connID, true, testGatewaySpiffeID)

	ctx := gatewayCtx(testGatewaySpiffeID)
	resp, err := svc.ListMcpServerToolsSouth(ctx, &brokerv1.ListMcpServerToolsRequest{
		TenantId:    testTenantUUID,
		ConnectorId: connID,
	})
	if err != nil {
		t.Fatalf("ListMcpServerToolsSouth: %v", err)
	}
	if len(resp.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(resp.Tools))
	}
	if resp.Tools[0].Name != "echo" {
		t.Errorf("tool name = %q, want %q", resp.Tools[0].Name, "echo")
	}
}

// ListMcpServerToolsSouth: non-gateway SPIFFE ID → PermissionDenied.
func TestListMcpServerToolsSouth_NonGatewayPeer_Denied(t *testing.T) {
	mcpSrv := fakeMCPSrvSimple(t)
	connID := uuid.New().String()

	svc := buildSouthSvcForMcp(t, mcpSrv.URL, connID, true, testGatewaySpiffeID)

	ctx := gatewayCtx("spiffe://aikonos.com/some-peer")
	_, err := svc.ListMcpServerToolsSouth(ctx, &brokerv1.ListMcpServerToolsRequest{
		TenantId:    testTenantUUID,
		ConnectorId: connID,
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-gateway peer: want PermissionDenied, got %v", err)
	}
}
