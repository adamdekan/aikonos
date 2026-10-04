package broker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/netacl"
	"github.com/adamdekan/aikonos/broker/internal/policy"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
	planv1 "github.com/adamdekan/aikonos/gen/go/plan/v1"
)

// capturingAudit records every emitted event.
type capturingAudit struct {
	mu     sync.Mutex
	events []*auditv1.AuditEvent
}

func (c *capturingAudit) Emit(_ context.Context, e *auditv1.AuditEvent) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, e)
	return nil
}

func (c *capturingAudit) ofType(eventType string) []*auditv1.AuditEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*auditv1.AuditEvent
	for _, e := range c.events {
		if e.EventType == eventType {
			out = append(out, e)
		}
	}
	return out
}

func field(e *auditv1.AuditEvent, path ...string) any {
	m := e.GetContext().AsMap()
	var cur any = m
	for _, p := range path {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = obj[p]
	}
	return cur
}

func TestRouteLayers_NamesWhatDecided(t *testing.T) {
	opaDec := func() *policy.Decision {
		return &policy.Decision{PolicyRuleID: "opa:tool_invocation", Evidence: &policy.Evidence{Query: "aikonos/tool_invocation"}}
	}
	if _, by := routeLayers(opaDec(), routeTrace{}); by != "opa:aikonos/tool_invocation" {
		t.Errorf("no layer fired: decided_by = %q", by)
	}
	gate := &policy.Decision{PolicyRuleID: "opa:aikonos/tenant_quota"}
	if _, by := routeLayers(gate, routeTrace{}); by != "opa:aikonos/tenant_quota" {
		t.Errorf("an extra gate decided: decided_by = %q", by)
	}

	layers, by := routeLayers(opaDec(), routeTrace{NetaclMutated: true, NetaclHost: "evil.example", NetaclAction: netacl.Ask})
	if by != "netacl" || len(layers) != 1 || layers[0].Effect != "approval_required" || layers[0].Detail["host"] != "evil.example" {
		t.Errorf("netacl ask: %q %+v", by, layers)
	}
	layers, by = routeLayers(opaDec(), routeTrace{NetaclMutated: true, NetaclAction: netacl.Deny, DisabledToolsHit: true,
		ConfigOverride: map[string]any{"source": "config", "class": "network_egress", "posture": "deny"}})
	if by != "tenant_config:effect_class_routing" || len(layers) != 3 || layers[0].Effect != "deny" {
		t.Errorf("several layers: the last one decides; got %q %+v", by, layers)
	}
}

func TestSubmitPlan_RecordsEachDecision(t *testing.T) {
	audit := &capturingAudit{}
	evidence := &policy.Evidence{Query: "aikonos/tool_invocation", Revision: "sha256:abc", Input: map[string]any{"tool": map[string]any{"id": "web.fetch"}},
		InputSHA256: "sha256:def", Result: map[string]any{"require_approval": true}}
	pol := &fakePolicyEngine{planAllow: true, toolDec: &policy.Decision{NeedsApproval: true, Reason: "needs a human",
		PolicyRuleID: "opa:tool_invocation", Evidence: evidence}}
	tasks := &fakeTaskStore{task: newFakeTask(db.TaskStateValidating)}
	svc := &SandboxService{deps: Deps{Logger: zap.NewNop(), Audit: audit, Tasks: tasks}, policy: pol}

	step := makeStep(1, "web.fetch", map[string]any{"url": "https://example.org"})
	step.EffectClass = planv1.EffectClass_NETWORK_EGRESS
	res, err := svc.SubmitPlan(context.Background(), &brokerv1.SubmitPlanRequest{
		TaskId: validTaskUUID(), SandboxSpiffeId: "spiffe://aikonos/sandbox/s1",
		Plan: makePlan(testTenantUUID, []*planv1.PlanStep{step}),
	})
	if err != nil {
		t.Fatalf("SubmitPlan: %v", err)
	}
	if res.Outcome != planv1.ValidationOutcome_NEEDS_HUMAN {
		t.Fatalf("outcome = %v", res.Outcome)
	}

	records := audit.ofType(PolicyDecisionEvent)
	if len(records) != 2 {
		t.Fatalf("want a plan_validation and a tool_invocation record, got %d", len(records))
	}
	var tool *auditv1.AuditEvent
	for _, r := range records {
		if field(r, "kind") == decisionKindToolInvocation {
			tool = r
		}
	}
	if tool == nil {
		t.Fatal("no tool_invocation record")
	}
	if tool.Decision != auditv1.PolicyDecision_APPROVAL_REQUIRED || tool.ResourceRef != "aikonos:task:"+validTaskUUID()+"#1" || tool.ActorUserId != "alice@example.com" {
		t.Errorf("record header: %+v", tool)
	}
	if field(tool, "outcome", "decision") != "approval_required" || field(tool, "outcome", "decided_by") != "opa:aikonos/tool_invocation" {
		t.Errorf("outcome: %v", field(tool, "outcome"))
	}
	if field(tool, "opa", "revision") != "sha256:abc" || field(tool, "opa", "input_sha256") != "sha256:def" {
		t.Errorf("opa evidence: %v", field(tool, "opa"))
	}
	if field(tool, "settings", "effect_class", "routed") != "network_egress" || field(tool, "step", "tool_id") != "web.fetch" {
		t.Errorf("settings/step: %v %v", field(tool, "settings"), field(tool, "step"))
	}

	// The plan-level event carries the approval gate that was set up.
	plans := audit.ofType("aikonos.broker.plan.validated")
	if len(plans) != 1 {
		t.Fatalf("want one plan.validated event, got %d", len(plans))
	}
	if field(plans[0], "approval", "required_approvals") != float64(1) || field(plans[0], "outcome") != "NEEDS_HUMAN" {
		t.Errorf("plan.validated context: %v", plans[0].GetContext().AsMap())
	}
}

