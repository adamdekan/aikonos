package broker

import (
	"context"
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
