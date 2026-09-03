package broker

// CP1 tests: CancelTask must succeed on an EXECUTING task (previously
// impossible — isValidTransition rejected EXECUTING→CANCELLED).

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/db"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// fakeCancelTaskStore is a narrow taskStore fake that (unlike fakeTaskStore
// in service_submitplan_test.go, which always succeeds) supports injecting a
// Transition error — needed to exercise CancelTask's FailedPrecondition path
// for an already-terminal task. Rest of taskStore via the embedded stub.
type fakeCancelTaskStore struct {
	stubTaskStore
	task        *db.Task
	getErr      error
	transitions [][2]db.TaskState
	transErr    error
	getCalled   bool   // records whether Get was ever invoked
	gotTenant   string // records the tenant Get was called with, if any
}

func (f *fakeCancelTaskStore) Get(_ context.Context, tenantID, _ string) (*db.Task, error) {
	f.getCalled = true
	f.gotTenant = tenantID
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.task, nil
}

func (f *fakeCancelTaskStore) Transition(_ context.Context, _, _ string, from, to db.TaskState) error {
	if f.transErr != nil {
		return f.transErr
	}
	f.transitions = append(f.transitions, [2]db.TaskState{from, to})
	f.task.State = to
	return nil
}

func newCancelTestTask(state db.TaskState) *db.Task {
	return &db.Task{TaskID: uuid.New(), OwnerUserID: "alice@example.com", State: state}
}

func TestCancelTask_Executing_Succeeds(t *testing.T) {
	task := newCancelTestTask(db.TaskStateExecuting)
	store := &fakeCancelTaskStore{task: task}

	svc := &BrokerService{deps: Deps{
		Logger: zap.NewNop(),
		Tasks:  store,
	}}

	resp, err := svc.CancelTask(ctxWithIdentity(testTenantUUID, "alice@example.com"), &brokerv1.CancelTaskRequest{
		TenantId: testTenantUUID, TaskId: task.TaskID.String(), Reason: "user requested",
	})
	if err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}
	if !resp.Success {
		t.Error("expected Success=true")
	}
	if len(store.transitions) != 1 || store.transitions[0] != [2]db.TaskState{db.TaskStateExecuting, db.TaskStateCancelled} {
		t.Fatalf("expected EXECUTING→CANCELLED, got %v", store.transitions)
	}
}

func TestCancelTask_TerminalTask_Rejected(t *testing.T) {
	task := newCancelTestTask(db.TaskStateCompleted)
	store := &fakeCancelTaskStore{task: task, transErr: errors.New("invalid transition COMPLETED->CANCELLED")}

	svc := &BrokerService{deps: Deps{
		Logger: zap.NewNop(),
		Tasks:  store,
	}}

	_, err := svc.CancelTask(ctxWithIdentity(testTenantUUID, "alice@example.com"), &brokerv1.CancelTaskRequest{
		TenantId: testTenantUUID, TaskId: task.TaskID.String(),
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition for an already-terminal task, got %v", err)
	}
}

func TestCancelTask_TaskNotFound_NotFound(t *testing.T) {
	store := &fakeCancelTaskStore{getErr: db.ErrNotFound}
	svc := &BrokerService{deps: Deps{Logger: zap.NewNop(), Tasks: store}}

	_, err := svc.CancelTask(ctxWithIdentity(testTenantUUID, "alice@example.com"), &brokerv1.CancelTaskRequest{TenantId: testTenantUUID, TaskId: "missing"})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("expected NotFound, got %v", err)
	}
}

// TestCancelTask_SpoofedTenant_RejectedBeforeStoreAccess is the F21 red-first
// counterpart to TestGetTaskState_SpoofedTenant_ResolvesToCallerIdentity: a
// caller authenticated for testTenantUUID must not be able to cancel a task
// under a different, spoofed req.TenantId. Pre-change, CancelTask called
// tasks.Get(ctx, req.TenantId, req.TaskId) directly, so the spoofed tenant
// would have reached the store; post-change it must be rejected before the
// store is ever touched.
func TestCancelTask_SpoofedTenant_RejectedBeforeStoreAccess(t *testing.T) {
	task := newCancelTestTask(db.TaskStateExecuting)
	store := &fakeCancelTaskStore{task: task}

	svc := &BrokerService{deps: Deps{
		Logger: zap.NewNop(),
		Tasks:  store,
	}}

	_, err := svc.CancelTask(ctxWithIdentity(testTenantUUID, "alice@example.com"), &brokerv1.CancelTaskRequest{
		TenantId: spoofedTenantUUID, TaskId: task.TaskID.String(), Reason: "user requested",
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected PermissionDenied for a spoofed tenant_id, got %v", err)
	}
	if store.getCalled {
		t.Fatalf("task store was queried (tenant=%q) despite the spoofed tenant_id — pre-change behavior (req.TenantId trusted directly)", store.gotTenant)
	}
}

func TestCancelTask_InternalGetError(t *testing.T) {
	store := &fakeCancelTaskStore{getErr: errors.New("db down")}
	svc := &BrokerService{deps: Deps{Logger: zap.NewNop(), Tasks: store}}

	_, err := svc.CancelTask(ctxWithIdentity(testTenantUUID, "alice@example.com"), &brokerv1.CancelTaskRequest{TenantId: testTenantUUID, TaskId: "x"})
	if status.Code(err) != codes.Internal {
		t.Fatalf("expected Internal, got %v", err)
	}
}
