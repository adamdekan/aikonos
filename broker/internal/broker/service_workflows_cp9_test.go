package broker

// Tests for CP9: ForkWorkflow + SetWorkflowVersionPin + ClearWorkflowVersionPin.
//
// Pattern mirrors service_workflows_cp8_test.go and service_workflows_cp7_test.go.
// Fakes satisfy workflowForkStore and workflowPinStore without Postgres.
// North-path tests wire BrokerService; south-path tests wire SandboxService.

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/policy"
	"github.com/adamdekan/aikonos/broker/internal/workflowsvc"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// ── fakeWorkflowForkStore ─────────────────────────────────────────────────────

type fakeWorkflowForkStore struct {
	stubWorkflowStore
	// current maps lineageID string → *WorkflowRow for GetCurrent.
	current   map[string]*db.WorkflowRow
	created   []db.WorkflowRow // rows passed to CreateVersion
	createErr error
}

func newFakeForkStore() *fakeWorkflowForkStore {
	return &fakeWorkflowForkStore{current: make(map[string]*db.WorkflowRow)}
}

func (f *fakeWorkflowForkStore) addCurrent(row db.WorkflowRow) {
	f.current[row.LineageID.String()] = &row
}

func (f *fakeWorkflowForkStore) GetCurrent(_ context.Context, _ string, lineageID uuid.UUID) (*db.WorkflowRow, error) {
	r, ok := f.current[lineageID.String()]
	if !ok {
		return nil, fmt.Errorf("workflow lineage %s not found", lineageID)
	}
	return r, nil
}

func (f *fakeWorkflowForkStore) CreateVersion(_ context.Context, _ string, row db.WorkflowRow) (db.WorkflowRow, error) {
	if f.createErr != nil {
		return db.WorkflowRow{}, f.createErr
	}
	row.ID = uuid.New()
	row.Version = 1
	if row.LineageID == uuid.Nil {
		row.LineageID = uuid.New()
	}
	f.created = append(f.created, row)
	return row, nil
}

// ── fakeWorkflowPinStore ──────────────────────────────────────────────────────

type fakeWorkflowPinStore struct {
	stubWorkflowStore
	// versions maps lineageID → version → *WorkflowRow
	versions map[string]map[int]*db.WorkflowRow
	// pins tracks SetVersionPin calls: "userID/lineageID" → version
	pins   map[string]int
	clears []string // "userID/lineageID" entries from ClearVersionPin
}

func newFakePinStore() *fakeWorkflowPinStore {
	return &fakeWorkflowPinStore{
		versions: make(map[string]map[int]*db.WorkflowRow),
		pins:     make(map[string]int),
	}
}

func (f *fakeWorkflowPinStore) addVersion(row db.WorkflowRow) {
	lid := row.LineageID.String()
	if f.versions[lid] == nil {
		f.versions[lid] = make(map[int]*db.WorkflowRow)
	}
	r := row
	f.versions[lid][row.Version] = &r
}

func (f *fakeWorkflowPinStore) GetVersion(_ context.Context, _ string, lineageID uuid.UUID, version int) (*db.WorkflowRow, error) {
	byLid, ok := f.versions[lineageID.String()]
	if !ok {
		return nil, fmt.Errorf("workflow lineage %s not found", lineageID)
	}
	r, ok := byLid[version]
	if !ok {
		return nil, fmt.Errorf("workflow lineage %s version %d not found", lineageID, version)
	}
	return r, nil
}

func (f *fakeWorkflowPinStore) SetVersionPin(_ context.Context, _, userID string, lineageID uuid.UUID, version int) error {
	f.pins[userID+"/"+lineageID.String()] = version
	return nil
}

func (f *fakeWorkflowPinStore) ClearVersionPin(_ context.Context, _, userID string, lineageID uuid.UUID) error {
	f.clears = append(f.clears, userID+"/"+lineageID.String())
	return nil
}

// ── helper builders ───────────────────────────────────────────────────────────

func newSandboxSvcForFork(t *testing.T, forkStore workflowsvc.Store, skillGranted bool) *SandboxService {
	t.Helper()
	em, err := audit.NewEmitter(context.Background(), audit.Config{})
	if err != nil {
		t.Fatalf("audit.NewEmitter: %v", err)
	}
	svc := NewSandboxService(Deps{
		Logger:          zap.NewNop(),
		Audit:           em,
		GatewaySpiffeID: testGWSpiffeID,
		GatewayGrantKey: testGrantKey,
		GatewayGrantTTL: testGrantTTL,
		Workflows:       forkStore,
	})
	svc.skillPolicy = &fakeSkillPolicy{enabled: true, granted: skillGranted}
	return svc
}

