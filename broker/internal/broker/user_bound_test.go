package broker

// user_bound_test.go — AG-02: an agent is bounded by the user it acts for.
//
//   - agent-bound task, human owner, skill the owner lacks: enforce denies the
//     step with the unbound-task reason; audit emits a would-deny and allows.
//   - human owner without can_access on the connector: InvokeTool emits
//     mcp_user_access_would_deny (audit) / denies mcp_user_access_denied
//     (enforce); plan time checks the same connector grant.
//   - svc- owners and service agents keep today's behaviour and carry
//     user_bound_exempt / settings.user_bound.exempt in their audit events.

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/capability"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/notify"
	"github.com/adamdekan/aikonos/broker/internal/policy"
	"github.com/adamdekan/aikonos/broker/internal/toolproxy"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
	planv1 "github.com/adamdekan/aikonos/gen/go/plan/v1"
)

const ubConn = "d0235c1c-d5db-45ae-95e1-8f38f6e682a9"

func userBoundCfg(mode string) *fakeConfigStore {
	cfg := newFakeConfigStore()
	if mode != "" {
		cfg.values[testTenantUUID+":"+userBoundSetting] = mode
	}
	return cfg
}

// submitUserBound runs SubmitPlan for one step on a task bound to an agent that
// holds the tool (skill or connector), with the owner's FGA check denied.
func submitUserBound(t *testing.T, owner, toolID, mode string, serviceAgent bool) (*planv1.PlanValidationResult, *fakeSkillPolicy, notify.Subscription) {
	t.Helper()
	aid := uuid.MustParse(testAgentIDStr)
	task := newFakeTask(db.TaskStateCreated)
	task.OwnerUserID = owner
	task.AgentID = &aid
	agents := &fakeAgentStoreForSkill{agent: &db.Agent{
		ID: aid, TenantID: uuid.MustParse(testTenantUUID), Name: "test-agent",
		Skills: []string{"web.fetch"}, McpServers: []string{ubConn}, McpTools: []string{"mcp:" + ubConn + ":echo"}, ServiceAgent: serviceAgent,
	}}
	sp := &fakeSkillPolicy{enabled: true, granted: false}
	em, sub := buildAuditBus(t, testTenantUUID)
	svc := &SandboxService{deps: Deps{
		Logger: zap.NewNop(), Audit: em, Tasks: &fakeTaskStore{task: task},
		Agents: agents, Config: userBoundCfg(mode), DevMode: true,
	}, policy: allowPol(), skillPolicy: sp}
	res, err := svc.SubmitPlan(context.Background(), &brokerv1.SubmitPlanRequest{
		TaskId: validTaskUUID(),
		Plan:   makePlan(testTenantUUID, []*planv1.PlanStep{makeStep(1, toolID, nil)}),
	})
	if err != nil {
		t.Fatalf("SubmitPlan: %v", err)
	}
	return res, sp, sub
}

func TestUserBound_Plan_AgentBoundSkill_Enforce_DeniedLikeUnbound(t *testing.T) {
	res, sp, _ := submitUserBound(t, "alice@example.com", "web.fetch", "enforce", false)
	if res.Outcome != planv1.ValidationOutcome_DENIED {
		t.Fatalf("outcome = %v, want DENIED", res.Outcome)
	}
	if sp.lastObject != "skill:web.fetch" || sp.lastUser != "user:alice@example.com" {
		t.Errorf("check = %s on %s, want the owner's skill grant", sp.lastUser, sp.lastObject)
	}
	// Same reason a personal (unbound) task gets today.
	unbound, _ := (&fakePolicyEngine{}).DecideToolCall(context.Background(), policy.ToolQuery{ToolID: "web.fetch", FGADecision: "deny"})
	if got := res.StepDecisions[0].DenyReason; got != unbound.Reason {
		t.Errorf("deny reason = %q, want %q", got, unbound.Reason)
	}
}

func TestUserBound_Plan_AgentBoundSkill_Audit_WouldDenyAndAllows(t *testing.T) {
	res, sp, sub := submitUserBound(t, "alice@example.com", "web.fetch", "", false)
	if res.Outcome != planv1.ValidationOutcome_APPROVED {
		t.Fatalf("audit mode: outcome = %v, want APPROVED", res.Outcome)
	}
	if sp.checkFGACalls != 1 {
		t.Errorf("CheckFGA calls = %d, want 1", sp.checkFGACalls)
	}
	ev := receiveAuditEvent(t, sub, userBoundWouldDenyEvent, 2*time.Second)
	f := ev.Context.Fields
	if f["reason"].GetStringValue() != "user_skill_would_deny" || f["user"].GetStringValue() != "user:alice@example.com" ||
		f["agent"].GetStringValue() != testAgentIDStr || f["tool_id"].GetStringValue() != "web.fetch" {
		t.Errorf("would-deny context = %v", f)
	}
}

