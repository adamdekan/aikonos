package db

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// AG-06: block / unblock / revoke-all round-trip through kill_switches,
// user_directory and agents, scoped to the tenant.
func TestRevocationRepo_BlockAndEpochs(t *testing.T) {
	pool := openTestPool(t)
	repo := NewRevocationRepo(pool, zap.NewNop())
	ctx := context.Background()
	tenant, other := uuid.NewString(), uuid.NewString()

	if changed, err := repo.BlockUser(ctx, tenant, "bob", "left", "admin"); err != nil || !changed {
		t.Fatalf("block: %v %v", changed, err)
	}
	if changed, err := repo.BlockUser(ctx, tenant, "bob", "again", "admin2"); err != nil || changed {
		t.Fatalf("second block must be a no-op: %v %v", changed, err)
	}
	if blocked, _, err := repo.UserState(ctx, tenant, "bob"); err != nil || !blocked {
		t.Fatalf("bob blocked: %v %v", blocked, err)
	}
	if blocked, _, _ := repo.UserState(ctx, other, "bob"); blocked {
		t.Fatal("a block must not leak to another tenant")
	}
	blocks, err := repo.UserBlocks(ctx, tenant)
	if err != nil || blocks["bob"].Reason != "left" || blocks["bob"].By != "admin" {
		t.Fatalf("blocks = %+v %v", blocks, err)
	}
	// F134: an admin block takes over a SCIM one (so SCIM reactivation cannot
	// lift it); a repeated SCIM block changes nothing.
	if _, err := repo.BlockUser(ctx, tenant, "dan", "idp", ScimBlockedBy); err != nil {
		t.Fatal(err)
	}
	if changed, _ := repo.BlockUser(ctx, tenant, "dan", "idp again", ScimBlockedBy); changed {
		t.Fatal("a repeated SCIM block must be a no-op")
	}
	if changed, err := repo.BlockUser(ctx, tenant, "dan", "incident", "admin"); err != nil || !changed {
		t.Fatalf("admin over SCIM block: %v %v", changed, err)
	}
	if blocks, _ := repo.UserBlocks(ctx, tenant); blocks["dan"].By != "admin" || blocks["dan"].Reason != "incident" {
		t.Fatalf("dan's block = %+v", blocks["dan"])
	}
	if changed, err := repo.UnblockUser(ctx, tenant, "bob", "admin"); err != nil || !changed {
		t.Fatalf("unblock: %v %v", changed, err)
	}
	if blocked, _, _ := repo.UserState(ctx, tenant, "bob"); blocked {
		t.Fatal("bob still blocked")
	}

	// Tenant block covers every subject, including the empty (service) one.
	if _, err := pool.Exec(ctx, `INSERT INTO kill_switches (tenant_id, scope, subject, activated_by) VALUES ($1, 'tenant', $2, 'ops')`, other, other); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"carol", ""} {
		if blocked, _, _ := repo.UserState(ctx, other, s); !blocked {
			t.Errorf("tenant block must cover %q", s)
		}
	}

	at := time.Now().UTC().Truncate(time.Microsecond)
	if err := repo.RevokeUserSessions(ctx, tenant, "nobody", at); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown user: want ErrNotFound, got %v", err)
	}
	dir := NewUserDirectoryRepo(pool, zap.NewNop())
	if err := dir.Upsert(ctx, tenant, "bob", "bob@example.com", "Bob"); err != nil {
		t.Fatal(err)
	}
	if err := repo.RevokeUserSessions(ctx, tenant, "bob", at); err != nil {
		t.Fatal(err)
	}
	if _, nb, _ := repo.UserState(ctx, tenant, "bob"); !nb.Equal(at) {
		t.Fatalf("user epoch = %v, want %v", nb, at)
	}

	agents := NewAgentRepo(pool, zap.NewNop())
	a := &Agent{ID: uuid.New(), TenantID: uuid.MustParse(tenant), Name: "a-" + uuid.NewString()[:8], ApprovalMode: "auto", CreatedBy: "admin"}
	if err := agents.Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	if st, nb, err := repo.AgentState(ctx, tenant, a.ID.String()); err != nil || !nb.IsZero() || st != AgentStaged {
		t.Fatalf("fresh agent state = %q %v %v", st, nb, err)
	}
	if err := repo.RevokeAgentSessions(ctx, tenant, a.ID.String(), at); err != nil {
		t.Fatal(err)
	}
	if _, nb, _ := repo.AgentState(ctx, tenant, a.ID.String()); !nb.Equal(at) {
		t.Fatalf("agent epoch = %v", nb)
	}
	if err := repo.RevokeAgentSessions(ctx, other, a.ID.String(), at); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("foreign tenant: want ErrAgentNotFound, got %v", err)
	}

	// AG-03: status moves compare-and-set, scoped to the tenant.
	if err := repo.SetAgentStatus(ctx, tenant, a.ID.String(), AgentActive, AgentSuspended, "x"); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("stale from: want ErrAgentNotFound, got %v", err)
	}
	if err := repo.SetAgentStatus(ctx, other, a.ID.String(), AgentStaged, AgentActive, ""); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("foreign tenant: want ErrAgentNotFound, got %v", err)
	}
	if err := repo.SetAgentStatus(ctx, tenant, a.ID.String(), AgentStaged, AgentActive, ""); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetAgentStatus(ctx, tenant, a.ID.String(), AgentActive, AgentSuspended, "incident 42"); err != nil {
		t.Fatal(err)
	}
	if got, _ := agents.Get(ctx, tenant, a.ID.String()); got.Status != AgentSuspended || got.StatusReason != "incident 42" {
		t.Fatalf("agent = %q %q", got.Status, got.StatusReason)
	}
	if st, _, _ := repo.AgentState(ctx, tenant, uuid.NewString()); st != "" {
		t.Fatalf("unknown agent status = %q, want empty", st)
	}
}

