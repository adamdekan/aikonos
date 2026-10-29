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

// TestTaskRepo_OBOColumns_RoundTrip proves Create binds and Get reads back the
// on-behalf-of columns (migration 048, AG-17) without swapping the two UUIDs.
func TestTaskRepo_OBOColumns_RoundTrip(t *testing.T) {
	pool := openTestPool(t)
	repo := NewTaskRepo(pool, zap.NewNop())
	ctx := context.Background()

	tenantID, taskID, client, grant := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	sender, kind := "bob@example.com", "workflow"
	if err := repo.Create(ctx, &Task{
		TaskID: taskID, TenantID: tenantID, OwnerUserID: "alice@example.com",
		Prompt: "p", CostBudget: 10,
		DelegatedByUserID: &sender, ExternalClientID: &client, GrantID: &grant, OBOKind: &kind,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := repo.Get(ctx, tenantID.String(), taskID.String())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	want := OnBehalfOf{Subject: "alice@example.com", DelegatedBy: sender,
		ExternalClientID: client.String(), GrantID: grant.String(), Kind: "workflow"}
	if o := got.OnBehalfOf(); o != want {
		t.Errorf("round trip: got %+v, want %+v", o, want)
	}
}

// TestTaskRepo_DelegationLimits_RoundTrip: envelope_id and delegated_scopes
// (migration 059, AG-11) survive Create/Get; an empty grant stays non-nil and
// an undelegated task reads back nil (no limit).
func TestTaskRepo_DelegationLimits_RoundTrip(t *testing.T) {
	pool := openTestPool(t)
	repo := NewTaskRepo(pool, zap.NewNop())
	ctx := context.Background()
	tenantID, envID := uuid.New(), uuid.New()

	for name, scopes := range map[string][]string{"scoped": {"siem:read", "slack:write"}, "empty": {}, "none": nil} {
		taskID := uuid.New()
		task := &Task{TaskID: taskID, TenantID: tenantID, OwnerUserID: "bob@example.com", Prompt: "p", CostBudget: 10, DelegatedScopes: scopes}
		if scopes != nil {
			task.EnvelopeID = &envID
		}
		if err := repo.Create(ctx, task); err != nil {
			t.Fatalf("%s: Create: %v", name, err)
		}
		got, err := repo.Get(ctx, tenantID.String(), taskID.String())
		if err != nil {
			t.Fatalf("%s: Get: %v", name, err)
		}
		if (got.DelegatedScopes == nil) != (scopes == nil) || len(got.DelegatedScopes) != len(scopes) {
			t.Errorf("%s: scopes = %#v, want %#v", name, got.DelegatedScopes, scopes)
		}
		if scopes != nil && got.OnBehalfOf().EnvelopeID != envID.String() {
			t.Errorf("%s: envelope_id = %q", name, got.OnBehalfOf().EnvelopeID)
		}
	}
}

// TestTaskRepo_ServiceAgent_ReadOnLoad: a task bound to a service agent reads
// back service_agent=true for the audit actor chain (AG-12); an ordinary
// agent and an unbound task read false.
func TestTaskRepo_ServiceAgent_ReadOnLoad(t *testing.T) {
	pool := openTestPool(t)
	repo := NewTaskRepo(pool, zap.NewNop())
	agents := NewAgentRepo(pool, zap.NewNop())
	ctx := context.Background()
	tenantID := uuid.New()

	for name, svc := range map[string]*bool{"service": ptrBool(true), "ordinary": ptrBool(false), "unbound": nil} {
		task := &Task{TaskID: uuid.New(), TenantID: tenantID, OwnerUserID: "alice@example.com", Prompt: "p", CostBudget: 10}
		if svc != nil {
			a := &Agent{ID: uuid.New(), TenantID: tenantID, Name: "a-" + name, ApprovalMode: "auto", ServiceAgent: *svc, Status: "active"}
			if err := agents.Create(ctx, a); err != nil {
				t.Fatalf("%s: create agent: %v", name, err)
			}
			task.AgentID = &a.ID
		}
		if err := repo.Create(ctx, task); err != nil {
			t.Fatalf("%s: Create: %v", name, err)
		}
		got, err := repo.Get(ctx, tenantID.String(), task.TaskID.String())
		if err != nil {
			t.Fatalf("%s: Get: %v", name, err)
		}
		if want := svc != nil && *svc; got.OnBehalfOf().ServiceAgent != want {
			t.Errorf("%s: service_agent = %v, want %v", name, got.OnBehalfOf().ServiceAgent, want)
		}
	}
}

func ptrBool(b bool) *bool { return &b }

// TestTaskRepo_ClaimPlanStep (H-03): a step is claimed only by its tool, its
// args hash and a non-DENY decision, and only once; a replan of the seq resets
// it and drops seqs the new plan no longer has.
func TestTaskRepo_ClaimPlanStep(t *testing.T) {
	pool := openTestPool(t)
	repo := NewTaskRepo(pool, zap.NewNop())
	ctx := context.Background()
	tenantID, taskID := uuid.New(), uuid.New()
	tenant, task := tenantID.String(), taskID.String()
	if err := repo.Create(ctx, &Task{TaskID: taskID, TenantID: tenantID, OwnerUserID: "alice@example.com", Prompt: "p", CostBudget: 10}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	step := func(seq int32, tool, hash string, d PolicyDecision) PlanStepRecord {
		return PlanStepRecord{TaskID: taskID, TenantID: tenantID, Seq: seq, ToolID: tool, ArgsHash: hash, EffectClass: "READ_ONLY", PolicyDecision: d}
	}
	if err := repo.InsertPlanSteps(ctx, tenant, []PlanStepRecord{
		step(1, "web.fetch", "h1", PolicyAllow), step(2, "web.fetch", "h2", PolicyDeny),
	}); err != nil {
		t.Fatalf("InsertPlanSteps: %v", err)
	}
	claim := func(seq int32, tool, hash string) PlanStepClaim {
		t.Helper()
		c, err := repo.ClaimPlanStep(ctx, tenant, task, seq, tool, []string{hash})
		if err != nil {
			t.Fatalf("ClaimPlanStep(%d): %v", seq, err)
		}
		return c
	}
	if c := claim(1, "web.search", "h1"); c.Claimed || c.ToolID != "web.fetch" {
		t.Errorf("other tool: %+v", c)
	}
	if c := claim(1, "web.fetch", "hX"); c.Claimed {
		t.Errorf("other args: %+v", c)
	}
	if c := claim(2, "web.fetch", "h2"); c.Claimed || c.PolicyDecision != string(PolicyDeny) {
		t.Errorf("denied step: %+v", c)
	}
	if c := claim(1, "web.fetch", "h1"); !c.Claimed {
		t.Errorf("matching step: %+v", c)
	}
	if c := claim(1, "web.fetch", "h1"); c.Claimed || !c.WasConsumed {
		t.Errorf("replay: %+v", c)
	}
	if _, err := repo.ClaimPlanStep(ctx, tenant, task, 9, "web.fetch", []string{"h1"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing step: want ErrNotFound, got %v", err)
	}
	// Replan: seq 1 is fresh again, seq 2 is gone.
	if err := repo.InsertPlanSteps(ctx, tenant, []PlanStepRecord{step(1, "web.fetch", "h1", PolicyAllow)}); err != nil {
		t.Fatalf("replan: %v", err)
	}
	if c := claim(1, "web.fetch", "h1"); !c.Claimed {
		t.Errorf("replanned step: %+v", c)
	}
	if _, err := repo.ClaimPlanStep(ctx, tenant, task, 2, "web.fetch", []string{"h2"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("dropped seq: want ErrNotFound, got %v", err)
	}
}