func TestUserBound_Plan_Mcp_ChecksConnectorGrant(t *testing.T) {
	tool := "mcp:" + ubConn + ":echo"
	res, sp, sub := submitUserBound(t, "alice@example.com", tool, "", false)
	if res.Outcome != planv1.ValidationOutcome_APPROVED {
		t.Fatalf("audit mode: outcome = %v, want APPROVED", res.Outcome)
	}
	if sp.lastRelation != "can_access" || sp.lastObject != "mcp_connector:"+ubConn {
		t.Errorf("check = %s %s, want can_access mcp_connector:%s", sp.lastRelation, sp.lastObject, ubConn)
	}
	ev := receiveAuditEvent(t, sub, userBoundWouldDenyEvent, 2*time.Second)
	if r := ev.Context.Fields["reason"].GetStringValue(); r != "mcp_user_access_would_deny" {
		t.Errorf("reason = %q", r)
	}
	if c := ev.Context.Fields["connector"].GetStringValue(); c != ubConn {
		t.Errorf("connector = %q", c)
	}

	res, _, _ = submitUserBound(t, "alice@example.com", tool, "enforce", false)
	if res.Outcome != planv1.ValidationOutcome_DENIED {
		t.Fatalf("enforce: outcome = %v, want DENIED", res.Outcome)
	}
}

// Personal task (no agent): the mcp: user check runs on the connector, never on
// the invalid skill:mcp:… object.
func TestUserBound_Plan_PersonalMcp_ChecksConnectorNotSkill(t *testing.T) {
	sp := &fakeSkillPolicy{enabled: true, granted: true}
	svc := newSvcWithSkillPolicy(personalTask(), allowPol(), sp)
	res, err := svc.SubmitPlan(context.Background(), &brokerv1.SubmitPlanRequest{
		TaskId: validTaskUUID(),
		Plan:   makePlan(testTenantUUID, []*planv1.PlanStep{makeStep(1, "mcp:"+ubConn+":echo", nil)}),
	})
	if err != nil || res.Outcome != planv1.ValidationOutcome_APPROVED {
		t.Fatalf("outcome = %v err = %v, want APPROVED", res.GetOutcome(), err)
	}
	if sp.lastObject != "mcp_connector:"+ubConn {
		t.Errorf("object = %q, want the connector", sp.lastObject)
	}
}

func TestUserBound_Plan_ExemptKeepsTodayAndIsFlagged(t *testing.T) {
	for _, tc := range []struct {
		name, owner, want string
		serviceAgent      bool
	}{
		{"svc owner", "svc-" + testAgentIDStr, "service_run", false},
		{"service agent", "alice@example.com", "service_agent", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, sp, sub := submitUserBound(t, tc.owner, "web.fetch", "enforce", tc.serviceAgent)
			if res.Outcome != planv1.ValidationOutcome_APPROVED {
				t.Fatalf("outcome = %v, want APPROVED", res.Outcome)
			}
			if sp.checkFGACalls != 0 {
				t.Errorf("CheckFGA calls = %d, want 0", sp.checkFGACalls)
			}
			for {
				ev := receiveAuditEvent(t, sub, PolicyDecisionEvent, 2*time.Second)
				if ev.Context.Fields["kind"].GetStringValue() != decisionKindToolInvocation {
					continue
				}
				ub := ev.Context.Fields["settings"].GetStructValue().GetFields()["user_bound"].GetStructValue().GetFields()
				if got := ub["exempt"].GetStringValue(); got != tc.want {
					t.Errorf("settings.user_bound.exempt = %q, want %q", got, tc.want)
				}
				return
			}
		})
	}
}

// ── InvokeTool ────────────────────────────────────────────────────────────────

