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

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/capability"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/policy"
	"github.com/adamdekan/aikonos/broker/internal/toolproxy"
	"github.com/adamdekan/aikonos/broker/internal/toolregistry"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
	planv1 "github.com/adamdekan/aikonos/gen/go/plan/v1"
	"github.com/google/uuid"
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

// mcpTestAgent is the agent the MCP routing tests' tasks are bound to and the
// one the fake FGA grants can_access.
const mcpTestAgent = "7e57a9e0-0000-4000-8000-000000000001"

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

	agentRef := "agent:" + mcpTestAgent
	agentID := uuid.MustParse(mcpTestAgent)
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
		Tasks:      &fakeInvokeToolTaskStore{task: &db.Task{State: db.TaskStateExecuting, AgentID: &agentID, CostBudget: 100}},
		Agents: &fakeAgentStoreForSkill{agent: &db.Agent{
			ID: agentID, McpServers: []string{connID}, McpTools: []string{"mcp:" + connID + ":echo"},
		}},
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

// mintMcpToken mints a Biscuit scoped to mcp:<connID>, bound to taskID.
func mintMcpToken(t *testing.T, m *capability.Minter, connID, taskID string) string {
	t.Helper()
	raw, err := m.Mint("spiffe://test/agent", taskID, db.OnBehalfOf{}, []string{"mcp:" + connID})
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

	tok := mintMcpToken(t, m, connID, "task-mcp-1")
	resp, err := svc.InvokeTool(context.Background(), &brokerv1.InvokeToolRequest{
		TaskId:          "task-mcp-1",
		ToolId:          fmt.Sprintf("mcp:%s:echo", connID),
		CapabilityToken: tok,
		TenantId:        testTenantUUID,
		AgentId:         mcpTestAgent,
	})
	if err != nil {
		t.Fatalf("InvokeTool: %v", err)
	}
	if !resp.Success {
		t.Fatalf("expected success, got error: %s", resp.Error)
	}
}

// AG-08: the agent's connector grant holds, but the tool is not in its
// McpTools allowlist → PermissionDenied before the MCP server is reached.
func TestInvokeTool_MCP_AG08_ToolNotAllowed_Denied(t *testing.T) {
	mcpSrv := fakeMCPSrvSimple(t)
	connID := uuid.New().String()

	svc, m := buildMcpSandboxSvc(t, connID, mcpSrv.URL, true /* allowed */)
	svc.deps.Agents.(*fakeAgentStoreForSkill).agent.McpTools = []string{"mcp:" + connID + ":other"}

	tok := mintMcpToken(t, m, connID, "task-mcp-1")
	_, err := svc.InvokeTool(context.Background(), &brokerv1.InvokeToolRequest{
		TaskId:          "task-mcp-1",
		ToolId:          fmt.Sprintf("mcp:%s:echo", connID),
		CapabilityToken: tok,
		TenantId:        testTenantUUID,
	})
	if status.Code(err) != codes.PermissionDenied || !strings.Contains(err.Error(), "allowed MCP tools") {
		t.Fatalf("tool outside mcp_tools: want PermissionDenied (allowed MCP tools), got %v", err)
	}
}

