package broker

import (
	"context"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/alerting"
	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/policy"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// fakeAlertRepo satisfies alerting.AlertRepo for broker-level tests.
// List filters by tenantID so that tests catch callers passing the wrong form.
type fakeAlertRepo struct {
	mu     sync.Mutex
	stored []alerting.StoredAlert
}

func (f *fakeAlertRepo) Insert(_ context.Context, a alerting.StoredAlert) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stored = append(f.stored, a)
	return nil
}

func (f *fakeAlertRepo) List(_ context.Context, tenantID string, limit int) ([]alerting.StoredAlert, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []alerting.StoredAlert
	for _, a := range f.stored {
		if a.TenantID == tenantID {
			out = append(out, a)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

const alertTestTenantName = "aikonos-dev"

// alertTestTenantUUID is a valid UUID for the request TenantId field.
// callerIdentity validates that reqTenant parses as UUID; Deps.TenantID is the
// human name used by fgaTenantObject() ("tenant:aikonos-dev").
const alertTestTenantUUID = "00000000-0000-0000-0000-000000000001"

func newAlertsDeps(t *testing.T, adminUser string, rows []alerting.StoredAlert) Deps {
	t.Helper()
	fga := &fakeFGA{admins: map[string]bool{"user:" + adminUser: true}}
	fgaSrv := fga.server(t)
	t.Cleanup(fgaSrv.Close)

	em, err := audit.NewEmitter(context.Background(), audit.Config{})
	if err != nil {
		t.Fatalf("audit.NewEmitter: %v", err)
	}

	eng, err := policy.NewEngine(context.Background(), policy.Config{
		OPAEndpoint:     "http://unused",
		OpenFGAEndpoint: fgaSrv.URL,
		OpenFGAStoreID:  "store-1",
	})
	if err != nil {
		t.Fatalf("policy.NewEngine: %v", err)
	}

	repo := &fakeAlertRepo{stored: append([]alerting.StoredAlert(nil), rows...)}

	return Deps{
		Logger:    zap.NewNop(),
		Policy:    eng,
		TenantID:  alertTestTenantName,
		Audit:     em,
		AlertRepo: repo,
	}
}

func makeTestAlert(rule, severity string) alerting.StoredAlert {
	// TenantID must be the UUID form — alerts are stored with ev.TenantId which
	// is the validated UUID returned by callerIdentity, not the human name.
	return alerting.StoredAlert{
		ID:       "alert-" + rule,
		TenantID: alertTestTenantUUID,
		Rule:     rule,
		Severity: severity,
		Summary:  map[string]interface{}{"detail": "test"},
		FiredAt:  time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC),
	}
}

// ── gate test: non-admin is denied ───────────────────────────────────────────

func TestListAlerts_NonAdminDenied(t *testing.T) {
	deps := newAlertsDeps(t, "admin@example.com", nil)
	svc := NewBrokerService(deps)

	_, err := svc.ListAlerts(context.Background(), &brokerv1.ListAlertsRequest{
		TenantId: alertTestTenantUUID,
		UserId:   "nonadmin@example.com",
	})
	if err == nil {
		t.Fatal("want error for non-admin, got nil")
	}
	if code := status.Code(err); code != codes.PermissionDenied {
		t.Errorf("want PermissionDenied, got %v", code)
	}
}

// ── happy path: admin receives stored alerts ──────────────────────────────────

func TestListAlerts_AdminReceivesAlerts(t *testing.T) {
	rows := []alerting.StoredAlert{
		makeTestAlert("denial", "warning"),
		makeTestAlert("high_deny_rate", "critical"),
	}
	deps := newAlertsDeps(t, "admin@example.com", rows)
	svc := NewBrokerService(deps)

	resp, err := svc.ListAlerts(context.Background(), &brokerv1.ListAlertsRequest{
		TenantId: alertTestTenantUUID,
		UserId:   "admin@example.com",
	})
	if err != nil {
		t.Fatalf("ListAlerts: %v", err)
	}
	if len(resp.Alerts) != 2 {
		t.Fatalf("want 2 alerts, got %d", len(resp.Alerts))
	}
	if resp.Alerts[0].Rule != "denial" {
		t.Errorf("alerts[0].rule: want denial, got %s", resp.Alerts[0].Rule)
	}
	if resp.Alerts[1].Severity != "critical" {
		t.Errorf("alerts[1].severity: want critical, got %s", resp.Alerts[1].Severity)
	}
}

// ── tenant UUID round-trip: alert stored under UUID is returned by ListAlerts ─
// Regression for the bug where s.deps.TenantID (human name) was passed to
// repo.List instead of the UUID returned by callerIdentity, causing List to
// always return empty when the repo filters by tenant.

func TestListAlerts_TenantUUIDRoundTrip(t *testing.T) {
	// Insert a row with TenantID = UUID (the form the engine writes).
	row := alerting.StoredAlert{
		ID:       "alert-rt-1",
		TenantID: alertTestTenantUUID,
		Rule:     "denial",
		Severity: "warning",
		Summary:  map[string]interface{}{"detail": "rt-test"},
		FiredAt:  time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC),
	}
	deps := newAlertsDeps(t, "admin@example.com", []alerting.StoredAlert{row})
	svc := NewBrokerService(deps)

	resp, err := svc.ListAlerts(context.Background(), &brokerv1.ListAlertsRequest{
		TenantId: alertTestTenantUUID,
		UserId:   "admin@example.com",
	})
	if err != nil {
		t.Fatalf("ListAlerts: %v", err)
	}
	// Before the fix this would return 0 (repo filtered by name, stored by UUID).
	if len(resp.Alerts) != 1 {
		t.Fatalf("want 1 alert (tenant UUID round-trip), got %d", len(resp.Alerts))
	}
	if resp.Alerts[0].AlertId != "alert-rt-1" {
		t.Errorf("alert id: want alert-rt-1, got %s", resp.Alerts[0].AlertId)
	}
}

// ── nil repo returns empty list, not error ────────────────────────────────────

func TestListAlerts_NilRepoEmptyList(t *testing.T) {
	fga := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	fgaSrv := fga.server(t)
	t.Cleanup(fgaSrv.Close)

	em, err := audit.NewEmitter(context.Background(), audit.Config{})
	if err != nil {
		t.Fatalf("audit.NewEmitter: %v", err)
	}
	eng, err := policy.NewEngine(context.Background(), policy.Config{
		OPAEndpoint:     "http://unused",
		OpenFGAEndpoint: fgaSrv.URL,
		OpenFGAStoreID:  "store-1",
	})
	if err != nil {
		t.Fatalf("policy.NewEngine: %v", err)
	}

	svc := NewBrokerService(Deps{
		Logger:    zap.NewNop(),
		Policy:    eng,
		TenantID:  alertTestTenantName,
		Audit:     em,
		AlertRepo: nil,
	})

	resp, err := svc.ListAlerts(context.Background(), &brokerv1.ListAlertsRequest{
		TenantId: alertTestTenantUUID,
		UserId:   "admin@example.com",
	})
	if err != nil {
		t.Fatalf("ListAlerts with nil repo: %v", err)
	}
	if len(resp.Alerts) != 0 {
		t.Errorf("want 0 alerts with nil repo, got %d", len(resp.Alerts))
	}
}

// ── limit is applied ──────────────────────────────────────────────────────────

func TestListAlerts_LimitApplied(t *testing.T) {
	rows := []alerting.StoredAlert{
		makeTestAlert("r1", "warning"),
		makeTestAlert("r2", "warning"),
		makeTestAlert("r3", "critical"),
	}
	deps := newAlertsDeps(t, "admin@example.com", rows)
	svc := NewBrokerService(deps)

	resp, err := svc.ListAlerts(context.Background(), &brokerv1.ListAlertsRequest{
		TenantId: alertTestTenantUUID,
		UserId:   "admin@example.com",
		Limit:    2,
	})
	if err != nil {
		t.Fatalf("ListAlerts: %v", err)
	}
	if len(resp.Alerts) != 2 {
		t.Fatalf("want 2 alerts (limit=2), got %d", len(resp.Alerts))
	}
}
