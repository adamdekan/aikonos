package approvalsvc

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/adamdekan/aikonos/broker/internal/auth"
	"github.com/adamdekan/aikonos/broker/internal/db"
)

// AG-13: each claim case of the step-up rule.
func TestStepUp_ClaimCases(t *testing.T) {
	now := time.Now()
	fresh, stale := now.Add(-2*time.Minute), now.Add(-11*time.Minute)
	rule := StepUp{ACR: "mfa", MaxAge: 10 * time.Minute}
	cases := []struct {
		name string
		rule StepUp
		id   *auth.Identity
		want bool
	}{
		{"no identity (south caller)", rule, nil, false},
		{"amr mfa, fresh", rule, &auth.Identity{AMR: []string{"pwd", "mfa"}, AuthTime: fresh}, true},
		{"amr otp, fresh", rule, &auth.Identity{AMR: []string{"pwd", "otp"}, AuthTime: fresh}, true},
		{"amr hwk, fresh", rule, &auth.Identity{AMR: []string{"hwk"}, AuthTime: fresh}, true},
		{"amr pwd only", rule, &auth.Identity{AMR: []string{"pwd"}, AuthTime: fresh}, false},
		{"acr matches", rule, &auth.Identity{ACR: "mfa", AuthTime: fresh}, true},
		{"acr does not match", rule, &auth.Identity{ACR: "1", AuthTime: fresh}, false},
		{"acrs holds the auth context", StepUp{ACR: "c1", MaxAge: 10 * time.Minute}, &auth.Identity{ACRS: []string{"c2", "c1"}, AuthTime: fresh}, true},
		{"acr unset in config: acr alone never counts", StepUp{MaxAge: 10 * time.Minute}, &auth.Identity{ACR: "", ACRS: []string{""}, AuthTime: fresh}, false},
		{"mfa but stale auth_time", rule, &auth.Identity{AMR: []string{"mfa"}, ACR: "mfa", AuthTime: stale}, false},
		{"mfa but no auth_time", rule, &auth.Identity{AMR: []string{"mfa"}, ACR: "mfa"}, false},
	}
	for _, c := range cases {
		if got := c.rule.SteppedUp(c.id, now); got != c.want {
			t.Errorf("%s: SteppedUp = %v, want %v", c.name, got, c.want)
		}
	}
}

// stepUpStore serves a pending approval marked step-up (or not).
type stepUpStore struct {
	stepsStore
	stepUp bool
}

func (f *stepUpStore) GetPendingApprovalByTask(_ context.Context, _, _ string) (*db.ApprovalRequest, error) {
	return &db.ApprovalRequest{ApprovalID: uuid.New(), Summary: map[string]any{"step_up": f.stepUp}}, nil
}

// AG-13: Resolve refuses a yes on a step-up approval without a recent MFA
// sign-in, before any vote; a no and a plain approval pass.
func TestResolve_StepUp(t *testing.T) {
	mint := func(_, _ string, _ db.OnBehalfOf, _ []StepScope) (map[int32]string, error) { return nil, nil }
	rule := StepUp{MaxAge: 10 * time.Minute}
	weak := auth.WithIdentity(context.Background(), &auth.Identity{Subject: "bob", AMR: []string{"pwd"}, AuthTime: time.Now()})
	strong := auth.WithIdentity(context.Background(), &auth.Identity{Subject: "bob", AMR: []string{"pwd", "mfa"}, AuthTime: time.Now()})
	run := func(ctx context.Context, stepUp, approved bool) (*stepUpStore, error) {
		st := &stepUpStore{stepsStore: stepsStore{resolvingStore: resolvingStore{task: &db.Task{OwnerUserID: "alice"}}}, stepUp: stepUp}
		_, _, _, _, err := Resolve(ctx, st, &fakeAccessPolicy{}, mint, zap.NewNop(),
			"tenant", "task", "bob", approved, "", ArgsDigest(nil), rule)
		return st, err
	}

	if st, err := run(weak, true, true); !IsStepUpRequired(err) || st.voted {
		t.Fatalf("weak sign-in: err=%v voted=%v, want step_up_required and no vote", err, st.voted)
	}
	if st, err := run(context.Background(), true, true); !IsStepUpRequired(err) || st.voted {
		t.Fatalf("no identity: err=%v voted=%v, want step_up_required and no vote", err, st.voted)
	}
	if st, err := run(strong, true, true); err != nil || !st.voted {
		t.Fatalf("mfa sign-in: err=%v voted=%v", err, st.voted)
	}
	if st, err := run(weak, true, false); err != nil || !st.voted {
		t.Fatalf("denial: err=%v voted=%v", err, st.voted)
	}
	if st, err := run(weak, false, true); err != nil || !st.voted {
		t.Fatalf("plain approval: err=%v voted=%v", err, st.voted)
	}
}
