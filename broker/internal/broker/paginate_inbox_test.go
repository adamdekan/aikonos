package broker

// Tests for paginateInbox (F19): additive limit/cursor pagination on
// ListInboxEnvelopes, following the audit-reader convention
// (broker/internal/audit/reader.go filterAndPage).

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/db"
)

func inboxEnvelopeAt(t time.Time) *db.Envelope {
	return &db.Envelope{EnvelopeID: uuid.New(), CreatedAt: t}
}

// TestPaginateInbox_LimitZero_ReturnsEverything pins the legacy contract:
// limit=0 (proto3 default) reproduces today's behavior — every row from the
// already-fetched (existing 100-row-capped) query, unpaged.
func TestPaginateInbox_LimitZero_ReturnsEverything(t *testing.T) {
	base := time.Now().UTC()
	envs := []*db.Envelope{
		inboxEnvelopeAt(base),
		inboxEnvelopeAt(base.Add(-time.Minute)),
		inboxEnvelopeAt(base.Add(-2 * time.Minute)),
	}
	page, next, err := paginateInbox(envs, 0, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(page) != 3 {
		t.Fatalf("limit=0: want all 3 envelopes, got %d", len(page))
	}
	if next != "" {
		t.Errorf("limit=0: want empty next_cursor, got %q", next)
	}
}

// TestPaginateInbox_LimitGreaterThanZero_FirstPageHasNextCursor verifies a
// bounded first page plus a non-empty next_cursor when more rows remain.
func TestPaginateInbox_LimitGreaterThanZero_FirstPageHasNextCursor(t *testing.T) {
	base := time.Now().UTC()
	envs := []*db.Envelope{
		inboxEnvelopeAt(base),
		inboxEnvelopeAt(base.Add(-time.Minute)),
		inboxEnvelopeAt(base.Add(-2 * time.Minute)),
	}
	page, next, err := paginateInbox(envs, 2, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(page) != 2 {
		t.Fatalf("limit=2: want 2 envelopes, got %d", len(page))
	}
	if next == "" {
		t.Error("limit=2 with more remaining: want non-empty next_cursor")
	}
}

// TestPaginateInbox_CursorContinues_NoOverlapOrGap verifies that paging with
// the returned next_cursor picks up exactly where the first page left off.
func TestPaginateInbox_CursorContinues_NoOverlapOrGap(t *testing.T) {
	base := time.Now().UTC()
	envs := []*db.Envelope{
		inboxEnvelopeAt(base),
		inboxEnvelopeAt(base.Add(-time.Minute)),
		inboxEnvelopeAt(base.Add(-2 * time.Minute)),
	}
	page1, cursor, err := paginateInbox(envs, 2, "")
	if err != nil || len(page1) != 2 || cursor == "" {
		t.Fatalf("setup: page1=%d cursor=%q err=%v", len(page1), cursor, err)
	}

	page2, next2, err := paginateInbox(envs, 2, cursor)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(page2) != 1 {
		t.Fatalf("continuation: want 1 remaining envelope, got %d", len(page2))
	}
	if next2 != "" {
		t.Errorf("continuation: want empty next_cursor on last page, got %q", next2)
	}
	// No overlap: page2's envelope must not be one of page1's.
	for _, p1 := range page1 {
		if p1.EnvelopeID == page2[0].EnvelopeID {
			t.Error("continuation overlaps with page1")
		}
	}
}

// TestPaginateInbox_InvalidCursor_InvalidArgument verifies a malformed cursor
// is rejected — a deliberate deviation from the audit reader's raw
// string-comparison cursor (which never fails to parse), justified because
// this cursor encodes a typed composite (created_at|envelope_id) value.
func TestPaginateInbox_InvalidCursor_InvalidArgument(t *testing.T) {
	envs := []*db.Envelope{inboxEnvelopeAt(time.Now())}
	_, _, err := paginateInbox(envs, 0, "not-a-timestamp")
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("malformed cursor: want InvalidArgument, got %v", err)
	}
}

// TestPaginateInbox_OldFormatCursor_InvalidArgument pins that a pre-composite
// bare-timestamp cursor (no "|<envelope_id>" suffix) is rejected rather than
// silently accepted — no production cursors predate the composite format, so
// there is no back-compat obligation.
func TestPaginateInbox_OldFormatCursor_InvalidArgument(t *testing.T) {
	envs := []*db.Envelope{inboxEnvelopeAt(time.Now())}
	oldCursor := time.Now().UTC().Format(time.RFC3339Nano) // bare timestamp, no envelope_id
	_, _, err := paginateInbox(envs, 0, oldCursor)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("old-format cursor: want InvalidArgument, got %v", err)
	}
}

// TestPaginateInbox_SameTimestamp_CursorBoundary_NoDropOrDuplicate pins the
// keyset-gap fix: when multiple envelopes share an identical created_at
// across a page boundary, the composite (created_at|envelope_id) cursor must
// neither drop nor duplicate any of them. Against the old bare-timestamp
// predicate (e.CreatedAt.Before(cursorTime)) every same-timestamp row is
// silently dropped once it becomes the cursor value — this is the red proof.
func TestPaginateInbox_SameTimestamp_CursorBoundary_NoDropOrDuplicate(t *testing.T) {
	tie := time.Now().UTC()
	envs := []*db.Envelope{
		inboxEnvelopeAt(tie),
		inboxEnvelopeAt(tie),
		inboxEnvelopeAt(tie),
	}

	page1, cursor, err := paginateInbox(envs, 2, "")
	if err != nil || len(page1) != 2 || cursor == "" {
		t.Fatalf("setup: page1=%d cursor=%q err=%v", len(page1), cursor, err)
	}

	page2, next2, err := paginateInbox(envs, 2, cursor)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(page2) != 1 {
		t.Fatalf("same-timestamp boundary: want 1 remaining envelope, got %d", len(page2))
	}
	if next2 != "" {
		t.Errorf("same-timestamp boundary: want empty next_cursor on last page, got %q", next2)
	}

	// No duplicate and no drop: the union of both pages must be exactly the
	// 3 distinct envelope IDs from the input.
	seen := map[uuid.UUID]int{}
	for _, e := range page1 {
		seen[e.EnvelopeID]++
	}
	for _, e := range page2 {
		seen[e.EnvelopeID]++
	}
	if len(seen) != 3 {
		t.Fatalf("want 3 distinct envelope IDs across both pages, got %d (%v)", len(seen), seen)
	}
	for id, count := range seen {
		if count != 1 {
			t.Errorf("envelope %s appeared %d times across pages, want exactly 1", id, count)
		}
	}
}
