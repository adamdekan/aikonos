package broker

// CP1 tests: CancelTask must succeed on an EXECUTING task (previously
// impossible — isValidTransition rejected EXECUTING→CANCELLED).

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/policy"
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
	withdrawnBy string // who WithdrawApproval closed the pending gate for
}

func (f *fakeCancelTaskStore) WithdrawApproval(_ context.Context, _, _, by string) (bool, error) {
	f.withdrawnBy = by
	return true, nil
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

// newCancelSvcWithFGA wires CancelTask to a fake OpenFGA so the tenant-admin
// fallback (requireTenantAdmin) is exercised for a non-owner caller.
func newCancelSvcWithFGA(t *testing.T, store taskStore, fga *fakeFGA) *BrokerService {
	t.Helper()
	srv := fga.server(t)
	t.Cleanup(srv.Close)
	eng, err := policy.NewEngine(context.Background(), policy.Config{
		OPAEndpoint: "http://unused", OpenFGAEndpoint: srv.URL, OpenFGAStoreID: "store-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	em, err := audit.NewEmitter(context.Background(), audit.Config{})
	if err != nil {
		t.Fatal(err)
	}
	return NewBrokerService(Deps{Logger: zap.NewNop(), Policy: eng, Audit: em, Tasks: store})
}

func TestCancelTask_TenantAdmin_Succeeds(t *testing.T) {
	task := newCancelTestTask(db.TaskStateExecuting)
	store := &fakeCancelTaskStore{task: task}
	svc := newCancelSvcWithFGA(t, store, &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}})

	_, err := svc.CancelTask(ctxWithIdentity(testTenantUUID, "admin@example.com"), &brokerv1.CancelTaskRequest{
		TenantId: testTenantUUID, TaskId: task.TaskID.String(),
	})
	if err != nil {
		t.Fatalf("admin cancel: %v", err)
	}
	if task.State != db.TaskStateCancelled {
		t.Fatalf("state = %s, want CANCELLED", task.State)
	}
}

func TestCancelTask_OtherMember_PermissionDenied(t *testing.T) {
	task := newCancelTestTask(db.TaskStateExecuting)
	store := &fakeCancelTaskStore{task: task}
	svc := newCancelSvcWithFGA(t, store, &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}})

	_, err := svc.CancelTask(ctxWithIdentity(testTenantUUID, "eve@example.com"), &brokerv1.CancelTaskRequest{
		TenantId: testTenantUUID, TaskId: task.TaskID.String(),
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("want PermissionDenied, got %v", err)
	}
	if len(store.transitions) != 0 {
		t.Fatalf("task must not transition, got %v", store.transitions)
	}
}

