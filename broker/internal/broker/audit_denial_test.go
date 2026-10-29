package broker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/capability"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/notify"
	"github.com/adamdekan/aikonos/broker/internal/policy"
	"github.com/adamdekan/aikonos/broker/internal/toolproxy"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
	"github.com/google/uuid"
)

// receiveAuditEvent drains the subscription until an event with the given
// event_type arrives or the timeout elapses.
func receiveAuditEvent(t *testing.T, sub notify.Subscription, wantType string, timeout time.Duration) *auditv1.AuditEvent {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case raw, ok := <-sub.Events():
			if !ok {
				t.Fatal("audit subscription closed before event arrived")
			}
			var ev auditv1.AuditEvent
			if err := json.Unmarshal(raw, &ev); err != nil {
				t.Fatalf("unmarshal audit event: %v", err)
			}
			if ev.EventType == wantType {
				return &ev
			}
		case <-deadline:
			t.Fatalf("timed out waiting for audit event %q", wantType)
		}
	}
}

// buildAuditBus returns an Emitter wired to a MemoryBus and a Subscription for
// the given tenant's audit subject.
func buildAuditBus(t *testing.T, tenantID string) (*audit.Emitter, notify.Subscription) {
	t.Helper()
	bus := notify.NewMemoryBus()
	em, err := audit.NewEmitter(context.Background(), audit.Config{})
	if err != nil {
		t.Fatalf("NewEmitter: %v", err)
	}
	em.AttachBus(bus)
	sub, err := bus.Subscribe(notify.AuditSubject(tenantID))
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(sub.Unsubscribe)
	return em, sub
}

// ── Test 1: InvokeTool unknown tool → DENY event + PermissionDenied ──────────

