package broker

// GetSessionUsage (session_usage.go): the per-session usage strip's read path.
// The tests that matter here are the scoping ones — this RPC is not admin-gated,
// so the user predicate derived from the verified identity is the only thing
// standing between a caller and another user's session cost.

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/db"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

const (
	suTenant  = "11111111-1111-1111-1111-111111111111"
	suSession = "session-abc"
)

// seedUsage inserts events straight through the fake's Insert so the rollup a
// test asserts on is produced by the same path production writes.
func seedUsage(t *testing.T, store *fakeUsageEventStore, evs ...db.UsageEvent) {
	t.Helper()
	for _, ev := range evs {
		if err := store.Insert(context.Background(), ev); err != nil {
			t.Fatalf("seed insert: %v", err)
		}
	}
}

// TestGetSessionUsage_RollsUpByProviderModel: two calls on the same model
// collapse into one row with summed tokens/cost; a second model is its own row,
// and the more expensive model sorts first so rows[0] is the primary one.
func TestGetSessionUsage_RollsUpByProviderModel(t *testing.T) {
	events := &fakeUsageEventStore{}
	seedUsage(t, events,
		db.UsageEvent{TenantID: suTenant, UserID: "alice@example.com", SessionID: suSession,
			Provider: "p1", Model: "cheap", TokensIn: 10, TokensOut: 5, CostMicros: 1_000},
		db.UsageEvent{TenantID: suTenant, UserID: "alice@example.com", SessionID: suSession,
			Provider: "p1", Model: "cheap", TokensIn: 20, TokensOut: 7, CostMicros: 2_000},
		db.UsageEvent{TenantID: suTenant, UserID: "alice@example.com", SessionID: suSession,
			Provider: "p1", Model: "pricey", TokensIn: 1, TokensOut: 1, CostMicros: 90_000},
	)

	svc := NewBrokerService(Deps{Logger: zap.NewNop(), UsageEvents: events})
	resp, err := svc.GetSessionUsage(
		ctxWithIdentity(suTenant, "alice@example.com"),
		&brokerv1.GetSessionUsageRequest{TenantId: suTenant, SessionId: suSession},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.Rows) != 2 {
		t.Fatalf("want 2 rows, got %d: %+v", len(resp.Rows), resp.Rows)
	}
	if resp.Rows[0].Model != "pricey" {
		t.Errorf("want most expensive model first, got %q", resp.Rows[0].Model)
	}
	cheap := resp.Rows[1]
	if cheap.Model != "cheap" {
		t.Fatalf("want cheap row second, got %q", cheap.Model)
	}
	if cheap.TokensIn != 30 || cheap.TokensOut != 12 || cheap.CostMicros != 3_000 || cheap.Calls != 2 {
		t.Errorf("cheap row not summed: in=%d out=%d cost=%d calls=%d",
			cheap.TokensIn, cheap.TokensOut, cheap.CostMicros, cheap.Calls)
	}
}

// TestGetSessionUsage_OtherUsersSessionIsInvisible is the security case: eve
// asks for a session id that exists but belongs to alice. The response must be
// empty — not alice's numbers, and not a NotFound that would confirm the id
// exists. Only the verified identity may scope the read, never a request field.
func TestGetSessionUsage_OtherUsersSessionIsInvisible(t *testing.T) {
	events := &fakeUsageEventStore{}
	seedUsage(t, events,
		db.UsageEvent{TenantID: suTenant, UserID: "alice@example.com", SessionID: suSession,
			Provider: "p1", Model: "m", TokensIn: 100, TokensOut: 50, CostMicros: 500_000},
	)

	svc := NewBrokerService(Deps{Logger: zap.NewNop(), UsageEvents: events})
	resp, err := svc.GetSessionUsage(
		ctxWithIdentity(suTenant, "eve@example.com"),
		&brokerv1.GetSessionUsageRequest{TenantId: suTenant, SessionId: suSession},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.Rows) != 0 {
		t.Fatalf("eve read alice's session usage: %+v", resp.Rows)
	}
}

