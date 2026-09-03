package broker

// Tests for CP8: ListWorkflows enrichment — group-visible shared workflows,
// per-workflow access state (runnable / greyed_out), dedup by lineage_id,
// is_owner flag.
//
// Pattern mirrors service_workflows_cp7_test.go. Fakes satisfy
// workflowListStore (ListByOwner + ListVisibleShared) and workflowAccessPolicy
// (FGAEnabled + ListObjects + CheckFGA). The north-path tests wire a real
// policy.Engine backed by fakeFGA (httptest); south-path tests use the same
// approach via newSandboxSvcForCP8.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/policy"
	"github.com/adamdekan/aikonos/broker/internal/workflowsvc"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// ── fakeWorkflowListStoreCP8 ──────────────────────────────────────────────────
//
// Extends fakeWorkflowListStore with a per-group shared-row registry to support
// ListVisibleShared. Does NOT embed fakeWorkflowListStore so the interface is
// satisfied cleanly without method aliasing ambiguity.

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

// fakeWorkflowAccessPolicy (satisfying workflowsvc.AccessPolicy without an
// httptest server) moved to workflowsvc's test files with the core-level
// listWorkflowsCore/List tests that used it (workflowsvc-extraction CP2) — no
// remaining test in this file needs it; the North/South CP8 tests below wire
// a real policy.Engine backed by fakeFGA instead.

// ── helpers ───────────────────────────────────────────────────────────────────

const testGroupAlpha = "alpha"

// sharedWorkflowJSON builds a valid published Workflow JSON whose requires.skills
// list exactly the given skills (set by computeRequires at publish time).
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

// ── North-path CP8 handler tests (BrokerService) ─────────────────────────────

// newBrokerSvcForCP8 builds a BrokerService with the CP8 list store and FGA wired.
func newBrokerSvcForCP8(
	t *testing.T,
	listStore workflowsvc.Store,
	fgaChecks map[string]bool,
	listObjectsByUser map[string][]string,
) *BrokerService {
	t.Helper()
	fga := &fakeFGA{
		checks:                  fgaChecks,
		listObjectsResultByUser: listObjectsByUser,
	}
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
		Workflows: listStore,
	})
}