func newSandboxSvcForPin(t *testing.T, pinStore workflowsvc.Store, skillGranted bool) *SandboxService {
	t.Helper()
	em, err := audit.NewEmitter(context.Background(), audit.Config{})
	if err != nil {
		t.Fatalf("audit.NewEmitter: %v", err)
	}
	svc := NewSandboxService(Deps{
		Logger:          zap.NewNop(),
		Audit:           em,
		GatewaySpiffeID: testGWSpiffeID,
		GatewayGrantKey: testGrantKey,
		GatewayGrantTTL: testGrantTTL,
		Workflows:       pinStore,
	})
	svc.skillPolicy = &fakeSkillPolicy{enabled: true, granted: skillGranted}
	return svc
}

func newBrokerSvcForCP9Fork(t *testing.T, forkStore workflowsvc.Store, skillGranted bool) *BrokerService {
	t.Helper()
	fga := &fakeFGA{checks: map[string]bool{
		"user:" + testWFOwner + "|can_invoke|skill:workflows": skillGranted,
	}}
	srv := fga.server(t)
	t.Cleanup(srv.Close)
	eng, err := policy.NewEngine(context.Background(), policy.Config{
		OPAEndpoint:     "http://unused",
		OpenFGAEndpoint: srv.URL,
		OpenFGAStoreID:  "store-1",
	})
	if err != nil {
		t.Fatalf("policy.NewEngine: %v", err)
	}
	em, err := audit.NewEmitter(context.Background(), audit.Config{})
	if err != nil {
		t.Fatalf("audit.NewEmitter: %v", err)
	}
	return NewBrokerService(Deps{
		Logger:    zap.NewNop(),
		Policy:    eng,
		Audit:     em,
		TenantID:  "aikonos-dev",
		Workflows: forkStore,
	})
}

func newBrokerSvcForCP9Pin(t *testing.T, pinStore workflowsvc.Store, skillGranted bool) *BrokerService {
	t.Helper()
	fga := &fakeFGA{checks: map[string]bool{
		"user:" + testWFOwner + "|can_invoke|skill:workflows": skillGranted,
	}}
	srv := fga.server(t)
	t.Cleanup(srv.Close)
	eng, err := policy.NewEngine(context.Background(), policy.Config{
		OPAEndpoint:     "http://unused",
		OpenFGAEndpoint: srv.URL,
		OpenFGAStoreID:  "store-1",
	})
	if err != nil {
		t.Fatalf("policy.NewEngine: %v", err)
	}
	em, err := audit.NewEmitter(context.Background(), audit.Config{})
	if err != nil {
		t.Fatalf("audit.NewEmitter: %v", err)
	}
	return NewBrokerService(Deps{
		Logger:    zap.NewNop(),
		Policy:    eng,
		Audit:     em,
		TenantID:  "aikonos-dev",
		Workflows: pinStore,
	})
}

// sourceWorkflowRow builds a private WorkflowRow owned by testWFOwner.
func sourceWorkflowRow(lineageID uuid.UUID) db.WorkflowRow {
	return db.WorkflowRow{
		ID:               uuid.New(),
		LineageID:        lineageID,
		Version:          1,
		TenantID:         uuid.MustParse(testWFTenant),
		OwnerUserID:      testWFOwner,
		Name:             "source-wf",
		Description:      "original description",
		Status:           "private",
		VisibilityKind:   "private",
		VisibilityGroups: json.RawMessage(`[]`),
		Definition:       json.RawMessage(minimalValidWorkflowJSON()),
		ApprovalState:    "approved",
	}
}

// ── South-path handler tests ──────────────────────────────────────────────────