// TestGetSessionUsage_EmptySessionIdReturnsNothing: session_id is "" on every
// path with no session of its own (external invoke, parent-side calls outside a
// session). Those events must never be lumped together and returned to whoever
// asks with a blank id.
func TestGetSessionUsage_EmptySessionIdReturnsNothing(t *testing.T) {
	events := &fakeUsageEventStore{}
	seedUsage(t, events,
		db.UsageEvent{TenantID: suTenant, UserID: "alice@example.com", SessionID: "",
			Provider: "p1", Model: "m", TokensIn: 9, TokensOut: 9, CostMicros: 9_000},
	)

	svc := NewBrokerService(Deps{Logger: zap.NewNop(), UsageEvents: events})
	resp, err := svc.GetSessionUsage(
		ctxWithIdentity(suTenant, "alice@example.com"),
		&brokerv1.GetSessionUsageRequest{TenantId: suTenant, SessionId: ""},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.Rows) != 0 {
		t.Fatalf("blank session id returned unattributed events: %+v", resp.Rows)
	}
}

// TestGetSessionUsage_CrossTenantIsDenied: a caller may not name a tenant other
// than the one on their identity (callerIdentity's existing guard — pinned here
// because this RPC has no admin gate in front of it).
func TestGetSessionUsage_CrossTenantIsDenied(t *testing.T) {
	svc := NewBrokerService(Deps{Logger: zap.NewNop(), UsageEvents: &fakeUsageEventStore{}})
	_, err := svc.GetSessionUsage(
		ctxWithIdentity(suTenant, "alice@example.com"),
		&brokerv1.GetSessionUsageRequest{
			TenantId:  "22222222-2222-2222-2222-222222222222",
			SessionId: suSession,
		},
	)
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("want PermissionDenied, got %v", err)
	}
}

// TestGetSessionUsage_OverlongSessionIdRejected: the id is bounded before it
// reaches the query.
func TestGetSessionUsage_OverlongSessionIdRejected(t *testing.T) {
	long := make([]byte, sessionIDMaxChars+1)
	for i := range long {
		long[i] = 'a'
	}
	svc := NewBrokerService(Deps{Logger: zap.NewNop(), UsageEvents: &fakeUsageEventStore{}})
	_, err := svc.GetSessionUsage(
		ctxWithIdentity(suTenant, "alice@example.com"),
		&brokerv1.GetSessionUsageRequest{TenantId: suTenant, SessionId: string(long)},
	)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
}

// TestGetSessionUsage_NilStoreIsEmptyNotError: a deployment with no usage-event
// store configured degrades to "no usage", matching how recordLlmUsage treats a
// nil store — the strip renders nothing rather than erroring the chat view.
func TestGetSessionUsage_NilStoreIsEmptyNotError(t *testing.T) {
	svc := NewBrokerService(Deps{Logger: zap.NewNop()})
	resp, err := svc.GetSessionUsage(
		ctxWithIdentity(suTenant, "alice@example.com"),
		&brokerv1.GetSessionUsageRequest{TenantId: suTenant, SessionId: suSession},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.Rows) != 0 {
		t.Fatalf("want no rows, got %+v", resp.Rows)
	}
}

// TestGetSessionUsage_StoreErrorSurfaces: a real DB failure is an error, not a
// silent zero — a zero cost is indistinguishable from a free session, so
// swallowing the error would show the user a confidently wrong number.
func TestGetSessionUsage_StoreErrorSurfaces(t *testing.T) {
	events := &fakeUsageEventStore{totalsErr: errors.New("db down")}
	svc := NewBrokerService(Deps{Logger: zap.NewNop(), UsageEvents: events})
	_, err := svc.GetSessionUsage(
		ctxWithIdentity(suTenant, "alice@example.com"),
		&brokerv1.GetSessionUsageRequest{TenantId: suTenant, SessionId: suSession},
	)
	if status.Code(err) != codes.Internal {
		t.Fatalf("want Internal, got %v", err)
	}
}
