package broker

// AG-07 slice 1: ListPendingApprovals (approver_set + FGA can_approve, digest
// round trip into ApproveTask) and the approval expiry sweep. The approver_set
// SQL filter and the n-of-m vote are covered against Postgres in
// db/approvals_pending_test.go; here the fake store returns per-user rows as
// that query would.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/policy"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

type pendingApprovalsStore struct {
	fakeApproveTaskStore
	byUser map[string][]db.ApprovalRequest // what ListPendingApprovalsFor's SQL returns per user
	steps  []db.ExecutableStep
}

func (f *pendingApprovalsStore) ListPendingApprovalsFor(_ context.Context, _, user string) ([]db.ApprovalRequest, error) {
	return f.byUser[user], nil
}

func (f *pendingApprovalsStore) ListMintableSteps(_ context.Context, _, _ string) ([]db.ExecutableStep, error) {
	return f.steps, nil
}

func TestListPendingApprovals_TwoApproversOneOutsider(t *testing.T) {
	gated := db.ApprovalRequest{
		ApprovalID: uuid.New(), TaskID: uuid.New(), RequesterID: "alice@example.com",
		ApproverSet: []string{"bob@example.com", "carol@example.com"}, RequiresN: 2,
		ApprovedBy: []string{"bob@example.com"}, Summary: map[string]any{"step_up": true},
		State: db.ApprovalPending, ExpiresAt: time.Now().Add(time.Hour),
	}
	// A legacy row with an empty approver_set is visible to everyone by the SQL
	// rule; the FGA can_approve check must still hide it from the outsider.
	legacy := gated
	legacy.ApprovalID, legacy.TaskID, legacy.ApproverSet, legacy.ApprovedBy = uuid.New(), uuid.New(), nil, nil
	store := &pendingApprovalsStore{
		fakeApproveTaskStore: fakeApproveTaskStore{ar: &gated},
		byUser: map[string][]db.ApprovalRequest{
			"carol@example.com": {gated},
			"dave@example.com":  {legacy},
		},
		steps: []db.ExecutableStep{{Seq: 1, ToolID: "email.send", Args: map[string]any{"to": "x@example.com"}}},
	}
	fga := &fakeFGA{checks: map[string]bool{
		"user:carol@example.com|can_approve|task:" + gated.TaskID.String(): true,
	}}
	srv := fga.server(t)
	t.Cleanup(srv.Close)
	em, err := audit.NewEmitter(context.Background(), audit.Config{})
	if err != nil {
		t.Fatal(err)
	}
	eng, err := policy.NewEngine(context.Background(), policy.Config{OPAEndpoint: "http://unused", OpenFGAEndpoint: srv.URL, OpenFGAStoreID: "store-1"})
	if err != nil {
		t.Fatal(err)
	}
	svc := NewBrokerService(Deps{Logger: zap.NewNop(), Audit: em, Policy: eng, Tasks: store})

	list := func(user string) []*brokerv1.PendingApproval {
		t.Helper()
		resp, err := svc.ListPendingApprovals(ctxWithIdentity(testTenantUUID, user), &brokerv1.ListPendingApprovalsRequest{})
		if err != nil {
			t.Fatalf("ListPendingApprovals(%s): %v", user, err)
		}
		return resp.Approvals
	}
	if got := list("dave@example.com"); len(got) != 0 {
		t.Fatalf("outsider sees %d approvals, want 0", len(got))
	}
	got := list("carol@example.com")
	if len(got) != 1 {
		t.Fatalf("second approver sees %d approvals, want 1", len(got))
	}
	pa := got[0]
	if pa.TaskId != gated.TaskID.String() || pa.RequiresN != 2 || pa.Approvals != 1 || !pa.StepUp ||
		len(pa.Steps) != 1 || pa.Steps[0].Args.Fields["to"].GetStringValue() != "x@example.com" {
		t.Fatalf("unexpected pending approval: %+v", pa)
	}

	// The second approver completes the gate from their own session with the
	// digest the list handed out.
	resp, err := svc.ApproveTask(ctxWithIdentity(testTenantUUID, "carol@example.com"), &brokerv1.ApproveTaskRequest{
		TaskId: pa.TaskId, Approved: true, ArgsDigest: pa.ArgsDigest,
	})
	if err != nil || resp.NewStatus != brokerv1.TaskStatus_APPROVED {
		t.Fatalf("ApproveTask with listed digest: resp=%v err=%v", resp, err)
	}
}

type expiryStore struct {
	rows        []db.ExpiredApproval
	expired     map[string]bool // approval id → ExpireApproval result
	transitions []string
}

func (f *expiryStore) ListExpiredApprovals(context.Context) ([]db.ExpiredApproval, error) {
	return f.rows, nil
}

func (f *expiryStore) ExpireApproval(_ context.Context, _, id string) (bool, error) {
	return f.expired[id], nil
}

func (f *expiryStore) Transition(_ context.Context, _, taskID string, from, to db.TaskState) error {
	f.transitions = append(f.transitions, taskID+":"+string(from)+"->"+string(to))
	return nil
}

func (f *expiryStore) Get(context.Context, string, string) (*db.Task, error) {
	return &db.Task{OwnerUserID: "alice@example.com"}, nil
}

// AG-07 (M-09): the sweep times out the task of every approval it expires,
// audits each, and leaves a row another sweeper already took alone.
func TestSweepExpiredApprovals(t *testing.T) {
	store := &expiryStore{
		rows: []db.ExpiredApproval{
			{TenantID: testTenantUUID, ApprovalID: "a1", TaskID: "t1"},
			{TenantID: testTenantUUID, ApprovalID: "a2", TaskID: "t2"},
		},
		expired: map[string]bool{"a1": true}, // a2 resolved meanwhile
	}
	em := &recordingEmitter{}
	if n := SweepExpiredApprovals(context.Background(), store, em, zap.NewNop()); n != 1 {
		t.Fatalf("expired %d, want 1", n)
	}
	if len(store.transitions) != 1 || store.transitions[0] != "t1:AWAITING_APPROVAL->TIMEOUT" {
		t.Fatalf("transitions = %v", store.transitions)
	}
	if em.count() != 1 || em.events[0].EventType != "aikonos.broker.approval.expired" ||
		em.events[0].ResourceRef != "aikonos:task:t1" || em.events[0].Context.Fields["obo"] == nil {
		t.Fatalf("audit events = %v", em.events)
	}
}
