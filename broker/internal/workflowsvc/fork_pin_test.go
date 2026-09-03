package workflowsvc

// Core-level direct-call tests relocated from
// broker/internal/broker/service_workflows_cp9_test.go (workflowsvc-extraction
// CP2 — see ). Fixtures
// (fakeWorkflowForkStore, fakeWorkflowPinStore, sourceWorkflowRow) are
// duplicated here per spec: broker's wrapper-level South/North CP9 tests still
// use the same fixtures under the same names.

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

// ── Fork (formerly forkCore) unit tests ──────────────────────────────────────

// TestForkCore_OwnSource_CreatesNewLineage: forker owns the source → new lineage
// created with parent=source, owner=forker, status=private, name=new_name,
// definition copied from source.
func TestForkCore_OwnSource_CreatesNewLineage(t *testing.T) {
	srcLineage := uuid.New()
	store := newFakeForkStore()
	src := sourceWorkflowRow(srcLineage)
	store.addCurrent(src)

	em, _ := audit.NewEmitter(context.Background(), audit.Config{})

	resp, err := Fork(
		context.Background(),
		testWFTenant, testWFOwner,
		&brokerv1.ForkWorkflowRequest{
			TenantId:        testWFTenant,
			OwnerUserId:     testWFOwner,
			SourceLineageId: srcLineage.String(),
			NewName:         "forked-name",
		},
		store, nil, em, zap.NewNop(),
	)
	if err != nil {
		t.Fatalf("Fork: unexpected error: %v", err)
	}
	if resp.LineageId == "" {
		t.Fatal("lineage_id must not be empty")
	}
	if resp.LineageId == srcLineage.String() {
		t.Error("fork must get a fresh lineage_id, not the source's")
	}
	if resp.LineageId != store.created[0].LineageID.String() {
		t.Errorf("response lineage_id %q does not match stored lineage_id %q",
			resp.LineageId, store.created[0].LineageID)
	}
	if len(store.created) != 1 {
		t.Fatalf("want 1 created row, got %d", len(store.created))
	}
	row := store.created[0]
	if row.OwnerUserID != testWFOwner {
		t.Errorf("owner: want %s, got %s", testWFOwner, row.OwnerUserID)
	}
	if row.Name != "forked-name" {
		t.Errorf("name: want forked-name, got %s", row.Name)
	}
	if row.Status != "private" {
		t.Errorf("status: want private, got %s", row.Status)
	}
	if row.VisibilityKind != "private" {
		t.Errorf("visibility_kind: want private, got %s", row.VisibilityKind)
	}
	if row.ParentLineageID == nil || *row.ParentLineageID != srcLineage {
		t.Errorf("parent_lineage_id: want %s, got %v", srcLineage, row.ParentLineageID)
	}
	if string(row.Definition) != minimalValidWorkflowJSON() {
		t.Error("definition must be copied from source")
	}
	if row.Description != src.Description {
		t.Errorf("description: want %q, got %q", src.Description, row.Description)
	}
}

// TestForkCore_EmptyName_InvalidArgument: empty new_name → InvalidArgument.
func TestForkCore_EmptyName_InvalidArgument(t *testing.T) {
	store := newFakeForkStore()
	em, _ := audit.NewEmitter(context.Background(), audit.Config{})

	_, err := Fork(
		context.Background(), testWFTenant, testWFOwner,
		&brokerv1.ForkWorkflowRequest{
			TenantId:        testWFTenant,
			SourceLineageId: uuid.New().String(),
			NewName:         "", // empty
		},
		store, nil, em, zap.NewNop(),
	)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty name: want InvalidArgument, got %v", err)
	}
}

// TestForkCore_SourceNotFound_PermissionDenied: source lineage not in store
// → PermissionDenied (opaque — don't reveal existence).
func TestForkCore_SourceNotFound_PermissionDenied(t *testing.T) {
	store := newFakeForkStore() // empty — no current row
	em, _ := audit.NewEmitter(context.Background(), audit.Config{})

	_, err := Fork(
		context.Background(), testWFTenant, "other@example.com",
		&brokerv1.ForkWorkflowRequest{
			TenantId:        testWFTenant,
			SourceLineageId: uuid.New().String(),
			NewName:         "fork",
		},
		store, nil, em, zap.NewNop(),
	)
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("source not found: want PermissionDenied, got %v", err)
	}
}

