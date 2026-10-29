package broker

import (
	"context"
	"fmt"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/adamdekan/aikonos/broker/internal/auth"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/gatewaygrant"
	"github.com/adamdekan/aikonos/broker/internal/notify"
	"github.com/adamdekan/aikonos/broker/internal/toolregistry"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

func TestValidateScheduleDef(t *testing.T) {
	now := time.Date(2026, 6, 3, 8, 0, 0, 0, time.UTC)
	future := timestamppb.New(now.Add(time.Hour))
	past := timestamppb.New(now.Add(-time.Hour))

	t.Run("cron ok", func(t *testing.T) {
		kind, expr, next, err := validateScheduleDef(toolregistry.PkgDefault(), "do it", brokerv1.ScheduleKind_SCHEDULE_KIND_CRON, "0 9 * * *", nil, nil, now, 0)
		if err != nil {
			t.Fatal(err)
		}
		if kind != "CRON" || expr != "0 9 * * *" || !next.Equal(time.Date(2026, 6, 3, 9, 0, 0, 0, time.UTC)) {
			t.Fatalf("got kind=%s expr=%s next=%v", kind, expr, next)
		}
	})

	t.Run("once ok", func(t *testing.T) {
		kind, _, next, err := validateScheduleDef(toolregistry.PkgDefault(), "do it", brokerv1.ScheduleKind_SCHEDULE_KIND_ONCE, "", future, nil, now, 0)
		if err != nil {
			t.Fatal(err)
		}
		if kind != "ONCE" || !next.Equal(future.AsTime()) {
			t.Fatalf("got kind=%s next=%v", kind, next)
		}
	})

	bad := []struct {
		name   string
		prompt string
		kind   brokerv1.ScheduleKind
		cron   string
		runAt  *timestamppb.Timestamp
		tools  []string
	}{
		{"empty prompt", "  ", brokerv1.ScheduleKind_SCHEDULE_KIND_CRON, "0 9 * * *", nil, nil},
		{"bad cron", "x", brokerv1.ScheduleKind_SCHEDULE_KIND_CRON, "not a cron", nil, nil},
		{"once past", "x", brokerv1.ScheduleKind_SCHEDULE_KIND_ONCE, "", past, nil},
		{"once missing time", "x", brokerv1.ScheduleKind_SCHEDULE_KIND_ONCE, "", nil, nil},
		{"unknown tool", "x", brokerv1.ScheduleKind_SCHEDULE_KIND_CRON, "0 9 * * *", nil, []string{"bogus.tool"}},
		{"unspecified kind", "x", brokerv1.ScheduleKind_SCHEDULE_KIND_UNSPECIFIED, "", nil, nil},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			_, _, _, err := validateScheduleDef(toolregistry.PkgDefault(), c.prompt, c.kind, c.cron, c.runAt, c.tools, now, 0)
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("want InvalidArgument, got %v", err)
			}
		})
	}

	t.Run("known tool accepted", func(t *testing.T) {
		_, _, _, err := validateScheduleDef(toolregistry.PkgDefault(), "x", brokerv1.ScheduleKind_SCHEDULE_KIND_CRON, "0 9 * * *", nil, []string{"web.fetch"}, now, 0)
		if err != nil {
			t.Fatalf("web.fetch should be accepted: %v", err)
		}
	})
}

func gatewayCtx(spiffeID string) context.Context {
	return auth.WithIdentity(context.Background(), &auth.Identity{SpiffeID: spiffeID})
}

func TestRequireGatewayPeer(t *testing.T) {
	const gw = "spiffe://aikonos.com/agent-gateway"
	svc := NewSandboxService(Deps{GatewaySpiffeID: gw})

	t.Run("no identity denied", func(t *testing.T) {
		if status.Code(svc.requireGatewayPeer(context.Background())) != codes.PermissionDenied {
			t.Fatal("missing identity should be denied")
		}
	})
	t.Run("empty spiffe denied", func(t *testing.T) {
		if status.Code(svc.requireGatewayPeer(gatewayCtx(""))) != codes.PermissionDenied {
			t.Fatal("empty spiffe should be denied")
		}
	})
	t.Run("wrong spiffe denied", func(t *testing.T) {
		if status.Code(svc.requireGatewayPeer(gatewayCtx("spiffe://aikonos.com/some-sandbox"))) != codes.PermissionDenied {
			t.Fatal("non-gateway peer should be denied")
		}
	})
	t.Run("gateway allowed", func(t *testing.T) {
		if err := svc.requireGatewayPeer(gatewayCtx(gw)); err != nil {
			t.Fatalf("gateway peer should pass: %v", err)
		}
	})
	t.Run("dev: any peer when unset", func(t *testing.T) {
		dev := NewSandboxService(Deps{GatewaySpiffeID: "", DevMode: true})
		if err := dev.requireGatewayPeer(gatewayCtx("spiffe://aikonos.com/whatever")); err != nil {
			t.Fatalf("unset gateway id should accept any trust-domain peer: %v", err)
		}
	})
	t.Run("non-dev: unset gateway id denies every peer", func(t *testing.T) {
		prod := NewSandboxService(Deps{GatewaySpiffeID: ""})
		if status.Code(prod.requireGatewayPeer(gatewayCtx(gw))) != codes.PermissionDenied {
			t.Fatal("an unset gateway id outside dev mode must fail closed")
		}
	})
}

