package broker

// Tests for two dev-fix follow-ups:
//   1. DeleteWorkflow (north) — owner-only lineage removal.
//   2. publishCore group-id prefix normalization — the webui picker sends
//      "group:"-prefixed ids; publishCore must strip the prefix before the FGA
//      membership check and store the bare form.
//
// Reuses the CP6b/CP7 fake harness in the same package (newFakeOwnerStore,
// ownerRow, fakeFGA, newSandboxSvcForCP7, fakeWorkflowPublishStore,
// minimalWorkflowRowForPublish, ctxWithSubject, mintWFGrant, gatewayCtxForWorkflow).

import (
	"context"
	"fmt"
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

// ── publish: group-id prefix normalization ───────────────────────────────────

// TestPublishWorkflow_PrefixedGroupId_NormalizedToBare proves the webui picker's
// "group:"-prefixed group id is accepted: publishCore strips the prefix before the
// FGA membership check (checks group:security-team, not group:group:security-team)
// and stores the bare form so read-time visibility matching works.
func TestPublishWorkflow_PrefixedGroupId_NormalizedToBare(t *testing.T) {
	lineageID := testLineageUUID(t)
	ownerStore := newFakeOwnerStore()
	ownerStore.addRow(ownerRow(lineageID))
	ps := &fakeWorkflowPublishStore{versionRow: minimalWorkflowRowForPublish(lineageID, 1)}

	// FGA membership is keyed on the BARE object id.
	fga := &fakeFGA{checks: map[string]bool{
		"user:" + testWFOwner + "|can_invoke|skill:workflows": true,
		"user:" + testWFOwner + "|member|group:security-team": true,
	}}
	svc := newSandboxSvcForCP7(t, ownerStore, ps, true, fga)

	resp, err := svc.PublishWorkflow(gatewayCtxForWorkflow(), &brokerv1.PublishWorkflowRequest{
		TenantId:    testWFTenant,
		OwnerUserId: testWFOwner,
		OwnerGrant:  mintWFGrant(t),
		LineageId:   lineageID.String(),
		Version:     1,
		GroupIds:    []string{"group:security-team"}, // prefixed, as the webui picker sends
	})
	if err != nil {
		t.Fatalf("publish with prefixed group id: unexpected error: %v", err)
	}
	if len(resp.Groups) != 1 || resp.Groups[0] != "security-team" {
		t.Errorf("response groups: want [security-team] (bare), got %v", resp.Groups)
	}
	if len(ps.published) != 1 {
		t.Fatalf("PublishVersion calls: want 1, got %d", len(ps.published))
	}
	if len(ps.published[0].groups) != 1 || ps.published[0].groups[0] != "security-team" {
		t.Errorf("stored groups: want [security-team] (bare), got %v", ps.published[0].groups)
	}
}

// ── delete: fake store ───────────────────────────────────────────────────────

// fakeWorkflowDeleteStore satisfies workflowStore for DeleteWorkflow tests
// (GetCurrent + DeleteLineage overridden; rest via the embedded stub). current
// is returned by GetCurrent (nil → not-found error); DeleteLineage records the
// lineage ids it receives and returns the configured version count.
type fakeWorkflowDeleteStore struct {
	stubWorkflowStore
	current  *db.WorkflowRow
	deleted  []uuid.UUID
	versions int
}

func (f *fakeWorkflowDeleteStore) GetCurrent(_ context.Context, _ string, lineageID uuid.UUID) (*db.WorkflowRow, error) {
	if f.current == nil {
		return nil, fmt.Errorf("workflow lineage %s not found", lineageID)
	}
	return f.current, nil
}

func (f *fakeWorkflowDeleteStore) DeleteLineage(_ context.Context, _ string, lineageID uuid.UUID) (int, error) {
	f.deleted = append(f.deleted, lineageID)
	return f.versions, nil
}

// newBrokerSvcForDelete builds a BrokerService wired with a delete store and an
// FGA backend (same httptest pattern as newBrokerSvcForCP7).
func newBrokerSvcForDelete(t *testing.T, deleteStore *fakeWorkflowDeleteStore, fgaChecks map[string]bool) *BrokerService {
	t.Helper()
	fga := &fakeFGA{checks: fgaChecks}
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
		Workflows: deleteStore,
	})
}