// TestForkCore_PrivateSourceOtherOwner_PermissionDenied: private workflow owned
// by someone else is not forkable.
func TestForkCore_PrivateSourceOtherOwner_PermissionDenied(t *testing.T) {
	srcLineage := uuid.New()
	store := newFakeForkStore()
	store.addCurrent(db.WorkflowRow{
		ID:               uuid.New(),
		LineageID:        srcLineage,
		Version:          1,
		TenantID:         uuid.MustParse(testWFTenant),
		OwnerUserID:      "other@example.com", // NOT the forker
		Name:             "secret-wf",
		Status:           "private", // private → not forkable by non-owner
		VisibilityKind:   "private",
		VisibilityGroups: json.RawMessage(`[]`),
		Definition:       json.RawMessage(minimalValidWorkflowJSON()),
		ApprovalState:    "approved",
	})
	em, _ := audit.NewEmitter(context.Background(), audit.Config{})

	_, err := Fork(
		context.Background(), testWFTenant, testWFOwner,
		&brokerv1.ForkWorkflowRequest{
			TenantId:        testWFTenant,
			SourceLineageId: srcLineage.String(),
			NewName:         "fork",
		},
		store, nil, em, zap.NewNop(),
	)
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("private source other owner: want PermissionDenied, got %v", err)
	}
}

// TestForkCore_PublishedSourceMemberOfGroup_Succeeds: published workflow shared
// to a group the forker belongs to → fork succeeds.
func TestForkCore_PublishedSourceMemberOfGroup_Succeeds(t *testing.T) {
	srcLineage := uuid.New()
	store := newFakeForkStore()
	store.addCurrent(db.WorkflowRow{
		ID:               uuid.New(),
		LineageID:        srcLineage,
		Version:          2,
		TenantID:         uuid.MustParse(testWFTenant),
		OwnerUserID:      "publisher@example.com",
		Name:             "shared-wf",
		Status:           "published",
		VisibilityKind:   "shared",
		VisibilityGroups: json.RawMessage(`["` + testGroupAlpha + `"]`),
		Definition:       json.RawMessage(minimalValidWorkflowJSON()),
		ApprovalState:    "approved",
	})

	// checkFGA: forker is member of group:alpha
	checkFGA := func(_ context.Context, user, relation, object string) (bool, error) {
		return user == "user:"+testWFOwner && relation == "member" && object == "group:"+testGroupAlpha, nil
	}
	em, _ := audit.NewEmitter(context.Background(), audit.Config{})

	resp, err := Fork(
		context.Background(), testWFTenant, testWFOwner,
		&brokerv1.ForkWorkflowRequest{
			TenantId:        testWFTenant,
			SourceLineageId: srcLineage.String(),
			NewName:         "my-fork",
		},
		store, checkFGA, em, zap.NewNop(),
	)
	if err != nil {
		t.Fatalf("published source member: unexpected error: %v", err)
	}
	if resp.LineageId == "" {
		t.Fatal("lineage_id must not be empty")
	}
	if len(store.created) != 1 {
		t.Fatalf("want 1 created row, got %d", len(store.created))
	}
}

// TestForkCore_PublishedSourceNotMemberOfGroup_PermissionDenied: published
// workflow but forker is NOT in any visibility group.
func TestForkCore_PublishedSourceNotMemberOfGroup_PermissionDenied(t *testing.T) {
	srcLineage := uuid.New()
	store := newFakeForkStore()
	store.addCurrent(db.WorkflowRow{
		ID:               uuid.New(),
		LineageID:        srcLineage,
		Version:          1,
		TenantID:         uuid.MustParse(testWFTenant),
		OwnerUserID:      "publisher@example.com",
		Name:             "shared-wf",
		Status:           "published",
		VisibilityKind:   "shared",
		VisibilityGroups: json.RawMessage(`["` + testGroupAlpha + `"]`),
		Definition:       json.RawMessage(minimalValidWorkflowJSON()),
		ApprovalState:    "approved",
	})

	// checkFGA: forker is NOT member of group:alpha
	checkFGA := func(_ context.Context, _, _, _ string) (bool, error) { return false, nil }
	em, _ := audit.NewEmitter(context.Background(), audit.Config{})

	_, err := Fork(
		context.Background(), testWFTenant, testWFOwner,
		&brokerv1.ForkWorkflowRequest{
			TenantId:        testWFTenant,
			SourceLineageId: srcLineage.String(),
			NewName:         "my-fork",
		},
		store, checkFGA, em, zap.NewNop(),
	)
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("not member: want PermissionDenied, got %v", err)
	}
}

