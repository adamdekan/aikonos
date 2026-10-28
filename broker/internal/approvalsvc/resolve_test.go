package approvalsvc

import (
	"context"
	"testing"

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
		"tenant", "task", "bob@example.com", true, ""); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != task.OnBehalfOf() || got.Subject != "alice@example.com" || got.Actor != agent.String() {
		t.Fatalf("mint obo = %+v, want %+v", got, task.OnBehalfOf())
	}
}
