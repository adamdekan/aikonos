package approvalsvc

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/adamdekan/aikonos/broker/internal/db"
)

// resolvingStore resolves the vote to APPROVED and returns an agent-bound task.
type resolvingStore struct {
	fakeApprovalStore
	task *db.Task
}

func (f *resolvingStore) GetPendingApprovalByTask(_ context.Context, _, _ string) (*db.ApprovalRequest, error) {
	return &db.ApprovalRequest{ApprovalID: uuid.New()}, nil
}

func (f *resolvingStore) ResolveApproval(_ context.Context, _, _, _ string, _ bool, _ string) (db.ApprovalState, bool, error) {
	return db.ApprovalApproved, true, nil
}

func (f *resolvingStore) Get(_ context.Context, _, _ string) (*db.Task, error) {
	return f.task, nil
}

// AG-17: both ApproveTask and ApproveGatewayTask mint through Resolve, so the
// tokens must carry the task's own on-behalf-of chain or identity.obo=enforce
// denies every approved task at InvokeTool.
func TestResolve_MintsWithTaskOnBehalfOf(t *testing.T) {
	agent := uuid.New()
	task := &db.Task{OwnerUserID: "alice@example.com", AgentID: &agent}
	var got db.OnBehalfOf
	mint := func(_, _ string, obo db.OnBehalfOf, _ []StepScope) (map[int32]string, error) {
		got = obo
		return map[int32]string{}, nil
	}
	if _, _, _, _, err := Resolve(context.Background(), &resolvingStore{task: task}, &fakeAccessPolicy{}, mint, zap.NewNop(),
		"tenant", "task", "bob@example.com", true, "", ArgsDigest(nil), StepUp{}); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != task.OnBehalfOf() || got.Subject != "alice@example.com" || got.Actor != agent.String() {
		t.Fatalf("mint obo = %+v, want %+v", got, task.OnBehalfOf())
	}
}

// stepsStore serves stored step args and records whether a vote was cast.
type stepsStore struct {
	resolvingStore
	steps []db.ExecutableStep
	voted bool
}

func (f *stepsStore) ListMintableSteps(_ context.Context, _, _ string) ([]db.ExecutableStep, error) {
	return f.steps, nil
}

func (f *stepsStore) ResolveApproval(ctx context.Context, tenantID, approvalID, approver string, approved bool, reason string) (db.ApprovalState, bool, error) {
	f.voted = true
	return f.resolvingStore.ResolveApproval(ctx, tenantID, approvalID, approver, approved, reason)
}

// AG-07 (M-28): an approve call whose digest does not match the stored
// arguments is refused before any vote is recorded; the matching digest and a
// digest-less denial both go through.
func TestResolve_ArgsDigestBinding(t *testing.T) {
	stored := []db.ExecutableStep{{Seq: 1, ToolID: "email.send", Args: map[string]any{"to": "bob@example.com", "body": "<hi> & bye"}}}
	shown := ArgsDigest([]db.ExecutableStep{{Args: map[string]any{"to": "eve@example.com", "body": "<hi> & bye"}}})
	mint := func(_, _ string, _ db.OnBehalfOf, _ []StepScope) (map[int32]string, error) { return nil, nil }
	run := func(approved bool, digest string) (*stepsStore, error) {
		st := &stepsStore{resolvingStore: resolvingStore{task: &db.Task{OwnerUserID: "alice@example.com"}}, steps: stored}
		_, _, _, _, err := Resolve(context.Background(), st, &fakeAccessPolicy{}, mint, zap.NewNop(),
			"tenant", "task", "bob@example.com", approved, "", digest, StepUp{})
		return st, err
	}

	for name, digest := range map[string]string{"other args": shown, "missing": ""} {
		st, err := run(true, digest)
		if status.Code(err) != codes.FailedPrecondition || st.voted {
			t.Fatalf("%s digest: err=%v voted=%v, want FailedPrecondition and no vote", name, err, st.voted)
		}
	}
	if st, err := run(true, ArgsDigest(stored)); err != nil || !st.voted {
		t.Fatalf("matching digest: err=%v voted=%v", err, st.voted)
	}
	if st, err := run(false, ""); err != nil || !st.voted {
		t.Fatalf("denial without digest: err=%v voted=%v", err, st.voted)
	}
}

// The digest is order-independent over map keys and does not HTML-escape, so
// it is a function of the argument values alone.
func TestArgsDigest_Canonical(t *testing.T) {
	a := ArgsDigest([]db.ExecutableStep{{Args: map[string]any{"b": 1.0, "a": "<x>"}}})
	b := ArgsDigest([]db.ExecutableStep{{Args: map[string]any{"a": "<x>", "b": 1.0}}})
	if a != b || len(a) != 64 {
		t.Fatalf("digest not canonical: %q vs %q", a, b)
	}
	if ArgsDigest([]db.ExecutableStep{{}}) != ArgsDigest([]db.ExecutableStep{{Args: map[string]any{}}}) {
		t.Fatal("nil args must digest like {}")
	}
}