// TestSouthCP9_ForkWorkflow_SkillDeny_PermissionDenied.
func TestSouthCP9_ForkWorkflow_SkillDeny_PermissionDenied(t *testing.T) {
	srcLineage := uuid.New()
	store := newFakeForkStore()
	store.addCurrent(sourceWorkflowRow(srcLineage))

	svc := newSandboxSvcForFork(t, store, false /* skill denied */)

	_, err := svc.ForkWorkflow(gatewayCtxForWorkflow(), &brokerv1.ForkWorkflowRequest{
		TenantId:        testWFTenant,
		OwnerUserId:     testWFOwner,
		OwnerGrant:      mintWFGrant(t),
		SourceLineageId: srcLineage.String(),
		NewName:         "fork",
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("skill deny: want PermissionDenied, got %v", err)
	}
}

// TestSouthCP9_ForkWorkflow_OwnSource_Succeeds.
func TestSouthCP9_ForkWorkflow_OwnSource_Succeeds(t *testing.T) {
	srcLineage := uuid.New()
	store := newFakeForkStore()
	store.addCurrent(sourceWorkflowRow(srcLineage))

	svc := newSandboxSvcForFork(t, store, true)

	resp, err := svc.ForkWorkflow(gatewayCtxForWorkflow(), &brokerv1.ForkWorkflowRequest{
		TenantId:        testWFTenant,
		OwnerUserId:     testWFOwner,
		OwnerGrant:      mintWFGrant(t),
		SourceLineageId: srcLineage.String(),
		NewName:         "my-fork",
	})
	if err != nil {
		t.Fatalf("south fork: unexpected error: %v", err)
	}
	if resp.LineageId == "" {
		t.Fatal("lineage_id must not be empty")
	}
}

// TestSouthCP9_SetVersionPin_ApprovedVersion_Succeeds.
func TestSouthCP9_SetVersionPin_ApprovedVersion_Succeeds(t *testing.T) {
	lineageID := uuid.New()
	store := newFakePinStore()
	store.addVersion(db.WorkflowRow{
		ID:            uuid.New(),
		LineageID:     lineageID,
		Version:       1,
		ApprovalState: "approved",
	})

	svc := newSandboxSvcForPin(t, store, true)

	_, err := svc.SetWorkflowVersionPin(gatewayCtxForWorkflow(), &brokerv1.SetWorkflowVersionPinRequest{
		TenantId:   testWFTenant,
		OwnerGrant: mintWFGrant(t),
		LineageId:  lineageID.String(),
		Version:    1,
	})
	if err != nil {
		t.Fatalf("south SetVersionPin: unexpected error: %v", err)
	}
}

// TestSouthCP9_SetVersionPin_ProposedVersion_FailedPrecondition.
func TestSouthCP9_SetVersionPin_ProposedVersion_FailedPrecondition(t *testing.T) {
	lineageID := uuid.New()
	store := newFakePinStore()
	store.addVersion(db.WorkflowRow{
		ID:            uuid.New(),
		LineageID:     lineageID,
		Version:       2,
		ApprovalState: "proposed",
	})

	svc := newSandboxSvcForPin(t, store, true)

	_, err := svc.SetWorkflowVersionPin(gatewayCtxForWorkflow(), &brokerv1.SetWorkflowVersionPinRequest{
		TenantId:   testWFTenant,
		OwnerGrant: mintWFGrant(t),
		LineageId:  lineageID.String(),
		Version:    2,
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("proposed version south: want FailedPrecondition, got %v", err)
	}
}

// TestSouthCP9_ClearVersionPin_Succeeds.
func TestSouthCP9_ClearVersionPin_Succeeds(t *testing.T) {
	lineageID := uuid.New()
	store := newFakePinStore()

	svc := newSandboxSvcForPin(t, store, true)

	_, err := svc.ClearWorkflowVersionPin(gatewayCtxForWorkflow(), &brokerv1.ClearWorkflowVersionPinRequest{
		TenantId:   testWFTenant,
		OwnerGrant: mintWFGrant(t),
		LineageId:  lineageID.String(),
	})
	if err != nil {
		t.Fatalf("south ClearVersionPin: unexpected error: %v", err)
	}
}

// TestSouthCP9_SkillDeny_Pin_PermissionDenied.
func TestSouthCP9_SkillDeny_Pin_PermissionDenied(t *testing.T) {
	store := newFakePinStore()
	svc := newSandboxSvcForPin(t, store, false /* skill denied */)

	_, err := svc.SetWorkflowVersionPin(gatewayCtxForWorkflow(), &brokerv1.SetWorkflowVersionPinRequest{
		TenantId:   testWFTenant,
		OwnerGrant: mintWFGrant(t),
		LineageId:  uuid.New().String(),
		Version:    1,
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("skill deny pin south: want PermissionDenied, got %v", err)
	}
}

// ── North-path handler tests ──────────────────────────────────────────────────

// TestNorthCP9_ForkWorkflow_OwnSource_Succeeds.
func TestNorthCP9_ForkWorkflow_OwnSource_Succeeds(t *testing.T) {
	srcLineage := uuid.New()
	store := newFakeForkStore()
	store.addCurrent(sourceWorkflowRow(srcLineage))

	svc := newBrokerSvcForCP9Fork(t, store, true)
	ctx := ctxWithSubject(testWFTenant, testWFOwner, "")

	resp, err := svc.ForkWorkflow(ctx, &brokerv1.ForkWorkflowRequest{
		TenantId:        testWFTenant,
		OwnerUserId:     testWFOwner,
		SourceLineageId: srcLineage.String(),
		NewName:         "north-fork",
	})
	if err != nil {
		t.Fatalf("north fork: unexpected error: %v", err)
	}
	if resp.LineageId == "" {
		t.Fatal("lineage_id must not be empty")
	}
}

// TestNorthCP9_ForkWorkflow_SkillDeny_PermissionDenied.
func TestNorthCP9_ForkWorkflow_SkillDeny_PermissionDenied(t *testing.T) {
	store := newFakeForkStore()
	svc := newBrokerSvcForCP9Fork(t, store, false /* skill denied */)
	ctx := ctxWithSubject(testWFTenant, testWFOwner, "")

	_, err := svc.ForkWorkflow(ctx, &brokerv1.ForkWorkflowRequest{
		TenantId:        testWFTenant,
		OwnerUserId:     testWFOwner,
		SourceLineageId: uuid.New().String(),
		NewName:         "fork",
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("north fork skill deny: want PermissionDenied, got %v", err)
	}
}

// TestNorthCP9_SetVersionPin_ApprovedVersion_Succeeds.
func TestNorthCP9_SetVersionPin_ApprovedVersion_Succeeds(t *testing.T) {
	lineageID := uuid.New()
	store := newFakePinStore()
	store.addVersion(db.WorkflowRow{
		ID:            uuid.New(),
		LineageID:     lineageID,
		Version:       1,
		ApprovalState: "approved",
	})

	svc := newBrokerSvcForCP9Pin(t, store, true)
	ctx := ctxWithSubject(testWFTenant, testWFOwner, "")

	_, err := svc.SetWorkflowVersionPin(ctx, &brokerv1.SetWorkflowVersionPinRequest{
		TenantId:    testWFTenant,
		OwnerUserId: testWFOwner,
		LineageId:   lineageID.String(),
		Version:     1,
	})
	if err != nil {
		t.Fatalf("north SetVersionPin: unexpected error: %v", err)
	}
}

// TestNorthCP9_SetVersionPin_ProposedVersion_FailedPrecondition.
func TestNorthCP9_SetVersionPin_ProposedVersion_FailedPrecondition(t *testing.T) {
	lineageID := uuid.New()
	store := newFakePinStore()
	store.addVersion(db.WorkflowRow{
		ID:            uuid.New(),
		LineageID:     lineageID,
		Version:       2,
		ApprovalState: "proposed",
	})

	svc := newBrokerSvcForCP9Pin(t, store, true)
	ctx := ctxWithSubject(testWFTenant, testWFOwner, "")

	_, err := svc.SetWorkflowVersionPin(ctx, &brokerv1.SetWorkflowVersionPinRequest{
		TenantId:    testWFTenant,
		OwnerUserId: testWFOwner,
		LineageId:   lineageID.String(),
		Version:     2,
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("north proposed version: want FailedPrecondition, got %v", err)
	}
}

// TestNorthCP9_ClearVersionPin_Succeeds.
func TestNorthCP9_ClearVersionPin_Succeeds(t *testing.T) {
	lineageID := uuid.New()
	store := newFakePinStore()

	svc := newBrokerSvcForCP9Pin(t, store, true)
	ctx := ctxWithSubject(testWFTenant, testWFOwner, "")

	_, err := svc.ClearWorkflowVersionPin(ctx, &brokerv1.ClearWorkflowVersionPinRequest{
		TenantId:    testWFTenant,
		OwnerUserId: testWFOwner,
		LineageId:   lineageID.String(),
	})
	if err != nil {
		t.Fatalf("north ClearVersionPin: unexpected error: %v", err)
	}
}

// TestNorthCP9_SkillDeny_Pin_PermissionDenied.
func TestNorthCP9_SkillDeny_Pin_PermissionDenied(t *testing.T) {
	store := newFakePinStore()
	svc := newBrokerSvcForCP9Pin(t, store, false /* skill denied */)
	ctx := ctxWithSubject(testWFTenant, testWFOwner, "")

	_, err := svc.SetWorkflowVersionPin(ctx, &brokerv1.SetWorkflowVersionPinRequest{
		TenantId:    testWFTenant,
		OwnerUserId: testWFOwner,
		LineageId:   uuid.New().String(),
		Version:     1,
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("north skill deny pin: want PermissionDenied, got %v", err)
	}
}
