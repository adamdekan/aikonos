package db

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
)

// F242: the owner's own revoke-all after they granted the delegation parks the
// schedule ("subject_revoked"); extend_delegation (the owner's fresh consent)
// re-stamps delegated_at so the resumed schedule fires again.
func TestScheduledRun_ClaimParksSubjectRevoked(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	repo := NewScheduledRunRepo(pool, zap.NewNop())
	rev := NewRevocationRepo(pool, zap.NewNop())
	tenant := uuid.New()
	past := time.Now().UTC().Add(-time.Minute)

	if err := NewUserDirectoryRepo(pool, zap.NewNop()).Upsert(ctx, tenant.String(), "alice", "alice@example.com", "Alice"); err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	if err := repo.Create(ctx, &ScheduledRun{
		ScheduledRunID: id, TenantID: tenant, OwnerUserID: "alice", Prompt: "p",
		Kind: ScheduleKindOnce, NextFireAt: &past, ApprovedTools: []string{}, CreatedBy: "alice",
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := rev.RevokeUserSessions(ctx, tenant.String(), "alice", time.Now().UTC().Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	due, err := repo.ClaimDue(ctx, tenant.String(), time.Now().UTC(), 10, false)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(due) != 1 || due[0].SkipReason != "subject_revoked" {
		t.Fatalf("want the run parked subject_revoked, got %+v", due)
	}

	// Recovery: after the epoch the owner extends (re-consents) and resumes.
	time.Sleep(1100 * time.Millisecond)
	if err := repo.ExtendDelegation(ctx, tenant.String(), id.String(), time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("extend: %v", err)
	}
	if err := repo.SetState(ctx, tenant.String(), id.String(), ScheduledRunActive, &past); err != nil {
		t.Fatalf("resume: %v", err)
	}
	due, err = repo.ClaimDue(ctx, tenant.String(), time.Now().UTC(), 10, false)
	if err != nil {
		t.Fatalf("claim after extend: %v", err)
	}
	if len(due) != 1 || due[0].SkipReason != "" {
		t.Fatalf("want the re-consented run to fire, got %+v", due)
	}
}

// F18: a workflow schedule fires only while its pinned version is approved and
// unchanged; a rewritten definition parks it as "workflow_changed". The Go hash
// of a definition read back matches migration 062's SQL backfill hash.
func TestScheduledRun_ClaimParksChangedWorkflow(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	repo := NewScheduledRunRepo(pool, zap.NewNop())
	wf := NewWorkflowRepo(pool, zap.NewNop())
	tenant := uuid.New()
	past := time.Now().UTC().Add(-time.Minute)

	row := baseRow(tenant, uuid.New(), "alice")
	row.Definition = json.RawMessage(`{ "steps" : [ ] ,"metadata":{"name":"x"}}`) // non-canonical spacing
	created, err := wf.CreateVersion(ctx, tenant.String(), row)
	if err != nil {
		t.Fatalf("create workflow: %v", err)
	}
	got, err := wf.GetVersion(ctx, tenant.String(), created.LineageID, created.Version)
	if err != nil {
		t.Fatal(err)
	}
	sum := WorkflowDefinitionSHA256(got.Definition)
	var sqlSum string
	if err := withConnErr(ctx, pool, tenant.String(), func(conn *pgxpool.Conn) error {
		return conn.QueryRow(ctx, `SELECT encode(sha256(convert_to(definition::text, 'UTF8')), 'hex') FROM workflows WHERE id = $1`, created.ID).Scan(&sqlSum)
	}); err != nil {
		t.Fatal(err)
	}
	if sqlSum != sum {
		t.Fatalf("Go hash %s != migration 062 SQL hash %s", sum, sqlSum)
	}

	mk := func(sha string) uuid.UUID {
		id, v := uuid.New(), created.Version
		if err := repo.Create(ctx, &ScheduledRun{
			ScheduledRunID: id, TenantID: tenant, OwnerUserID: "alice",
			Kind: ScheduleKindOnce, NextFireAt: &past, ApprovedTools: []string{}, CreatedBy: "alice",
			WorkflowLineageID: &created.LineageID, WorkflowVersion: &v, WorkflowDefinitionSHA256: sha,
		}); err != nil {
			t.Fatalf("create run: %v", err)
		}
		return id
	}
	same := mk(sum)
	changed := mk(WorkflowDefinitionSHA256([]byte(`{"steps":[]}`)))
	unpinned := mk("")

	due, err := repo.ClaimDue(ctx, tenant.String(), time.Now().UTC(), 10, false)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	reasons := map[uuid.UUID]string{}
	for _, r := range due {
		reasons[r.ScheduledRunID] = r.SkipReason
	}
	want := map[uuid.UUID]string{same: "", changed: "workflow_changed", unpinned: "workflow_changed"}
	for id, w := range want {
		if r, ok := reasons[id]; !ok || r != w {
			t.Errorf("run %s: reason %q (claimed %v), want %q", id, r, ok, w)
		}
	}
}

// F172: only a run ClaimDue fired can be reported, once per claim.
func TestScheduledRun_ReportRequiresClaim(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	repo := NewScheduledRunRepo(pool, zap.NewNop())
	tenant := uuid.New()
	past := time.Now().UTC().Add(-time.Minute)
	future := time.Now().UTC().Add(time.Hour)

	mk := func(next *time.Time) uuid.UUID {
		id := uuid.New()
		if err := repo.Create(ctx, &ScheduledRun{
			ScheduledRunID: id, TenantID: tenant, OwnerUserID: "alice", Prompt: "p",
			Kind: ScheduleKindOnce, NextFireAt: next, ApprovedTools: []string{}, CreatedBy: "alice",
		}); err != nil {
			t.Fatalf("create: %v", err)
		}
		return id
	}
	pending := mk(&future)
	due := mk(&past)

	if err := repo.ReportResult(ctx, tenant.String(), pending.String(), true, "forged"); !errors.Is(err, ErrScheduledRunNotClaimed) {
		t.Fatalf("report of an unclaimed run: want ErrScheduledRunNotClaimed, got %v", err)
	}
	if r, _ := repo.Get(ctx, tenant.String(), pending.String()); r.State != ScheduledRunActive || r.NextFireAt == nil {
		t.Fatalf("an unclaimed one-off must stay pending, got %+v", r)
	}

	if _, err := repo.ClaimDue(ctx, tenant.String(), time.Now().UTC(), 10, false); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := repo.ReportResult(ctx, tenant.String(), due.String(), true, "ok"); err != nil {
		t.Fatalf("report of a claimed run: %v", err)
	}
	if err := repo.ReportResult(ctx, tenant.String(), due.String(), true, "again"); !errors.Is(err, ErrScheduledRunNotClaimed) {
		t.Fatalf("second report of one claim: want ErrScheduledRunNotClaimed, got %v", err)
	}
}
