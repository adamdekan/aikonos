package db

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"
)

// newApprovalFixture creates a task for alice and a PENDING approval gate on
// it, returning the repo, tenant, task and approval ids.
func newApprovalFixture(t *testing.T, approverSet []string, requiresN int16, expiresAt time.Time) (*TaskRepo, string, string, string) {
	t.Helper()
	pool := openTestPool(t)
	repo := NewTaskRepo(pool, zap.NewNop())
	ctx := context.Background()
	tenant, task := uuid.New(), uuid.New()
	if err := repo.Create(ctx, &Task{TaskID: task, TenantID: tenant, OwnerUserID: "alice@example.com", Prompt: "p", CostBudget: 10}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	id, err := repo.CreateApprovalRequest(ctx, &ApprovalRequest{
		TaskID: task, TenantID: tenant, RequesterID: "alice@example.com",
		Summary: map[string]any{"step_up": true}, ApproverSet: approverSet, RequiresN: requiresN, ExpiresAt: expiresAt,
	})
	if err != nil {
		t.Fatalf("CreateApprovalRequest: %v", err)
	}
	return repo, tenant.String(), task.String(), id.String()
}

// AG-07: ListPendingApprovalsFor returns a 2-of-n gate to each of its two
// approvers and never to an outsider; once an approver has voted it drops off
// their list, and the second approver completes the gate.
func TestListPendingApprovalsFor_TwoApproversOneOutsider(t *testing.T) {
	repo, tenant, _, id := newApprovalFixture(t, []string{"bob@example.com", "carol@example.com"}, 2, time.Now().Add(time.Hour))
	ctx := context.Background()

	count := func(user string) int {
		t.Helper()
		rows, err := repo.ListPendingApprovalsFor(ctx, tenant, user)
		if err != nil {
			t.Fatalf("ListPendingApprovalsFor(%s): %v", user, err)
		}
		return len(rows)
	}
	if count("bob@example.com") != 1 || count("carol@example.com") != 1 {
		t.Fatal("both approvers must see the pending gate")
	}
	if count("dave@example.com") != 0 {
		t.Fatal("an outsider must not see the gate")
	}

	if st, resolved, err := repo.ResolveApproval(ctx, tenant, id, "bob@example.com", true, ""); err != nil || resolved || st != ApprovalPending {
		t.Fatalf("first vote: state=%s resolved=%v err=%v, want PENDING", st, resolved, err)
	}
	if count("bob@example.com") != 0 {
		t.Fatal("an approver who already voted must not see the gate again")
	}
	if count("carol@example.com") != 1 {
		t.Fatal("the second approver must still see the gate")
	}
	if st, resolved, err := repo.ResolveApproval(ctx, tenant, id, "carol@example.com", true, ""); err != nil || !resolved || st != ApprovalApproved {
		t.Fatalf("second vote: state=%s resolved=%v err=%v, want APPROVED", st, resolved, err)
	}
	if count("carol@example.com") != 0 {
		t.Fatal("a resolved gate must leave every list")
	}
}

// AG-07 (M-09): an approval past expires_at cannot be loaded or voted on; the
// sweep finds it and expires it exactly once.
func TestApprovalExpiry(t *testing.T) {
	repo, tenant, task, id := newApprovalFixture(t, []string{"bob@example.com"}, 1, time.Now().Add(-time.Minute))
	ctx := context.Background()

	if _, err := repo.GetPendingApprovalByTask(ctx, tenant, task); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("GetPendingApprovalByTask on expired row: err=%v, want ErrNoRows", err)
	}
	if _, _, err := repo.ResolveApproval(ctx, tenant, id, "bob@example.com", true, ""); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("ResolveApproval on expired row: err=%v, want ErrNoRows", err)
	}
	if rows, _ := repo.ListPendingApprovalsFor(ctx, tenant, "bob@example.com"); len(rows) != 0 {
		t.Fatal("an expired approval must not be listed as pending")
	}

	expired, err := repo.ListExpiredApprovals(ctx)
	if err != nil {
		t.Fatalf("ListExpiredApprovals: %v", err)
	}
	found := false
	for _, e := range expired {
		found = found || (e.ApprovalID == id && e.TaskID == task && e.TenantID == tenant)
	}
	if !found {
		t.Fatal("ListExpiredApprovals must return the expired row")
	}
	if ok, err := repo.ExpireApproval(ctx, tenant, id); err != nil || !ok {
		t.Fatalf("ExpireApproval: ok=%v err=%v", ok, err)
	}
	if ok, err := repo.ExpireApproval(ctx, tenant, id); err != nil || ok {
		t.Fatalf("second ExpireApproval: ok=%v err=%v, want false", ok, err)
	}
}
