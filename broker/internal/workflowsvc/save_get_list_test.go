package workflowsvc

// Core-level direct-call tests relocated from broker/internal/broker
// (workflowsvc-extraction CP2 — see ).
//
// TestSave_OwnerBoundFromIdentity was the "owner_bound_from_identity" subtest
// of broker's TestNorthWorkflow_SaveWorkflow_OwnerFromIdentity — the only
// core-level part of that test (it called saveWorkflowCore directly); the
// "spoof_rejected" subtest stayed in broker (it exercises callerIdentity via
// svc.SaveWorkflow, a wrapper-level concern).
//
// TestListCore_* were TestListWorkflowsCore_* in
// broker/internal/broker/service_workflows_cp8_test.go — direct calls to the
// former listWorkflowsCore, now List. Fixtures (fakeWorkflowListStoreCP8,
// fakeWorkflowAccessPolicy, testGroupAlpha, sharedWorkflowJSON, publishedRow)
// are duplicated here per spec: broker's wrapper-level North/South CP8 tests
// still use the same fixtures under the same names.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/db"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// ── fakeWorkflowStore ──────────────────────────────────────────────────────────

// fakeWorkflowStore satisfies Store for Save unit tests (GetCurrent +
// CreateVersion overridden; everything else via the embedded stub).
type fakeWorkflowStore struct {
	stubWorkflowStore
	created []db.WorkflowRow
	// currentBoundAgent is returned as GetCurrent's BoundAgentID so edit-path
	// tests can prove the binding is inherited (nil = unbound current version).
	currentBoundAgent *uuid.UUID
}