// AG-06 with AG-17 S4: a scheduled-run claim parks the run of a blocked owner
// ("subject_blocked") and of an actor revoked after the schedule was created
// ("actor_revoked") and of a retired actor (AG-03, "actor_retired"); a schedule
// created after the revoke-all still fires.
func TestScheduledRun_ClaimParksBlockedAndRevoked(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	repo := NewScheduledRunRepo(pool, zap.NewNop())
	rev := NewRevocationRepo(pool, zap.NewNop())
	tenant := uuid.New()
	now := time.Now().UTC()
	past := now.Add(-time.Minute)

	agents := NewAgentRepo(pool, zap.NewNop())
	agent := &Agent{ID: uuid.New(), TenantID: tenant, Name: "s-" + uuid.NewString()[:8], ApprovalMode: "auto", CreatedBy: "alice"}
	if err := agents.Create(ctx, agent); err != nil {
		t.Fatal(err)
	}
	mk := func(owner string, actor *uuid.UUID) uuid.UUID {
		id := uuid.New()
		if err := repo.Create(ctx, &ScheduledRun{
			ScheduledRunID: id, TenantID: tenant, OwnerUserID: owner, Prompt: "p",
			Kind: ScheduleKindOnce, NextFireAt: &past, ApprovedTools: []string{}, CreatedBy: owner,
			ActorAgentID: actor,
		}); err != nil {
			t.Fatalf("create: %v", err)
		}
		return id
	}
	blockedRun := mk("bob", nil)
	revokedRun := mk("alice", &agent.ID)
	retired := &Agent{ID: uuid.New(), TenantID: tenant, Name: "r-" + uuid.NewString()[:8], ApprovalMode: "auto", CreatedBy: "alice", Status: AgentRetired}
	if err := agents.Create(ctx, retired); err != nil {
		t.Fatal(err)
	}
	retiredRun := mk("alice", &retired.ID)
	if _, err := rev.BlockUser(ctx, tenant.String(), "bob", "left", "admin"); err != nil {
		t.Fatal(err)
	}
	if err := rev.RevokeAgentSessions(ctx, tenant.String(), agent.ID.String(), time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond) // the next schedule is created after the epoch
	liveRun := mk("alice", &agent.ID)

	due, err := repo.ClaimDue(ctx, tenant.String(), time.Now().UTC(), 10, false)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	got := map[uuid.UUID]string{}
	for _, r := range due {
		got[r.ScheduledRunID] = r.SkipReason
	}
	want := map[uuid.UUID]string{blockedRun: "subject_blocked", revokedRun: "actor_revoked", retiredRun: "actor_retired", liveRun: ""}
	for id, w := range want {
		if r, ok := got[id]; !ok || r != w {
			t.Errorf("run %s: reason %q (claimed %v), want %q", id, r, ok, w)
		}
	}
	for _, id := range []uuid.UUID{blockedRun, revokedRun, retiredRun} {
		r, err := repo.Get(ctx, tenant.String(), id.String())
		if err != nil {
			t.Fatal(err)
		}
		if r.State != ScheduledRunPaused || r.NextFireAt != nil || r.LastStatus != want[id] {
			t.Errorf("run %s parked as %+v", id, r)
		}
	}
}
