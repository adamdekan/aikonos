package db

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// TestScheduledRun_ClaimSkipsDeadDelegation (AG-17 S4): ClaimDue parks a
// revoked or expired delegation (PAUSED, last_status = reason) and flags it
// with SkipReason; a live one is advanced. Create defaults the expiry to 90 d.
func TestScheduledRun_ClaimSkipsDeadDelegation(t *testing.T) {
	pool := openTestPool(t)
	repo := NewScheduledRunRepo(pool, zap.NewNop())
	ctx := context.Background()
	tenant := uuid.New()
	now := time.Now().UTC()
	past := now.Add(-time.Minute)

	mk := func(expires time.Time) uuid.UUID {
		id := uuid.New()
		if err := repo.Create(ctx, &ScheduledRun{
			ScheduledRunID: id, TenantID: tenant, OwnerUserID: "alice", Prompt: "p",
			Kind: ScheduleKindOnce, NextFireAt: &past, ApprovedTools: []string{}, CreatedBy: "alice",
			DelegationExpiresAt: expires,
		}); err != nil {
			t.Fatalf("create: %v", err)
		}
		return id
	}
	live := mk(time.Time{}) // column default
	expired := mk(now.Add(-time.Hour))
	revoked := mk(now.Add(time.Hour))
	if err := repo.RevokeDelegation(ctx, tenant.String(), revoked.String(), "admin"); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	got, err := repo.Get(ctx, tenant.String(), live.String())
	if err != nil {
		t.Fatalf("get live: %v", err)
	}
	if d := got.DelegationExpiresAt.Sub(now); d < 89*24*time.Hour || d > 91*24*time.Hour {
		t.Errorf("default expiry in %v, want ~90d", d)
	}

	// Revoke parks the row outright; force it ACTIVE again to prove the claim
	// guard holds on its own.
	if err := repo.SetState(ctx, tenant.String(), revoked.String(), ScheduledRunActive, &past); err != nil {
		t.Fatalf("reactivate: %v", err)
	}

	due, err := repo.ClaimDue(ctx, tenant.String(), now, 10, false)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	reasons := map[uuid.UUID]string{}
	for _, r := range due {
		reasons[r.ScheduledRunID] = r.SkipReason
	}
	want := map[uuid.UUID]string{live: "", expired: "delegation_expired", revoked: "delegation_revoked"}
	for id, w := range want {
		if r, ok := reasons[id]; !ok || r != w {
			t.Errorf("run %s: skip reason %q (claimed %v), want %q", id, r, ok, w)
		}
	}
	for _, id := range []uuid.UUID{expired, revoked} {
		r, err := repo.Get(ctx, tenant.String(), id.String())
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if r.State != ScheduledRunPaused || r.NextFireAt != nil || r.LastStatus != want[id] || r.RunCount != 0 {
			t.Errorf("run %s parked as %+v", id, r)
		}
	}
	r, _ := repo.Get(ctx, tenant.String(), revoked.String())
	if r.DelegationRevokedAt == nil || r.DelegationRevokedBy != "admin" {
		t.Errorf("revocation not recorded: %+v", r)
	}
}
