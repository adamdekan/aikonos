package approvalsvc

// Direct-call table test of Build's n-of-m threshold + separation-of-duty
// logic, exercised through its own narrow interfaces (Store/AccessPolicy/
// ConfigProvider/AuditEmitter) rather than through SubmitPlan — the
// broker-package approval_n_test.go cases this mirrors already prove the
// wrapper wiring is unchanged; this test proves the gate logic itself in
// isolation, which the pre-extraction inline block never had.

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/policy"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	planv1 "github.com/adamdekan/aikonos/gen/go/plan/v1"
)

// ── fakes ─────────────────────────────────────────────────────────────────────

// fakeApprovalStore implements Store fully (zero-value bodies for the
// methods Build never calls) so it satisfies the interface without relying
// on an embedded-nil-interface trick — mirrors the stubTaskStore/
// stubWorkflowStore convention used elsewhere in this codebase.
type fakeApprovalStore struct {
	created *db.ApprovalRequest
}

func (f *fakeApprovalStore) CreateApprovalRequest(_ context.Context, ar *db.ApprovalRequest) (uuid.UUID, error) {
	f.created = ar
	return uuid.New(), nil
}

func (f *fakeApprovalStore) GetPendingApprovalByTask(_ context.Context, _, _ string) (*db.ApprovalRequest, error) {
	return nil, nil
}

func (f *fakeApprovalStore) ResolveApproval(_ context.Context, _, _, _ string, _ bool, _ string) (db.ApprovalState, bool, error) {
	return "", false, nil
}

func (f *fakeApprovalStore) Transition(_ context.Context, _, _ string, _, _ db.TaskState) error {
	return nil
}

func (f *fakeApprovalStore) Get(_ context.Context, _, _ string) (*db.Task, error) {
	return nil, nil
}

func (f *fakeApprovalStore) ListMintableSteps(_ context.Context, _, _ string) ([]db.ExecutableStep, error) {
	return nil, nil
}

var _ Store = (*fakeApprovalStore)(nil)

type fakeAccessPolicy struct {
	enabled bool
	tuples  []policy.Relation
}

func (f *fakeAccessPolicy) FGAEnabled() bool { return f.enabled }
func (f *fakeAccessPolicy) ReadTuples(_ context.Context, _ policy.ReadFilter) ([]policy.Relation, error) {
	return f.tuples, nil
}
func (f *fakeAccessPolicy) CheckFGA(_ context.Context, _, _, _ string) (bool, error) {
	return true, nil
}

type fakeConfig struct {
	requiredN   int
	expiryHours int
}

func (f *fakeConfig) GetInt(_ context.Context, _, key string) int {
	switch key {
	case "approval_required_n":
		return f.requiredN
	case "approval_expiry_hours":
		return f.expiryHours
	}
	return 0
}

type recordingEmitter struct {
	events []*auditv1.AuditEvent
}

func (r *recordingEmitter) Emit(_ context.Context, event *auditv1.AuditEvent) error {
	r.events = append(r.events, event)
	return nil
}

// ── table test ────────────────────────────────────────────────────────────────

