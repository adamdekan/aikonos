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

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/auth"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/gatewaygrant"
	"github.com/adamdekan/aikonos/broker/internal/toolregistry"
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
		dev := NewSandboxService(Deps{GatewaySpiffeID: ""})
		if err := dev.requireGatewayPeer(gatewayCtx("spiffe://aikonos.com/whatever")); err != nil {
			t.Fatalf("unset gateway id should accept any trust-domain peer: %v", err)
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

func (f *fakeClaimDueStore) ClaimDue(_ context.Context, _ string, _ time.Time, _ int) ([]*db.ScheduledRun, error) {
	return f.runs, nil
}

// TestClaimDueScheduledRuns_GrantVerifiesToRunOwner proves that
// ClaimDueScheduledRuns attaches a broker-issued owner-grant to each DueRun and
// that Verify→the run's stored owner.  This is the transport-level proof that
// the scheduler path satisfies the CP2 mint contract.
func TestClaimDueScheduledRuns_GrantVerifiesToRunOwner(t *testing.T) {
	runID := uuid.New()
	claimStore := &fakeClaimDueStore{
		runs: []*db.ScheduledRun{
			{
				ScheduledRunID: runID,
				OwnerUserID:    "alice@example.com",
				Prompt:         "summarize",
				ApprovedTools:  []string{"doc.read"},
			},
		},
	}

	em, _ := audit.NewEmitter(context.Background(), audit.Config{})
	deps := Deps{
		Logger:          zap.NewNop(),
		Audit:           em,
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
	run := resp.Runs[0]
	if run.OwnerGrant == "" {
		t.Fatal("OwnerGrant must be non-empty")
	}
	grantTenant, grantOwner, err := gatewaygrant.Verify(testGrantKey, run.OwnerGrant)
	if err != nil {
		t.Fatalf("Verify owner grant: %v", err)
	}
	if grantTenant != testTenantUUID {
		t.Errorf("grant tenant = %q, want %q", grantTenant, testTenantUUID)
	}
	if grantOwner != "alice@example.com" {
		t.Errorf("grant owner = %q, want alice@example.com", grantOwner)
	}
}