// AG-01: a task with no bound agent has no MCP principal; mcp: calls are
// denied, never resolved from the caller.
func TestInvokeTool_MCP_EmptyAgentID_Denied(t *testing.T) {
	mcpSrv := fakeMCPSrvSimple(t)
	connID := uuid.New().String()

	svc, m := buildMcpSandboxSvc(t, connID, mcpSrv.URL, true)
	svc.deps.Tasks = executingTaskStore("") // legacy unbound row

	tok := mintMcpToken(t, m, connID, "task-mcp-2")
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
	svc, m := buildMcpSandboxSvc(t, connID, mcpSrv.URL, true /* mcpTestAgent is FGA-allowed */)

	boundAgent := uuid.New()
	svc.deps.Tasks = &fakeInvokeToolTaskStore{task: &db.Task{State: db.TaskStateExecuting, AgentID: &boundAgent, CostBudget: 100}}

	tok := mintMcpToken(t, m, connID, "task-mcp-mismatch")
	_, err := svc.InvokeTool(context.Background(), &brokerv1.InvokeToolRequest{
		TaskId:          "task-mcp-mismatch",
		ToolId:          fmt.Sprintf("mcp:%s:echo", connID),
		CapabilityToken: tok,
		TenantId:        testTenantUUID,
		AgentId:         mcpTestAgent, // does not match boundAgent
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("agent_id mismatch with bound task: want PermissionDenied, got %v", err)
	}
}

// TestInvokeTool_MCP_TaskLoadTransportError_Denied verifies a transport/DB
// error on the task load is not silently treated like an unbound task: with no
// row there is no identity to run as, so the task binding stops the call
// (Unavailable — no policy decision was made) before the mcp: branch could
// fall back to trusting req.AgentId. A missing row is PermissionDenied.
func TestInvokeTool_MCP_TaskLoadTransportError_Denied(t *testing.T) {
	mcpSrv := fakeMCPSrvSimple(t)
	connID := uuid.New().String()
	svc, m := buildMcpSandboxSvc(t, connID, mcpSrv.URL, true /* mcpTestAgent is FGA-allowed */)
	svc.deps.Tasks = &fakeInvokeToolTaskStore{getErr: errors.New("connection reset by peer")}

	tok := mintMcpToken(t, m, connID, "task-mcp-transport-err")
	_, err := svc.InvokeTool(context.Background(), &brokerv1.InvokeToolRequest{
		TaskId:          "task-mcp-transport-err",
		ToolId:          fmt.Sprintf("mcp:%s:echo", connID),
		CapabilityToken: tok,
		TenantId:        testTenantUUID,
		AgentId:         mcpTestAgent,
	})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("task load transport error: want Unavailable, got %v", err)
	}

	svc.deps.Tasks = &fakeInvokeToolTaskStore{getErr: db.ErrNotFound}
	_, err = svc.InvokeTool(context.Background(), &brokerv1.InvokeToolRequest{
		TaskId:          "task-mcp-transport-err",
		ToolId:          fmt.Sprintf("mcp:%s:echo", connID),
		CapabilityToken: tok,
		TenantId:        testTenantUUID,
		AgentId:         mcpTestAgent,
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("task not found: want PermissionDenied, got %v", err)
	}
}

// TestInvokeTool_MCP_UnboundTaskRefusesCallerAgent (AG-01): the callerAgent
// fallback is gone. A task with no agent binding refuses a caller agent claim,
// even one the FGA would allow, instead of checking the claim.
func TestInvokeTool_MCP_UnboundTaskRefusesCallerAgent(t *testing.T) {
	mcpSrv := fakeMCPSrvSimple(t)
	connID := uuid.New().String()
	svc, m := buildMcpSandboxSvc(t, connID, mcpSrv.URL, true)
	svc.deps.Tasks = &fakeInvokeToolTaskStore{task: &db.Task{State: db.TaskStateExecuting, CostBudget: 100}} // no AgentID bound

	tok := mintMcpToken(t, m, connID, "task-mcp-nobinding")
	_, err := svc.InvokeTool(context.Background(), &brokerv1.InvokeToolRequest{
		TaskId:          "task-mcp-nobinding",
		ToolId:          fmt.Sprintf("mcp:%s:echo", connID),
		CapabilityToken: tok,
		TenantId:        testTenantUUID,
		AgentId:         mcpTestAgent,
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("unbound task with caller agent: want PermissionDenied, got %v", err)
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
	svc := &SandboxService{deps: Deps{Logger: zap.NewNop(), Capability: m, Tasks: executingTaskStore("")}} // Policy left nil

	tok := mintMcpToken(t, m, connID, "task-mcp-nopolicy")
	_, err = svc.InvokeTool(context.Background(), &brokerv1.InvokeToolRequest{
		TaskId:          "task-mcp-nopolicy",
		ToolId:          fmt.Sprintf("mcp:%s:echo", connID),
		CapabilityToken: tok,
		AgentId:         mcpTestAgent,
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

	tok := mintMcpToken(t, m, connID, "task-mcp-3")
	_, err := svc.InvokeTool(context.Background(), &brokerv1.InvokeToolRequest{
		TaskId:          "task-mcp-3",
		ToolId:          fmt.Sprintf("mcp:%s:echo", connID),
		CapabilityToken: tok,
		TenantId:        testTenantUUID,
		AgentId:         mcpTestAgent,
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
	// AG-08: no admin confirmation yet → the floor, unconfirmed.
	if resp.Tools[0].EffectClass != "destructive" || resp.Tools[0].EffectClassConfirmed {
		t.Errorf("unconfirmed tool = (%q,%v), want (destructive,false)", resp.Tools[0].EffectClass, resp.Tools[0].EffectClassConfirmed)
	}
	// An admin-confirmed overlay row is reported as confirmed.
	reg := toolregistry.NewRegistry()
	reg.SetTenantOverlay(testTenantUUID, []toolregistry.OverlayRow{
		{ToolID: "mcp:" + connID + ":echo", Enabled: true, EffectClass: planv1.EffectClass_READ_ONLY, ExecutorKind: "mcp"},
	})
	svc.deps.ToolRegistry = reg
	resp, err = svc.ListMcpServerTools(ctx, &brokerv1.ListMcpServerToolsRequest{ConnectorId: connID})
	if err != nil {
		t.Fatalf("ListMcpServerTools: %v", err)
	}
	if resp.Tools[0].EffectClass != "read_only" || !resp.Tools[0].EffectClassConfirmed {
		t.Errorf("confirmed tool = (%q,%v), want (read_only,true)", resp.Tools[0].EffectClass, resp.Tools[0].EffectClassConfirmed)
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

	agentRef := "agent:" + mcpTestAgent
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
		Agents: &fakeAgentStoreForSkill{agent: &db.Agent{
			ID: uuid.MustParse(mcpTestAgent), McpServers: []string{connID}, McpTools: []string{"mcp:" + connID + ":echo"},
		}},
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
		AgentId:  mcpTestAgent,
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
		AgentId:  mcpTestAgent,
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
		AgentId:  mcpTestAgent,
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
		AgentId:     "agent:" + mcpTestAgent,
		UserId:      "svc-ticker",
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

// AG-08: a tool outside the agent's mcp_tools allowlist is not listed, so the
// model never sees it; a missing agent_id is refused.
func TestListMcpServerToolsSouth_OnlyAllowlistedTools(t *testing.T) {
	mcpSrv := fakeMCPSrvSimple(t)
	connID := uuid.New().String()
	svc := buildSouthSvcForMcp(t, mcpSrv.URL, connID, true, testGatewaySpiffeID)
	svc.deps.Agents.(*fakeAgentStoreForSkill).agent.McpTools = []string{"mcp:" + connID + ":other"}
	ctx := gatewayCtx(testGatewaySpiffeID)

	resp, err := svc.ListMcpServerToolsSouth(ctx, &brokerv1.ListMcpServerToolsRequest{
		TenantId: testTenantUUID, ConnectorId: connID, AgentId: mcpTestAgent, UserId: "svc-ticker",
	})
	if err != nil || len(resp.Tools) != 0 {
		t.Fatalf("echo not allowlisted: want no tools, got %v err=%v", resp, err)
	}
	if _, err := svc.ListMcpServerToolsSouth(ctx, &brokerv1.ListMcpServerToolsRequest{
		TenantId: testTenantUUID, ConnectorId: connID, UserId: "svc-ticker",
	}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("no agent_id: want InvalidArgument, got %v", err)
	}
}

// AG-02: a human subject without can_access on the connector sees none of
// its tools when the tenant enforces; audit mode still lists them.
func TestListMcpServerToolsSouth_UserBound(t *testing.T) {
	mcpSrv := fakeMCPSrvSimple(t)
	connID := uuid.New().String()
	svc := buildSouthSvcForMcp(t, mcpSrv.URL, connID, true, testGatewaySpiffeID)
	ctx := gatewayCtx(testGatewaySpiffeID)
	req := &brokerv1.ListMcpServerToolsRequest{
		TenantId: testTenantUUID, ConnectorId: connID, AgentId: mcpTestAgent, UserId: "alice@example.com",
	}
	for mode, want := range map[string]int{"enforce": 0, "audit": 1} {
		svc.deps.Config = userBoundCfg(mode)
		resp, err := svc.ListMcpServerToolsSouth(ctx, req)
		if err != nil || len(resp.Tools) != want {
			t.Errorf("%s: want %d tools, got %v err=%v", mode, want, resp, err)
		}
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

// AG-01: the retired "<user>-agent" pseudo-principal is refused, and an
// "agent:"-prefixed UUID is accepted like a bare one.
func TestListAccessibleMcpServersForAgent_PseudoAgentRefused(t *testing.T) {
	mcpSrv := fakeMCPSrvSimple(t)
	connID := uuid.New().String()
	svc := buildSouthSvcForMcp(t, mcpSrv.URL, connID, true, testGatewaySpiffeID)
	ctx := gatewayCtx(testGatewaySpiffeID)

	_, err := svc.ListAccessibleMcpServersForAgent(ctx, &brokerv1.ListAccessibleMcpServersRequest{
		TenantId: testTenantUUID, AgentId: "alice-agent",
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("pseudo agent: want InvalidArgument, got %v", err)
	}
	resp, err := svc.ListAccessibleMcpServersForAgent(ctx, &brokerv1.ListAccessibleMcpServersRequest{
		TenantId: testTenantUUID, AgentId: "agent:" + mcpTestAgent,
	})
	if err != nil || len(resp.Connections) != 1 {
		t.Fatalf("prefixed agent: want 1 connection, got %v %v", resp, err)
	}
}

// A loopback MCP server refused by the SSRF guard names the host and the
// switch that allows it, as a caller-class error the web UI shows.
func TestListMcpServerTools_PrivateTargetNamesReason(t *testing.T) {
	mcpSrv := fakeMCPSrvSimple(t)
	connID := uuid.New().String()
	svc := buildMcpBrokerSvc(t, mcpSrv.URL, connID)
	svc.deps.Mcp.AllowPrivate = false

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	_, err := svc.ListMcpServerTools(ctx, &brokerv1.ListMcpServerToolsRequest{ConnectorId: connID})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("want FailedPrecondition, got %v", err)
	}
	msg := status.Convert(err).Message()
	if !strings.Contains(msg, "127.0.0.1 is private/loopback") || !strings.Contains(msg, "AIKONOS_TOOLPROXY_MCP_ALLOW_PRIVATE") || strings.Contains(msg, "http://") {
		t.Errorf("message = %q", msg)
	}
}