// TestNorthCP8_ViewerInGroup_SeesSharedWorkflow: a viewer in group alpha sees
// the published workflow via the north ListWorkflows handler.
func TestNorthCP8_ViewerInGroup_SeesSharedWorkflow(t *testing.T) {
	lineageShared := uuid.New()
	store := newFakeListStoreCP8()
	store.addShared(publishedRow(lineageShared, "owner@example.com", "shared-wf",
		sharedWorkflowJSON(t, "doc.read")), []string{testGroupAlpha})

	svc := newBrokerSvcForCP8(t, store,
		map[string]bool{
			// skill gate: viewer holds skill:workflows
			"user:" + testWFOwner + "|can_invoke|skill:workflows": true,
			// per-workflow access: viewer holds doc.read
			"user:" + testWFOwner + "|can_invoke|skill:doc.read": true,
		},
		map[string][]string{
			// ListObjects(user:alice, member, group) → group:alpha
			"user:" + testWFOwner: {"group:" + testGroupAlpha},
		},
	)
	ctx := ctxWithSubject(testWFTenant, testWFOwner, "")

	resp, err := svc.ListWorkflows(ctx, &brokerv1.ListWorkflowsRequest{
		TenantId: testWFTenant,
		UserId:   testWFOwner,
	})
	if err != nil {
		t.Fatalf("ListWorkflows: unexpected error: %v", err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("want 1 item, got %d", len(resp.Items))
	}
	if resp.Items[0].LineageId != lineageShared.String() {
		t.Errorf("lineage_id: want %s, got %s", lineageShared, resp.Items[0].LineageId)
	}
	if resp.Items[0].IsOwner {
		t.Error("is_owner must be false for shared-not-owned workflow")
	}
}

// TestNorthCP8_ViewerNotInGroup_DoesNotSeeSharedWorkflow.
func TestNorthCP8_ViewerNotInGroup_DoesNotSeeSharedWorkflow(t *testing.T) {
	lineageShared := uuid.New()
	store := newFakeListStoreCP8()
	store.addShared(publishedRow(lineageShared, "owner@example.com", "shared-wf",
		sharedWorkflowJSON(t, "doc.read")), []string{testGroupAlpha})

	svc := newBrokerSvcForCP8(t, store,
		map[string]bool{
			"user:" + testWFOwner + "|can_invoke|skill:workflows": true,
		},
		map[string][]string{
			"user:" + testWFOwner: {}, // no groups
		},
	)
	ctx := ctxWithSubject(testWFTenant, testWFOwner, "")

	resp, err := svc.ListWorkflows(ctx, &brokerv1.ListWorkflowsRequest{
		TenantId: testWFTenant,
		UserId:   testWFOwner,
	})
	if err != nil {
		t.Fatalf("ListWorkflows: unexpected error: %v", err)
	}
	if len(resp.Items) != 0 {
		t.Errorf("want 0 items (viewer not in group), got %d", len(resp.Items))
	}
}

// TestNorthCP8_LackingSkill_GreyedOut.
func TestNorthCP8_LackingSkill_GreyedOut(t *testing.T) {
	lineageShared := uuid.New()
	store := newFakeListStoreCP8()
	store.addShared(publishedRow(lineageShared, "owner@example.com", "shared-wf",
		sharedWorkflowJSON(t, "doc.read")), []string{testGroupAlpha})

	svc := newBrokerSvcForCP8(t, store,
		map[string]bool{
			"user:" + testWFOwner + "|can_invoke|skill:workflows": true,
			// skill:doc.read intentionally absent
		},
		map[string][]string{
			"user:" + testWFOwner: {"group:" + testGroupAlpha},
		},
	)
	ctx := ctxWithSubject(testWFTenant, testWFOwner, "")

	resp, err := svc.ListWorkflows(ctx, &brokerv1.ListWorkflowsRequest{
		TenantId: testWFTenant,
		UserId:   testWFOwner,
	})
	if err != nil {
		t.Fatalf("ListWorkflows: unexpected error: %v", err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("want 1 item, got %d", len(resp.Items))
	}
	item := resp.Items[0]
	if item.AccessState != "greyed_out" {
		t.Errorf("access_state: want greyed_out, got %q", item.AccessState)
	}
	if len(item.MissingRequirements) == 0 {
		t.Error("missing_requirements must not be empty for greyed_out")
	}
	if item.MissingRequirements[0] != "doc.read" {
		t.Errorf("missing_requirements[0]: want doc.read, got %s", item.MissingRequirements[0])
	}
}

// TestNorthCP8_OwnerWorkflow_Deduped_IsOwner: the owner's own workflow appears
// once with is_owner=true even though it is also returned by ListVisibleShared.
func TestNorthCP8_OwnerWorkflow_Deduped_IsOwner(t *testing.T) {
	lineageOwned := uuid.New()
	ownDef := minimalValidWorkflowJSON()
	store := newFakeListStoreCP8()

	// Own row.
	store.addOwn(db.WorkflowRow{
		ID: uuid.New(), LineageID: lineageOwned, Version: 1,
		TenantID: uuid.MustParse(testWFTenant), OwnerUserID: testWFOwner,
		Name: "my-wf", Status: "published", VisibilityKind: "shared",
		VisibilityGroups: json.RawMessage(`["` + testGroupAlpha + `"]`),
		Definition:       json.RawMessage(ownDef), ApprovalState: "approved",
	})
	// Also in shared (same lineage should be deduped, own wins).
	store.addShared(db.WorkflowRow{
		ID: uuid.New(), LineageID: lineageOwned, Version: 1,
		TenantID: uuid.MustParse(testWFTenant), OwnerUserID: testWFOwner,
		Name: "my-wf", Status: "published", VisibilityKind: "shared",
		VisibilityGroups: json.RawMessage(`["` + testGroupAlpha + `"]`),
		Definition:       json.RawMessage(ownDef), ApprovalState: "approved",
	}, []string{testGroupAlpha})

	svc := newBrokerSvcForCP8(t, store,
		map[string]bool{
			"user:" + testWFOwner + "|can_invoke|skill:workflows": true,
		},
		map[string][]string{
			"user:" + testWFOwner: {"group:" + testGroupAlpha},
		},
	)
	ctx := ctxWithSubject(testWFTenant, testWFOwner, "")

	resp, err := svc.ListWorkflows(ctx, &brokerv1.ListWorkflowsRequest{
		TenantId: testWFTenant,
		UserId:   testWFOwner,
	})
	if err != nil {
		t.Fatalf("ListWorkflows: unexpected error: %v", err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("dedup: want 1 item, got %d", len(resp.Items))
	}
	if !resp.Items[0].IsOwner {
		t.Error("is_owner must be true for owner's own workflow")
	}
}

// ── South-path CP8 handler tests (SandboxService) ────────────────────────────

// newSandboxSvcForCP8 builds a SandboxService with the CP8 list store and FGA.
// deps.Policy carries the group-lookup engine (same field as north path). The
// skill gate uses svc.skillPolicy (fakeSkillPolicy, test-only override) so
// the two can be set independently, matching the existing south test pattern.
func newSandboxSvcForCP8(
	t *testing.T,
	listStore workflowsvc.Store,
	skillGranted bool,
	ap *policy.Engine,
) *SandboxService {
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
		Workflows:       listStore,
		Policy:          ap,
	})
	svc.skillPolicy = &fakeSkillPolicy{enabled: true, granted: skillGranted}
	return svc
}

// buildPolicyEngineCP8 builds a policy.Engine backed by a fakeFGA configured
// for CP8 list-objects + check scenarios.
func buildPolicyEngineCP8(
	t *testing.T,
	checks map[string]bool,
	listObjectsByUser map[string][]string,
) *policy.Engine {
	t.Helper()
	fga := &fakeFGA{
		checks:                  checks,
		listObjectsResultByUser: listObjectsByUser,
	}
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
	return eng
}

// TestSouthCP8_ViewerInGroup_SeesSharedWorkflow: south path — viewer in group
// alpha sees the published workflow via the SPIFFE-gated ListWorkflows handler.
func TestSouthCP8_ViewerInGroup_SeesSharedWorkflow(t *testing.T) {
	lineageShared := uuid.New()
	store := newFakeListStoreCP8()
	store.addShared(publishedRow(lineageShared, "owner@example.com", "shared-wf",
		sharedWorkflowJSON(t, "doc.read")), []string{testGroupAlpha})

	eng := buildPolicyEngineCP8(t,
		map[string]bool{
			"user:" + testWFOwner + "|can_invoke|skill:doc.read": true,
		},
		map[string][]string{
			"user:" + testWFOwner: {"group:" + testGroupAlpha},
		},
	)
	svc := newSandboxSvcForCP8(t, store, true, eng)

	resp, err := svc.ListWorkflows(gatewayCtxForWorkflow(), &brokerv1.ListWorkflowsRequest{
		TenantId:   testWFTenant,
		UserId:     testWFOwner,
		OwnerGrant: mintWFGrant(t),
	})
	if err != nil {
		t.Fatalf("south ListWorkflows: unexpected error: %v", err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("want 1 item (shared wf), got %d", len(resp.Items))
	}
	if resp.Items[0].IsOwner {
		t.Error("is_owner must be false for shared-not-owned workflow")
	}
	if resp.Items[0].AccessState != "runnable" {
		t.Errorf("access_state: want runnable, got %q", resp.Items[0].AccessState)
	}
}

// TestSouthCP8_LackingSkill_GreyedOut: south path — viewer lacks a required
// skill → greyed_out + missing_requirements lists the skill.
func TestSouthCP8_LackingSkill_GreyedOut(t *testing.T) {
	lineageShared := uuid.New()
	store := newFakeListStoreCP8()
	store.addShared(publishedRow(lineageShared, "owner@example.com", "shared-wf",
		sharedWorkflowJSON(t, "doc.read")), []string{testGroupAlpha})

	eng := buildPolicyEngineCP8(t,
		map[string]bool{
			// skill:doc.read intentionally absent for this viewer
		},
		map[string][]string{
			"user:" + testWFOwner: {"group:" + testGroupAlpha},
		},
	)
	svc := newSandboxSvcForCP8(t, store, true, eng)

	resp, err := svc.ListWorkflows(gatewayCtxForWorkflow(), &brokerv1.ListWorkflowsRequest{
		TenantId:   testWFTenant,
		UserId:     testWFOwner,
		OwnerGrant: mintWFGrant(t),
	})
	if err != nil {
		t.Fatalf("south ListWorkflows: unexpected error: %v", err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("want 1 item, got %d", len(resp.Items))
	}
	item := resp.Items[0]
	if item.AccessState != "greyed_out" {
		t.Errorf("access_state: want greyed_out, got %q", item.AccessState)
	}
	if len(item.MissingRequirements) == 0 || item.MissingRequirements[0] != "doc.read" {
		t.Errorf("missing_requirements: want [doc.read], got %v", item.MissingRequirements)
	}
}
