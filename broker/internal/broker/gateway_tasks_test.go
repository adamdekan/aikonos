package broker

// Tests for Checkpoint 2:
//   1. CreateTask sub→owner binding (north)
//   2. CreateGatewayTask (south twin — SPIFFE-gated, gateway-managed)
//   3. ApproveGatewayTask (south twin — SPIFFE-gated, mints capability tokens)
//   4. CP2 owner-grant enforcement (impersonation-closed tests)

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/auth"
	"github.com/adamdekan/aikonos/broker/internal/capability"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/gatewaygrant"
	"github.com/adamdekan/aikonos/broker/internal/policy"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// testGrantKey is a fixed 32-byte key used across all CP2 grant tests.
var testGrantKey = []byte("cp2-test-key-must-be-32-bytes!!!")

const testGrantTTL = time.Hour

// ── fakes ─────────────────────────────────────────────────────────────────────

// fakeCreateStore satisfies taskStore (Create only; rest via the embedded stub).
type fakeCreateStore struct {
	stubTaskStore
	created []*db.Task
}

func (f *fakeCreateStore) Create(_ context.Context, t *db.Task) error {
	f.created = append(f.created, t)
	return nil
}

// fakeGatewayApproveStore satisfies taskStore, covering both the create role
// (CreateGatewayTask) and the approve role (ApproveGatewayTask) — both
// injected via the single Deps.Tasks field.
type fakeGatewayApproveStore struct {
	stubTaskStore
	created      []*db.Task
	approval     *db.ApprovalRequest // returned by GetPendingApprovalByTask
	resolveState db.ApprovalState    // if zero, defaults to ApprovalApproved
}

func (f *fakeGatewayApproveStore) Create(_ context.Context, t *db.Task) error {
	f.created = append(f.created, t)
	return nil
}

func (f *fakeGatewayApproveStore) Get(_ context.Context, _, taskID string) (*db.Task, error) {
	for _, t := range f.created {
		if t.TaskID.String() == taskID {
			return t, nil
		}
	}
	return nil, status.Errorf(codes.NotFound, "task not found")
}

func (f *fakeGatewayApproveStore) Transition(_ context.Context, _, _ string, _, _ db.TaskState) error {
	return nil
}

func (f *fakeGatewayApproveStore) GetPendingApprovalByTask(_ context.Context, _, _ string) (*db.ApprovalRequest, error) {
	if f.approval != nil {
		return f.approval, nil
	}
	return nil, status.Errorf(codes.FailedPrecondition, "no pending approval")
}

func (f *fakeGatewayApproveStore) ResolveApproval(_ context.Context, _, _, _ string, _ bool, _ string) (db.ApprovalState, bool, error) {
	s := f.resolveState
	if s == "" {
		s = db.ApprovalApproved
	}
	return s, true, nil
}

