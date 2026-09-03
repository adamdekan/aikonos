package broker

import (
	"context"
	"errors"
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

// ── DismissEnvelope tests ─────────────────────────────────────────────────────

// fakeDismissStore satisfies taskStore (GetEnvelope/DismissEnvelope only;
// the rest via the embedded stub) for unit tests without Postgres. Create and
// Transition are overridden to fail loud (mirroring noopTaskStore in
// simulate_test.go) so that if DismissEnvelope is ever changed to spawn a
// task via spawnDelegatedTask (which calls Tasks.Create), the test catches
// it instead of silently accepting a zero-value return.
type fakeDismissStore struct {
	stubTaskStore
	t             *testing.T
	envelope      *db.Envelope
	getErr        error
	dismissErr    error
	dismissCalled bool
}

func (s *fakeDismissStore) Create(_ context.Context, _ *db.Task) error {
	s.t.Fatalf("DismissEnvelope must not spawn a task: Create")
	return nil
}

func (s *fakeDismissStore) Transition(_ context.Context, _, _ string, _, _ db.TaskState) error {
	s.t.Fatalf("DismissEnvelope must not spawn a task: Transition")
	return nil
}

func (s *fakeDismissStore) GetEnvelope(_ context.Context, _, _ string) (*db.Envelope, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	return s.envelope, nil
}

func (s *fakeDismissStore) DismissEnvelope(_ context.Context, _, _, _ string) (db.EnvelopeState, error) {
	s.dismissCalled = true
	if s.dismissErr != nil {
		return "", s.dismissErr
	}
	return db.EnvelopeDismissed, nil
}

func testDismissDeps(t *testing.T, store taskStore) Deps {
	t.Helper()
	em, err := audit.NewEmitter(context.Background(), audit.Config{})
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeFGA{}
	fgaSrv := f.server(t)
	t.Cleanup(fgaSrv.Close)

	cfg := policy.Config{OpenFGAEndpoint: fgaSrv.URL, OpenFGAStoreID: "store-1"}
	eng, err := policy.NewEngine(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}

	return Deps{
		Logger:   zap.NewNop(),
		Audit:    em,
		Policy:   eng,
		TenantID: "aikonos-dev",
		Tasks:    store,
	}
}

func deliveredEnvelope(tenantID, envelopeID, toUser string) *db.Envelope {
	tid := uuid.MustParse(tenantID)
	eid := uuid.MustParse(envelopeID)
	return &db.Envelope{
		EnvelopeID: eid,
		TenantID:   tid,
		FromUserID: "alice@example.com",
		ToTarget:   map[string]any{"type": "user", "id": toUser},
		TaskSpec:   map[string]any{"intent": "do the thing"},
		State:      db.EnvelopeDelivered,
	}
}

// TestDismissEnvelope_Delivered_Transitions verifies that dismissing a DELIVERED
// envelope calls the repo dismiss method and returns success=true.
func TestDismissEnvelope_Delivered_Transitions(t *testing.T) {
	const (
		tenantID   = testTenantUUID
		envelopeID = "11111111-1111-1111-1111-111111111111"
		userID     = "bob@example.com"
	)

	store := &fakeDismissStore{
		t:        t,
		envelope: deliveredEnvelope(tenantID, envelopeID, userID),
	}
	svc := NewBrokerService(testDismissDeps(t, store))
	ctx := ctxWithIdentity(tenantID, userID)

	resp, err := svc.DismissEnvelope(ctx, &brokerv1.DismissEnvelopeRequest{
		EnvelopeId: envelopeID,
		UserId:     userID,
		TenantId:   tenantID,
	})
	if err != nil {
		t.Fatalf("DismissEnvelope: unexpected error: %v", err)
	}
	if !resp.Success {
		t.Error("DismissEnvelope: expected success=true")
	}
	if !store.dismissCalled {
		t.Error("DismissEnvelope: DismissEnvelope repo method was not called")
	}
}

// TestDismissEnvelope_NoTaskSpawned verifies that dismissing does not spawn a
// task: DismissEnvelope never calls spawnDelegatedTask. fakeDismissStore's
// Create/Transition overrides fail loud (t.Fatalf) if that ever changes, so
// this proves the RPC completes without touching task-creation at all.
func TestDismissEnvelope_NoTaskSpawned(t *testing.T) {
	const (
		tenantID   = testTenantUUID
		envelopeID = "22222222-2222-2222-2222-222222222222"
		userID     = "bob@example.com"
	)

	store := &fakeDismissStore{
		t:        t,
		envelope: deliveredEnvelope(tenantID, envelopeID, userID),
	}
	svc := NewBrokerService(testDismissDeps(t, store))
	ctx := ctxWithIdentity(tenantID, userID)

	if _, err := svc.DismissEnvelope(ctx, &brokerv1.DismissEnvelopeRequest{
		EnvelopeId: envelopeID,
		UserId:     userID,
		TenantId:   tenantID,
	}); err != nil {
		t.Fatalf("DismissEnvelope: unexpected error: %v", err)
	}
}

// TestDismissEnvelope_NotFound returns NotFound when GetEnvelope returns an
// error matching db.ErrNotFound (F15 — repo layer translates pgx.ErrNoRows to
// this sentinel; the fake here stands in for an already-translated repo).
func TestDismissEnvelope_NotFound(t *testing.T) {
	const tenantID = testTenantUUID

	store := &fakeDismissStore{t: t, getErr: db.ErrNotFound}
	svc := NewBrokerService(testDismissDeps(t, store))
	ctx := ctxWithIdentity(tenantID, "bob@example.com")

	_, err := svc.DismissEnvelope(ctx, &brokerv1.DismissEnvelopeRequest{
		EnvelopeId: "33333333-3333-3333-3333-333333333333",
		UserId:     "bob@example.com",
		TenantId:   tenantID,
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("missing envelope: want NotFound, got %v", err)
	}
}

// TestDismissEnvelope_AlreadyTerminal returns FailedPrecondition when the repo
// rejects the transition (envelope already terminal or not addressed to user).
func TestDismissEnvelope_AlreadyTerminal(t *testing.T) {
	const (
		tenantID   = testTenantUUID
		envelopeID = "44444444-4444-4444-4444-444444444444"
		userID     = "bob@example.com"
	)

	store := &fakeDismissStore{
		t:          t,
		envelope:   deliveredEnvelope(tenantID, envelopeID, userID),
		dismissErr: errors.New("envelope already terminal"),
	}
	svc := NewBrokerService(testDismissDeps(t, store))
	ctx := ctxWithIdentity(tenantID, userID)

	_, err := svc.DismissEnvelope(ctx, &brokerv1.DismissEnvelopeRequest{
		EnvelopeId: envelopeID,
		UserId:     userID,
		TenantId:   tenantID,
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("terminal envelope: want FailedPrecondition, got %v", err)
	}
}
