package broker

// Tests for CP10c: ListWorkflowVersions north+south.
//
// WHY: the version-switcher in the webui needs all versions for a lineage
// (version number + approval_state) so the user can pick a prior approved
// version to pin as a downgrade. Skill-gated, like all other workflow RPCs.
//
// Pattern mirrors service_workflows_cp9_test.go — fake store, north+south each
// covered for the skill-deny and allow paths.

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

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

// ── fakeWorkflowVersionsStore ─────────────────────────────────────────────────

type fakeWorkflowVersionsStore struct {
	stubWorkflowStore
	rows map[string][]*db.WorkflowRow
	err  error
}

func newFakeVersionsStore() *fakeWorkflowVersionsStore {
	return &fakeWorkflowVersionsStore{rows: make(map[string][]*db.WorkflowRow)}
}

func (f *fakeWorkflowVersionsStore) addRow(row db.WorkflowRow) {
	lid := row.LineageID.String()
	r := row
	f.rows[lid] = append(f.rows[lid], &r)
}

func (f *fakeWorkflowVersionsStore) ListVersions(_ context.Context, _ string, lineageID uuid.UUID, _, _ int) ([]*db.WorkflowRow, error) {
	if f.err != nil {
		return nil, f.err
	}
	rows, ok := f.rows[lineageID.String()]
	if !ok {
		return nil, fmt.Errorf("lineage %s not found", lineageID)
	}
	return rows, nil
}

// ── helper builders ───────────────────────────────────────────────────────────

func newSandboxSvcForVersions(t *testing.T, store workflowsvc.Store, skillGranted bool) *SandboxService {
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
	svc.skillPolicy = &fakeSkillPolicy{enabled: true, granted: skillGranted}
	return svc
}

func newBrokerSvcForVersions(t *testing.T, store workflowsvc.Store, skillGranted bool) *BrokerService {
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
		Workflows: store,
	})
}

func makeVersionRows(lineageID uuid.UUID) []*db.WorkflowRow {
	now := time.Now().UTC()
	return []*db.WorkflowRow{
		{
			ID:               uuid.New(),
			LineageID:        lineageID,
			Version:          2,
			TenantID:         uuid.MustParse(testWFTenant),
			OwnerUserID:      testWFOwner,
			Name:             "wf",
			ApprovalState:    "approved",
			CreatedAt:        now.Add(-time.Minute),
			VisibilityGroups: json.RawMessage(`[]`),
			Definition:       json.RawMessage(minimalValidWorkflowJSON()),
		},
		{
			ID:               uuid.New(),
			LineageID:        lineageID,
			Version:          1,
			TenantID:         uuid.MustParse(testWFTenant),
			OwnerUserID:      testWFOwner,
			Name:             "wf",
			ApprovalState:    "approved",
			CreatedAt:        now.Add(-2 * time.Minute),
			VisibilityGroups: json.RawMessage(`[]`),
			Definition:       json.RawMessage(minimalValidWorkflowJSON()),
		},
	}
}

// Note (Issue 3 — pin FailedPrecondition surface): the server-side guard that
// rejects a pin attempt on a non-approved version is exercised by
// TestPinCore_ProposedVersion_FailedPrecondition in service_workflows_cp9_test.go.
// The version-switcher UI hides the pin button for non-approved versions, but the
// server-side guard is the authoritative safety net and is already covered there.

// ── south (SandboxService) tests ──────────────────────────────────────────────

