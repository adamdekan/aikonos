package broker

// F21 red-first test: GetTaskState must resolve the tenant from the
// authenticated identity, not trust req.TenantId directly — otherwise a caller
// authenticated for tenant A can read a task by spoofing req.TenantId to
// tenant B (or any string). Pre-change, GetTaskState called
// s.deps.Tasks.Get(ctx, req.TenantId, req.TaskId) directly, so the fake
// taskStore below observed the spoofed tenant, not the caller's real one.

import (
	"context"
	"testing"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/db"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// fakeGetTaskStore is a taskStore fake (Get only; rest via the embedded stub)
// recording the tenant it was called with, so the test can assert which
// tenant GetTaskState actually queried.
type fakeGetTaskStore struct {
	stubTaskStore
	gotTenant string
	task      *db.Task
}

func (f *fakeGetTaskStore) Get(_ context.Context, tenantID, _ string) (*db.Task, error) {
	f.gotTenant = tenantID
	return f.task, nil
}

const spoofedTenantUUID = "00000000-0000-0000-0000-000000000000"

func TestGetTaskState_SpoofedTenant_ResolvesToCallerIdentity(t *testing.T) {
	store := &fakeGetTaskStore{task: &db.Task{State: db.TaskStateCompleted}}
	svc := &BrokerService{deps: Deps{Logger: zap.NewNop(), Tasks: store}}

	// Caller is authenticated for testTenantUUID but the request spoofs a
	// different tenant.
	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")

	_, err := svc.GetTaskState(ctx, &brokerv1.GetTaskStateRequest{
		TenantId: spoofedTenantUUID,
		TaskId:   "task-1",
	})

	// callerIdentity rejects a mismatched req.TenantId outright (PermissionDenied)
	// rather than silently substituting the real tenant — either way, the task
	// store must never be queried under the spoofed tenant.
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected PermissionDenied for a spoofed tenant_id, got %v", err)
	}
	if store.gotTenant == spoofedTenantUUID {
		t.Fatalf("task lookup ran under the spoofed tenant %q — pre-change behavior (req.TenantId trusted directly)", spoofedTenantUUID)
	}
}