func (f *fakeWorkflowStore) GetCurrent(_ context.Context, _ string, _ uuid.UUID) (*db.WorkflowRow, error) {
	// Fake: return a private approved row so the edit path can inherit from it.
	return &db.WorkflowRow{
		Status:           "private",
		VisibilityKind:   "private",
		VisibilityGroups: json.RawMessage(`[]`),
		BoundAgentID:     f.currentBoundAgent,
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

// TestSave_OwnerBoundFromIdentity proves that Save stores the owner it is
// given (the caller-resolved identity), not any wire-supplied field — the
// core has no notion of a wire "OwnerUserId" at all, since the wrapper already
// resolved and passed the verified subject as ownerUserID.
func TestSave_OwnerBoundFromIdentity(t *testing.T) {
	// WHY: callerIdentity is the trust anchor on the north path (verified in
	// broker's TestNorthWorkflow_SaveWorkflow_OwnerFromIdentity/spoof_rejected).
	// This core-level half proves the second half of that invariant: once the
	// wrapper resolves the verified subject, Save persists exactly that value
	// as OwnerUserID — never a value from the request body.
	const (
		realOwner   = "alice@example.com"
		spoofedUser = "eve@example.com"
	)

	store := &fakeWorkflowStore{}
	em, err := audit.NewEmitter(context.Background(), audit.Config{})
	if err != nil {
		t.Fatalf("audit.NewEmitter: %v", err)
	}

	req := &brokerv1.SaveWorkflowRequest{
		TenantId:       testWFTenant,
		OwnerUserId:    spoofedUser, // wire field — must be ignored by core
		DefinitionJson: minimalValidWorkflowJSON(),
		Name:           "test",
	}
	// callerIdentity would resolve tenant=testWFTenant, user=realOwner (from Subject).
	resp, err := Save(context.Background(), testWFTenant, realOwner, req, store, nil, em, zap.NewNop())
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if resp.WorkflowId == "" {
		t.Fatal("expected non-empty workflow_id")
	}
	if len(store.created) != 1 {
		t.Fatalf("want 1 stored row, got %d", len(store.created))
	}
	if got := store.created[0].OwnerUserID; got != realOwner {
		t.Errorf("owner stored = %q, want %q (the verified subject, not OwnerUserId wire field)", got, realOwner)
	}
}

// ── fakeWorkflowListStoreCP8 ──────────────────────────────────────────────────

type fakeWorkflowListStoreCP8 struct {
	stubWorkflowStore
	// own maps ownerUserID → rows returned by ListByOwner.
	own map[string][]*db.WorkflowRow
	// shared is the set of rows returned when any of the requested groups
	// intersects sharedGroups. A row is included when its lineage ID is in
	// sharedLineages and any of the queried groups is in sharedGroups[lineageID].
	shared       []*db.WorkflowRow
	sharedGroups map[string][]string // lineageID.String() → groups that can see it
}

func newFakeListStoreCP8() *fakeWorkflowListStoreCP8 {
	return &fakeWorkflowListStoreCP8{
		own:          make(map[string][]*db.WorkflowRow),
		sharedGroups: make(map[string][]string),
	}
}

func (f *fakeWorkflowListStoreCP8) addOwn(row db.WorkflowRow) {
	f.own[row.OwnerUserID] = append(f.own[row.OwnerUserID], &row)
}

func (f *fakeWorkflowListStoreCP8) addShared(row db.WorkflowRow, groups []string) {
	f.shared = append(f.shared, &row)
	f.sharedGroups[row.LineageID.String()] = groups
}

func (f *fakeWorkflowListStoreCP8) ListByOwner(_ context.Context, _, ownerUserID, _ string, _ int) ([]*db.WorkflowRow, error) {
	return f.own[ownerUserID], nil
}

func (f *fakeWorkflowListStoreCP8) ListVisibleShared(_ context.Context, _ string, groups []string, _ string, _ int) ([]*db.WorkflowRow, error) {
	groupSet := make(map[string]struct{}, len(groups))
	for _, g := range groups {
		groupSet[g] = struct{}{}
	}
	var out []*db.WorkflowRow
	for _, r := range f.shared {
		for _, g := range f.sharedGroups[r.LineageID.String()] {
			if _, ok := groupSet[g]; ok {
				out = append(out, r)
				break
			}
		}
	}
	return out, nil
}

// ── fakeWorkflowAccessPolicy ─────────────────────────────────────────────────

type fakeWorkflowAccessPolicy struct {
	enabled    bool
	groups     []string        // full refs e.g. "group:alpha" returned by ListObjects
	skillGrant map[string]bool // "skill:X" → granted
}

func (p *fakeWorkflowAccessPolicy) FGAEnabled() bool { return p.enabled }

func (p *fakeWorkflowAccessPolicy) ListObjects(_ context.Context, _, _, _ string) ([]string, error) {
	return p.groups, nil
}

func (p *fakeWorkflowAccessPolicy) CheckFGA(_ context.Context, _, _, object string) (bool, error) {
	return p.skillGrant[object], nil
}

var _ AccessPolicy = (*fakeWorkflowAccessPolicy)(nil)

// countingWorkflowAccessPolicy wraps fakeWorkflowAccessPolicy semantics but
// also counts CheckFGA invocations and can be told to error on specific
// skills — used to pin the F18 dedup fix (distinct skills resolved once
// across all rows, not once per row).
type countingWorkflowAccessPolicy struct {
	enabled    bool
	groups     []string
	skillGrant map[string]bool // "skill:X" → granted
	skillErr   map[string]bool // "skill:X" → CheckFGA returns an error for this skill
	checkCalls int
}

func (p *countingWorkflowAccessPolicy) FGAEnabled() bool { return p.enabled }

func (p *countingWorkflowAccessPolicy) ListObjects(_ context.Context, _, _, _ string) ([]string, error) {
	return p.groups, nil
}

func (p *countingWorkflowAccessPolicy) CheckFGA(_ context.Context, _, _, object string) (bool, error) {
	p.checkCalls++
	if p.skillErr[object] {
		return false, errCheckFGAFake
	}
	return p.skillGrant[object], nil
}

var _ AccessPolicy = (*countingWorkflowAccessPolicy)(nil)

var errCheckFGAFake = errors.New("fake CheckFGA failure")

// ── helpers ───────────────────────────────────────────────────────────────────

const testGroupAlpha = "alpha"

// sharedWorkflowJSON builds a valid published Workflow JSON whose requires.skills
// list exactly the given skills (set by ComputeRequires at publish time).
func sharedWorkflowJSON(t *testing.T, skills ...string) string {
	t.Helper()
	m := map[string]any{
		"apiVersion": "aikonos.com/v1",
		"kind":       "Workflow",
		"metadata": map[string]any{
			"name": "shared-wf",
			"visibility": map[string]any{
				"kind": "shared",
			},
		},
		"requires": map[string]any{
			"skills": skills,
		},
		"steps": []any{
			map[string]any{"skill": "doc.read"},
		},
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func publishedRow(lineageID uuid.UUID, owner, name, defJSON string) db.WorkflowRow {
	return db.WorkflowRow{
		ID:               uuid.New(),
		LineageID:        lineageID,
		Version:          1,
		TenantID:         uuid.MustParse(testWFTenant),
		OwnerUserID:      owner,
		Name:             name,
		Status:           "published",
		VisibilityKind:   "shared",
		VisibilityGroups: json.RawMessage(`["` + testGroupAlpha + `"]`),
		Definition:       json.RawMessage(defJSON),
		ApprovalState:    "approved",
	}
}

// ── List (formerly listWorkflowsCore) unit tests ─────────────────────────────

// TestListWorkflowsCore_ViewerInGroup_SeesSharedWorkflow: a viewer in group
// alpha sees the published workflow shared to that group.
func TestListWorkflowsCore_ViewerInGroup_SeesSharedWorkflow(t *testing.T) {
	lineageShared := uuid.New()
	store := newFakeListStoreCP8()
	store.addShared(publishedRow(lineageShared, "owner@example.com", "shared-wf",
		sharedWorkflowJSON(t, "doc.read")), []string{testGroupAlpha})

	ap := &fakeWorkflowAccessPolicy{
		enabled: true,
		groups:  []string{"group:" + testGroupAlpha}, // viewer is in alpha
		skillGrant: map[string]bool{
			"skill:doc.read": true,
		},
	}

	resp, err := List(context.Background(), testWFTenant, testWFOwner, store, ap, testWFTenantObject, zap.NewNop(), 0, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("want 1 item (shared wf), got %d", len(resp.Items))
	}
	item := resp.Items[0]
	if item.LineageId != lineageShared.String() {
		t.Errorf("lineage_id: want %s, got %s", lineageShared, item.LineageId)
	}
	if item.IsOwner {
		t.Error("is_owner must be false for a shared-not-owned workflow")
	}
}

// TestListWorkflowsCore_ViewerNotInGroup_DoesNotSeeSharedWorkflow: a viewer
// who is NOT in group alpha must not see the workflow shared to alpha.
func TestListWorkflowsCore_ViewerNotInGroup_DoesNotSeeSharedWorkflow(t *testing.T) {
	lineageShared := uuid.New()
	store := newFakeListStoreCP8()
	store.addShared(publishedRow(lineageShared, "owner@example.com", "shared-wf",
		sharedWorkflowJSON(t, "doc.read")), []string{testGroupAlpha})

	ap := &fakeWorkflowAccessPolicy{
		enabled: true,
		groups:  []string{}, // viewer belongs to NO groups
		skillGrant: map[string]bool{
			"skill:doc.read": true,
		},
	}

	resp, err := List(context.Background(), testWFTenant, testWFOwner, store, ap, testWFTenantObject, zap.NewNop(), 0, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.Items) != 0 {
		t.Errorf("viewer not in group: want 0 items, got %d", len(resp.Items))
	}
}

// TestListWorkflowsCore_LackingSkill_GreyedOut: a viewer in group alpha who
// lacks "doc.read" sees the shared workflow as greyed_out with the missing skill named.
func TestListWorkflowsCore_LackingSkill_GreyedOut(t *testing.T) {
	lineageShared := uuid.New()
	store := newFakeListStoreCP8()
	store.addShared(publishedRow(lineageShared, "owner@example.com", "shared-wf",
		sharedWorkflowJSON(t, "doc.read")), []string{testGroupAlpha})

	ap := &fakeWorkflowAccessPolicy{
		enabled:    true,
		groups:     []string{"group:" + testGroupAlpha},
		skillGrant: map[string]bool{
			// "skill:doc.read" intentionally absent → lacking
		},
	}

	resp, err := List(context.Background(), testWFTenant, testWFOwner, store, ap, testWFTenantObject, zap.NewNop(), 0, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("want 1 item, got %d", len(resp.Items))
	}
	item := resp.Items[0]
	if item.AccessState != "greyed_out" {
		t.Errorf("access_state: want greyed_out, got %q", item.AccessState)
	}
	if len(item.MissingRequirements) != 1 || item.MissingRequirements[0] != "doc.read" {
		t.Errorf("missing_requirements: want [doc.read], got %v", item.MissingRequirements)
	}
}

// TestListWorkflowsCore_HoldingSkill_Runnable: a viewer in group alpha who
// holds "doc.read" sees the shared workflow as runnable.
func TestListWorkflowsCore_HoldingSkill_Runnable(t *testing.T) {
	lineageShared := uuid.New()
	store := newFakeListStoreCP8()
	store.addShared(publishedRow(lineageShared, "owner@example.com", "shared-wf",
		sharedWorkflowJSON(t, "doc.read")), []string{testGroupAlpha})

	ap := &fakeWorkflowAccessPolicy{
		enabled: true,
		groups:  []string{"group:" + testGroupAlpha},
		skillGrant: map[string]bool{
			"skill:doc.read": true,
		},
	}

	resp, err := List(context.Background(), testWFTenant, testWFOwner, store, ap, testWFTenantObject, zap.NewNop(), 0, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("want 1 item, got %d", len(resp.Items))
	}
	item := resp.Items[0]
	if item.AccessState != "runnable" {
		t.Errorf("access_state: want runnable, got %q", item.AccessState)
	}
	if len(item.MissingRequirements) != 0 {
		t.Errorf("missing_requirements: want empty, got %v", item.MissingRequirements)
	}
}

// TestListWorkflowsCore_OwnWorkflow_Deduped_IsOwner: the owner's own workflow
// appears exactly once with is_owner=true even when it is also in sharedRows
// (dedup by lineage_id; own takes precedence).
func TestListWorkflowsCore_OwnWorkflow_Deduped_IsOwner(t *testing.T) {
	lineageOwned := uuid.New()
	ownDef := minimalValidWorkflowJSON()

	store := newFakeListStoreCP8()
	// Add as own row.
	store.addOwn(db.WorkflowRow{
		ID: uuid.New(), LineageID: lineageOwned, Version: 2,
		TenantID: uuid.MustParse(testWFTenant), OwnerUserID: testWFOwner,
		Name: "my-wf", Status: "published", VisibilityKind: "shared",
		VisibilityGroups: json.RawMessage(`["` + testGroupAlpha + `"]`),
		Definition:       json.RawMessage(ownDef),
		ApprovalState:    "approved",
	})
	// Also surface it as shared (ListVisibleShared returns the same lineage).
	store.addShared(db.WorkflowRow{
		ID: uuid.New(), LineageID: lineageOwned, Version: 2,
		TenantID: uuid.MustParse(testWFTenant), OwnerUserID: testWFOwner,
		Name: "my-wf", Status: "published", VisibilityKind: "shared",
		VisibilityGroups: json.RawMessage(`["` + testGroupAlpha + `"]`),
		Definition:       json.RawMessage(ownDef),
		ApprovalState:    "approved",
	}, []string{testGroupAlpha})

	ap := &fakeWorkflowAccessPolicy{
		enabled:    true,
		groups:     []string{"group:" + testGroupAlpha},
		skillGrant: map[string]bool{},
	}

	resp, err := List(context.Background(), testWFTenant, testWFOwner, store, ap, testWFTenantObject, zap.NewNop(), 0, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("dedup: want 1 item, got %d", len(resp.Items))
	}
	item := resp.Items[0]
	if !item.IsOwner {
		t.Error("is_owner must be true for the owner's own workflow")
	}
	if item.LineageId != lineageOwned.String() {
		t.Errorf("lineage_id mismatch: want %s, got %s", lineageOwned, item.LineageId)
	}
}

// TestListWorkflowsCore_FGADisabled_AllRunnable: when FGA is disabled (ap=nil),
// all workflows are runnable with no missing requirements.
func TestListWorkflowsCore_FGADisabled_AllRunnable(t *testing.T) {
	lineageA := uuid.New()
	store := newFakeListStoreCP8()
	store.addOwn(db.WorkflowRow{
		ID: uuid.New(), LineageID: lineageA, Version: 1,
		TenantID: uuid.MustParse(testWFTenant), OwnerUserID: testWFOwner,
		Name: "wf-a", Status: "private", VisibilityKind: "private",
		VisibilityGroups: json.RawMessage(`[]`),
		Definition:       json.RawMessage(minimalValidWorkflowJSON()),
	})

	resp, err := List(context.Background(), testWFTenant, testWFOwner, store, nil /* FGA off */, testWFTenantObject, zap.NewNop(), 0, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("want 1 item, got %d", len(resp.Items))
	}
	if resp.Items[0].AccessState != "runnable" {
		t.Errorf("FGA disabled: want runnable, got %q", resp.Items[0].AccessState)
	}
	// own rows always set is_owner=true — independent of whether FGA is wired.
	if !resp.Items[0].IsOwner {
		t.Error("own workflow must have is_owner=true even when FGA is disabled")
	}
}

// TestListWorkflowsCore_DedupsFGACallsAcrossRows (F18): three rows share two
// distinct skills across their requires.skills — CheckFGA must be called
// exactly once per distinct skill (2), not once per row-skill pair (would be
// more without dedup), and mixed grant/deny/error rows must still produce
// the same per-row results as calling CheckFGA per row-skill would.
func TestListWorkflowsCore_DedupsFGACallsAcrossRows(t *testing.T) {
	store := newFakeListStoreCP8()
	lineageGranted := uuid.New()
	lineageDenied := uuid.New()
	lineageErrored := uuid.New()

	// row1: requires skillA only (granted) → runnable.
	store.addOwn(db.WorkflowRow{
		ID: uuid.New(), LineageID: lineageGranted, Version: 1,
		TenantID: uuid.MustParse(testWFTenant), OwnerUserID: testWFOwner,
		Name: "wf-granted", Status: "private", VisibilityKind: "private",
		VisibilityGroups: json.RawMessage(`[]`),
		Definition:       json.RawMessage(sharedWorkflowJSON(t, "skillA")),
	})
	// row2: requires skillB only (denied) → greyed_out.
	store.addOwn(db.WorkflowRow{
		ID: uuid.New(), LineageID: lineageDenied, Version: 1,
		TenantID: uuid.MustParse(testWFTenant), OwnerUserID: testWFOwner,
		Name: "wf-denied", Status: "private", VisibilityKind: "private",
		VisibilityGroups: json.RawMessage(`[]`),
		Definition:       json.RawMessage(sharedWorkflowJSON(t, "skillB")),
	})
	// row3: requires both skillA (granted) and skillB (errors → fail closed,
	// same as a denial) → greyed_out on skillB only.
	store.addOwn(db.WorkflowRow{
		ID: uuid.New(), LineageID: lineageErrored, Version: 1,
		TenantID: uuid.MustParse(testWFTenant), OwnerUserID: testWFOwner,
		Name: "wf-mixed", Status: "private", VisibilityKind: "private",
		VisibilityGroups: json.RawMessage(`[]`),
		Definition:       json.RawMessage(sharedWorkflowJSON(t, "skillA", "skillB")),
	})

	ap := &countingWorkflowAccessPolicy{
		enabled: true,
		skillGrant: map[string]bool{
			"skill:skillA": true,
		},
		skillErr: map[string]bool{
			"skill:skillB": true, // CheckFGA errors for skillB on every call site
		},
	}

	resp, err := List(context.Background(), testWFTenant, testWFOwner, store, ap, testWFTenantObject, zap.NewNop(), 0, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.Items) != 3 {
		t.Fatalf("want 3 items, got %d", len(resp.Items))
	}

	// Two distinct skills (skillA, skillB) across three rows: exactly 2 calls.
	if ap.checkCalls != 2 {
		t.Errorf("CheckFGA calls: want 2 (distinct skills), got %d", ap.checkCalls)
	}

	byLineage := make(map[string]*brokerv1.WorkflowSummary, len(resp.Items))
	for _, item := range resp.Items {
		byLineage[item.LineageId] = item
	}

	granted := byLineage[lineageGranted.String()]
	if granted.AccessState != "runnable" || len(granted.MissingRequirements) != 0 {
		t.Errorf("granted row: want runnable/no missing, got %q %v", granted.AccessState, granted.MissingRequirements)
	}

	denied := byLineage[lineageDenied.String()]
	if denied.AccessState != "greyed_out" || len(denied.MissingRequirements) != 1 || denied.MissingRequirements[0] != "skillB" {
		t.Errorf("denied row: want greyed_out/[skillB], got %q %v", denied.AccessState, denied.MissingRequirements)
	}

	mixed := byLineage[lineageErrored.String()]
	if mixed.AccessState != "greyed_out" || len(mixed.MissingRequirements) != 1 || mixed.MissingRequirements[0] != "skillB" {
		t.Errorf("mixed row: want greyed_out/[skillB] (skillA granted, skillB errors→fail-closed), got %q %v", mixed.AccessState, mixed.MissingRequirements)
	}
}

// ── List pagination (F19) ────────────────────────────────────────────────────
//
// Cursor is the raw lineage_id of the last returned item, sorted ascending —
// no encoding, mirroring the audit reader's convention (proto/broker.proto
// QueryAuditRequest / audit/reader.go filterAndPage): a plain domain-value
// string comparison, so a malformed cursor never errors.

func threeOwnRows(t *testing.T, store *fakeWorkflowListStoreCP8) []uuid.UUID {
	t.Helper()
	ids := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}
	for _, id := range ids {
		store.addOwn(db.WorkflowRow{
			ID: uuid.New(), LineageID: id, Version: 1,
			TenantID: uuid.MustParse(testWFTenant), OwnerUserID: testWFOwner,
			Name: "wf-" + id.String(), Status: "private", VisibilityKind: "private",
			VisibilityGroups: json.RawMessage(`[]`),
			Definition:       json.RawMessage(minimalValidWorkflowJSON()),
		})
	}
	return ids
}

// TestList_LimitZero_ReturnsEverything pins the legacy contract: limit=0
// (proto3 default) reproduces today's unbounded behavior exactly.
func TestList_LimitZero_ReturnsEverything(t *testing.T) {
	store := newFakeListStoreCP8()
	threeOwnRows(t, store)

	resp, err := List(context.Background(), testWFTenant, testWFOwner, store, nil, testWFTenantObject, zap.NewNop(), 0, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.Items) != 3 {
		t.Fatalf("limit=0: want all 3 items, got %d", len(resp.Items))
	}
	if resp.NextCursor != "" {
		t.Errorf("limit=0: want empty next_cursor, got %q", resp.NextCursor)
	}
}

// TestList_LimitGreaterThanZero_FirstPageHasNextCursor verifies a bounded
// first page plus a non-empty next_cursor when more rows remain.
func TestList_LimitGreaterThanZero_FirstPageHasNextCursor(t *testing.T) {
	store := newFakeListStoreCP8()
	threeOwnRows(t, store)

	resp, err := List(context.Background(), testWFTenant, testWFOwner, store, nil, testWFTenantObject, zap.NewNop(), 2, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.Items) != 2 {
		t.Fatalf("limit=2: want 2 items, got %d", len(resp.Items))
	}
	if resp.NextCursor == "" {
		t.Error("limit=2 with more remaining: want non-empty next_cursor")
	}
}

// TestList_CursorContinues_NoOverlapOrGap verifies that paging with the
// returned next_cursor picks up exactly where the first page left off, and
// that the two pages together cover every row with no duplicate.
func TestList_CursorContinues_NoOverlapOrGap(t *testing.T) {
	store := newFakeListStoreCP8()
	threeOwnRows(t, store)

	page1, err := List(context.Background(), testWFTenant, testWFOwner, store, nil, testWFTenantObject, zap.NewNop(), 2, "")
	if err != nil || len(page1.Items) != 2 || page1.NextCursor == "" {
		t.Fatalf("setup: items=%d cursor=%q err=%v", len(page1.Items), page1.NextCursor, err)
	}

	page2, err := List(context.Background(), testWFTenant, testWFOwner, store, nil, testWFTenantObject, zap.NewNop(), 2, page1.NextCursor)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(page2.Items) != 1 {
		t.Fatalf("continuation: want 1 remaining item, got %d", len(page2.Items))
	}
	if page2.NextCursor != "" {
		t.Errorf("continuation: want empty next_cursor on last page, got %q", page2.NextCursor)
	}
	seen := map[string]bool{}
	for _, item := range page1.Items {
		seen[item.LineageId] = true
	}
	if seen[page2.Items[0].LineageId] {
		t.Error("continuation overlaps with page1")
	}
}

// ── F9: agent binding (Save/Get/List/MayOperateAgent) ────────────────────────

// fakeWorkflowGetStore serves a single row for Get: ResolveVersionForUser +
// GetVersion both return it, so Get's bound_agent_id passthrough is testable.
type fakeWorkflowGetStore struct {
	stubWorkflowStore
	row *db.WorkflowRow
}

func (f *fakeWorkflowGetStore) ResolveVersionForUser(_ context.Context, _, _ string, _ uuid.UUID) (int, error) {
	return f.row.Version, nil
}

func (f *fakeWorkflowGetStore) GetVersion(_ context.Context, _ string, _ uuid.UUID, _ int) (*db.WorkflowRow, error) {
	return f.row, nil
}

func newEmitter(t *testing.T) AuditEmitter {
	t.Helper()
	em, err := audit.NewEmitter(context.Background(), audit.Config{})
	if err != nil {
		t.Fatalf("audit.NewEmitter: %v", err)
	}
	return em
}

// TestSave_NewLineage_ValidAgentId_BindsAgent: a brand-new lineage takes its
// agent binding from req.AgentId.
func TestSave_NewLineage_ValidAgentId_BindsAgent(t *testing.T) {
	agentID := uuid.New()
	store := &fakeWorkflowStore{}

	req := &brokerv1.SaveWorkflowRequest{
		TenantId:       testWFTenant,
		DefinitionJson: minimalValidWorkflowJSON(),
		Name:           "bound-wf",
		AgentId:        agentID.String(), // brand-new lineage → binding taken from request
	}
	if _, err := Save(context.Background(), testWFTenant, testWFOwner, req, store, nil, newEmitter(t), zap.NewNop()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if len(store.created) != 1 {
		t.Fatalf("want 1 stored row, got %d", len(store.created))
	}
	got := store.created[0].BoundAgentID
	if got == nil || *got != agentID {
		t.Errorf("bound_agent_id: want %s, got %v", agentID, got)
	}
}

// TestSave_EditInheritsBinding_IgnoresAgentId: an edit (existing lineage)
// inherits the current version's binding and ignores req.AgentId entirely —
// the binding is lineage-immutable.
func TestSave_EditInheritsBinding_IgnoresAgentId(t *testing.T) {
	inherited := uuid.New()
	spoofed := uuid.New() // a different, valid agent id the edit must ignore
	store := &fakeWorkflowStore{currentBoundAgent: &inherited}

	req := &brokerv1.SaveWorkflowRequest{
		TenantId:       testWFTenant,
		LineageId:      uuid.New().String(), // edit path
		DefinitionJson: minimalValidWorkflowJSON(),
		Name:           "edited-wf",
		AgentId:        spoofed.String(), // must be ignored on the edit path
	}
	if _, err := Save(context.Background(), testWFTenant, testWFOwner, req, store, nil, newEmitter(t), zap.NewNop()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if len(store.created) != 1 {
		t.Fatalf("want 1 stored row, got %d", len(store.created))
	}
	got := store.created[0].BoundAgentID
	if got == nil || *got != inherited {
		t.Errorf("bound_agent_id: want inherited %s (not request %s), got %v", inherited, spoofed, got)
	}
}

// TestSave_NewLineage_NonUuidAgentId_Unbound: a malformed agent_id on a new
// lineage is treated as unbound (NULL), never an error.
func TestSave_NewLineage_NonUuidAgentId_Unbound(t *testing.T) {
	store := &fakeWorkflowStore{}
	req := &brokerv1.SaveWorkflowRequest{
		TenantId:       testWFTenant,
		DefinitionJson: minimalValidWorkflowJSON(),
		Name:           "unbound-wf",
		AgentId:        "not-a-uuid",
	}
	if _, err := Save(context.Background(), testWFTenant, testWFOwner, req, store, nil, newEmitter(t), zap.NewNop()); err != nil {
		t.Fatalf("Save must not error on a malformed agent_id: %v", err)
	}
	if len(store.created) != 1 {
		t.Fatalf("want 1 stored row, got %d", len(store.created))
	}
	if got := store.created[0].BoundAgentID; got != nil {
		t.Errorf("bound_agent_id: want nil (unbound), got %v", got)
	}
}

// TestGet_ReturnsBoundAgentId: Get echoes the stored bound_agent_id.
func TestGet_ReturnsBoundAgentId(t *testing.T) {
	agentID := uuid.New()
	store := &fakeWorkflowGetStore{row: &db.WorkflowRow{
		Version:      3,
		Definition:   json.RawMessage(minimalValidWorkflowJSON()),
		BoundAgentID: &agentID,
	}}

	resp, err := Get(context.Background(), testWFTenant, testWFOwner,
		&brokerv1.GetWorkflowRequest{LineageId: uuid.New().String()}, store, zap.NewNop())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if resp.BoundAgentId != agentID.String() {
		t.Errorf("bound_agent_id: want %s, got %q", agentID, resp.BoundAgentId)
	}
}

// ownBoundRow builds a private own row bound to agentID (F9 List tests).
func ownBoundRow(lineageID, agentID uuid.UUID) db.WorkflowRow {
	return db.WorkflowRow{
		ID: uuid.New(), LineageID: lineageID, Version: 1,
		TenantID: uuid.MustParse(testWFTenant), OwnerUserID: testWFOwner,
		Name: "bound-wf", Status: "private", VisibilityKind: "private",
		VisibilityGroups: json.RawMessage(`[]`),
		Definition:       json.RawMessage(minimalValidWorkflowJSON()),
		BoundAgentID:     &agentID,
	}
}

// TestList_BoundAgent_DeniedWhenNoOperateGrant: FGA on, viewer holds neither
// can_use on the agent nor tenant admin → bound_agent_ok=false, id echoed.
func TestList_BoundAgent_DeniedWhenNoOperateGrant(t *testing.T) {
	lineage := uuid.New()
	agentID := uuid.New()
	store := newFakeListStoreCP8()
	store.addOwn(ownBoundRow(lineage, agentID))

	ap := &fakeWorkflowAccessPolicy{enabled: true, skillGrant: map[string]bool{}} // denies everything

	resp, err := List(context.Background(), testWFTenant, testWFOwner, store, ap, testWFTenantObject, zap.NewNop(), 0, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("want 1 item, got %d", len(resp.Items))
	}
	item := resp.Items[0]
	if item.BoundAgentId != agentID.String() {
		t.Errorf("bound_agent_id: want %s, got %q", agentID, item.BoundAgentId)
	}
	if item.BoundAgentOk {
		t.Error("bound_agent_ok: want false when viewer cannot operate the agent")
	}
}

// TestList_BoundAgent_OkWhenCanUseGranted: can_use on the agent → ok=true.
func TestList_BoundAgent_OkWhenCanUseGranted(t *testing.T) {
	lineage := uuid.New()
	agentID := uuid.New()
	store := newFakeListStoreCP8()
	store.addOwn(ownBoundRow(lineage, agentID))

	ap := &fakeWorkflowAccessPolicy{enabled: true, skillGrant: map[string]bool{
		"agent:" + agentID.String(): true,
	}}

	resp, err := List(context.Background(), testWFTenant, testWFOwner, store, ap, testWFTenantObject, zap.NewNop(), 0, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if !resp.Items[0].BoundAgentOk {
		t.Error("bound_agent_ok: want true when can_use is granted")
	}
}

// TestList_BoundAgent_OkWhenFGADisabled: FGA off → bound rows are ok (allow-all).
func TestList_BoundAgent_OkWhenFGADisabled(t *testing.T) {
	lineage := uuid.New()
	agentID := uuid.New()
	store := newFakeListStoreCP8()
	store.addOwn(ownBoundRow(lineage, agentID))

	resp, err := List(context.Background(), testWFTenant, testWFOwner, store, nil, testWFTenantObject, zap.NewNop(), 0, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if !resp.Items[0].BoundAgentOk {
		t.Error("bound_agent_ok: want true when FGA is disabled")
	}
	if resp.Items[0].BoundAgentId != agentID.String() {
		t.Errorf("bound_agent_id: want %s, got %q", agentID, resp.Items[0].BoundAgentId)
	}
}

// TestMayOperateAgent covers the full gate matrix (F9).
func TestMayOperateAgent(t *testing.T) {
	agentID := uuid.New().String()
	agentObj := "agent:" + agentID

	cases := []struct {
		name string
		ap   AccessPolicy
		want bool
	}{
		{"nil ap → allow-all", nil, true},
		{"fga disabled → allow-all", &countingWorkflowAccessPolicy{enabled: false}, true},
		{"can_use granted", &countingWorkflowAccessPolicy{enabled: true, skillGrant: map[string]bool{agentObj: true}}, true},
		{"admin fallback", &countingWorkflowAccessPolicy{enabled: true, skillGrant: map[string]bool{testWFTenantObject: true}}, true},
		{"both denied", &countingWorkflowAccessPolicy{enabled: true, skillGrant: map[string]bool{}}, false},
		{"can_use error → fail closed", &countingWorkflowAccessPolicy{enabled: true, skillErr: map[string]bool{agentObj: true}}, false},
		{"admin error → fail closed", &countingWorkflowAccessPolicy{enabled: true, skillErr: map[string]bool{testWFTenantObject: true}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := MayOperateAgent(context.Background(), tc.ap, testWFOwner, agentID, testWFTenantObject)
			if got != tc.want {
				t.Errorf("MayOperateAgent = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestMayAccessWorkflow covers the workflow-dimension gate matrix (F9 SEC fix).
// caller == me throughout; ownership/visibility/FGA vary per case.
func TestMayAccessWorkflow(t *testing.T) {
	me := testWFOwner
	sharedGroups := json.RawMessage(`["eng"]`)

	fgaOn := func(grants map[string]bool) AccessPolicy {
		return &fakeWorkflowAccessPolicy{enabled: true, skillGrant: grants}
	}

	cases := []struct {
		name string
		ap   AccessPolicy
		row  *db.WorkflowRow
		want bool
	}{
		{"nil row → deny", fgaOn(nil), nil, false},
		{"owner (private) → allow", fgaOn(nil), &db.WorkflowRow{OwnerUserID: me, VisibilityKind: "private"}, true},
		{"owner (shared) → allow", fgaOn(nil), &db.WorkflowRow{OwnerUserID: me, VisibilityKind: "shared", VisibilityGroups: sharedGroups}, true},
		{"private non-owner (fga on) → deny", fgaOn(nil), &db.WorkflowRow{OwnerUserID: "bob", VisibilityKind: "private"}, false},
		{"private non-owner (fga off) → deny", nil, &db.WorkflowRow{OwnerUserID: "bob", VisibilityKind: "private"}, false},
		{"shared member (fga on) → allow", fgaOn(map[string]bool{"group:eng": true}), &db.WorkflowRow{OwnerUserID: "bob", VisibilityKind: "shared", VisibilityGroups: sharedGroups}, true},
		{"shared non-member (fga on) → deny", fgaOn(nil), &db.WorkflowRow{OwnerUserID: "bob", VisibilityKind: "shared", VisibilityGroups: sharedGroups}, false},
		// FGA-off shared: List hides shared rows entirely when FGA is off, so a
		// non-owner never sees a shared workflow via List → mirror that with deny.
		{"shared non-owner (fga off) → deny", nil, &db.WorkflowRow{OwnerUserID: "bob", VisibilityKind: "shared", VisibilityGroups: sharedGroups}, false},
		// A CheckFGA transport error on the only group fails closed (not-a-member).
		{"shared member CheckFGA error → deny", &countingWorkflowAccessPolicy{enabled: true, skillErr: map[string]bool{"group:eng": true}}, &db.WorkflowRow{OwnerUserID: "bob", VisibilityKind: "shared", VisibilityGroups: sharedGroups}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := MayAccessWorkflow(context.Background(), tc.ap, me, tc.row)
			if got != tc.want {
				t.Errorf("MayAccessWorkflow = %v, want %v", got, tc.want)
			}
		})
	}
}

// ── mcp: skill + agent-bound grey-out skips ──────────────────────────────────
//
// The per-user skill grey-out in List must skip (a) agent-bound rows entirely —
// they run under the agent's authority (checkAgentSkills), not the viewer's
// can_invoke grants — and (b) mcp: skills on any row — authorized by the agent's
// connector can_access grant at InvokeTool, whose "skill:mcp:<conn>:<tool>" FGA
// object is invalid. Mirrors service_plan.go's plan-time gate.

// TestList_BoundAgent_McpSkill_Runnable: an agent-bound workflow requiring an
// mcp: skill the viewer has no can_invoke for is still runnable (the bound-row
// skip) — the viewer's personal skill grants are irrelevant for a bound row.
func TestList_BoundAgent_McpSkill_Runnable(t *testing.T) {
	lineage := uuid.New()
	agentID := uuid.New()
	store := newFakeListStoreCP8()
	store.addOwn(db.WorkflowRow{
		ID: uuid.New(), LineageID: lineage, Version: 1,
		TenantID: uuid.MustParse(testWFTenant), OwnerUserID: testWFOwner,
		Name: "bound-mcp-wf", Status: "private", VisibilityKind: "private",
		VisibilityGroups: json.RawMessage(`[]`),
		Definition:       json.RawMessage(sharedWorkflowJSON(t, "mcp:gdrive:list")),
		BoundAgentID:     &agentID,
	})

	// can_use on the agent granted (viewer may operate it); no skill grants, so
	// CheckFGA would fail closed for the mcp skill if it were ever consulted.
	ap := &fakeWorkflowAccessPolicy{enabled: true, skillGrant: map[string]bool{
		"agent:" + agentID.String(): true,
	}}

	resp, err := List(context.Background(), testWFTenant, testWFOwner, store, ap, testWFTenantObject, zap.NewNop(), 0, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("want 1 item, got %d", len(resp.Items))
	}
	item := resp.Items[0]
	if item.AccessState != "runnable" {
		t.Errorf("access_state: want runnable for agent-bound mcp workflow, got %q", item.AccessState)
	}
	if len(item.MissingRequirements) != 0 {
		t.Errorf("missing_requirements: want empty, got %v", item.MissingRequirements)
	}
	if !item.BoundAgentOk {
		t.Error("bound_agent_ok: want true when can_use is granted")
	}
}

// TestList_Unbound_McpSkill_NotGreyed: an UNBOUND workflow requiring an mcp:
// skill is runnable — the mcp skill is skipped in both passes, so CheckFGA is
// never issued for it (asserted via checkCalls==0; the fake would error if it
// were, which would fail closed and grey the row).
func TestList_Unbound_McpSkill_NotGreyed(t *testing.T) {
	lineage := uuid.New()
	store := newFakeListStoreCP8()
	store.addOwn(db.WorkflowRow{
		ID: uuid.New(), LineageID: lineage, Version: 1,
		TenantID: uuid.MustParse(testWFTenant), OwnerUserID: testWFOwner,
		Name: "unbound-mcp-wf", Status: "private", VisibilityKind: "private",
		VisibilityGroups: json.RawMessage(`[]`),
		Definition:       json.RawMessage(sharedWorkflowJSON(t, "mcp:gdrive:list")),
	})

	ap := &countingWorkflowAccessPolicy{
		enabled:  true,
		skillErr: map[string]bool{"skill:mcp:gdrive:list": true},
	}

	resp, err := List(context.Background(), testWFTenant, testWFOwner, store, ap, testWFTenantObject, zap.NewNop(), 0, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("want 1 item, got %d", len(resp.Items))
	}
	item := resp.Items[0]
	if item.AccessState != "runnable" || len(item.MissingRequirements) != 0 {
		t.Errorf("unbound mcp: want runnable/no-missing, got %q %v", item.AccessState, item.MissingRequirements)
	}
	if ap.checkCalls != 0 {
		t.Errorf("mcp skill must never reach CheckFGA: want 0 calls, got %d", ap.checkCalls)
	}
}

// TestList_Unbound_MixedMcpAndMissingSkill_GreysOnNonMcpOnly: regression — an
// UNBOUND workflow requiring both a lacked non-mcp skill and an mcp skill is
// greyed_out with only the non-mcp skill in missing_requirements (mcp excluded).
func TestList_Unbound_MixedMcpAndMissingSkill_GreysOnNonMcpOnly(t *testing.T) {
	lineage := uuid.New()
	store := newFakeListStoreCP8()
	store.addOwn(db.WorkflowRow{
		ID: uuid.New(), LineageID: lineage, Version: 1,
		TenantID: uuid.MustParse(testWFTenant), OwnerUserID: testWFOwner,
		Name: "unbound-mixed-wf", Status: "private", VisibilityKind: "private",
		VisibilityGroups: json.RawMessage(`[]`),
		Definition:       json.RawMessage(sharedWorkflowJSON(t, "doc.write", "mcp:gdrive:list")),
	})

	ap := &fakeWorkflowAccessPolicy{enabled: true, skillGrant: map[string]bool{}} // doc.write lacked

	resp, err := List(context.Background(), testWFTenant, testWFOwner, store, ap, testWFTenantObject, zap.NewNop(), 0, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("want 1 item, got %d", len(resp.Items))
	}
	item := resp.Items[0]
	if item.AccessState != "greyed_out" {
		t.Errorf("access_state: want greyed_out (non-mcp skill lacked), got %q", item.AccessState)
	}
	if len(item.MissingRequirements) != 1 || item.MissingRequirements[0] != "doc.write" {
		t.Errorf("missing_requirements: want [doc.write] (mcp excluded), got %v", item.MissingRequirements)
	}
}