// TestSouthListWorkflowVersions_SkillDenied: skill gate → PermissionDenied.
func TestSouthListWorkflowVersions_SkillDenied(t *testing.T) {
	lineageID := uuid.New()
	store := newFakeVersionsStore()
	for _, r := range makeVersionRows(lineageID) {
		store.addRow(*r)
	}
	svc := newSandboxSvcForVersions(t, store, false)

	_, err := svc.ListWorkflowVersions(gatewayCtxForWorkflow(), &brokerv1.ListWorkflowVersionsRequest{
		TenantId:   testWFTenant,
		OwnerGrant: mintWFGrant(t),
		LineageId:  lineageID.String(),
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("skill deny: want PermissionDenied, got %v", err)
	}
}

// TestSouthListWorkflowVersions_Allow_ReturnsVersions: skill granted → items.
func TestSouthListWorkflowVersions_Allow_ReturnsVersions(t *testing.T) {
	lineageID := uuid.New()
	store := newFakeVersionsStore()
	for _, r := range makeVersionRows(lineageID) {
		store.addRow(*r)
	}
	svc := newSandboxSvcForVersions(t, store, true)

	resp, err := svc.ListWorkflowVersions(gatewayCtxForWorkflow(), &brokerv1.ListWorkflowVersionsRequest{
		TenantId:   testWFTenant,
		OwnerGrant: mintWFGrant(t),
		LineageId:  lineageID.String(),
	})
	if err != nil {
		t.Fatalf("south allow: unexpected error: %v", err)
	}
	if len(resp.Items) != 2 {
		t.Fatalf("want 2 items, got %d", len(resp.Items))
	}
	// Newest first (version 2).
	if resp.Items[0].Version != 2 {
		t.Errorf("items[0].version: want 2, got %d", resp.Items[0].Version)
	}
	if resp.Items[0].ApprovalState != "approved" {
		t.Errorf("items[0].approval_state: want approved, got %s", resp.Items[0].ApprovalState)
	}
	if resp.Items[0].CreatedAt == "" {
		t.Error("items[0].created_at must not be empty")
	}
}

// TestSouthListWorkflowVersions_StoreError_Internal: store returns an error
// → codes.Internal (not a panic, not a 200 with empty list).
// WHY: listWorkflowVersionsCore must surface store failures so callers know the
// list is incomplete rather than silently treating an error as "no versions".
func TestSouthListWorkflowVersions_StoreError_Internal(t *testing.T) {
	store := newFakeVersionsStore()
	store.err = fmt.Errorf("db connection lost")
	svc := newSandboxSvcForVersions(t, store, true /* skill granted */)

	// Use a lineage that is not in the store so the err path is hit.
	_, err := svc.ListWorkflowVersions(gatewayCtxForWorkflow(), &brokerv1.ListWorkflowVersionsRequest{
		TenantId:   testWFTenant,
		OwnerGrant: mintWFGrant(t),
		LineageId:  uuid.New().String(),
	})
	if status.Code(err) != codes.Internal {
		t.Fatalf("store error: want Internal, got %v", err)
	}
}

// ── north (BrokerService) tests ───────────────────────────────────────────────

// TestNorthListWorkflowVersions_SkillDenied: skill gate → PermissionDenied.
func TestNorthListWorkflowVersions_SkillDenied(t *testing.T) {
	lineageID := uuid.New()
	store := newFakeVersionsStore()
	for _, r := range makeVersionRows(lineageID) {
		store.addRow(*r)
	}
	svc := newBrokerSvcForVersions(t, store, false)

	ctx := ctxWithSubject(testWFTenant, testWFOwner, "")
	_, err := svc.ListWorkflowVersions(ctx, &brokerv1.ListWorkflowVersionsRequest{
		TenantId:    testWFTenant,
		OwnerUserId: testWFOwner,
		LineageId:   lineageID.String(),
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("north skill deny: want PermissionDenied, got %v", err)
	}
}

// TestNorthListWorkflowVersions_Allow_ReturnsVersions: skill granted → items.
func TestNorthListWorkflowVersions_Allow_ReturnsVersions(t *testing.T) {
	lineageID := uuid.New()
	store := newFakeVersionsStore()
	for _, r := range makeVersionRows(lineageID) {
		store.addRow(*r)
	}
	svc := newBrokerSvcForVersions(t, store, true)

	ctx := ctxWithSubject(testWFTenant, testWFOwner, "")
	resp, err := svc.ListWorkflowVersions(ctx, &brokerv1.ListWorkflowVersionsRequest{
		TenantId:    testWFTenant,
		OwnerUserId: testWFOwner,
		LineageId:   lineageID.String(),
	})
	if err != nil {
		t.Fatalf("north allow: unexpected error: %v", err)
	}
	if len(resp.Items) != 2 {
		t.Fatalf("want 2 items, got %d", len(resp.Items))
	}
	if resp.Items[0].Version != 2 {
		t.Errorf("items[0].version: want 2, got %d", resp.Items[0].Version)
	}
}