// A delegated task belongs to its recipient; the delegator may not cancel it.
func TestCancelTask_Delegator_PermissionDenied(t *testing.T) {
	task := newCancelTestTask(db.TaskStateExecuting)
	delegator := "bob@example.com"
	task.DelegatedByUserID = &delegator
	store := &fakeCancelTaskStore{task: task}
	svc := newCancelSvcWithFGA(t, store, &fakeFGA{})

	_, err := svc.CancelTask(ctxWithIdentity(testTenantUUID, delegator), &brokerv1.CancelTaskRequest{
		TenantId: testTenantUUID, TaskId: task.TaskID.String(),
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("want PermissionDenied, got %v", err)
	}
}

// An FGA transport error on the admin fallback fails closed.
func TestCancelTask_AdminCheckError_FailsClosed(t *testing.T) {
	task := newCancelTestTask(db.TaskStateExecuting)
	store := &fakeCancelTaskStore{task: task}
	svc := newCancelSvcWithFGA(t, store, &fakeFGA{checkErr: true})

	_, err := svc.CancelTask(ctxWithIdentity(testTenantUUID, "eve@example.com"), &brokerv1.CancelTaskRequest{
		TenantId: testTenantUUID, TaskId: task.TaskID.String(),
	})
	if status.Code(err) != codes.Internal || len(store.transitions) != 0 {
		t.Fatalf("want Internal and no transition, got %v / %v", err, store.transitions)
	}
}

// The requester withdraws their own pending request: CancelTask on a task
// awaiting approval closes the gate (audited approval.withdrawn) and cancels
// the task. A task not awaiting approval has no gate to close.
func TestCancelTask_AwaitingApproval_WithdrawsGate(t *testing.T) {
	task := newCancelTestTask(db.TaskStateAwaitingApproval)
	store := &fakeCancelTaskStore{task: task}
	rec := &recordingEmitter{}
	svc := &BrokerService{deps: Deps{Logger: zap.NewNop(), Tasks: store, Audit: rec}}

	if _, err := svc.CancelTask(ctxWithIdentity(testTenantUUID, "alice@example.com"), &brokerv1.CancelTaskRequest{
		TenantId: testTenantUUID, TaskId: task.TaskID.String(), Reason: "withdrawn",
	}); err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	if store.withdrawnBy != "alice@example.com" {
		t.Errorf("gate withdrawn by %q, want alice", store.withdrawnBy)
	}
	if task.State != db.TaskStateCancelled {
		t.Errorf("state = %s, want CANCELLED", task.State)
	}
	var types []string
	for _, ev := range rec.events {
		types = append(types, ev.EventType)
	}
	if !slices.Contains(types, "aikonos.broker.approval.withdrawn") || !slices.Contains(types, "aikonos.broker.task.cancelled") {
		t.Errorf("audit events = %v, want approval.withdrawn and task.cancelled", types)
	}

	other := &fakeCancelTaskStore{task: newCancelTestTask(db.TaskStateExecuting)}
	svc.deps.Tasks = other
	if _, err := svc.CancelTask(ctxWithIdentity(testTenantUUID, "alice@example.com"), &brokerv1.CancelTaskRequest{
		TenantId: testTenantUUID, TaskId: other.task.TaskID.String(),
	}); err != nil {
		t.Fatal(err)
	}
	if other.withdrawnBy != "" {
		t.Error("a task not awaiting approval must not touch approvals")
	}
}

// A new task carries its tenant, so the tenant's approvers hold can_approve
// on it (separation-of-duty gates).
func TestWriteTaskRelations_LinksTenant(t *testing.T) {
	fga := &fakeFGA{}
	srv := fga.server(t)
	t.Cleanup(srv.Close)
	eng, err := policy.NewEngine(context.Background(), policy.Config{OPAEndpoint: "http://unused", OpenFGAEndpoint: srv.URL, OpenFGAStoreID: "store-1"})
	if err != nil {
		t.Fatal(err)
	}
	writeTaskRelationsFromDeps(context.Background(), Deps{Logger: zap.NewNop(), Policy: eng, TenantID: "aikonos-dev"}, "alice@example.com", "t1")
	body, _ := json.Marshal(fga.writes)
	if !strings.Contains(string(body), `{"object":"task:t1","relation":"tenant","user":"tenant:aikonos-dev"}`) {
		t.Fatalf("writes = %s, want the task:t1 tenant tuple", body)
	}
}

// M-01: GetTaskState is readable by the owner, an approver of the task, or a
// tenant admin; any other member is denied, and an FGA error fails closed.
func TestGetTaskState_Authorization(t *testing.T) {
	task := newCancelTestTask(db.TaskStateAwaitingApproval)
	approve := "user:carol@example.com|can_approve|task:" + task.TaskID.String()
	cases := []struct {
		name, caller string
		fga          *fakeFGA
		want         codes.Code
	}{
		{"owner", "alice@example.com", &fakeFGA{checkErr: true}, codes.OK},
		{"admin", "admin@example.com", &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}, codes.OK},
		{"approver", "carol@example.com", &fakeFGA{checks: map[string]bool{approve: true}}, codes.OK},
		{"other member", "eve@example.com", &fakeFGA{checks: map[string]bool{approve: true}}, codes.PermissionDenied},
		{"fga error", "eve@example.com", &fakeFGA{checkErr: true}, codes.Internal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := newCancelSvcWithFGA(t, &fakeCancelTaskStore{task: task}, tc.fga)
			_, err := svc.GetTaskState(ctxWithIdentity(testTenantUUID, tc.caller), &brokerv1.GetTaskStateRequest{
				TenantId: testTenantUUID, TaskId: task.TaskID.String(),
			})
			if status.Code(err) != tc.want {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
		})
	}
}
