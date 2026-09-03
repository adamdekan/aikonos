package workflowsvc

// Tests for ListVersions pagination (F19): additive limit/cursor, following
// the audit-reader cursor convention (proto/broker.proto QueryAuditRequest /
// audit/reader.go filterAndPage).

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/db"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// fakeVersionsStore satisfies Store, returning a fixed, already-sorted (by
// version DESC — matching the real SQL query) row set for ListVersions.
type fakeVersionsStore struct {
	stubWorkflowStore
	rows []*db.WorkflowRow
}

// ListVersions simulates the SQL keyset+LIMIT bounding: f.rows is version-DESC,
// so it drops rows with version >= beforeVersion (the cursor) then caps at limit.
func (f *fakeVersionsStore) ListVersions(_ context.Context, _ string, _ uuid.UUID, beforeVersion, limit int) ([]*db.WorkflowRow, error) {
	var out []*db.WorkflowRow
	for _, r := range f.rows {
		if beforeVersion > 0 && r.Version >= beforeVersion {
			continue
		}
		out = append(out, r)
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out, nil
}

func threeVersionRows(lineageID uuid.UUID) []*db.WorkflowRow {
	// Descending by version, as the real SQL query (ORDER BY version DESC) returns.
	return []*db.WorkflowRow{
		{LineageID: lineageID, Version: 3, ApprovalState: "approved", CreatedAt: time.Now()},
		{LineageID: lineageID, Version: 2, ApprovalState: "approved", CreatedAt: time.Now()},
		{LineageID: lineageID, Version: 1, ApprovalState: "approved", CreatedAt: time.Now()},
	}
}

// TestListVersions_LimitZero_ReturnsEverything pins the legacy contract:
// limit=0 (proto3 default) reproduces today's unbounded behavior exactly.
func TestListVersions_LimitZero_ReturnsEverything(t *testing.T) {
	lineageID := uuid.New()
	store := &fakeVersionsStore{rows: threeVersionRows(lineageID)}
	req := &brokerv1.ListWorkflowVersionsRequest{LineageId: lineageID.String()}

	resp, err := ListVersions(context.Background(), testWFTenant, req, store, zap.NewNop())
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

// TestListVersions_LimitGreaterThanZero_FirstPageHasNextCursor verifies a
// bounded first page plus a non-empty next_cursor when more rows remain.
func TestListVersions_LimitGreaterThanZero_FirstPageHasNextCursor(t *testing.T) {
	lineageID := uuid.New()
	store := &fakeVersionsStore{rows: threeVersionRows(lineageID)}
	req := &brokerv1.ListWorkflowVersionsRequest{LineageId: lineageID.String(), Limit: 2}

	resp, err := ListVersions(context.Background(), testWFTenant, req, store, zap.NewNop())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.Items) != 2 {
		t.Fatalf("limit=2: want 2 items, got %d", len(resp.Items))
	}
	if resp.Items[0].Version != 3 || resp.Items[1].Version != 2 {
		t.Errorf("first page: want versions [3,2], got [%d,%d]", resp.Items[0].Version, resp.Items[1].Version)
	}
	if resp.NextCursor != "2" {
		t.Errorf("next_cursor: want \"2\" (last returned version), got %q", resp.NextCursor)
	}
}

// TestListVersions_CursorContinues_NoOverlapOrGap verifies that paging with
// the returned next_cursor picks up exactly where the first page left off.
func TestListVersions_CursorContinues_NoOverlapOrGap(t *testing.T) {
	lineageID := uuid.New()
	store := &fakeVersionsStore{rows: threeVersionRows(lineageID)}

	page1, err := ListVersions(context.Background(), testWFTenant,
		&brokerv1.ListWorkflowVersionsRequest{LineageId: lineageID.String(), Limit: 2},
		store, zap.NewNop())
	if err != nil || len(page1.Items) != 2 || page1.NextCursor == "" {
		t.Fatalf("setup: items=%d cursor=%q err=%v", len(page1.Items), page1.NextCursor, err)
	}

	page2, err := ListVersions(context.Background(), testWFTenant,
		&brokerv1.ListWorkflowVersionsRequest{LineageId: lineageID.String(), Limit: 2, Cursor: page1.NextCursor},
		store, zap.NewNop())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(page2.Items) != 1 || page2.Items[0].Version != 1 {
		t.Fatalf("continuation: want [version 1], got %v", page2.Items)
	}
	if page2.NextCursor != "" {
		t.Errorf("continuation: want empty next_cursor on last page, got %q", page2.NextCursor)
	}
}

// TestListVersions_InvalidCursor_InvalidArgument verifies a malformed cursor
// is rejected — a deliberate deviation from the audit reader's raw
// string-comparison cursor (which never fails to parse), justified because
// this cursor encodes a typed integer (version number).
func TestListVersions_InvalidCursor_InvalidArgument(t *testing.T) {
	lineageID := uuid.New()
	store := &fakeVersionsStore{rows: threeVersionRows(lineageID)}
	req := &brokerv1.ListWorkflowVersionsRequest{LineageId: lineageID.String(), Cursor: "not-a-version"}

	_, err := ListVersions(context.Background(), testWFTenant, req, store, zap.NewNop())
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("malformed cursor: want InvalidArgument, got %v", err)
	}
}
