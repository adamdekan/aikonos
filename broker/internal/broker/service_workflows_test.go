package broker

// Tests for SaveWorkflow (CP3), GetWorkflow (CP4), and ListWorkflows (CP5a) —
// south-bound handlers.
//
// DB-gated: skip when AIKONOS_TEST_DB_URL is not set (mirrors the cp2 pattern
// from gateway_tasks_test.go and the providers test suite). The fake-store path
// is exercised to cover the guard conditions (nil Workflows repo, invalid JSON,
// grant validation) without a real database.
//
// North-handler tests (BrokerService — OIDC bearer path) are at the bottom of
// this file, prefixed TestNorthWorkflow_*.

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
	"github.com/adamdekan/aikonos/broker/internal/auth"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/gatewaygrant"
	"github.com/adamdekan/aikonos/broker/internal/policy"
	"github.com/adamdekan/aikonos/broker/internal/workflowsvc"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// newSandboxSvcWithFGA builds a SandboxService with a single workflow store
// (whichever fake the caller's scenario exercises) and an injected skill-gate
// policy (allow or deny). Uses the package-level fakeSkillPolicy defined in
// skill_gate_test.go.
func newSandboxSvcWithFGA(t *testing.T, store workflowsvc.Store, allow bool) *SandboxService {
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
		Workflows:       store,
	})
	svc.skillPolicy = &fakeSkillPolicy{enabled: true, granted: allow}
	return svc
}

// ── fakeWorkflowStore ──────────────────────────────────────────────────────────

// fakeWorkflowStore satisfies workflowStore for SaveWorkflow unit tests
// (GetCurrent + CreateVersion overridden; everything else via the embedded stub).
type fakeWorkflowStore struct {
	stubWorkflowStore
	created []db.WorkflowRow
}

func (f *fakeWorkflowStore) GetCurrent(_ context.Context, _ string, _ uuid.UUID) (*db.WorkflowRow, error) {
	// Fake: return a private approved row so the edit path can inherit from it.
	return &db.WorkflowRow{
		Status:           "private",
		VisibilityKind:   "private",
		VisibilityGroups: json.RawMessage(`[]`),
	}, nil
}