// CreateScheduledRun must deny a caller lacking the scheduler skill before it
// ever touches the store (Scheduled is nil here, so a leak would panic).
func TestCreateScheduledRun_SkillGateDenied(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}} // bob not allowed
	srv := f.server(t)
	defer srv.Close()
	svc := NewBrokerService(testAdminDeps(t, srv.URL))

	ctx := ctxWithIdentity(testTenantUUID, "bob@example.com")
	_, err := svc.CreateScheduledRun(ctx, &brokerv1.CreateScheduledRunRequest{
		UserId:   "bob@example.com",
		Prompt:   "summarize my drive",
		Kind:     brokerv1.ScheduleKind_SCHEDULE_KIND_CRON,
		CronExpr: "0 9 * * *",
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-scheduler should be PermissionDenied, got %v", err)
	}
}

func TestCreateScheduledRun_RejectsUserIdSpoofing(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:alice@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	svc := NewBrokerService(testAdminDeps(t, srv.URL))

	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")
	_, err := svc.CreateScheduledRun(ctx, &brokerv1.CreateScheduledRunRequest{
		UserId:   "bob@example.com", // claiming to be someone else
		Prompt:   "x",
		Kind:     brokerv1.ScheduleKind_SCHEDULE_KIND_CRON,
		CronExpr: "0 9 * * *",
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("spoofed user_id should be PermissionDenied, got %v", err)
	}
}

// Listing another user's runs is admin-gated; a non-admin filtering by owner is
// rejected before the store is read.
func TestListScheduledRuns_OwnerFilterRequiresAdmin(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	svc := NewBrokerService(testAdminDeps(t, srv.URL))

	ctx := ctxWithIdentity(testTenantUUID, "bob@example.com")
	_, err := svc.ListScheduledRuns(ctx, &brokerv1.ListScheduledRunsRequest{
		UserId:      "bob@example.com",
		OwnerFilter: "alice@example.com",
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-admin owner_filter should be PermissionDenied, got %v", err)
	}
}

// ── CP2: ClaimDueScheduledRuns mints an owner grant per run ──────────────────

// fakeClaimDueStore returns a canned list of ScheduledRuns for ClaimDue tests.
// Satisfies scheduledRunStore (ClaimDue only; the rest via the embedded stub).
type fakeClaimDueStore struct {
	stubScheduledStore
	runs []*db.ScheduledRun
}

func (f *fakeClaimDueStore) ClaimDue(_ context.Context, _ string, _ time.Time, _ int, requireActor bool) ([]*db.ScheduledRun, error) {
	for _, r := range f.runs {
		if requireActor && r.ActorAgentID == nil && r.SkipReason == "" {
			r.SkipReason = "actor_missing"
		}
	}
	return f.runs, nil
}

// parkClaimStore records the clock ClaimDue was given and every Park.
type parkClaimStore struct {
	fakeClaimDueStore
	now    time.Time
	parked map[string]string // run id → reason
}

func (f *parkClaimStore) ClaimDue(ctx context.Context, tenant string, now time.Time, limit int, requireActor bool) ([]*db.ScheduledRun, error) {
	f.now = now
	return f.fakeClaimDueStore.ClaimDue(ctx, tenant, now, limit, requireActor)
}

func (f *parkClaimStore) Park(_ context.Context, _, id, reason string) error {
	if f.parked == nil {
		f.parked = map[string]string{}
	}
	f.parked[id] = reason
	return nil
}

// TestClaimDueScheduledRuns_IgnoresGatewayClock (F12): due-time and delegation
// expiry are judged on the broker's clock; a backdated req.Now cannot keep an
// expired delegation alive.
func TestClaimDueScheduledRuns_IgnoresGatewayClock(t *testing.T) {
	store := &parkClaimStore{}
	svc := NewSandboxService(Deps{
		Logger: zap.NewNop(), Audit: &fakeInvokeAudit{}, GatewaySpiffeID: testGWSpiffeID,
		GatewayGrantKey: testGrantKey, GatewayGrantTTL: testGrantTTL, Scheduled: store,
	})
	before := time.Now().UTC()
	if _, err := svc.ClaimDueScheduledRuns(
		auth.WithIdentity(context.Background(), &auth.Identity{SpiffeID: testGWSpiffeID}),
		&brokerv1.ClaimDueScheduledRunsRequest{TenantId: testTenantUUID, Now: timestamppb.New(before.Add(-365 * 24 * time.Hour))},
	); err != nil {
		t.Fatalf("ClaimDueScheduledRuns: %v", err)
	}
	if store.now.Before(before) {
		t.Fatalf("ClaimDue got now=%v, want the broker clock (>= %v)", store.now, before)
	}
}

// TestClaimDueScheduledRuns_OwnerLostSchedulerSkill (F192): a due run whose
// owner no longer holds skill:scheduler is parked and audited, not fired;
// another owner's run in the same batch still fires.
func TestClaimDueScheduledRuns_OwnerLostSchedulerSkill(t *testing.T) {
	f := &fakeFGA{checks: map[string]bool{"user:bob@example.com|can_invoke|skill:scheduler": true}}
	srv := f.server(t)
	defer srv.Close()
	aliceRun, bobRun, actor := uuid.New(), uuid.New(), uuid.New()
	store := &parkClaimStore{fakeClaimDueStore: fakeClaimDueStore{runs: []*db.ScheduledRun{
		{ScheduledRunID: aliceRun, OwnerUserID: "alice@example.com", Prompt: "p", ActorAgentID: &actor},
		{ScheduledRunID: bobRun, OwnerUserID: "bob@example.com", Prompt: "p", ActorAgentID: &actor},
	}}}
	deps := testAdminDeps(t, srv.URL)
	aud := &fakeInvokeAudit{}
	deps.Audit, deps.Scheduled = aud, store
	deps.GatewaySpiffeID, deps.GatewayGrantKey, deps.GatewayGrantTTL = testGWSpiffeID, testGrantKey, testGrantTTL
	f.checks["user:bob@example.com|can_use|agent:"+actor.String()] = true
	svc := NewSandboxService(deps)

	resp, err := svc.ClaimDueScheduledRuns(
		auth.WithIdentity(context.Background(), &auth.Identity{SpiffeID: testGWSpiffeID}),
		&brokerv1.ClaimDueScheduledRunsRequest{TenantId: testTenantUUID},
	)
	if err != nil {
		t.Fatalf("ClaimDueScheduledRuns: %v", err)
	}
	if len(resp.Runs) != 1 || resp.Runs[0].Id != bobRun.String() {
		t.Fatalf("want only bob's run fired, got %v", resp.Runs)
	}
	if store.parked[aliceRun.String()] != skipSkillRevoked {
		t.Errorf("alice's run must be parked %q, got %v", skipSkillRevoked, store.parked)
	}
	found := false
	for _, e := range aud.events {
		if e.EventType == "aikonos.scheduler.run.skipped."+skipSkillRevoked && e.Decision == auditv1.PolicyDecision_DENY {
			found = true
		}
	}
	if !found {
		t.Error("the skill_scheduler_revoked skip must be audited as DENY")
	}
}

// TestClaimDueScheduledRuns_GrantVerifiesToRunOwner proves that
// ClaimDueScheduledRuns attaches a broker-issued owner-grant to each DueRun and
// that Verify→the run's stored owner.  This is the transport-level proof that
// the scheduler path satisfies the CP2 mint contract.
func TestClaimDueScheduledRuns_GrantVerifiesToRunOwner(t *testing.T) {
	runID, actorID := uuid.New(), uuid.New()
	claimStore := &fakeClaimDueStore{
		runs: []*db.ScheduledRun{
			{
				ScheduledRunID: runID,
				OwnerUserID:    "alice@example.com",
				Prompt:         "summarize",
				ApprovedTools:  []string{"doc.read"},
				ActorAgentID:   &actorID,
			},
		},
	}

	aud := &fakeInvokeAudit{}
	deps := Deps{
		Logger:          zap.NewNop(),
		Audit:           aud,
		GatewaySpiffeID: testGWSpiffeID,
		GatewayGrantKey: testGrantKey,
		GatewayGrantTTL: testGrantTTL,
		Scheduled:       claimStore,
	}
	svc := NewSandboxService(deps)

	resp, err := svc.ClaimDueScheduledRuns(
		auth.WithIdentity(context.Background(), &auth.Identity{SpiffeID: testGWSpiffeID}),
		&brokerv1.ClaimDueScheduledRunsRequest{TenantId: testTenantUUID},
	)
	if err != nil {
		t.Fatalf("ClaimDueScheduledRuns: %v", err)
	}
	if len(resp.Runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(resp.Runs))
	}
	// run.fired carries the chain: subject = owner, grant_id = schedule id (AG-17).
	if len(aud.events) != 1 {
		t.Fatalf("expected 1 run.fired event, got %d", len(aud.events))
	}
	obo := aud.events[0].Context.Fields["obo"].GetStructValue().GetFields()
	if obo["subject"].GetStringValue() != "alice@example.com" || obo["grant_id"].GetStringValue() != runID.String() {
		t.Errorf("run.fired obo = %v, want subject=alice@example.com grant_id=%s", obo, runID)
	}
	run := resp.Runs[0]
	if run.OwnerGrant == "" {
		t.Fatal("OwnerGrant must be non-empty")
	}
	g, err := gatewaygrant.Verify(testGrantKey, run.OwnerGrant, false)
	if err != nil {
		t.Fatalf("Verify owner grant: %v", err)
	}
	if g.TenantID != testTenantUUID {
		t.Errorf("grant tenant = %q, want %q", g.TenantID, testTenantUUID)
	}
	if g.Owner() != "alice@example.com" || g.Kind != gatewaygrant.KindScheduled || g.GrantID != runID.String() || g.Actor != actorID.String() {
		t.Errorf("grant = %+v, want owner alice@example.com, kind scheduled, grant_id %s, actor %s", g, runID, actorID)
	}
	if run.ActorAgentId != actorID.String() || aud.events[0].ActorAgentId != actorID.String() {
		t.Errorf("DueRun actor = %q, run.fired actor = %q, want %s", run.ActorAgentId, aud.events[0].ActorAgentId, actorID)
	}
	if obo["kind"].GetStringValue() != "scheduled" {
		t.Errorf("run.fired obo kind = %q, want scheduled", obo["kind"].GetStringValue())
	}
}

// TestClaimDue_SkipsExpiredRevoked: a run ClaimDue parked for a dead delegation
// (AG-17 S4) is never returned to the gateway and leaves a DENY skip event.
// ponytail: blocked-subject (AG-06) and suspended-actor (AG-03) cases join
// this table when those checks exist.
func TestClaimDue_SkipsExpiredRevoked(t *testing.T) {
	live := uuid.New()
	claimStore := &fakeClaimDueStore{runs: []*db.ScheduledRun{
		{ScheduledRunID: uuid.New(), OwnerUserID: "alice@example.com", SkipReason: "delegation_expired"},
		{ScheduledRunID: uuid.New(), OwnerUserID: "alice@example.com", SkipReason: "delegation_revoked"},
		{ScheduledRunID: live, OwnerUserID: "alice@example.com"},
	}}
	aud := &fakeInvokeAudit{}
	svc := NewSandboxService(Deps{
		Logger: zap.NewNop(), Audit: aud, GatewaySpiffeID: testGWSpiffeID,
		GatewayGrantKey: testGrantKey, GatewayGrantTTL: testGrantTTL, Scheduled: claimStore,
	})
	resp, err := svc.ClaimDueScheduledRuns(
		auth.WithIdentity(context.Background(), &auth.Identity{SpiffeID: testGWSpiffeID}),
		&brokerv1.ClaimDueScheduledRunsRequest{TenantId: testTenantUUID},
	)
	if err != nil {
		t.Fatalf("ClaimDueScheduledRuns: %v", err)
	}
	if len(resp.Runs) != 1 || resp.Runs[0].Id != live.String() {
		t.Fatalf("runs = %v, want only %s", resp.Runs, live)
	}
	want := []string{"aikonos.scheduler.run.skipped.delegation_expired", "aikonos.scheduler.run.skipped.delegation_revoked", "aikonos.scheduler.run.fired"}
	if len(aud.events) != len(want) {
		t.Fatalf("got %d audit events, want %d", len(aud.events), len(want))
	}
	for i, w := range want {
		if aud.events[i].EventType != w {
			t.Errorf("event %d = %q, want %q", i, aud.events[i].EventType, w)
		}
	}
	if aud.events[0].Decision != auditv1.PolicyDecision_DENY {
		t.Errorf("skip decision = %v, want DENY", aud.events[0].Decision)
	}
}

// TestClaimDue_ActorMissingSkippedInEnforce: identity.obo=enforce parks an
// actorless run instead of handing it to the gateway, and refuses to create
// one (AG-17 S8); audit mode still fires it.
func TestClaimDue_ActorMissingSkippedInEnforce(t *testing.T) {
	runID := uuid.New()
	for mode, want := range map[string]string{
		"audit":   "aikonos.scheduler.run.fired",
		"enforce": "aikonos.scheduler.run.skipped.actor_missing",
	} {
		cfg := newFakeConfigStore()
		cfg.values[testTenantUUID+":identity.obo"] = mode
		aud := &fakeInvokeAudit{}
		svc := NewSandboxService(Deps{
			Logger: zap.NewNop(), Audit: aud, Config: cfg, GatewaySpiffeID: testGWSpiffeID,
			GatewayGrantKey: testGrantKey, GatewayGrantTTL: testGrantTTL,
			Scheduled: &fakeClaimDueStore{runs: []*db.ScheduledRun{{ScheduledRunID: runID, OwnerUserID: "alice@example.com"}}},
		})
		resp, err := svc.ClaimDueScheduledRuns(
			auth.WithIdentity(context.Background(), &auth.Identity{SpiffeID: testGWSpiffeID}),
			&brokerv1.ClaimDueScheduledRunsRequest{TenantId: testTenantUUID},
		)
		if err != nil {
			t.Fatalf("%s: ClaimDueScheduledRuns: %v", mode, err)
		}
		if fired := len(resp.Runs) == 1; fired != (mode == "audit") {
			t.Errorf("%s: runs = %v", mode, resp.Runs)
		}
		if len(aud.events) != 1 || aud.events[0].EventType != want {
			t.Errorf("%s: events = %v, want one %s", mode, aud.events, want)
		}
		// Create refuses an actorless schedule in enforce rather than parking it later.
		_, err = (&BrokerService{deps: Deps{Config: cfg}}).scheduleActor(context.Background(), testTenantUUID, "alice@example.com", "")
		if (status.Code(err) == codes.FailedPrecondition) != (mode == "enforce") {
			t.Errorf("%s: scheduleActor(\"\") err = %v", mode, err)
		}
	}
}

// TestUpdateScheduledRun_RevokeThenResumeRefused: revoke goes through the
// store; resuming a revoked or expired delegation is FailedPrecondition.
func TestUpdateScheduledRun_RevokeThenResumeRefused(t *testing.T) {
	deps := testAdminDeps(t, "")
	runID := uuid.New()
	revokedAt := time.Now()
	for name, row := range map[string]*db.ScheduledRun{
		"revoked": {ScheduledRunID: runID, OwnerUserID: "alice@example.com", Kind: db.ScheduleKindCron, CronExpr: "0 9 * * *",
			DelegationExpiresAt: time.Now().Add(time.Hour), DelegationRevokedAt: &revokedAt},
		"expired": {ScheduledRunID: runID, OwnerUserID: "alice@example.com", Kind: db.ScheduleKindCron, CronExpr: "0 9 * * *",
			DelegationExpiresAt: time.Now().Add(-time.Hour)},
	} {
		deps.Scheduled = &revokeScheduledStore{captureScheduledStore: captureScheduledStore{get: row}}
		svc := NewBrokerService(deps)
		ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")
		_, err := svc.UpdateScheduledRun(ctx, &brokerv1.UpdateScheduledRunRequest{
			UserId: "alice@example.com", Id: runID.String(), DesiredState: brokerv1.ScheduledRunState_SCHEDULED_RUN_ACTIVE,
		})
		if status.Code(err) != codes.FailedPrecondition {
			t.Errorf("%s: resume = %v, want FailedPrecondition", name, err)
		}
	}

	store := &revokeScheduledStore{captureScheduledStore: captureScheduledStore{get: &db.ScheduledRun{ScheduledRunID: runID, OwnerUserID: "alice@example.com"}}}
	deps.Scheduled = store
	if _, err := NewBrokerService(deps).UpdateScheduledRun(ctxWithIdentity(testTenantUUID, "alice@example.com"),
		&brokerv1.UpdateScheduledRunRequest{UserId: "alice@example.com", Id: runID.String(), RevokeDelegation: true}); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if store.revokedBy != "alice@example.com" {
		t.Errorf("RevokeDelegation by = %q, want alice@example.com", store.revokedBy)
	}
}

type revokeScheduledStore struct {
	captureScheduledStore
	revokedBy string
}

func (f *revokeScheduledStore) RevokeDelegation(_ context.Context, _, _, by string) error {
	f.revokedBy = by
	return nil
}

// stateScheduledStore records SetState on top of captureScheduledStore.
type stateScheduledStore struct {
	captureScheduledStore
	state db.ScheduledRunState
}

func (f *stateScheduledStore) SetState(_ context.Context, _, _ string, s db.ScheduledRunState, _ *time.Time) error {
	f.state = s
	return nil
}

// TestUpdateScheduledRun_Actor: a full edit may re-point the schedule's actor,
// checked as at create (agent in tenant + can_use) and denied + audited the
// same way; an absent actor keeps the row's; setting one on a row parked
// actor_missing (identity.obo=enforce) lets resume reactivate it.
func TestUpdateScheduledRun_Actor(t *testing.T) {
	agent, other := uuid.New(), uuid.New()
	f := &fakeFGA{checks: map[string]bool{
		"user:alice@example.com|can_use|agent:" + agent.String(): true,
		"user:alice@example.com|can_invoke|skill:scheduler":      true,
	}}
	srv := f.server(t)
	defer srv.Close()
	cfg := newFakeConfigStore()
	cfg.values[testTenantUUID+":identity.obo"] = "enforce"

	setup := func(t *testing.T) (*BrokerService, *stateScheduledStore, notify.Subscription) {
		deps, sub := testAdminDepsWithConfigAndBus(t, srv.URL, cfg)
		tid := uuid.MustParse(testTenantUUID)
		deps.Agents = &fakeAgentStore{agents: []*db.Agent{
			{ID: agent, TenantID: tid, Name: "a"}, {ID: other, TenantID: tid, Name: "b"},
		}}
		store := &stateScheduledStore{captureScheduledStore: captureScheduledStore{get: &db.ScheduledRun{
			ScheduledRunID: uuid.New(), OwnerUserID: "alice@example.com", Prompt: "p",
			Kind: db.ScheduleKindCron, CronExpr: "0 9 * * *", State: db.ScheduledRunPaused,
			LastStatus: "actor_missing", DelegationExpiresAt: time.Now().Add(time.Hour),
		}}}
		deps.Scheduled = store
		return NewBrokerService(deps), store, sub
	}
	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")
	edit := func(svc *BrokerService, id string, actor *string) error {
		_, err := svc.UpdateScheduledRun(ctx, &brokerv1.UpdateScheduledRunRequest{
			UserId: "alice@example.com", Id: id, Prompt: "p",
			Kind: brokerv1.ScheduleKind_SCHEDULE_KIND_CRON, CronExpr: "0 9 * * *", ActorAgentId: actor,
		})
		return err
	}
	ptr := func(s string) *string { return &s }

	t.Run("permitted then resume reactivates actor_missing", func(t *testing.T) {
		svc, store, _ := setup(t)
		id := store.get.ScheduledRunID.String()
		if err := edit(svc, id, ptr(agent.String())); err != nil {
			t.Fatalf("edit: %v", err)
		}
		if a := store.updated.ActorAgentID; a == nil || *a != agent {
			t.Fatalf("updated actor = %v, want %s", a, agent)
		}
		store.get.ActorAgentID = store.updated.ActorAgentID // persisted
		if _, err := svc.UpdateScheduledRun(ctx, &brokerv1.UpdateScheduledRunRequest{
			UserId: "alice@example.com", Id: id, DesiredState: brokerv1.ScheduledRunState_SCHEDULED_RUN_ACTIVE,
		}); err != nil {
			t.Fatalf("resume: %v", err)
		}
		if store.state != db.ScheduledRunActive {
			t.Errorf("state after resume = %q, want ACTIVE", store.state)
		}
	})

	t.Run("absent keeps actor", func(t *testing.T) {
		svc, store, _ := setup(t)
		store.get.ActorAgentID = &agent
		if err := edit(svc, store.get.ScheduledRunID.String(), nil); err != nil {
			t.Fatalf("edit: %v", err)
		}
		if a := store.updated.ActorAgentID; a == nil || *a != agent {
			t.Errorf("updated actor = %v, want kept %s", a, agent)
		}
	})

	for name, c := range map[string]struct {
		actor string
		code  codes.Code
	}{
		"denied (no can_use)": {other.String(), codes.PermissionDenied},
		"not in tenant":       {uuid.NewString(), codes.NotFound},
		"invalid":             {"alice-agent", codes.InvalidArgument},
		"cleared in enforce":  {"", codes.FailedPrecondition},
	} {
		t.Run(name, func(t *testing.T) {
			svc, store, sub := setup(t)
			err := edit(svc, store.get.ScheduledRunID.String(), &c.actor)
			if status.Code(err) != c.code {
				t.Fatalf("err = %v, want %v", err, c.code)
			}
			if store.updated != nil {
				t.Error("a refused actor must not reach the store")
			}
			if ev := receiveAuditEvent(t, sub, "aikonos.scheduler.run.actor_denied", 2*time.Second); ev.Decision != auditv1.PolicyDecision_DENY {
				t.Errorf("decision = %v, want DENY", ev.Decision)
			}
		})
	}
}

// run.created and run.completed carry the same owner/actor/grant chain as
// run.fired (AG-17), so a schedule's whole lifecycle traces to its agent.
func TestSchedulerLifecycleEvents_CarryOBO(t *testing.T) {
	actorID := uuid.New()
	checkOBO := func(t *testing.T, ev *auditv1.AuditEvent, runID string) {
		t.Helper()
		obo := ev.Context.Fields["obo"].GetStructValue().GetFields()
		if ev.ActorUserId != "alice@example.com" || ev.ActorAgentId != actorID.String() ||
			obo["subject"].GetStringValue() != "alice@example.com" || obo["grant_id"].GetStringValue() != runID {
			t.Errorf("%s: actor_user=%q actor_agent=%q obo=%v, want alice@example.com / %s / grant %s",
				ev.EventType, ev.ActorUserId, ev.ActorAgentId, obo, actorID, runID)
		}
	}

	t.Run("created", func(t *testing.T) {
		deps := testAdminDeps(t, "") // FGA disabled: skill and can_use allow-all
		deps.Agents = &fakeAgentStore{agents: []*db.Agent{{ID: actorID, TenantID: uuid.MustParse(testTenantUUID), Name: "a"}}}
		store := &captureScheduledStore{}
		deps.Scheduled = store
		aud := &fakeInvokeAudit{}
		deps.Audit = aud
		_, err := NewBrokerService(deps).CreateScheduledRun(ctxWithIdentity(testTenantUUID, "alice@example.com"), &brokerv1.CreateScheduledRunRequest{
			UserId: "alice@example.com", Prompt: "summarize", Kind: brokerv1.ScheduleKind_SCHEDULE_KIND_CRON,
			CronExpr: "0 9 * * *", ActorAgentId: actorID.String(),
		})
		if err != nil {
			t.Fatalf("CreateScheduledRun: %v", err)
		}
		for _, ev := range aud.events {
			if ev.EventType == "aikonos.scheduler.run.created" {
				checkOBO(t, ev, store.created.ScheduledRunID.String())
				return
			}
		}
		t.Fatalf("no run.created event in %v", aud.events)
	})

	for _, success := range []bool{true, false} {
		t.Run(fmt.Sprintf("report success=%v", success), func(t *testing.T) {
			runID := uuid.New()
			aud := &fakeInvokeAudit{}
			svc := NewSandboxService(Deps{
				Logger: zap.NewNop(), Audit: aud, GatewaySpiffeID: testGWSpiffeID,
				Scheduled: &fakeReportStore{runs: map[string]*db.ScheduledRun{
					runID.String(): {ScheduledRunID: runID, OwnerUserID: "alice@example.com", ActorAgentID: &actorID},
				}},
			})
			if _, err := svc.ReportScheduledRunResult(gwReportCtx(), &brokerv1.ReportScheduledRunResultRequest{
				TenantId: testTenantUUID, Id: runID.String(), Success: success,
			}); err != nil {
				t.Fatalf("ReportScheduledRunResult: %v", err)
			}
			if len(aud.events) != 1 {
				t.Fatalf("want 1 event, got %v", aud.events)
			}
			checkOBO(t, aud.events[0], runID.String())
		})
	}
}

// TestUpdateScheduledRun_ResumeActorlessRefusedInEnforce: under
// identity.obo=enforce resuming a parked actorless row is refused up front
// (ClaimDue would only re-park it); audit mode still resumes it.
func TestUpdateScheduledRun_ResumeActorlessRefusedInEnforce(t *testing.T) {
	for mode, want := range map[string]codes.Code{"enforce": codes.FailedPrecondition, "audit": codes.OK} {
		cfg := newFakeConfigStore()
		cfg.values[testTenantUUID+":identity.obo"] = mode
		deps := testAdminDeps(t, "")
		deps.Config = cfg
		store := &stateScheduledStore{captureScheduledStore: captureScheduledStore{get: &db.ScheduledRun{
			ScheduledRunID: uuid.New(), OwnerUserID: "alice@example.com", Kind: db.ScheduleKindCron, CronExpr: "0 9 * * *",
			State: db.ScheduledRunPaused, LastStatus: "actor_missing", DelegationExpiresAt: time.Now().Add(time.Hour),
		}}}
		deps.Scheduled = store
		_, err := NewBrokerService(deps).UpdateScheduledRun(ctxWithIdentity(testTenantUUID, "alice@example.com"),
			&brokerv1.UpdateScheduledRunRequest{UserId: "alice@example.com", Id: store.get.ScheduledRunID.String(),
				DesiredState: brokerv1.ScheduledRunState_SCHEDULED_RUN_ACTIVE})
		if status.Code(err) != want {
			t.Errorf("%s: resume = %v, want %v", mode, err, want)
		}
		if mode == "enforce" && store.state != "" {
			t.Errorf("enforce: a refused resume must not reach the store (state %q)", store.state)
		}
	}
}

type extendScheduledStore struct {
	captureScheduledStore
	until time.Time
}

func (f *extendScheduledStore) ExtendDelegation(_ context.Context, _, _ string, until time.Time) error {
	f.until = until
	return nil
}

// TestUpdateScheduledRun_ExtendDelegation: the owner renews an expired
// delegation to now+90d (audited); an admin who is not the owner cannot
// re-grant it, and a revoked delegation stays final.
func TestUpdateScheduledRun_ExtendDelegation(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true},
		checks: map[string]bool{"user:alice@example.com|can_invoke|skill:scheduler": true}}
	srv := f.server(t)
	defer srv.Close()
	revokedAt := time.Now()
	row := func() *db.ScheduledRun {
		return &db.ScheduledRun{ScheduledRunID: uuid.New(), OwnerUserID: "alice@example.com", Kind: db.ScheduleKindCron,
			CronExpr: "0 9 * * *", State: db.ScheduledRunPaused, LastStatus: "delegation_expired",
			DelegationExpiresAt: time.Now().Add(-time.Hour)}
	}
	extend := func(t *testing.T, caller string, r *db.ScheduledRun) (*extendScheduledStore, notify.Subscription, error) {
		deps, sub := testAdminDepsWithConfigAndBus(t, srv.URL, newFakeConfigStore())
		store := &extendScheduledStore{captureScheduledStore: captureScheduledStore{get: r}}
		deps.Scheduled = store
		_, err := NewBrokerService(deps).UpdateScheduledRun(ctxWithIdentity(testTenantUUID, caller),
			&brokerv1.UpdateScheduledRunRequest{UserId: caller, Id: r.ScheduledRunID.String(), ExtendDelegation: true})
		return store, sub, err
	}

	t.Run("owner extends expired", func(t *testing.T) {
		before := time.Now()
		store, sub, err := extend(t, "alice@example.com", row())
		if err != nil {
			t.Fatalf("extend: %v", err)
		}
		if d := store.until.Sub(before); d < delegationTTL || d > delegationTTL+time.Minute {
			t.Errorf("until = now+%v, want now+%v", d, delegationTTL)
		}
		ev := receiveAuditEvent(t, sub, "aikonos.scheduler.run.delegation_extended", 2*time.Second)
		if ev.Decision != auditv1.PolicyDecision_ALLOW || ev.ActorUserId != "alice@example.com" {
			t.Errorf("audit = %v by %q, want ALLOW by alice", ev.Decision, ev.ActorUserId)
		}
	})
	t.Run("admin not owner refused", func(t *testing.T) {
		store, _, err := extend(t, "admin@example.com", row())
		if status.Code(err) != codes.PermissionDenied || !store.until.IsZero() {
			t.Errorf("err = %v, until = %v; want PermissionDenied and no store write", err, store.until)
		}
	})
	t.Run("revoked refused", func(t *testing.T) {
		r := row()
		r.DelegationRevokedAt = &revokedAt
		store, _, err := extend(t, "alice@example.com", r)
		if status.Code(err) != codes.FailedPrecondition || !store.until.IsZero() {
			t.Errorf("err = %v, until = %v; want FailedPrecondition and no store write", err, store.until)
		}
	})
}

