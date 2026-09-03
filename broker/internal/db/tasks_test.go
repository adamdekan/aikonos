package db

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/adamdekan/aikonos/broker/internal/metrics"
)

// ── DB-backed tests (require TEST_DATABASE_URL; skip when absent) ─────────────
//
// Mirrors the openTestPool skip pattern in workflows_test.go — no live
// Postgres is required for `go test` to pass; the test only exercises the
// repo layer when TEST_DATABASE_URL is set.

// TestTaskRepo_Get_NotFound verifies F15: a missing task surfaces as an error
// matching db.ErrNotFound (errors.Is), not a dead pgx.ErrNoRows comparison.
func TestTaskRepo_Get_NotFound(t *testing.T) {
	pool := openTestPool(t)
	repo := NewTaskRepo(pool, zap.NewNop())
	ctx := context.Background()
	tenant := uuid.New().String()

	_, err := repo.Get(ctx, tenant, uuid.New().String())
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(missing task): got %v, want errors.Is(err, ErrNotFound)", err)
	}
}

// fakeTaskMetricsRecorder captures RecordTransition calls without requiring
// the OTel SDK — used both for the no-DB wiring tests below and for
// TestTaskRepo_Transition_RecordsOnSuccessOnly's DB-gated positive-path
// assertion.
type fakeTaskMetricsRecorder struct {
	transitions []transitionCall
}

type transitionCall struct {
	from, to, tenant string
}

func (f *fakeTaskMetricsRecorder) RecordTransition(_ context.Context, from, to, tenant string) {
	f.transitions = append(f.transitions, transitionCall{from, to, tenant})
}
func (f *fakeTaskMetricsRecorder) RecordPlanOutcome(_ context.Context, _ string) {}
func (f *fakeTaskMetricsRecorder) RecordToolInvokeDuration(_ context.Context, _ float64, _, _ string) {
}

// TestNewTaskRepo_RecorderUnsetByDefault proves F23's additive-injection
// contract: existing NewTaskRepo callers (no SetRecorder call) get a repo
// whose recorder field is nil — Transition must skip the metric emit rather
// than panic on a nil interface.
func TestNewTaskRepo_RecorderUnsetByDefault(t *testing.T) {
	repo := NewTaskRepo(nil, zap.NewNop())
	if repo.recorder != nil {
		t.Fatal("expected recorder to be nil when SetRecorder was never called")
	}
}

// TestTaskRepo_SetRecorder_WiresRecorder proves SetRecorder is the intended
// injection seam and accepts any metrics.TaskMetricsRecorder implementation.
func TestTaskRepo_SetRecorder_WiresRecorder(t *testing.T) {
	repo := NewTaskRepo(nil, zap.NewNop())
	fake := &fakeTaskMetricsRecorder{}
	repo.SetRecorder(fake)
	if repo.recorder == nil {
		t.Fatal("expected recorder to be set after SetRecorder")
	}
	var _ metrics.TaskMetricsRecorder = fake
}

// TestTaskRepo_Transition_RecordsOnSuccessOnly is the reviewer-requested
// DB-gated positive-path test: RecordTransition's emit sits below the SQL
// layer (after RowsAffected() confirms the UPDATE matched a row), so it can
// only be proven end-to-end against a real Postgres row — the no-DB tests
// above only prove the injection seam, not that Transition actually calls
// the recorder. Accepted limitation: this test skips (via openTestPool) when
// TEST_DATABASE_URL is unset, same as TestTaskRepo_Get_NotFound.
func TestTaskRepo_Transition_RecordsOnSuccessOnly(t *testing.T) {
	pool := openTestPool(t)
	repo := NewTaskRepo(pool, zap.NewNop())
	fake := &fakeTaskMetricsRecorder{}
	repo.SetRecorder(fake)
	ctx := context.Background()

	tenantID := uuid.New()
	taskID := uuid.New()
	if err := repo.Create(ctx, &Task{
		TaskID:      taskID,
		TenantID:    tenantID,
		OwnerUserID: "alice@example.com",
		Prompt:      "test prompt",
		CostBudget:  1000,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Valid transition, matching the row's actual state — must record exactly
	// one call with the correct from/to/tenant attrs.
	if err := repo.Transition(ctx, tenantID.String(), taskID.String(), TaskStateCreated, TaskStatePlanning); err != nil {
		t.Fatalf("Transition(CREATED->PLANNING): %v", err)
	}
	if len(fake.transitions) != 1 {
		t.Fatalf("expected exactly 1 recorded transition, got %d: %+v", len(fake.transitions), fake.transitions)
	}
	got := fake.transitions[0]
	if got.from != string(TaskStateCreated) || got.to != string(TaskStatePlanning) || got.tenant != tenantID.String() {
		t.Errorf("recorded transition = %+v, want from=%s to=%s tenant=%s",
			got, TaskStateCreated, TaskStatePlanning, tenantID.String())
	}

	// Invalid transition — the row is now PLANNING, not VALIDATING, so the
	// UPDATE's WHERE clause matches zero rows (RowsAffected()==0) and
	// Transition returns an error before reaching the recorder call. Confirms
	// the recorder count is untouched by the failed attempt.
	if err := repo.Transition(ctx, tenantID.String(), taskID.String(), TaskStateValidating, TaskStateApproved); err == nil {
		t.Fatal("expected an error transitioning from a state the row is not actually in")
	}
	if len(fake.transitions) != 1 {
		t.Fatalf("failed transition must not record a metric; got %d recorded: %+v", len(fake.transitions), fake.transitions)
	}
}