func TestSubmitPlan_AgentBoundaryDenialIsRecorded(t *testing.T) {
	audit := &capturingAudit{}
	tasks := &fakeTaskStore{task: newFakeTask(db.TaskStateValidating)}
	agentID := uuid.MustParse(validTaskUUID())
	tasks.task.AgentID = &agentID
	svc := &SandboxService{deps: Deps{Logger: zap.NewNop(), Audit: audit, Tasks: tasks}, policy: &fakePolicyEngine{planAllow: true}}

	res, err := svc.SubmitPlan(context.Background(), &brokerv1.SubmitPlanRequest{
		TaskId: validTaskUUID(), Plan: makePlan(testTenantUUID, []*planv1.PlanStep{makeStep(1, "doc.read", nil)}),
	})
	if err != nil || res.Outcome != planv1.ValidationOutcome_DENIED {
		t.Fatalf("SubmitPlan = %v, %v; want DENIED (no agent store)", res, err)
	}
	records := audit.ofType(PolicyDecisionEvent)
	if len(records) != 1 || field(records[0], "kind") != decisionKindAgentSkills || field(records[0], "outcome", "decided_by") != "broker:agent_skills" {
		t.Fatalf("agent boundary record: %+v", records)
	}
}

// The input digest must survive the audit store's encoding (structpb, then
// encoding/json with HTML escaping) so scripts/replay-decision.sh can
// recompute it from the stored event with jq -cS.
func TestDecisionRecord_InputDigestSurvivesTheAuditEncoding(t *testing.T) {
	opa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"result":{"allow":true},"provenance":{"version":"0.70.0","bundles":{"aikonos":{"revision":"sha256:abc"}}}}`))
	}))
	defer opa.Close()
	eng, err := policy.NewEngine(context.Background(), policy.Config{OPAEndpoint: opa.URL, PolicyBundle: "aikonos"})
	if err != nil {
		t.Fatal(err)
	}
	dec, err := eng.DecideToolCall(context.Background(), policy.ToolQuery{
		ToolID: "mcp:<conn>&co", EffectClass: planv1.EffectClass_READ_ONLY, ActorSpiffeID: "spiffe://aikonos/sandbox/s1",
		FGADecision: "allow", RiskScore: 7, Now: time.Date(2026, 10, 5, 23, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctxStruct, err := recordStruct(decisionRecord{Kind: decisionKindToolInvocation, OPA: dec.Evidence})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := json.Marshal(&auditv1.AuditEvent{EventId: "e1", TenantId: testTenantUUID, Context: ctxStruct})
	if err != nil {
		t.Fatal(err)
	}
	var back struct {
		Context struct {
			OPA struct {
				Input map[string]any `json:"input"`
			} `json:"opa"`
		} `json:"context"`
	}
	if err := json.Unmarshal(stored, &back); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(back.Context.OPA.Input); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(bytes.TrimSuffix(buf.Bytes(), []byte("\n")))
	if got := "sha256:" + hex.EncodeToString(sum[:]); got != dec.Evidence.InputSHA256 {
		t.Fatalf("stored input digests to %s, recorded %s", got, dec.Evidence.InputSHA256)
	}
}
