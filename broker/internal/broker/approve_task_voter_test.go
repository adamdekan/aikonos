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

	"github.com/adamdekan/aikonos/broker/internal/approvalsvc"
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
		ArgsDigest:     noStepsDigest,
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
		ArgsDigest:     noStepsDigest,
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

// ── C-01: approver/tenant bound to the authenticated identity ───────────────

// recordingApproveTaskStore records the tenant/approver ResolveApproval saw.
type recordingApproveTaskStore struct {
	fakeApproveTaskStore
	gotTenant, gotApprover string
}

func (f *recordingApproveTaskStore) ResolveApproval(ctx context.Context, tenantID, approvalID, approverUserID string, approved bool, reason string) (db.ApprovalState, bool, error) {
	f.gotTenant, f.gotApprover = tenantID, approverUserID
	return f.fakeApproveTaskStore.ResolveApproval(ctx, tenantID, approvalID, approverUserID, approved, reason)
}

func newRecordingApproveStore() *recordingApproveTaskStore {
	return &recordingApproveTaskStore{fakeApproveTaskStore: fakeApproveTaskStore{ar: &db.ApprovalRequest{
		ApprovalID:  uuid.New(),
		TaskID:      uuid.New(),
		TenantID:    uuid.MustParse(testTenantUUID),
		RequesterID: "alice@example.com",
		ApproverSet: []string{"alice@example.com"},
		RequiresN:   1,
		State:       db.ApprovalPending,
	}}}
}

func TestApproveTask_ForgedApprover_PermissionDenied(t *testing.T) {
	store := newRecordingApproveStore()
	svc := newBrokerSvcForApproveTest(t, store)
	ctx := ctxWithIdentity(testTenantUUID, "eve@example.com")

	_, err := svc.ApproveTask(ctx, &brokerv1.ApproveTaskRequest{
		TenantId:       testTenantUUID,
		TaskId:         store.ar.TaskID.String(),
		ApproverUserId: "alice@example.com", // forged: the task owner
		Approved:       true,
		ArgsDigest:     noStepsDigest,
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("want PermissionDenied for forged approver_user_id, got %v", err)
	}
	if store.gotApprover != "" {
		t.Fatalf("ResolveApproval ran as %q despite the forged approver", store.gotApprover)
	}
}

func TestApproveTask_ForgedTenant_PermissionDenied(t *testing.T) {
	store := newRecordingApproveStore()
	svc := newBrokerSvcForApproveTest(t, store)
	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")

	_, err := svc.ApproveTask(ctx, &brokerv1.ApproveTaskRequest{
		TenantId:       spoofedTenantUUID,
		TaskId:         store.ar.TaskID.String(),
		ApproverUserId: "alice@example.com",
		Approved:       true,
		ArgsDigest:     noStepsDigest,
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("want PermissionDenied for forged tenant_id, got %v", err)
	}
	if store.gotTenant != "" {
		t.Fatalf("ResolveApproval ran under tenant %q despite the forged tenant", store.gotTenant)
	}
}

func TestApproveTask_UsesAuthenticatedIdentity(t *testing.T) {
	store := newRecordingApproveStore()
	svc := newBrokerSvcForApproveTest(t, store)
	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")

	// Request fields omitted: tenant and approver come from the verified identity.
	if _, err := svc.ApproveTask(ctx, &brokerv1.ApproveTaskRequest{
		TaskId:     store.ar.TaskID.String(),
		Approved:   true,
		ArgsDigest: noStepsDigest,
	}); err != nil {
		t.Fatalf("matching identity: unexpected error: %v", err)
	}
	if store.gotTenant != testTenantUUID || store.gotApprover != "alice@example.com" {
		t.Fatalf("ResolveApproval got (%q, %q), want (%q, alice@example.com)", store.gotTenant, store.gotApprover, testTenantUUID)
	}
}

func TestApproveTask_DevModeUsesRequestApprover(t *testing.T) {
	store := newRecordingApproveStore()
	svc := newBrokerSvcForApproveTest(t, store)

	// No identity on the context (OIDC disabled): request fields are the source.
	if _, err := svc.ApproveTask(context.Background(), &brokerv1.ApproveTaskRequest{
		TenantId:       testTenantUUID,
		TaskId:         store.ar.TaskID.String(),
		ApproverUserId: "alice@example.com",
		Approved:       true,
		ArgsDigest:     noStepsDigest,
	}); err != nil {
		t.Fatalf("dev mode: unexpected error: %v", err)
	}
	if store.gotApprover != "alice@example.com" {
		t.Fatalf("ResolveApproval approver = %q, want alice@example.com", store.gotApprover)
	}
}

// noStepsDigest is the args digest of the fake store's plan steps (AG-07).
var noStepsDigest = approvalsvc.ArgsDigest(nil)
