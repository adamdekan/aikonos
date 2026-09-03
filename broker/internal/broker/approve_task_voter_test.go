package broker

// Tests that ApproveTask maps db.ErrVoterNotInApproverSet to gRPC
// codes.PermissionDenied (not codes.Internal).
//
// Uses the taskStore fake seam so no Postgres or real FGA is needed.

import (
	"context"
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

// fakeApproveTaskStore satisfies taskStore (via the embedded stub).
// resolveErr, if non-nil, is returned from ResolveApproval.
type fakeApproveTaskStore struct {
	stubTaskStore
	ar         *db.ApprovalRequest
	resolveErr error
}

func (f *fakeApproveTaskStore) GetPendingApprovalByTask(_ context.Context, _, _ string) (*db.ApprovalRequest, error) {
	return f.ar, nil
}

func (f *fakeApproveTaskStore) ResolveApproval(_ context.Context, _, _, _ string, _ bool, _ string) (db.ApprovalState, bool, error) {
	if f.resolveErr != nil {
		return db.ApprovalPending, false, f.resolveErr
	}
	return db.ApprovalApproved, true, nil
}

// Get, Transition, ListMintableSteps override the embedded stub's zero
// values; the voter tests never reach them (resolveErr fires first).
func (f *fakeApproveTaskStore) Get(_ context.Context, _, _ string) (*db.Task, error) {
	return &db.Task{OwnerUserID: "alice@example.com"}, nil
}

func (f *fakeApproveTaskStore) Transition(_ context.Context, _, _ string, _, _ db.TaskState) error {
	return nil
}

func (f *fakeApproveTaskStore) ListMintableSteps(_ context.Context, _, _ string) ([]db.ExecutableStep, error) {
	return nil, nil
}

// allowAllFGA satisfies the CheckFGA surface (policy.Engine is too heavy for
// a unit test; BrokerService.ApproveTask calls only CheckFGA before resolve).
type allowAllFGA struct{}

func (allowAllFGA) CheckFGA(_ context.Context, _, _, _ string) (bool, error) {
	return true, nil
}

// newBrokerSvcForApproveTest builds the minimal BrokerService needed to reach
// the ResolveApproval call: a nop logger, a nop audit emitter, the injected
// taskStore fake, and an always-allow FGA engine.
func newBrokerSvcForApproveTest(t *testing.T, store taskStore) *BrokerService {
	t.Helper()
	em, err := audit.NewEmitter(context.Background(), audit.Config{})
	if err != nil {
		t.Fatal(err)
	}
	pCfg := policy.Config{
		OPAEndpoint:     "http://unused",
		OpenFGAEndpoint: "",
		OpenFGAStoreID:  "",
	}
	eng, err := policy.NewEngine(context.Background(), pCfg)
	if err != nil {
		t.Fatal(err)
	}
	svc := NewBrokerService(Deps{
		Logger: zap.NewNop(),
		Audit:  em,
		Policy: eng,
	})
	svc.deps.Tasks = store
	return svc
}

func TestApproveTask_VoterNotInSet_ReturnsPermissionDenied(t *testing.T) {
	approvalID := uuid.New()
	store := &fakeApproveTaskStore{
		ar: &db.ApprovalRequest{
			ApprovalID:  approvalID,
			TaskID:      uuid.New(),
			TenantID:    uuid.MustParse(testTenantUUID),
			RequesterID: "alice@example.com",
			ApproverSet: []string{"bob@example.com"},
			RequiresN:   1,
			State:       db.ApprovalPending,
		},
		// Simulate the DB layer returning ErrVoterNotInApproverSet (as it would
		// when eve, who is not in [bob], attempts to vote).
		resolveErr: db.ErrVoterNotInApproverSet,
	}

	svc := newBrokerSvcForApproveTest(t, store)

	_, err := svc.ApproveTask(context.Background(), &brokerv1.ApproveTaskRequest{
		TenantId:       testTenantUUID,
		TaskId:         store.ar.TaskID.String(),
		ApproverUserId: "eve@example.com",
		Approved:       true,
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got: %v", err)
	}
	if st.Code() != codes.PermissionDenied {
		t.Errorf("expected PermissionDenied, got %s: %s", st.Code(), st.Message())
	}
}

func TestApproveTask_InternalError_ReturnsInternal(t *testing.T) {
	// A non-sentinel error must still map to codes.Internal (regression guard).
	approvalID := uuid.New()
	store := &fakeApproveTaskStore{
		ar: &db.ApprovalRequest{
			ApprovalID:  approvalID,
			TaskID:      uuid.New(),
			TenantID:    uuid.MustParse(testTenantUUID),
			RequesterID: "alice@example.com",
			State:       db.ApprovalPending,
		},
		resolveErr: context.DeadlineExceeded,
	}

	svc := newBrokerSvcForApproveTest(t, store)

	_, err := svc.ApproveTask(context.Background(), &brokerv1.ApproveTaskRequest{
		TenantId:       testTenantUUID,
		TaskId:         store.ar.TaskID.String(),
		ApproverUserId: "alice@example.com",
		Approved:       true,
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got: %v", err)
	}
	if st.Code() != codes.Internal {
		t.Errorf("expected Internal for non-sentinel error, got %s", st.Code())
	}
}