// TestInvokeTool_UnknownTool_EmitsDenyAndReturnsPermissionDenied verifies that
// calling InvokeTool with an unregistered tool_id emits a DENY audit event of
// type "aikonos.broker.tool.denied" AND still returns codes.PermissionDenied.
// The unknown-tool check runs before the capability gate, so no token needed.
// No task is bound yet, so the event is the broker's own tenant with no actor;
// the request's tenant, user and tool id are only claims in context (F194).
func TestInvokeTool_UnknownTool_EmitsDenyAndReturnsPermissionDenied(t *testing.T) {
	em, sub := buildAuditBus(t, "aikonos-dev")

	svc := NewSandboxService(Deps{GatewaySpiffeID: testGWSpiffeID,
		Logger:   zap.NewNop(),
		Audit:    em,
		TenantID: "aikonos-dev",
	})

	_, err := svc.InvokeTool(asGateway(context.Background()), &brokerv1.InvokeToolRequest{
		StepId:   "1",
		TaskId:   "task-unknown-1",
		ToolId:   "totally.unknown.tool",
		TenantId: testTenantUUID,
		UserId:   "alice@example.com",
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("want PermissionDenied, got %v", err)
	}

	ev := receiveAuditEvent(t, sub, "aikonos.broker.tool.denied", 2*time.Second)
	if ev.Decision != auditv1.PolicyDecision_DENY {
		t.Errorf("Decision = %v, want DENY", ev.Decision)
	}
	if ev.ResourceRef != "aikonos:tool:unknown" {
		t.Errorf("ResourceRef = %q, want %q", ev.ResourceRef, "aikonos:tool:unknown")
	}
	if ev.TenantId != "aikonos-dev" || ev.ActorUserId != "" {
		t.Errorf("tenant/actor = %q/%q, want the broker tenant and no actor", ev.TenantId, ev.ActorUserId)
	}
	if ev.Context == nil {
		t.Fatal("Context must not be nil")
	}
	if reason := ev.Context.Fields["reason"].GetStringValue(); reason != "unknown_tool" {
		t.Errorf("context.reason = %q, want %q", reason, "unknown_tool")
	}
	for k, want := range map[string]string{
		"claimed_tenant_id": testTenantUUID,
		"claimed_user_id":   "alice@example.com",
		"claimed_tool_id":   "totally.unknown.tool",
	} {
		if got := ev.Context.Fields[k].GetStringValue(); got != want {
			t.Errorf("context.%s = %q, want %q", k, got, want)
		}
	}
	// No task row in scope yet: no on-behalf-of chain (AG-17).
	if ev.ActorAgentId != "" || ev.Context.Fields["obo"] != nil {
		t.Errorf("unknown_tool: want no obo chain, got actor_agent_id=%q obo=%v", ev.ActorAgentId, ev.Context.Fields["obo"])
	}
}

// ── Test 2: InvokeTool MCP FGA-denied → mcp_access_denied + PermissionDenied ─

// TestInvokeTool_MCPFGADenied_EmitsMcpAccessDeniedAndReturnsPermissionDenied
// verifies that InvokeTool for an MCP tool where FGA denies can_access emits a
// DENY event with reason "mcp_access_denied" AND returns codes.PermissionDenied.
func TestInvokeTool_MCPFGADenied_EmitsMcpAccessDeniedAndReturnsPermissionDenied(t *testing.T) {
	connID := uuid.New().String()
	mcpSrv := fakeMCPSrvSimple(t)

	m, err := capability.GenerateMinter()
	if err != nil {
		t.Fatalf("GenerateMinter: %v", err)
	}

	em, sub := buildAuditBus(t, testTenantUUID)

	// FGA denies can_access for the agent.
	fgaSvc := &fgaForMcpRouting{
		allowed: map[string]bool{},
		admins:  map[string]bool{},
	}
	fgaSrv := fgaSvc.server()
	t.Cleanup(fgaSrv.Close)

	eng, err := policy.NewEngine(context.Background(), policy.Config{
		OPAEndpoint:     "http://unused",
		OpenFGAEndpoint: fgaSrv.URL,
		OpenFGAStoreID:  "s1",
	})
	if err != nil {
		t.Fatalf("policy.NewEngine: %v", err)
	}

	tenantUUID := uuid.MustParse(testTenantUUID)
	repo := &fakeMcpRepo{
		conns: []*db.McpConnection{{
			ID:       uuid.MustParse(connID),
			TenantID: tenantUUID,
			URL:      mcpSrv.URL,
			AuthType: "none",
		}},
	}

	proxy := toolproxy.New(toolproxy.Config{
		McpConnections:  repo,
		AllowMcpPrivate: true,
	}, zap.NewNop())

	// Agent-bound task: the denial must carry the row's on-behalf-of chain (AG-17).
	agentID := uuid.New()
	tasks := executingTaskStore("alice@example.com")
	tasks.task.AgentID = &agentID
	svc := NewSandboxService(Deps{GatewaySpiffeID: testGWSpiffeID,
		Logger:     zap.NewNop(),
		Capability: m,
		ToolProxy:  proxy,
		Policy:     eng,
		Audit:      em,
		Mcp:        McpDeps{Connections: repo},
		TenantID:   "aikonos-dev",
		Tasks:      tasks,
	})

	raw, err := m.Mint("spiffe://test/agent", "task-mcp-deny-1", tasks.task.OnBehalfOf(), []string{"mcp:" + connID})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	tok := base64.StdEncoding.EncodeToString(raw)
	_, err = svc.InvokeTool(asGateway(context.Background()), &brokerv1.InvokeToolRequest{
		StepId:          "1",
		TaskId:          "task-mcp-deny-1",
		ToolId:          fmt.Sprintf("mcp:%s:echo", connID),
		CapabilityToken: tok,
		TenantId:        testTenantUUID,
		AgentId:         agentID.String(),
		UserId:          "alice@example.com",
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("want PermissionDenied, got %v", err)
	}

	ev := receiveAuditEvent(t, sub, "aikonos.broker.tool.denied", 2*time.Second)
	if ev.Decision != auditv1.PolicyDecision_DENY {
		t.Errorf("Decision = %v, want DENY", ev.Decision)
	}
	if ev.Context == nil {
		t.Fatal("Context must not be nil")
	}
	if reason := ev.Context.Fields["reason"].GetStringValue(); reason != "mcp_access_denied" {
		t.Errorf("context.reason = %q, want %q", reason, "mcp_access_denied")
	}
	if ev.ActorAgentId != agentID.String() {
		t.Errorf("actor_agent_id = %q, want %q", ev.ActorAgentId, agentID)
	}
	obo := ev.Context.Fields["obo"].GetStructValue().GetFields()
	if obo["subject"].GetStringValue() != "alice@example.com" || obo["actor"].GetStringValue() != agentID.String() ||
		obo["service_run"].GetBoolValue() {
		t.Errorf("context.obo = %v, want subject=alice@example.com actor=%s service_run=false", obo, agentID)
	}
}

// ── Test 3: requireTenantAdmin FGA transport error → DENY event + Internal ───

// TestRequireTenantAdmin_FGAError_EmitsDenyAndReturnsInternal verifies that when
// the OpenFGA /check endpoint returns an error (transport failure), requireTenantAdmin
// emits a DENY event with reason "fga_error" AND returns codes.Internal.
func TestRequireTenantAdmin_FGAError_EmitsDenyAndReturnsInternal(t *testing.T) {
	// checkErr=true causes the fake to return HTTP 500 on every /check call.
	f := &fakeFGA{checkErr: true}
	srv := f.server(t)
	defer srv.Close()

	// Admin gate emits with TenantId = s.deps.TenantID ("aikonos-dev"), not the
	// request UUID — subscribe on the broker-process tenant subject.
	em, sub := buildAuditBus(t, "aikonos-dev")

	eng, err := policy.NewEngine(context.Background(), policy.Config{
		OPAEndpoint:     "http://unused",
		OpenFGAEndpoint: srv.URL,
		OpenFGAStoreID:  "store-1",
	})
	if err != nil {
		t.Fatalf("policy.NewEngine: %v", err)
	}
	svc := NewBrokerService(Deps{
		Logger:     zap.NewNop(),
		Audit:      em,
		Policy:     eng,
		TenantID:   "aikonos-dev",
		KnownUsers: []string{"admin@example.com", "alice@example.com"},
	})

	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")
	_, err = svc.ListAssignments(ctx, &brokerv1.ListAssignmentsRequest{
		TenantId: testTenantUUID,
		UserId:   "alice@example.com",
	})
	if status.Code(err) != codes.Internal {
		t.Fatalf("want Internal, got %v", err)
	}

	ev := receiveAuditEvent(t, sub, "aikonos.broker.admin.access.denied", 2*time.Second)
	if ev.Decision != auditv1.PolicyDecision_DENY {
		t.Errorf("Decision = %v, want DENY", ev.Decision)
	}
	if ev.Context == nil {
		t.Fatal("Context must not be nil")
	}
	if reason := ev.Context.Fields["reason"].GetStringValue(); reason != "fga_error" {
		t.Errorf("context.reason = %q, want %q", reason, "fga_error")
	}
}

// ── Test 4: requireTenantAdmin not-admin → DENY event + PermissionDenied ─────

// TestRequireTenantAdmin_NotAdmin_EmitsDenyAndReturnsPermissionDenied verifies
// that ListAssignments called by a non-admin emits a DENY event of type
// "aikonos.broker.admin.access.denied" AND returns codes.PermissionDenied.
//
// The admin denial path uses s.deps.TenantID (the broker process tenant, "aikonos-dev")
// not the request UUID, so we subscribe on that subject.
func TestRequireTenantAdmin_NotAdmin_EmitsDenyAndReturnsPermissionDenied(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()

	// Admin gate emits with TenantId = s.deps.TenantID ("aikonos-dev"), not the
	// request UUID — subscribe on the broker-process tenant subject.
	em, sub := buildAuditBus(t, "aikonos-dev")

	eng, err := policy.NewEngine(context.Background(), policy.Config{
		OPAEndpoint:     "http://unused",
		OpenFGAEndpoint: srv.URL,
		OpenFGAStoreID:  "store-1",
	})
	if err != nil {
		t.Fatalf("policy.NewEngine: %v", err)
	}
	svc := NewBrokerService(Deps{
		Logger:     zap.NewNop(),
		Audit:      em,
		Policy:     eng,
		TenantID:   "aikonos-dev",
		KnownUsers: []string{"admin@example.com", "alice@example.com"},
	})

	// alice is not an admin.
	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")
	_, err = svc.ListAssignments(ctx, &brokerv1.ListAssignmentsRequest{
		TenantId: testTenantUUID,
		UserId:   "alice@example.com",
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("want PermissionDenied, got %v", err)
	}

	ev := receiveAuditEvent(t, sub, "aikonos.broker.admin.access.denied", 2*time.Second)
	if ev.Decision != auditv1.PolicyDecision_DENY {
		t.Errorf("Decision = %v, want DENY", ev.Decision)
	}
	if ev.Context == nil {
		t.Fatal("Context must not be nil")
	}
	if reason := ev.Context.Fields["reason"].GetStringValue(); reason != "not_admin" {
		t.Errorf("context.reason = %q, want %q", reason, "not_admin")
	}
}