func (f *fakeGatewayApproveStore) ListMintableSteps(_ context.Context, _, _ string) ([]db.ExecutableStep, error) {
	return []db.ExecutableStep{
		{Seq: 0, ToolID: "web.fetch"},
	}, nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

const testGWSpiffeID = "spiffe://aikonos.com/agent-gateway"

func newMinimalDeps(t *testing.T) Deps {
	t.Helper()
	em, err := audit.NewEmitter(context.Background(), audit.Config{})
	if err != nil {
		t.Fatal(err)
	}
	pCfg := policy.Config{OPAEndpoint: "http://unused"}
	eng, err := policy.NewEngine(context.Background(), pCfg)
	if err != nil {
		t.Fatal(err)
	}
	return Deps{
		Logger: zap.NewNop(),
		Audit:  em,
		Policy: eng,
	}
}

// newSandboxSvcForGatewayTest wires the single Deps.Tasks field. Every caller
// exercises exactly one of the create/approve roles per call — never both —
// so a single store param covers CreateGatewayTask and ApproveGatewayTask
// alike (rpc-twins-tails CP3 nit: the former two-param version silently
// preferred createStore when both were non-nil, which no call site needed).
func newSandboxSvcForGatewayTest(t *testing.T, store taskStore, minter *capability.Minter) *SandboxService {
	t.Helper()
	deps := newMinimalDeps(t)
	deps.GatewaySpiffeID = testGWSpiffeID
	deps.Capability = minter
	deps.GatewayGrantKey = testGrantKey
	deps.GatewayGrantTTL = testGrantTTL
	deps.Tasks = store
	return NewSandboxService(deps)
}

// mintTestGrant mints a grant for the given tenant+owner using the test key.
func mintTestGrant(t *testing.T, tenantID, ownerUserID string) string {
	t.Helper()
	g, err := gatewaygrant.Mint(testGrantKey, tenantID, ownerUserID, testGrantTTL)
	if err != nil {
		t.Fatalf("mintTestGrant: %v", err)
	}
	return g
}

func newBrokerSvcForCreateTaskTest(t *testing.T, createStore taskStore) *BrokerService {
	t.Helper()
	deps := newMinimalDeps(t)
	deps.Tasks = createStore
	return NewBrokerService(deps)
}

func gatewaySpiffeCtx() context.Context {
	return auth.WithIdentity(context.Background(), &auth.Identity{SpiffeID: testGWSpiffeID})
}

func nonGatewayCtx() context.Context {
	return auth.WithIdentity(context.Background(), &auth.Identity{SpiffeID: "spiffe://aikonos.com/some-sandbox"})
}

// ── Test 1: CreateTask sub→owner binding ─────────────────────────────────────

// TestCreateTask_OIDCMismatch_PermissionDenied proves that when an OIDC identity
// is on the context and req.UserId disagrees with the token actor, CreateTask
// returns PermissionDenied — the forge-owner hole is closed.
func TestCreateTask_OIDCMismatch_PermissionDenied(t *testing.T) {
	store := &fakeCreateStore{}
	svc := newBrokerSvcForCreateTaskTest(t, store)

	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")
	_, err := svc.CreateTask(ctx, &brokerv1.CreateTaskRequest{
		TenantId: testTenantUUID,
		UserId:   "mallory@example.com", // disagrees with the token (alice)
		Prompt:   "do something",
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("mismatched user_id: want PermissionDenied, got %v", err)
	}
	if len(store.created) != 0 {
		t.Error("no task must be created when identity check fails")
	}
}

// TestCreateTask_OIDCMatch_OK proves that when req.UserId echoes the token actor,
// CreateTask succeeds and the task owner is the verified actor.
func TestCreateTask_OIDCMatch_OK(t *testing.T) {
	store := &fakeCreateStore{}
	svc := newBrokerSvcForCreateTaskTest(t, store)

	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")
	resp, err := svc.CreateTask(ctx, &brokerv1.CreateTaskRequest{
		TenantId: testTenantUUID,
		UserId:   "alice@example.com", // matches the token
		Prompt:   "do something",
	})
	if err != nil {
		t.Fatalf("matching user_id: unexpected error %v", err)
	}
	if resp.TaskId == "" {
		t.Fatal("expected a task_id in the response")
	}
	if len(store.created) != 1 || store.created[0].OwnerUserID != "alice@example.com" {
		t.Errorf("task owner should be alice, got %v", store.created)
	}
}

// TestCreateTask_DevMode_NoIdentity_UsesReqFields proves that when there is no
// OIDC identity on the context (dev / unit-test passthrough), CreateTask falls
// back to the request body fields — backwards-compatible with all existing tests
// and south callers.
func TestCreateTask_DevMode_NoIdentity_UsesReqFields(t *testing.T) {
	store := &fakeCreateStore{}
	svc := newBrokerSvcForCreateTaskTest(t, store)

	resp, err := svc.CreateTask(context.Background(), &brokerv1.CreateTaskRequest{
		TenantId: testTenantUUID,
		UserId:   "dev-user@example.com",
		Prompt:   "dev task",
	})
	if err != nil {
		t.Fatalf("dev mode (no identity): unexpected error %v", err)
	}
	if resp.TaskId == "" {
		t.Fatal("expected a task_id")
	}
	if len(store.created) != 1 || store.created[0].OwnerUserID != "dev-user@example.com" {
		t.Errorf("dev mode: owner should be dev-user, got %v", store.created)
	}
}

// ── Test 2: CreateGatewayTask (south twin) ────────────────────────────────────

// TestCreateGatewayTask_GatewayPeer_CreatesGatewayManagedTask proves that the
// gateway SVID can create a task with GatewayManaged=true and the owner bound
// from the broker-issued grant (not the asserted OwnerUserId field).
func TestCreateGatewayTask_GatewayPeer_CreatesGatewayManagedTask(t *testing.T) {
	store := &fakeCreateStore{}
	svc := newSandboxSvcForGatewayTest(t, store, nil)

	grant := mintTestGrant(t, testTenantUUID, "alice@example.com")
	resp, err := svc.CreateGatewayTask(gatewaySpiffeCtx(), &brokerv1.CreateGatewayTaskRequest{
		TenantId:    testTenantUUID,
		OwnerUserId: "alice@example.com",
		Prompt:      "scheduled task",
		CostBudget:  500,
		OwnerGrant:  grant,
	})
	if err != nil {
		t.Fatalf("gateway peer CreateGatewayTask: %v", err)
	}
	if resp.TaskId == "" {
		t.Fatal("expected a task_id")
	}
	if len(store.created) != 1 {
		t.Fatalf("expected 1 task created, got %d", len(store.created))
	}
	task := store.created[0]
	if !task.GatewayManaged {
		t.Error("task must have GatewayManaged=true")
	}
	if task.OwnerUserID != "alice@example.com" {
		t.Errorf("owner = %q, want alice@example.com", task.OwnerUserID)
	}
	if task.CostBudget != 500 {
		t.Errorf("cost_budget = %d, want 500", task.CostBudget)
	}
}

// TestCreateGatewayTask_NonGatewayPeer_PermissionDenied proves that a non-gateway
// SPIFFE peer is rejected before any task is created.
func TestCreateGatewayTask_NonGatewayPeer_PermissionDenied(t *testing.T) {
	store := &fakeCreateStore{}
	svc := newSandboxSvcForGatewayTest(t, store, nil)

	_, err := svc.CreateGatewayTask(nonGatewayCtx(), &brokerv1.CreateGatewayTaskRequest{
		TenantId:    testTenantUUID,
		OwnerUserId: "alice@example.com",
		Prompt:      "evil task",
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-gateway peer: want PermissionDenied, got %v", err)
	}
	if len(store.created) != 0 {
		t.Error("no task must be created when peer check fails")
	}
}

// TestCreateGatewayTask_NoIdentity_PermissionDenied proves that a context with
// no SPIFFE identity is also rejected (fail closed).
func TestCreateGatewayTask_NoIdentity_PermissionDenied(t *testing.T) {
	store := &fakeCreateStore{}
	svc := newSandboxSvcForGatewayTest(t, store, nil)

	_, err := svc.CreateGatewayTask(context.Background(), &brokerv1.CreateGatewayTaskRequest{
		TenantId:    testTenantUUID,
		OwnerUserId: "alice@example.com",
		Prompt:      "no identity",
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("no identity: want PermissionDenied, got %v", err)
	}
}

// ── Test 3: ApproveGatewayTask (south twin) ───────────────────────────────────

// TestApproveGatewayTask_GatewayPeer_MintsTokens proves that a gateway-peer call
// resolves the approval and returns a non-empty CapabilityTokenIds map.
func TestApproveGatewayTask_GatewayPeer_MintsTokens(t *testing.T) {
	minter, err := capability.GenerateMinter()
	if err != nil {
		t.Fatalf("GenerateMinter: %v", err)
	}

	taskID := uuid.New()
	approveStore := &fakeGatewayApproveStore{
		created: []*db.Task{
			{
				TaskID:         taskID,
				TenantID:       uuid.MustParse(testTenantUUID),
				OwnerUserID:    "alice@example.com",
				GatewayManaged: true,
				State:          db.TaskStateAwaitingApproval,
			},
		},
		approval: &db.ApprovalRequest{
			ApprovalID:  uuid.New(),
			TaskID:      taskID,
			TenantID:    uuid.MustParse(testTenantUUID),
			RequesterID: "alice@example.com",
			ApproverSet: []string{"alice@example.com"},
			RequiresN:   1,
			State:       db.ApprovalPending,
		},
	}
	svc := newSandboxSvcForGatewayTest(t, approveStore, minter)

	resp, err := svc.ApproveGatewayTask(gatewaySpiffeCtx(), &brokerv1.ApproveGatewayTaskRequest{
		TenantId:    testTenantUUID,
		TaskId:      taskID.String(),
		OwnerUserId: "alice@example.com",
		Approved:    true,
	})
	if err != nil {
		t.Fatalf("gateway ApproveGatewayTask: %v", err)
	}
	if !resp.Success {
		t.Fatal("expected success=true")
	}
	if len(resp.CapabilityTokenIds) == 0 {
		t.Error("expected at least one capability token to be minted")
	}
}

// TestApproveGatewayTask_NonGatewayPeer_PermissionDenied proves that the RPC
// fails closed for non-gateway SPIFFE peers.
func TestApproveGatewayTask_NonGatewayPeer_PermissionDenied(t *testing.T) {
	svc := newSandboxSvcForGatewayTest(t, nil, nil)

	_, err := svc.ApproveGatewayTask(nonGatewayCtx(), &brokerv1.ApproveGatewayTaskRequest{
		TenantId:    testTenantUUID,
		TaskId:      uuid.New().String(),
		OwnerUserId: "alice@example.com",
		Approved:    true,
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-gateway peer: want PermissionDenied, got %v", err)
	}
}

// TestApproveGatewayTask_Denied_StatusIsDenied proves that when the approval
// resolves to DENIED, ApproveGatewayTask returns NewStatus==DENIED (not APPROVED).
// Regression guard: the EventType was previously hardcoded to "approved".
func TestApproveGatewayTask_Denied_StatusIsDenied(t *testing.T) {
	taskID := uuid.New()
	approveStore := &fakeGatewayApproveStore{
		created: []*db.Task{
			{
				TaskID:         taskID,
				TenantID:       uuid.MustParse(testTenantUUID),
				OwnerUserID:    "alice@example.com",
				GatewayManaged: true,
				State:          db.TaskStateAwaitingApproval,
			},
		},
		approval: &db.ApprovalRequest{
			ApprovalID:  uuid.New(),
			TaskID:      taskID,
			TenantID:    uuid.MustParse(testTenantUUID),
			RequesterID: "alice@example.com",
			ApproverSet: []string{"alice@example.com"},
			RequiresN:   1,
			State:       db.ApprovalPending,
		},
		resolveState: db.ApprovalDenied, // fake resolves to denied
	}
	svc := newSandboxSvcForGatewayTest(t, approveStore, nil)

	resp, err := svc.ApproveGatewayTask(gatewaySpiffeCtx(), &brokerv1.ApproveGatewayTaskRequest{
		TenantId:    testTenantUUID,
		TaskId:      taskID.String(),
		OwnerUserId: "alice@example.com",
		Approved:    false,
	})
	if err != nil {
		t.Fatalf("denial path: unexpected error %v", err)
	}
	if resp.NewStatus != brokerv1.TaskStatus_DENIED {
		t.Errorf("denial path: want DENIED, got %s", resp.NewStatus)
	}
	if len(resp.CapabilityTokenIds) != 0 {
		t.Error("denial path: must not mint tokens on denial")
	}
}

// ── CP2: owner-grant enforcement tests ───────────────────────────────────────
//
// These tests encode the WHY: the grant requirement closes the
// "gateway asserts arbitrary owner" impersonation primitive.  Each test name
// names the attack it blocks.

// TestCreateGatewayTask_GrantBindsOwner_NotAssertedField proves that the task
// owner comes from the verified grant, not from req.OwnerUserId.
// Attack blocked: gateway code path asserting a different owner than the grant.
func TestCreateGatewayTask_GrantBindsOwner_NotAssertedField(t *testing.T) {
	store := &fakeCreateStore{}
	svc := newSandboxSvcForGatewayTest(t, store, nil)

	// Grant is for alice; req.OwnerUserId claims bob — grant wins.
	grant := mintTestGrant(t, testTenantUUID, "alice@example.com")
	resp, err := svc.CreateGatewayTask(gatewaySpiffeCtx(), &brokerv1.CreateGatewayTaskRequest{
		TenantId:    testTenantUUID,
		OwnerUserId: "bob@example.com", // attacker tries to assert a different owner
		Prompt:      "do something",
		OwnerGrant:  grant,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(store.created) != 1 {
		t.Fatalf("expected 1 task, got %d", len(store.created))
	}
	// The bound owner must be alice (from the grant), never bob.
	if got := store.created[0].OwnerUserID; got != "alice@example.com" {
		t.Errorf("owner = %q, want alice@example.com (grant wins over asserted field)", got)
	}
	_ = resp
}

// TestCreateGatewayTask_NoGrant_PermissionDenied blocks the pre-CP2 attack:
// a compromised gateway asserting an arbitrary owner without a broker-issued grant.
func TestCreateGatewayTask_NoGrant_PermissionDenied(t *testing.T) {
	store := &fakeCreateStore{}
	svc := newSandboxSvcForGatewayTest(t, store, nil)

	_, err := svc.CreateGatewayTask(gatewaySpiffeCtx(), &brokerv1.CreateGatewayTaskRequest{
		TenantId:    testTenantUUID,
		OwnerUserId: "victim@example.com",
		Prompt:      "evil task",
		// OwnerGrant deliberately absent — attacker has no broker-issued grant
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("missing grant: want PermissionDenied, got %v", err)
	}
	if len(store.created) != 0 {
		t.Error("no task must be created without a grant")
	}
}

// TestCreateGatewayTask_TenantMismatchInGrant_PermissionDenied blocks
// cross-tenant impersonation: a grant minted for tenant A presented to tenant B.
func TestCreateGatewayTask_TenantMismatchInGrant_PermissionDenied(t *testing.T) {
	store := &fakeCreateStore{}
	svc := newSandboxSvcForGatewayTest(t, store, nil)

	otherTenant := "ffffffff-ffff-ffff-ffff-ffffffffffff"
	// Mint grant for otherTenant but present it to testTenantUUID.
	grant := mintTestGrant(t, otherTenant, "alice@example.com")
	_, err := svc.CreateGatewayTask(gatewaySpiffeCtx(), &brokerv1.CreateGatewayTaskRequest{
		TenantId:    testTenantUUID,
		OwnerUserId: "alice@example.com",
		Prompt:      "task",
		OwnerGrant:  grant,
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("grant tenant mismatch: want PermissionDenied, got %v", err)
	}
	if len(store.created) != 0 {
		t.Error("no task must be created when grant tenant mismatches request")
	}
}

// TestCreateGatewayTask_ExpiredGrant_PermissionDenied proves that a token
// whose TTL has elapsed is rejected — the reuse window is enforced.
func TestCreateGatewayTask_ExpiredGrant_PermissionDenied(t *testing.T) {
	store := &fakeCreateStore{}
	svc := newSandboxSvcForGatewayTest(t, store, nil)

	// A negative TTL sets expiresUnix in the past, so Verify rejects it as
	// ErrExpired without any clock manipulation.
	grant, err := gatewaygrant.Mint(testGrantKey, testTenantUUID, "alice@example.com", -1*time.Hour)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	_, err = svc.CreateGatewayTask(gatewaySpiffeCtx(), &brokerv1.CreateGatewayTaskRequest{
		TenantId:    testTenantUUID,
		OwnerUserId: "alice@example.com",
		Prompt:      "task",
		OwnerGrant:  grant,
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expired grant: want PermissionDenied, got %v", err)
	}
}

// TestApproveGatewayTask_UsesStoredOwner_NotAssertedField proves that
// ApproveGatewayTask derives the approver from the stored task row, not from
// req.OwnerUserId.  Attack blocked: gateway asserting a different approver to
// approve a task it doesn't own.
func TestApproveGatewayTask_UsesStoredOwner_NotAssertedField(t *testing.T) {
	minter, err := capability.GenerateMinter()
	if err != nil {
		t.Fatalf("GenerateMinter: %v", err)
	}

	taskID := uuid.New()
	// Task is owned by alice.
	approveStore := &fakeGatewayApproveStore{
		created: []*db.Task{
			{
				TaskID:         taskID,
				TenantID:       uuid.MustParse(testTenantUUID),
				OwnerUserID:    "alice@example.com",
				GatewayManaged: true,
				State:          db.TaskStateAwaitingApproval,
			},
		},
		approval: &db.ApprovalRequest{
			ApprovalID:  uuid.New(),
			TaskID:      taskID,
			TenantID:    uuid.MustParse(testTenantUUID),
			RequesterID: "alice@example.com",
			ApproverSet: []string{"alice@example.com"},
			RequiresN:   1,
			State:       db.ApprovalPending,
		},
	}
	svc := newSandboxSvcForGatewayTest(t, approveStore, minter)

	// Attacker sends req.OwnerUserId="bob" — broker must use alice from the task row.
	resp, err := svc.ApproveGatewayTask(gatewaySpiffeCtx(), &brokerv1.ApproveGatewayTaskRequest{
		TenantId:    testTenantUUID,
		TaskId:      taskID.String(),
		OwnerUserId: "bob@example.com", // gateway asserts wrong approver
		Approved:    true,
	})
	if err != nil {
		t.Fatalf("ApproveGatewayTask with wrong asserted owner: %v", err)
	}
	// Approval must succeed (alice is in the approver set) — bob was never consulted.
	if !resp.Success {
		t.Fatal("expected success=true; alice (from task row) should approve")
	}
	if len(resp.CapabilityTokenIds) == 0 {
		t.Error("expected capability tokens minted for the approved task")
	}
}

// TestCreateTask_TenantMismatch_PermissionDenied proves that when the OIDC
// identity carries a tenant and req.TenantId disagrees, CreateTask returns
// PermissionDenied — preventing a token holder from forging a different tenant.
func TestCreateTask_TenantMismatch_PermissionDenied(t *testing.T) {
	store := &fakeCreateStore{}
	svc := newBrokerSvcForCreateTaskTest(t, store)

	otherTenant := "ffffffff-ffff-ffff-ffff-ffffffffffff"
	// OIDC identity has testTenantUUID; request carries a different tenant.
	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")
	_, err := svc.CreateTask(ctx, &brokerv1.CreateTaskRequest{
		TenantId: otherTenant, // disagrees with the token tenant
		UserId:   "alice@example.com",
		Prompt:   "do something",
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("mismatched tenant_id: want PermissionDenied, got %v", err)
	}
	if len(store.created) != 0 {
		t.Error("no task must be created when tenant check fails")
	}
}