func (f *fakeWorkflowStore) CreateVersion(_ context.Context, _ string, row db.WorkflowRow) (db.WorkflowRow, error) {
	row.ID = uuid.New()
	row.Version = 1
	if row.LineageID == uuid.Nil {
		row.LineageID = uuid.New()
	}
	f.created = append(f.created, row)
	return row, nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

const (
	testWFTenant = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	testWFOwner  = "alice@example.com"
)

// minimalValidWorkflowJSON returns a schema-valid Workflow JSON string.
func minimalValidWorkflowJSON() string {
	m := map[string]any{
		"apiVersion": "aikonos.com/v1",
		"kind":       "Workflow",
		"metadata": map[string]any{
			"name": "test-workflow",
			"visibility": map[string]any{
				"kind": "private",
			},
		},
		"steps": []any{
			map[string]any{"skill": "doc.read"},
		},
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func newSandboxSvcForWorkflowTest(t *testing.T, store workflowsvc.Store) *SandboxService {
	t.Helper()
	em, err := audit.NewEmitter(context.Background(), audit.Config{})
	if err != nil {
		t.Fatalf("audit.NewEmitter: %v", err)
	}
	return NewSandboxService(Deps{
		Logger:          zap.NewNop(),
		Audit:           em,
		GatewaySpiffeID: testGWSpiffeID,
		GatewayGrantKey: testGrantKey,
		GatewayGrantTTL: testGrantTTL,
		Workflows:       store,
	})
}

// gatewayCtxForWorkflow returns a context with the gateway SPIFFE identity.
func gatewayCtxForWorkflow() context.Context {
	return auth.WithIdentity(context.Background(), &auth.Identity{SpiffeID: testGWSpiffeID})
}

// mintWFGrant mints an owner grant for workflow tests.
func mintWFGrant(t *testing.T) string {
	t.Helper()
	g, err := gatewaygrant.Mint(testGrantKey, testWFTenant, testWFOwner, testGrantTTL)
	if err != nil {
		t.Fatalf("mintWFGrant: %v", err)
	}
	return g
}

// ── Tests ─────────────────────────────────────────────────────────────────────

// TestSaveWorkflow_NilWorkflows_FailedPrecondition verifies that calling
// SaveWorkflow when Deps.Workflows is nil (feature not wired) returns
// FailedPrecondition — safe fail-closed behaviour.
func TestSaveWorkflow_NilWorkflows_FailedPrecondition(t *testing.T) {
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
		// Workflows: nil — not wired
	})

	_, err = svc.SaveWorkflow(gatewayCtxForWorkflow(), &brokerv1.SaveWorkflowRequest{
		TenantId:       testWFTenant,
		OwnerUserId:    testWFOwner,
		OwnerGrant:     mintWFGrant(t),
		Name:           "test",
		DefinitionJson: minimalValidWorkflowJSON(),
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("nil Workflows: want FailedPrecondition, got %v", err)
	}
}

// TestSaveWorkflow_InvalidJSON_InvalidArgument verifies that a malformed
// definition_json is rejected with a field-named error.
func TestSaveWorkflow_InvalidJSON_InvalidArgument(t *testing.T) {
	store := &fakeWorkflowStore{}
	svc := newSandboxSvcForWorkflowTest(t, store)

	_, err := svc.SaveWorkflow(gatewayCtxForWorkflow(), &brokerv1.SaveWorkflowRequest{
		TenantId:       testWFTenant,
		OwnerUserId:    testWFOwner,
		OwnerGrant:     mintWFGrant(t),
		Name:           "test",
		DefinitionJson: "not-valid-json",
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("invalid JSON: want InvalidArgument, got %v", err)
	}
}

// TestSaveWorkflow_InvalidWorkflowSchema_InvalidArgument verifies that a
// JSON object that fails workflow validation (e.g. wrong apiVersion) is
// rejected with InvalidArgument naming the field.
func TestSaveWorkflow_InvalidWorkflowSchema_InvalidArgument(t *testing.T) {
	store := &fakeWorkflowStore{}
	svc := newSandboxSvcForWorkflowTest(t, store)

	badWF, _ := json.Marshal(map[string]any{
		"apiVersion": "wrong/v99",
		"kind":       "Workflow",
		"metadata":   map[string]any{"name": "bad", "visibility": map[string]any{"kind": "private"}},
	})

	_, err := svc.SaveWorkflow(gatewayCtxForWorkflow(), &brokerv1.SaveWorkflowRequest{
		TenantId:       testWFTenant,
		OwnerUserId:    testWFOwner,
		OwnerGrant:     mintWFGrant(t),
		Name:           "bad",
		DefinitionJson: string(badWF),
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("schema violation: want InvalidArgument, got %v", err)
	}
}

// TestSaveWorkflow_BadGrant_PermissionDenied verifies that an invalid owner
// grant is rejected with PermissionDenied — the same gate as CreateGatewayTask.
func TestSaveWorkflow_BadGrant_PermissionDenied(t *testing.T) {
	store := &fakeWorkflowStore{}
	svc := newSandboxSvcForWorkflowTest(t, store)

	_, err := svc.SaveWorkflow(gatewayCtxForWorkflow(), &brokerv1.SaveWorkflowRequest{
		TenantId:       testWFTenant,
		OwnerUserId:    testWFOwner,
		OwnerGrant:     "totally-invalid-grant",
		Name:           "test",
		DefinitionJson: minimalValidWorkflowJSON(),
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("bad grant: want PermissionDenied, got %v", err)
	}
}

// TestSaveWorkflow_NotGateway_PermissionDenied verifies that a non-gateway
// SPIFFE identity is rejected.
func TestSaveWorkflow_NotGateway_PermissionDenied(t *testing.T) {
	store := &fakeWorkflowStore{}
	svc := newSandboxSvcForWorkflowTest(t, store)

	nonGWCtx := auth.WithIdentity(context.Background(), &auth.Identity{SpiffeID: "spiffe://aikonos.com/some-sandbox"})
	_, err := svc.SaveWorkflow(nonGWCtx, &brokerv1.SaveWorkflowRequest{
		TenantId:       testWFTenant,
		OwnerUserId:    testWFOwner,
		OwnerGrant:     mintWFGrant(t),
		Name:           "test",
		DefinitionJson: minimalValidWorkflowJSON(),
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-gateway SPIFFE: want PermissionDenied, got %v", err)
	}
}

// ── fakeWorkflowGetStore ───────────────────────────────────────────────────────

// fakeWorkflowGetStore satisfies workflowStore for GetWorkflow unit tests
// (ResolveVersionForUser + GetVersion overridden; rest via the embedded stub).
type fakeWorkflowGetStore struct {
	stubWorkflowStore
	// rows maps lineage_id string → slice of WorkflowRow (one per version).
	rows map[string][]*db.WorkflowRow
	// pins maps "userID/lineageID" → pinned version. Absent = use current.
	pins map[string]int
}

func newFakeGetStore() *fakeWorkflowGetStore {
	return &fakeWorkflowGetStore{
		rows: map[string][]*db.WorkflowRow{},
		pins: map[string]int{},
	}
}

func (f *fakeWorkflowGetStore) add(row db.WorkflowRow) {
	key := row.LineageID.String()
	f.rows[key] = append(f.rows[key], &row)
}

func (f *fakeWorkflowGetStore) pin(userID string, lineageID uuid.UUID, version int) {
	f.pins[userID+"/"+lineageID.String()] = version
}

func (f *fakeWorkflowGetStore) ResolveVersionForUser(_ context.Context, _, userID string, lineageID uuid.UUID) (int, error) {
	if v, ok := f.pins[userID+"/"+lineageID.String()]; ok {
		return v, nil
	}
	rows := f.rows[lineageID.String()]
	if len(rows) == 0 {
		return 0, fmt.Errorf("workflow lineage %s not found", lineageID)
	}
	max := rows[0].Version
	for _, r := range rows[1:] {
		if r.Version > max {
			max = r.Version
		}
	}
	return max, nil
}

func (f *fakeWorkflowGetStore) GetVersion(_ context.Context, _ string, lineageID uuid.UUID, version int) (*db.WorkflowRow, error) {
	for _, r := range f.rows[lineageID.String()] {
		if r.Version == version {
			return r, nil
		}
	}
	return nil, fmt.Errorf("workflow lineage %s version %d not found", lineageID, version)
}

// newSandboxSvcForGetTest builds a SandboxService with the get-store injected.
func newSandboxSvcForGetTest(t *testing.T, store workflowsvc.Store) *SandboxService {
	t.Helper()
	em, err := audit.NewEmitter(context.Background(), audit.Config{})
	if err != nil {
		t.Fatalf("audit.NewEmitter: %v", err)
	}
	return NewSandboxService(Deps{
		Logger:          zap.NewNop(),
		Audit:           em,
		GatewaySpiffeID: testGWSpiffeID,
		GatewayGrantKey: testGrantKey,
		GatewayGrantTTL: testGrantTTL,
		Workflows:       store,
	})
}

// ── GetWorkflow tests ─────────────────────────────────────────────────────────

// TestGetWorkflow_NilWorkflows_FailedPrecondition verifies fail-closed when
// the Workflows repo is not wired.
func TestGetWorkflow_NilWorkflows_FailedPrecondition(t *testing.T) {
	em, _ := audit.NewEmitter(context.Background(), audit.Config{})
	svc := NewSandboxService(Deps{
		Logger:          zap.NewNop(),
		Audit:           em,
		GatewaySpiffeID: testGWSpiffeID,
		GatewayGrantKey: testGrantKey,
		GatewayGrantTTL: testGrantTTL,
		// Workflows: nil — not wired
	})
	lineageID := uuid.New()
	_, err := svc.GetWorkflow(gatewayCtxForWorkflow(), &brokerv1.GetWorkflowRequest{
		TenantId:   testWFTenant,
		UserId:     testWFOwner,
		OwnerGrant: mintWFGrant(t),
		LineageId:  lineageID.String(),
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("nil store: want FailedPrecondition, got %v", err)
	}
}

// TestGetWorkflow_BadGrant_PermissionDenied verifies that an invalid owner
// grant is rejected.
func TestGetWorkflow_BadGrant_PermissionDenied(t *testing.T) {
	store := newFakeGetStore()
	svc := newSandboxSvcForGetTest(t, store)
	_, err := svc.GetWorkflow(gatewayCtxForWorkflow(), &brokerv1.GetWorkflowRequest{
		TenantId:   testWFTenant,
		UserId:     testWFOwner,
		OwnerGrant: "bad-grant",
		LineageId:  uuid.New().String(),
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("bad grant: want PermissionDenied, got %v", err)
	}
}

// TestGetWorkflow_NotFound_NotFound verifies that an unknown lineage returns
// NotFound (not a panic or internal error).
func TestGetWorkflow_NotFound_NotFound(t *testing.T) {
	store := newFakeGetStore()
	svc := newSandboxSvcForGetTest(t, store)
	_, err := svc.GetWorkflow(gatewayCtxForWorkflow(), &brokerv1.GetWorkflowRequest{
		TenantId:   testWFTenant,
		UserId:     testWFOwner,
		OwnerGrant: mintWFGrant(t),
		LineageId:  uuid.New().String(), // not in store
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("unknown lineage: want NotFound, got %v", err)
	}
}

// TestGetWorkflow_CurrentVersion returns the highest version when no pin exists.
func TestGetWorkflow_CurrentVersion(t *testing.T) {
	store := newFakeGetStore()
	lineageID := uuid.New()
	tenantID := uuid.MustParse(testWFTenant)
	defJSON := minimalValidWorkflowJSON()

	store.add(db.WorkflowRow{
		ID: uuid.New(), LineageID: lineageID, Version: 1,
		TenantID: tenantID, OwnerUserID: testWFOwner,
		Name: "wf", Status: "private", VisibilityKind: "private",
		VisibilityGroups: []byte("[]"), Definition: []byte(defJSON),
	})
	store.add(db.WorkflowRow{
		ID: uuid.New(), LineageID: lineageID, Version: 2,
		TenantID: tenantID, OwnerUserID: testWFOwner,
		Name: "wf", Status: "private", VisibilityKind: "private",
		VisibilityGroups: []byte("[]"), Definition: []byte(defJSON),
	})

	svc := newSandboxSvcForGetTest(t, store)
	resp, err := svc.GetWorkflow(gatewayCtxForWorkflow(), &brokerv1.GetWorkflowRequest{
		TenantId:   testWFTenant,
		UserId:     testWFOwner,
		OwnerGrant: mintWFGrant(t),
		LineageId:  lineageID.String(),
	})
	if err != nil {
		t.Fatalf("GetWorkflow: unexpected error: %v", err)
	}
	if resp.Version != 2 {
		t.Errorf("want current version 2, got %d", resp.Version)
	}
	if resp.DefinitionJson != defJSON {
		t.Errorf("definition_json mismatch")
	}
}

// TestGetWorkflow_PinnedVersion returns the pinned version when one is set,
// even when a newer version exists.
func TestGetWorkflow_PinnedVersion(t *testing.T) {
	store := newFakeGetStore()
	lineageID := uuid.New()
	tenantID := uuid.MustParse(testWFTenant)
	defV1 := minimalValidWorkflowJSON()
	defV2 := minimalValidWorkflowJSON() // same content; version is what we care about

	store.add(db.WorkflowRow{
		ID: uuid.New(), LineageID: lineageID, Version: 1,
		TenantID: tenantID, OwnerUserID: testWFOwner,
		Name: "wf", Status: "private", VisibilityKind: "private",
		VisibilityGroups: []byte("[]"), Definition: []byte(defV1),
	})
	store.add(db.WorkflowRow{
		ID: uuid.New(), LineageID: lineageID, Version: 2,
		TenantID: tenantID, OwnerUserID: testWFOwner,
		Name: "wf", Status: "private", VisibilityKind: "private",
		VisibilityGroups: []byte("[]"), Definition: []byte(defV2),
	})
	// Pin user to version 1.
	store.pin(testWFOwner, lineageID, 1)

	svc := newSandboxSvcForGetTest(t, store)
	resp, err := svc.GetWorkflow(gatewayCtxForWorkflow(), &brokerv1.GetWorkflowRequest{
		TenantId:   testWFTenant,
		UserId:     testWFOwner,
		OwnerGrant: mintWFGrant(t),
		LineageId:  lineageID.String(),
	})
	if err != nil {
		t.Fatalf("GetWorkflow pinned: unexpected error: %v", err)
	}
	// Must return version 1 despite version 2 existing.
	if resp.Version != 1 {
		t.Errorf("pinned: want version 1, got %d", resp.Version)
	}
}

// TestSaveWorkflow_Valid_PersistsAndReturnsIDs verifies the happy-path: a valid
// request persists one WorkflowRow and returns non-empty ids.
func TestSaveWorkflow_Valid_PersistsAndReturnsIDs(t *testing.T) {
	store := &fakeWorkflowStore{}
	svc := newSandboxSvcForWorkflowTest(t, store)

	resp, err := svc.SaveWorkflow(gatewayCtxForWorkflow(), &brokerv1.SaveWorkflowRequest{
		TenantId:       testWFTenant,
		OwnerUserId:    testWFOwner,
		OwnerGrant:     mintWFGrant(t),
		Name:           "my-workflow",
		Description:    "does stuff",
		DefinitionJson: minimalValidWorkflowJSON(),
		VisibilityKind: "private",
	})
	if err != nil {
		t.Fatalf("SaveWorkflow: unexpected error: %v", err)
	}
	if resp.WorkflowId == "" {
		t.Error("workflow_id must not be empty")
	}
	if resp.LineageId == "" {
		t.Error("lineage_id must not be empty")
	}
	if resp.Version != 1 {
		t.Errorf("version: want 1, got %d", resp.Version)
	}

	// Exactly one row persisted.
	if len(store.created) != 1 {
		t.Fatalf("expected 1 row persisted, got %d", len(store.created))
	}
	row := store.created[0]
	if row.OwnerUserID != testWFOwner {
		t.Errorf("owner: want %s, got %s", testWFOwner, row.OwnerUserID)
	}
	if row.Status != "private" {
		t.Errorf("status: want \"private\", got %q", row.Status)
	}
	if row.VisibilityKind != "private" {
		t.Errorf("visibility_kind: want \"private\", got %q", row.VisibilityKind)
	}
}

// ── CP5a: FGA skill gate tests ────────────────────────────────────────────────

// TestSaveWorkflow_FGADeny_PermissionDenied verifies that a user without the
// skill:workflows grant is denied with PermissionDenied — deny-by-default.
func TestSaveWorkflow_FGADeny_PermissionDenied(t *testing.T) {
	store := &fakeWorkflowStore{}
	svc := newSandboxSvcWithFGA(t, store, false /* deny */)

	_, err := svc.SaveWorkflow(gatewayCtxForWorkflow(), &brokerv1.SaveWorkflowRequest{
		TenantId:       testWFTenant,
		OwnerUserId:    testWFOwner,
		OwnerGrant:     mintWFGrant(t),
		Name:           "test",
		DefinitionJson: minimalValidWorkflowJSON(),
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("FGA deny: want PermissionDenied, got %v", err)
	}
}

// TestSaveWorkflow_FGAAllow_Proceeds verifies that a user with skill:workflows
// proceeds past the FGA gate and saves the workflow.
func TestSaveWorkflow_FGAAllow_Proceeds(t *testing.T) {
	store := &fakeWorkflowStore{}
	svc := newSandboxSvcWithFGA(t, store, true /* allow */)

	resp, err := svc.SaveWorkflow(gatewayCtxForWorkflow(), &brokerv1.SaveWorkflowRequest{
		TenantId:       testWFTenant,
		OwnerUserId:    testWFOwner,
		OwnerGrant:     mintWFGrant(t),
		Name:           "test",
		DefinitionJson: minimalValidWorkflowJSON(),
	})
	if err != nil {
		t.Fatalf("FGA allow: unexpected error: %v", err)
	}
	if resp.WorkflowId == "" {
		t.Error("workflow_id must not be empty after FGA allow")
	}
}

// TestGetWorkflow_FGADeny_PermissionDenied verifies that a user without the
// skill:workflows grant is denied with PermissionDenied on GetWorkflow.
func TestGetWorkflow_FGADeny_PermissionDenied(t *testing.T) {
	getStore := newFakeGetStore()
	svc := newSandboxSvcWithFGA(t, getStore, false /* deny */)

	_, err := svc.GetWorkflow(gatewayCtxForWorkflow(), &brokerv1.GetWorkflowRequest{
		TenantId:   testWFTenant,
		UserId:     testWFOwner,
		OwnerGrant: mintWFGrant(t),
		LineageId:  uuid.New().String(),
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("GetWorkflow FGA deny: want PermissionDenied, got %v", err)
	}
}

// TestGetWorkflow_FGAAllow_Proceeds verifies that a user with skill:workflows
// passes the FGA gate and proceeds to the store lookup.
func TestGetWorkflow_FGAAllow_Proceeds(t *testing.T) {
	getStore := newFakeGetStore()
	lineageID := uuid.New()
	tenantID := uuid.MustParse(testWFTenant)
	getStore.add(db.WorkflowRow{
		ID: uuid.New(), LineageID: lineageID, Version: 1,
		TenantID: tenantID, OwnerUserID: testWFOwner,
		Name: "wf", Status: "private", VisibilityKind: "private",
		VisibilityGroups: []byte("[]"), Definition: []byte(minimalValidWorkflowJSON()),
	})

	svc := newSandboxSvcWithFGA(t, getStore, true /* allow */)

	resp, err := svc.GetWorkflow(gatewayCtxForWorkflow(), &brokerv1.GetWorkflowRequest{
		TenantId:   testWFTenant,
		UserId:     testWFOwner,
		OwnerGrant: mintWFGrant(t),
		LineageId:  lineageID.String(),
	})
	if err != nil {
		t.Fatalf("GetWorkflow FGA allow: unexpected error: %v", err)
	}
	if resp.Version != 1 {
		t.Errorf("want version 1, got %d", resp.Version)
	}
}

// ── fakeWorkflowListStore ──────────────────────────────────────────────────────

// fakeWorkflowListStore satisfies workflowStore for ListWorkflows unit tests
// (ListByOwner + ListVisibleShared overridden; rest via the embedded stub).
type fakeWorkflowListStore struct {
	stubWorkflowStore
	rows map[string][]*db.WorkflowRow // key = ownerUserID
}

func newFakeListStore() *fakeWorkflowListStore {
	return &fakeWorkflowListStore{rows: map[string][]*db.WorkflowRow{}}
}

func (f *fakeWorkflowListStore) add(row db.WorkflowRow) {
	f.rows[row.OwnerUserID] = append(f.rows[row.OwnerUserID], &row)
}

// The tenant arg is ignored here on purpose: this fake only exercises the
// handler's owner binding and FGA gate. Tenant isolation is enforced by RLS in
// the real db.WorkflowRepo (verified separately), not by this in-memory fake.
func (f *fakeWorkflowListStore) ListByOwner(_ context.Context, _, ownerUserID, _ string, _ int) ([]*db.WorkflowRow, error) {
	return f.rows[ownerUserID], nil
}

// ListVisibleShared is a no-op on the basic fake — CP8 tests use fakeWorkflowListStoreCP8.
func (f *fakeWorkflowListStore) ListVisibleShared(_ context.Context, _ string, _ []string, _ string, _ int) ([]*db.WorkflowRow, error) {
	return nil, nil
}

// ── ListWorkflows tests ────────────────────────────────────────────────────────

// TestListWorkflows_FGADeny_PermissionDenied verifies deny-by-default: a user
// without skill:workflows receives PermissionDenied.
func TestListWorkflows_FGADeny_PermissionDenied(t *testing.T) {
	listStore := newFakeListStore()
	svc := newSandboxSvcWithFGA(t, listStore, false /* deny */)

	_, err := svc.ListWorkflows(gatewayCtxForWorkflow(), &brokerv1.ListWorkflowsRequest{
		TenantId:   testWFTenant,
		UserId:     testWFOwner,
		OwnerGrant: mintWFGrant(t),
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("ListWorkflows FGA deny: want PermissionDenied, got %v", err)
	}
}

// TestListWorkflows_FGAAllow_ReturnsOwnedWorkflows verifies that a user with
// skill:workflows receives their own workflows and no others.
func TestListWorkflows_FGAAllow_ReturnsOwnedWorkflows(t *testing.T) {
	listStore := newFakeListStore()
	tenantID := uuid.MustParse(testWFTenant)

	lineageA := uuid.New()
	lineageB := uuid.New()
	listStore.add(db.WorkflowRow{
		ID: uuid.New(), LineageID: lineageA, Version: 2,
		TenantID: tenantID, OwnerUserID: testWFOwner,
		Name: "workflow-alpha", Status: "private", VisibilityKind: "private",
		VisibilityGroups: []byte("[]"), Definition: []byte(minimalValidWorkflowJSON()),
	})
	listStore.add(db.WorkflowRow{
		ID: uuid.New(), LineageID: lineageB, Version: 1,
		TenantID: tenantID, OwnerUserID: testWFOwner,
		Name: "workflow-beta", Status: "private", VisibilityKind: "private",
		VisibilityGroups: []byte("[]"), Definition: []byte(minimalValidWorkflowJSON()),
	})
	// A row owned by a different user — must not appear in the response.
	listStore.add(db.WorkflowRow{
		ID: uuid.New(), LineageID: uuid.New(), Version: 1,
		TenantID: tenantID, OwnerUserID: "other@example.com",
		Name: "other-workflow", Status: "private", VisibilityKind: "private",
		VisibilityGroups: []byte("[]"), Definition: []byte(minimalValidWorkflowJSON()),
	})

	svc := newSandboxSvcWithFGA(t, listStore, true /* allow */)

	resp, err := svc.ListWorkflows(gatewayCtxForWorkflow(), &brokerv1.ListWorkflowsRequest{
		TenantId:   testWFTenant,
		UserId:     testWFOwner,
		OwnerGrant: mintWFGrant(t),
	})
	if err != nil {
		t.Fatalf("ListWorkflows: unexpected error: %v", err)
	}
	if len(resp.Items) != 2 {
		t.Fatalf("want 2 items, got %d", len(resp.Items))
	}
	// Verify both lineage IDs appear.
	seen := map[string]bool{}
	for _, item := range resp.Items {
		seen[item.LineageId] = true
	}
	if !seen[lineageA.String()] || !seen[lineageB.String()] {
		t.Errorf("expected lineage IDs %s and %s in response, got: %v", lineageA, lineageB, resp.Items)
	}
}

// TestListWorkflows_NilStore_FailedPrecondition verifies fail-closed when the
// Workflows repo is not wired.
func TestListWorkflows_NilStore_FailedPrecondition(t *testing.T) {
	em, _ := audit.NewEmitter(context.Background(), audit.Config{})
	svc := NewSandboxService(Deps{
		Logger:          zap.NewNop(),
		Audit:           em,
		GatewaySpiffeID: testGWSpiffeID,
		GatewayGrantKey: testGrantKey,
		GatewayGrantTTL: testGrantTTL,
		// Workflows: nil — not wired
	})
	svc.skillPolicy = &fakeSkillPolicy{enabled: true, granted: true}

	_, err := svc.ListWorkflows(gatewayCtxForWorkflow(), &brokerv1.ListWorkflowsRequest{
		TenantId:   testWFTenant,
		UserId:     testWFOwner,
		OwnerGrant: mintWFGrant(t),
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("nil store: want FailedPrecondition, got %v", err)
	}
}

// ── North-path (BrokerService — OIDC bearer) tests ───────────────────────────
//
// These tests exercise BrokerService.SaveWorkflow / GetWorkflow / ListWorkflows
// (the interactive OIDC-bearer path added in finding 1 of the CP5b review).
//
// Identity is injected via ctxWithIdentity (auth.WithIdentity — Subject-first
// PrincipalID), matching the pattern used by connectors_test.go, alerts_test.go,
// and files_test.go for all other north-bound handlers.
//
// FGA is exercised through a real policy.Engine backed by fakeFGA (the same
// httptest approach as newAlertsDeps in alerts_test.go) so that
// checkWorkflowSkillNorth exercises the actual CheckFGA call path.

// TestNorthWorkflow_SaveWorkflow_OwnerFromIdentity proves that the authenticated
// OIDC subject is used as the owner, NOT req.OwnerUserId. A spoofed OwnerUserId
// that differs from the verified subject must be rejected with PermissionDenied.
func TestNorthWorkflow_SaveWorkflow_OwnerFromIdentity(t *testing.T) {
	// WHY: callerIdentity is the trust anchor on the north path. If req.OwnerUserId
	// were used as the owner, any caller could persist a workflow attributed to an
	// arbitrary user. The broker must bind owner from the verified OIDC subject.

	const (
		realOwner   = "alice@example.com"
		spoofedUser = "eve@example.com"
	)

	checks := map[string]bool{
		"user:" + realOwner + "|can_invoke|skill:workflows": true,
	}
	fga := &fakeFGA{checks: checks}
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

	// ── Sub-test A: spoofed OwnerUserId != verified subject → PermissionDenied ──
	t.Run("spoof_rejected", func(t *testing.T) {
		svc := NewBrokerService(Deps{
			Logger:   zap.NewNop(),
			Policy:   eng,
			Audit:    em,
			TenantID: "aikonos-dev",
		})
		// Context carries realOwner as subject; request carries spoofedUser.
		ctx := ctxWithSubject(testWFTenant, realOwner, "")
		_, err := svc.SaveWorkflow(ctx, &brokerv1.SaveWorkflowRequest{
			TenantId:       testWFTenant,
			OwnerUserId:    spoofedUser, // mismatch — must be rejected
			DefinitionJson: minimalValidWorkflowJSON(),
			Name:           "test",
		})
		if got := status.Code(err); got != codes.PermissionDenied {
			t.Fatalf("spoof: want PermissionDenied, got %v (err: %v)", got, err)
		}
	})

	// The "owner_bound_from_identity" half of this test (calling
	// saveWorkflowCore/workflowsvc.Save directly with the resolved owner) moved
	// to workflowsvc.TestSave_OwnerBoundFromIdentity — a core-level direct-call
	// test (workflowsvc-extraction CP2). This function now covers only the
	// wrapper-level "spoof_rejected" case, which exercises callerIdentity via
	// svc.SaveWorkflow.
}

// TestNorthWorkflow_SkillDenied_PermissionDenied proves that checkWorkflowSkillNorth
// returns PermissionDenied when FGA denies skill:workflows for the caller.
func TestNorthWorkflow_SkillDenied_PermissionDenied(t *testing.T) {
	// WHY: the skill:workflows gate is deny-by-default. A BrokerService north
	// handler must reject callers that lack the grant with PermissionDenied,
	// not proceed to the workflow store. This test exercises checkWorkflowSkillNorth
	// via the real policy.Engine→fakeFGA path so the FGA call itself is verified.

	const owner = "bob@example.com"

	// FGA returns false for all checks (no entry in checks map, no admins).
	fga := &fakeFGA{checks: map[string]bool{}}
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

	svc := NewBrokerService(Deps{
		Logger:   zap.NewNop(),
		Policy:   eng,
		Audit:    em,
		TenantID: "aikonos-dev",
	})

	ctx := ctxWithSubject(testWFTenant, owner, "")

	// All three north handlers must honour the skill gate.
	t.Run("SaveWorkflow", func(t *testing.T) {
		_, err := svc.SaveWorkflow(ctx, &brokerv1.SaveWorkflowRequest{
			TenantId:       testWFTenant,
			OwnerUserId:    owner,
			DefinitionJson: minimalValidWorkflowJSON(),
			Name:           "test",
		})
		if got := status.Code(err); got != codes.PermissionDenied {
			t.Fatalf("SaveWorkflow: want PermissionDenied, got %v (err: %v)", got, err)
		}
	})

	t.Run("GetWorkflow", func(t *testing.T) {
		_, err := svc.GetWorkflow(ctx, &brokerv1.GetWorkflowRequest{
			TenantId:  testWFTenant,
			UserId:    owner,
			LineageId: uuid.New().String(),
		})
		if got := status.Code(err); got != codes.PermissionDenied {
			t.Fatalf("GetWorkflow: want PermissionDenied, got %v (err: %v)", got, err)
		}
	})

	t.Run("ListWorkflows", func(t *testing.T) {
		_, err := svc.ListWorkflows(ctx, &brokerv1.ListWorkflowsRequest{
			TenantId: testWFTenant,
			UserId:   owner,
		})
		if got := status.Code(err); got != codes.PermissionDenied {
			t.Fatalf("ListWorkflows: want PermissionDenied, got %v (err: %v)", got, err)
		}
	})
}

// TestNorthWorkflow_SvcSubjectRejected proves that callerIdentity rejects
// svc- principals on all three north workflow handlers with PermissionDenied.
func TestNorthWorkflow_SvcSubjectRejected(t *testing.T) {
	// WHY: svc- subjects are service accounts for external agent API-key access.
	// They must never appear as human callers on the north-bound surface. This is
	// enforced in callerIdentity; the test locks in that the workflow handlers
	// inherit the rejection without any per-handler special-casing.

	const svcSubject = "svc-agent-abc123"

	svc := NewBrokerService(Deps{
		Logger:   zap.NewNop(),
		TenantID: "aikonos-dev",
	})

	ctx := ctxWithSubject(testWFTenant, svcSubject, "")

	t.Run("SaveWorkflow", func(t *testing.T) {
		_, err := svc.SaveWorkflow(ctx, &brokerv1.SaveWorkflowRequest{
			TenantId:       testWFTenant,
			OwnerUserId:    svcSubject,
			DefinitionJson: minimalValidWorkflowJSON(),
			Name:           "test",
		})
		if got := status.Code(err); got != codes.PermissionDenied {
			t.Fatalf("SaveWorkflow: want PermissionDenied for svc- subject, got %v (err: %v)", got, err)
		}
	})

	t.Run("GetWorkflow", func(t *testing.T) {
		_, err := svc.GetWorkflow(ctx, &brokerv1.GetWorkflowRequest{
			TenantId:  testWFTenant,
			UserId:    svcSubject,
			LineageId: uuid.New().String(),
		})
		if got := status.Code(err); got != codes.PermissionDenied {
			t.Fatalf("GetWorkflow: want PermissionDenied for svc- subject, got %v (err: %v)", got, err)
		}
	})

	t.Run("ListWorkflows", func(t *testing.T) {
		_, err := svc.ListWorkflows(ctx, &brokerv1.ListWorkflowsRequest{
			TenantId: testWFTenant,
			UserId:   svcSubject,
		})
		if got := status.Code(err); got != codes.PermissionDenied {
			t.Fatalf("ListWorkflows: want PermissionDenied for svc- subject, got %v (err: %v)", got, err)
		}
	})
}