// invokeUserBound runs one mcp: call on an agent-bound task whose agent holds
// the connector; userGrant decides the owner's own can_access.
func invokeUserBound(t *testing.T, owner, mode string, userGrant, serviceAgent bool) (error, notify.Subscription) {
	t.Helper()
	mcpSrv := fakeMCPSrvSimple(t)
	m, err := capability.GenerateMinter()
	if err != nil {
		t.Fatal(err)
	}
	em, sub := buildAuditBus(t, testTenantUUID)
	agentID := uuid.New()
	object := "mcp_connector:" + ubConn
	fgaSvc := &fgaForMcpRouting{allowed: map[string]bool{"agent:" + agentID.String() + "||can_access||" + object: true}, admins: map[string]bool{}}
	if userGrant {
		fgaSvc.allowed["user:"+owner+"||can_access||"+object] = true
	}
	fgaSrv := fgaSvc.server()
	t.Cleanup(fgaSrv.Close)
	eng, err := policy.NewEngine(context.Background(), policy.Config{OPAEndpoint: "http://unused", OpenFGAEndpoint: fgaSrv.URL, OpenFGAStoreID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	repo := &fakeMcpRepo{conns: []*db.McpConnection{{ID: uuid.MustParse(ubConn), TenantID: uuid.MustParse(testTenantUUID), URL: mcpSrv.URL, AuthType: "none"}}}
	tasks := executingTaskStore(owner)
	tasks.task.AgentID = &agentID
	svc := NewSandboxService(Deps{
		Logger: zap.NewNop(), Capability: m, Policy: eng, Audit: em,
		ToolProxy: toolproxy.New(toolproxy.Config{McpConnections: repo, AllowMcpPrivate: true}, zap.NewNop()),
		Mcp:       McpDeps{Connections: repo}, TenantID: "aikonos-dev", Tasks: tasks,
		Config: userBoundCfg(mode),
		Agents: &fakeAgentStoreForSkill{agent: &db.Agent{ID: agentID, ServiceAgent: serviceAgent, McpTools: []string{"mcp:" + ubConn + ":echo"}}},
	})
	raw, err := m.Mint("spiffe://test/agent", "task-ub-1", tasks.task.OnBehalfOf(), []string{"mcp:" + ubConn})
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.InvokeTool(context.Background(), &brokerv1.InvokeToolRequest{
		TaskId: "task-ub-1", ToolId: fmt.Sprintf("mcp:%s:echo", ubConn),
		CapabilityToken: base64.StdEncoding.EncodeToString(raw), TenantId: testTenantUUID,
	})
	return err, sub
}

func TestUserBound_Invoke_NoUserGrant_Audit_WouldDeny(t *testing.T) {
	err, sub := invokeUserBound(t, "alice@example.com", "", false, false)
	if err != nil {
		t.Fatalf("audit mode: want call to proceed, got %v", err)
	}
	ev := receiveAuditEvent(t, sub, userBoundWouldDenyEvent, 2*time.Second)
	f := ev.Context.Fields
	if f["reason"].GetStringValue() != "mcp_user_access_would_deny" || f["user"].GetStringValue() != "user:alice@example.com" ||
		!strings.HasPrefix(f["agent"].GetStringValue(), "agent:") || f["connector"].GetStringValue() != ubConn {
		t.Errorf("would-deny context = %v", f)
	}
	if ev.Context.Fields["obo"] == nil {
		t.Error("would-deny must carry the on-behalf-of chain")
	}
	receiveAuditEvent(t, sub, "aikonos.broker.tool.invoked", 2*time.Second)
}

func TestUserBound_Invoke_NoUserGrant_Enforce_Denied(t *testing.T) {
	err, sub := invokeUserBound(t, "alice@example.com", "enforce", false, false)
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("enforce: want PermissionDenied, got %v", err)
	}
	ev := receiveAuditEvent(t, sub, "aikonos.broker.tool.denied", 2*time.Second)
	f := ev.Context.Fields
	if f["reason"].GetStringValue() != "mcp_user_access_denied" || f["connector"].GetStringValue() != ubConn || f["user"].GetStringValue() != "user:alice@example.com" {
		t.Errorf("denied context = %v", f)
	}
}

func TestUserBound_Invoke_UserGrant_Enforce_Allowed(t *testing.T) {
	if err, _ := invokeUserBound(t, "alice@example.com", "enforce", true, false); err != nil {
		t.Fatalf("permitted user: want allowed, got %v", err)
	}
}

func TestUserBound_Invoke_ExemptKeepsTodayAndIsFlagged(t *testing.T) {
	for _, tc := range []struct {
		name, owner, want string
		serviceAgent      bool
	}{
		{"svc owner", "svc-ext", "service_run", false},
		{"service agent", "alice@example.com", "service_agent", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// enforce, no user grant: an exempt call still runs on the agent grant.
			err, sub := invokeUserBound(t, tc.owner, "enforce", false, tc.serviceAgent)
			if err != nil {
				t.Fatalf("want allowed, got %v", err)
			}
			ev := receiveAuditEvent(t, sub, "aikonos.broker.tool.invoked", 2*time.Second)
			if got := ev.Context.Fields["user_bound_exempt"].GetStringValue(); got != tc.want {
				t.Errorf("user_bound_exempt = %q, want %q", got, tc.want)
			}
		})
	}
}