// TestUpdateScheduledRun_OwnerLostSchedulerSkill (F192): once skill:scheduler
// is revoked, the owner can no longer extend, resume or edit a schedule, and
// the denial is audited; pause and revoke stay open so they can still stop it.
func TestUpdateScheduledRun_OwnerLostSchedulerSkill(t *testing.T) {
	f := &fakeFGA{} // FGA on, alice holds no skill:scheduler
	srv := f.server(t)
	defer srv.Close()
	run := func(t *testing.T, req *brokerv1.UpdateScheduledRunRequest) (*stateScheduledStore, notify.Subscription, error) {
		deps, sub := testAdminDepsWithConfigAndBus(t, srv.URL, newFakeConfigStore())
		store := &stateScheduledStore{captureScheduledStore: captureScheduledStore{get: &db.ScheduledRun{
			ScheduledRunID: uuid.New(), OwnerUserID: "alice@example.com", Prompt: "p", Kind: db.ScheduleKindCron,
			CronExpr: "0 9 * * *", State: db.ScheduledRunPaused, DelegationExpiresAt: time.Now().Add(time.Hour),
		}}}
		deps.Scheduled = store
		req.UserId, req.Id = "alice@example.com", store.get.ScheduledRunID.String()
		_, err := NewBrokerService(deps).UpdateScheduledRun(ctxWithIdentity(testTenantUUID, "alice@example.com"), req)
		return store, sub, err
	}
	for name, req := range map[string]*brokerv1.UpdateScheduledRunRequest{
		"extend": {ExtendDelegation: true},
		"resume": {DesiredState: brokerv1.ScheduledRunState_SCHEDULED_RUN_ACTIVE},
		"edit":   {Prompt: "new", Kind: brokerv1.ScheduleKind_SCHEDULE_KIND_CRON, CronExpr: "0 9 * * *"},
	} {
		t.Run(name, func(t *testing.T) {
			store, sub, err := run(t, req)
			if status.Code(err) != codes.PermissionDenied || store.state != "" || store.updated != nil {
				t.Fatalf("err = %v (state %q, updated %v); want PermissionDenied and no store write", err, store.state, store.updated)
			}
			receiveAuditEvent(t, sub, "aikonos.scheduler.run.skill_denied", 2*time.Second)
		})
	}
	t.Run("pause still allowed", func(t *testing.T) {
		store, _, err := run(t, &brokerv1.UpdateScheduledRunRequest{DesiredState: brokerv1.ScheduledRunState_SCHEDULED_RUN_PAUSED})
		if err != nil || store.state != db.ScheduledRunPaused {
			t.Fatalf("pause: err = %v, state %q", err, store.state)
		}
	})
}

