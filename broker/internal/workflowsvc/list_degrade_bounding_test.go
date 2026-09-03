package workflowsvc

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/adamdekan/aikonos/broker/internal/db"
)

// ── Task 3: shared_unavailable degradation flag ──────────────────────────────

// listObjectsErrPolicy is FGA-enabled but errors on ListObjects, simulating an
// FGA outage on the shared-workflow resolution path.
type listObjectsErrPolicy struct{}

func (listObjectsErrPolicy) FGAEnabled() bool { return true }
func (listObjectsErrPolicy) ListObjects(context.Context, string, string, string) ([]string, error) {
	return nil, errors.New("fga unavailable")
}
func (listObjectsErrPolicy) CheckFGA(context.Context, string, string, string) (bool, error) {
	return true, nil
}

var _ AccessPolicy = listObjectsErrPolicy{}

// TestList_FGAOutage_SetsSharedUnavailable: when ListObjects errors, the
// response carries own workflows only and shared_unavailable=true.
func TestList_FGAOutage_SetsSharedUnavailable(t *testing.T) {
	lineage := uuid.New()
	store := newFakeListStoreCP8()
	store.addOwn(db.WorkflowRow{
		ID: uuid.New(), LineageID: lineage, Version: 1,
		TenantID: uuid.MustParse(testWFTenant), OwnerUserID: testWFOwner,
		Name: "own-wf", Status: "private", VisibilityKind: "private",
		VisibilityGroups: json.RawMessage(`[]`),
		Definition:       json.RawMessage(minimalValidWorkflowJSON()),
	})

	resp, err := List(context.Background(), testWFTenant, testWFOwner, store,
		listObjectsErrPolicy{}, testWFTenantObject, zap.NewNop(), 0, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if !resp.SharedUnavailable {
		t.Error("shared_unavailable: want true on FGA ListObjects error")
	}
	if len(resp.Items) != 1 || !resp.Items[0].IsOwner {
		t.Errorf("want 1 own-only item, got %d items", len(resp.Items))
	}
}

// TestList_HealthyFGA_SharedAvailable: no FGA error → shared_unavailable=false.
func TestList_HealthyFGA_SharedAvailable(t *testing.T) {
	store := newFakeListStoreCP8()
	ap := &fakeWorkflowAccessPolicy{enabled: true, groups: []string{}, skillGrant: map[string]bool{}}

	resp, err := List(context.Background(), testWFTenant, testWFOwner, store, ap,
		testWFTenantObject, zap.NewNop(), 0, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if resp.SharedUnavailable {
		t.Error("shared_unavailable: want false when FGA is healthy")
	}
}

// ── Task 2: List pushes keyset+limit bounding into the SQL layer ─────────────

// recordingListStore captures the afterLineage/limit args List passes to the
// per-source fetches, proving bounding reaches the store instead of paging the
// whole table in memory.
type recordingListStore struct {
	stubWorkflowStore
	ownAfter, sharedAfter string
	ownLimit, sharedLimit int
}

func (r *recordingListStore) ListByOwner(_ context.Context, _, _, afterLineage string, limit int) ([]*db.WorkflowRow, error) {
	r.ownAfter, r.ownLimit = afterLineage, limit
	return nil, nil
}

// TestList_PassesCursorAndBoundedLimitToStore: List forwards the request cursor
// and limit+1 (slack for next_cursor detection) to the per-source SQL fetch.
func TestList_PassesCursorAndBoundedLimitToStore(t *testing.T) {
	store := &recordingListStore{}
	cursor := uuid.New().String()

	_, err := List(context.Background(), testWFTenant, testWFOwner, store, nil,
		testWFTenantObject, zap.NewNop(), 5, cursor)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if store.ownAfter != cursor {
		t.Errorf("ListByOwner afterLineage: want %q, got %q", cursor, store.ownAfter)
	}
	if store.ownLimit != 6 {
		t.Errorf("ListByOwner limit: want 6 (limit+1 slack), got %d", store.ownLimit)
	}
}

// TestList_LimitZero_UnboundedFetch: limit=0 forwards an unbounded fetch (0) to
// the store, preserving the legacy contract.
func TestList_LimitZero_UnboundedFetch(t *testing.T) {
	store := &recordingListStore{}
	_, err := List(context.Background(), testWFTenant, testWFOwner, store, nil,
		testWFTenantObject, zap.NewNop(), 0, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if store.ownLimit != 0 {
		t.Errorf("ListByOwner limit: want 0 (unbounded) for limit=0, got %d", store.ownLimit)
	}
}