// TestDeleteWorkflow_Owner_Succeeds: the lineage owner deletes; DeleteLineage is
// called once and the version count is returned.
func TestDeleteWorkflow_Owner_Succeeds(t *testing.T) {
	lineageID := testLineageUUID(t)
	row := ownerRow(lineageID)
	ds := &fakeWorkflowDeleteStore{current: &row, versions: 3}

	svc := newBrokerSvcForDelete(t, ds, map[string]bool{
		"user:" + testWFOwner + "|can_invoke|skill:workflows": true,
	})
	ctx := ctxWithSubject(testWFTenant, testWFOwner, "")

	resp, err := svc.DeleteWorkflow(ctx, &brokerv1.DeleteWorkflowRequest{
		TenantId:    testWFTenant,
		OwnerUserId: testWFOwner,
		LineageId:   lineageID.String(),
	})
	if err != nil {
		t.Fatalf("DeleteWorkflow owner: unexpected error: %v", err)
	}
	if resp.VersionsDeleted != 3 {
		t.Errorf("VersionsDeleted: want 3, got %d", resp.VersionsDeleted)
	}
	if len(ds.deleted) != 1 || ds.deleted[0] != lineageID {
		t.Errorf("DeleteLineage: want one call for %s, got %v", lineageID, ds.deleted)
	}
}

// TestDeleteWorkflow_NonOwner_PermissionDenied: a non-owner is rejected and the
// lineage is never touched.
func TestDeleteWorkflow_NonOwner_PermissionDenied(t *testing.T) {
	lineageID := testLineageUUID(t)
	row := ownerRow(lineageID) // owned by testWFOwner
	ds := &fakeWorkflowDeleteStore{current: &row, versions: 1}

	svc := newBrokerSvcForDelete(t, ds, map[string]bool{
		"user:eve@example.com|can_invoke|skill:workflows": true,
	})
	ctx := ctxWithSubject(testWFTenant, "eve@example.com", "")

	_, err := svc.DeleteWorkflow(ctx, &brokerv1.DeleteWorkflowRequest{
		TenantId:    testWFTenant,
		OwnerUserId: "eve@example.com",
		LineageId:   lineageID.String(),
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-owner delete: want PermissionDenied, got %v", err)
	}
	if len(ds.deleted) != 0 {
		t.Error("DeleteLineage must not be called for a non-owner")
	}
}

// TestDeleteWorkflow_SkillDenied_PermissionDenied: without skill:workflows the
// delete is rejected before any owner lookup or deletion.
func TestDeleteWorkflow_SkillDenied_PermissionDenied(t *testing.T) {
	lineageID := testLineageUUID(t)
	row := ownerRow(lineageID)
	ds := &fakeWorkflowDeleteStore{current: &row, versions: 1}

	svc := newBrokerSvcForDelete(t, ds, map[string]bool{
		// skill:workflows NOT granted
	})
	ctx := ctxWithSubject(testWFTenant, testWFOwner, "")

	_, err := svc.DeleteWorkflow(ctx, &brokerv1.DeleteWorkflowRequest{
		TenantId:    testWFTenant,
		OwnerUserId: testWFOwner,
		LineageId:   lineageID.String(),
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("skill-denied delete: want PermissionDenied, got %v", err)
	}
	if len(ds.deleted) != 0 {
		t.Error("DeleteLineage must not be called when the skill is denied")
	}
}

// TestDeleteWorkflow_NotFound: a missing lineage surfaces NotFound.
func TestDeleteWorkflow_NotFound(t *testing.T) {
	lineageID := testLineageUUID(t)
	ds := &fakeWorkflowDeleteStore{current: nil} // GetCurrent → not found

	svc := newBrokerSvcForDelete(t, ds, map[string]bool{
		"user:" + testWFOwner + "|can_invoke|skill:workflows": true,
	})
	ctx := ctxWithSubject(testWFTenant, testWFOwner, "")

	_, err := svc.DeleteWorkflow(ctx, &brokerv1.DeleteWorkflowRequest{
		TenantId:    testWFTenant,
		OwnerUserId: testWFOwner,
		LineageId:   lineageID.String(),
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("missing lineage delete: want NotFound, got %v", err)
	}
}