// TestUpdateScheduledRun_AdminCannotRewrite (F187): a tenant admin may pause or
// revoke another user's schedule but never rewrite what it runs — it fires
// under the owner's delegation and pre-approvals.
func TestUpdateScheduledRun_AdminCannotRewrite(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true},
		checks: map[string]bool{"user:alice@example.com|can_invoke|skill:scheduler": true}}
	srv := f.server(t)
	defer srv.Close()
	run := func(t *testing.T, req *brokerv1.UpdateScheduledRunRequest) (*stateScheduledStore, notify.Subscription, error) {
		deps, sub := testAdminDepsWithConfigAndBus(t, srv.URL, newFakeConfigStore())
		store := &stateScheduledStore{captureScheduledStore: captureScheduledStore{get: &db.ScheduledRun{
			ScheduledRunID: uuid.New(), OwnerUserID: "alice@example.com", Prompt: "summarise my inbox", Kind: db.ScheduleKindCron,
			CronExpr: "0 9 * * *", State: db.ScheduledRunActive, DelegationExpiresAt: time.Now().Add(time.Hour),
		}}}
		deps.Scheduled = store
		req.UserId, req.Id = "admin@example.com", store.get.ScheduledRunID.String()
		_, err := NewBrokerService(deps).UpdateScheduledRun(ctxWithIdentity(testTenantUUID, "admin@example.com"), req)
		return store, sub, err
	}
	t.Run("full edit denied", func(t *testing.T) {
		store, sub, err := run(t, &brokerv1.UpdateScheduledRunRequest{
			Prompt: "email the finance folder out", Kind: brokerv1.ScheduleKind_SCHEDULE_KIND_CRON, CronExpr: "0 9 * * *",
		})
		if status.Code(err) != codes.PermissionDenied || store.updated != nil {
			t.Fatalf("err = %v, updated %v; want PermissionDenied and no store write", err, store.updated)
		}
		ev := receiveAuditEvent(t, sub, "aikonos.scheduler.run.update_denied", 2*time.Second)
		if ev.Decision != auditv1.PolicyDecision_DENY || ev.ActorUserId != "admin@example.com" {
			t.Errorf("audit = %v by %q, want DENY by admin", ev.Decision, ev.ActorUserId)
		}
	})
	t.Run("pause allowed", func(t *testing.T) {
		store, _, err := run(t, &brokerv1.UpdateScheduledRunRequest{DesiredState: brokerv1.ScheduledRunState_SCHEDULED_RUN_PAUSED})
		if err != nil || store.state != db.ScheduledRunPaused {
			t.Fatalf("pause: err = %v, state %q", err, store.state)
		}
	})
}