func TestBuild_ThresholdAndSeparationOfDuty(t *testing.T) {
	const owner = "alice@example.com"

	cases := []struct {
		name                  string
		outcome               planv1.ValidationOutcome
		cfgN                  int
		policy                AccessPolicy // nil ⇒ FGA disabled, owner-fallback approver set
		wantRequiresN         int16
		wantApprovers         []string // exact expected ApproverSet after SoD (order-sensitive: owner-fallback or FGA-tuple order)
		wantInsufficientAudit bool
	}{
		{
			// Test 1 mirror: config n=2, FGA disabled → approverSet falls back to
			// [owner], SoD then excludes owner → empty set, requiresN stays 2.
			name:                  "config_n2_fga_disabled_owner_excluded",
			outcome:               planv1.ValidationOutcome_NEEDS_HUMAN,
			cfgN:                  2,
			policy:                nil,
			wantRequiresN:         2,
			wantApprovers:         []string{},
			wantInsufficientAudit: true,
		},
		{
			// Test 2 mirror: config n=1 below the step-up floor of 2 → floor wins.
			// Owner-fallback set of 1 is still short of n=2, so the same
			// insufficient_approvers audit fires as case 1.
			name:                  "step_up_floor2_overrides_config_n1",
			outcome:               planv1.ValidationOutcome_NEEDS_STEP_UP,
			cfgN:                  1,
			policy:                nil,
			wantRequiresN:         2,
			wantApprovers:         []string{},
			wantInsufficientAudit: true,
		},
		{
			// Test 3 mirror: config n=3 (above floor) is honored as-is. Same
			// owner-fallback shortfall as case 1/2.
			name:                  "config_n3_applied_above_floor",
			outcome:               planv1.ValidationOutcome_NEEDS_HUMAN,
			cfgN:                  3,
			policy:                nil,
			wantRequiresN:         3,
			wantApprovers:         []string{},
			wantInsufficientAudit: true,
		},
		{
			// Test 4 mirror: default n=1 → no SoD exclusion, owner stays in set.
			name:          "default_n1_owner_in_set",
			outcome:       planv1.ValidationOutcome_NEEDS_HUMAN,
			cfgN:          1,
			policy:        nil,
			wantRequiresN: 1,
			wantApprovers: []string{owner},
		},
		{
			// Test 5 mirror: FGA approvers alice(owner)/bob/carol, n=2 → SoD
			// excludes alice, bob+carol remain.
			name:    "fga_approvers_sod_excludes_owner",
			outcome: planv1.ValidationOutcome_NEEDS_HUMAN,
			cfgN:    2,
			policy: &fakeAccessPolicy{
				enabled: true,
				tuples: []policy.Relation{
					{User: "user:" + owner, Relation: "approver", Object: "task:t1"},
					{User: "user:bob@example.com", Relation: "approver", Object: "task:t1"},
					{User: "user:carol@example.com", Relation: "approver", Object: "task:t1"},
				},
			},
			wantRequiresN: 2,
			wantApprovers: []string{"bob@example.com", "carol@example.com"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeApprovalStore{}
			emitter := &recordingEmitter{}
			cfg := &fakeConfig{requiredN: tc.cfgN, expiryHours: 24}

			err := Build(context.Background(), BuildInput{
				Store:       store,
				Policy:      tc.policy,
				Config:      cfg,
				Audit:       emitter,
				Logger:      zap.NewNop(),
				TraceID:     "trace-1",
				TenantID:    "tenant-1",
				TaskID:      "t1",
				TenantUUID:  uuid.New(),
				TaskUUID:    uuid.New(),
				OwnerUserID: owner,
				Outcome:     tc.outcome,
				PlanID:      "plan-1",
				NSteps:      1,
			})
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			if store.created == nil {
				t.Fatal("Build did not call CreateApprovalRequest")
			}
			if store.created.RequiresN != tc.wantRequiresN {
				t.Errorf("RequiresN = %d, want %d", store.created.RequiresN, tc.wantRequiresN)
			}
			if len(store.created.ApproverSet) != len(tc.wantApprovers) {
				t.Fatalf("ApproverSet = %v, want %v", store.created.ApproverSet, tc.wantApprovers)
			}
			for _, want := range tc.wantApprovers {
				found := false
				for _, got := range store.created.ApproverSet {
					if got == want {
						found = true
					}
				}
				if !found {
					t.Errorf("ApproverSet = %v, want to contain %q", store.created.ApproverSet, want)
				}
			}
			if tc.wantRequiresN >= 2 {
				for _, got := range store.created.ApproverSet {
					if got == owner {
						t.Errorf("owner %q must never appear when requiresN>=2 and SoD excludes it; got %v", owner, store.created.ApproverSet)
					}
				}
			}

			gotInsufficientAudit := false
			for _, ev := range emitter.events {
				if ev.EventType == "aikonos.broker.approval.insufficient_approvers" {
					gotInsufficientAudit = true
				}
			}
			if gotInsufficientAudit != tc.wantInsufficientAudit {
				t.Errorf("insufficient_approvers audit fired = %v, want %v", gotInsufficientAudit, tc.wantInsufficientAudit)
			}
		})
	}
}

// TestBuild_FGADisabledDoesNotDereferenceNilPolicy proves the typed-nil-safe
// contract: a genuinely nil AccessPolicy interface (as the broker package
// must pass when Deps.Policy is a nil *policy.Engine) must never be called
// into — Build must guard with `in.Policy != nil` before FGAEnabled/ReadTuples.
func TestBuild_FGADisabledDoesNotDereferenceNilPolicy(t *testing.T) {
	store := &fakeApprovalStore{}
	err := Build(context.Background(), BuildInput{
		Store:       store,
		Policy:      nil, // must not panic
		Config:      &fakeConfig{requiredN: 1, expiryHours: 24},
		Audit:       &recordingEmitter{},
		Logger:      zap.NewNop(),
		TenantID:    "tenant-1",
		TaskID:      "t1",
		TenantUUID:  uuid.New(),
		TaskUUID:    uuid.New(),
		OwnerUserID: "alice@example.com",
		Outcome:     planv1.ValidationOutcome_NEEDS_HUMAN,
		PlanID:      "plan-1",
		NSteps:      1,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if store.created == nil || store.created.RequiresN != 1 {
		t.Fatalf("expected a RequiresN=1 approval request, got %+v", store.created)
	}
}