// TestForkCore_InheritsBinding: a fork copies the source's agent binding into
// the new lineage's v1 (lineage-immutable inheritance, F9).
func TestForkCore_InheritsBinding(t *testing.T) {
	srcLineage := uuid.New()
	agentID := uuid.New()
	store := newFakeForkStore()
	src := sourceWorkflowRow(srcLineage)
	src.BoundAgentID = &agentID
	store.addCurrent(src)

	em, _ := audit.NewEmitter(context.Background(), audit.Config{})
	_, err := Fork(
		context.Background(), testWFTenant, testWFOwner,
		&brokerv1.ForkWorkflowRequest{
			TenantId:        testWFTenant,
			SourceLineageId: srcLineage.String(),
			NewName:         "forked-bound",
		},
		store, nil, em, zap.NewNop(),
	)
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if len(store.created) != 1 {
		t.Fatalf("want 1 created row, got %d", len(store.created))
	}
	got := store.created[0].BoundAgentID
	if got == nil || *got != agentID {
		t.Errorf("bound_agent_id: want inherited %s, got %v", agentID, got)
	}
}

// ── Pin (formerly pinCore) unit tests ────────────────────────────────────────

// TestPinCore_ApprovedVersion_SetsCalled: targeting an approved version calls
// SetVersionPin with the right args.
func TestPinCore_ApprovedVersion_SetsCalled(t *testing.T) {
	lineageID := uuid.New()
	store := newFakePinStore()
	store.addVersion(db.WorkflowRow{
		ID:            uuid.New(),
		LineageID:     lineageID,
		Version:       2,
		TenantID:      uuid.MustParse(testWFTenant),
		OwnerUserID:   testWFOwner,
		ApprovalState: "approved",
	})

	em, _ := audit.NewEmitter(context.Background(), audit.Config{})
	_, err := Pin(
		context.Background(), testWFTenant, testWFOwner,
		&brokerv1.SetWorkflowVersionPinRequest{
			TenantId:  testWFTenant,
			LineageId: lineageID.String(),
			Version:   2,
		},
		store, em, zap.NewNop(),
	)
	if err != nil {
		t.Fatalf("pin approved: unexpected error: %v", err)
	}
	key := testWFOwner + "/" + lineageID.String()
	if v, ok := store.pins[key]; !ok || v != 2 {
		t.Errorf("SetVersionPin not called correctly: pins=%v", store.pins)
	}
}

// TestPinCore_ProposedVersion_FailedPrecondition: targeting a proposed version
// → FailedPrecondition.
func TestPinCore_ProposedVersion_FailedPrecondition(t *testing.T) {
	lineageID := uuid.New()
	store := newFakePinStore()
	store.addVersion(db.WorkflowRow{
		ID:            uuid.New(),
		LineageID:     lineageID,
		Version:       3,
		TenantID:      uuid.MustParse(testWFTenant),
		OwnerUserID:   testWFOwner,
		ApprovalState: "proposed", // NOT approved
	})

	em, _ := audit.NewEmitter(context.Background(), audit.Config{})
	_, err := Pin(
		context.Background(), testWFTenant, testWFOwner,
		&brokerv1.SetWorkflowVersionPinRequest{
			TenantId:  testWFTenant,
			LineageId: lineageID.String(),
			Version:   3,
		},
		store, em, zap.NewNop(),
	)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("proposed version: want FailedPrecondition, got %v", err)
	}
	// SetVersionPin must NOT have been called.
	if len(store.pins) != 0 {
		t.Errorf("SetVersionPin must not be called for proposed version, got pins=%v", store.pins)
	}
}

// TestPinCore_VersionNotFound_NotFound.
func TestPinCore_VersionNotFound_NotFound(t *testing.T) {
	store := newFakePinStore() // empty
	em, _ := audit.NewEmitter(context.Background(), audit.Config{})

	_, err := Pin(
		context.Background(), testWFTenant, testWFOwner,
		&brokerv1.SetWorkflowVersionPinRequest{
			TenantId:  testWFTenant,
			LineageId: uuid.New().String(),
			Version:   1,
		},
		store, em, zap.NewNop(),
	)
	if status.Code(err) != codes.NotFound {
		t.Fatalf("version not found: want NotFound, got %v", err)
	}
}

// ── ClearPin (formerly clearPinCore) unit tests ──────────────────────────────

// TestClearPinCore_CallsClearVersionPin.
func TestClearPinCore_CallsClearVersionPin(t *testing.T) {
	lineageID := uuid.New()
	store := newFakePinStore()
	em, _ := audit.NewEmitter(context.Background(), audit.Config{})

	_, err := ClearPin(
		context.Background(), testWFTenant, testWFOwner,
		&brokerv1.ClearWorkflowVersionPinRequest{
			TenantId:  testWFTenant,
			LineageId: lineageID.String(),
		},
		store, em, zap.NewNop(),
	)
	if err != nil {
		t.Fatalf("clearPin: unexpected error: %v", err)
	}
	expected := testWFOwner + "/" + lineageID.String()
	if len(store.clears) != 1 || store.clears[0] != expected {
		t.Errorf("ClearVersionPin not called correctly: clears=%v", store.clears)
	}
}
