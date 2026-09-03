package broker

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/db"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// capturingM365Secrets records M365 app-secret writes/deletes and can be
// pre-seeded with a stored secret (blank-preserves tests). Embeds
// capturingSecrets purely for the LlmProvider-key stub methods that come
// along for the ride when satisfying secrets.Provider.
type capturingM365Secrets struct {
	capturingSecrets
	stored      string
	writtenM365 map[string]string // tenant -> secret
	deletedM365 []string
}

func newCapturingM365Secrets() *capturingM365Secrets {
	return &capturingM365Secrets{capturingSecrets: *newCapturingSecrets(), writtenM365: map[string]string{}}
}

func (c *capturingM365Secrets) ReadM365App(_ context.Context, tenant string) (string, bool, error) {
	if v, ok := c.writtenM365[tenant]; ok {
		return v, true, nil
	}
	return c.stored, c.stored != "", nil
}
func (c *capturingM365Secrets) WriteM365App(_ context.Context, tenant, secret string) error {
	c.writtenM365[tenant] = secret
	return nil
}
func (c *capturingM365Secrets) DeleteM365App(_ context.Context, tenant string) error {
	c.deletedM365 = append(c.deletedM365, tenant)
	delete(c.writtenM365, tenant)
	return nil
}

func validM365Connection() *brokerv1.M365Connection {
	return &brokerv1.M365Connection{EntraTenantId: "entra-1", ClientId: "client-1", Enabled: true}
}

// ── gate tests: all four RPCs deny non-admin and reject svc- subjects ────────

func TestGetM365Connection_NonAdminDenied(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	svc := NewBrokerService(testProviderDeps(t, srv.URL, nil))

	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")
	_, err := svc.GetM365Connection(ctx, &brokerv1.GetM365ConnectionRequest{})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-admin: want PermissionDenied, got %v", err)
	}
}

func TestGetM365Connection_RejectsSvcSubject(t *testing.T) {
	svc := NewBrokerService(testProviderDeps(t, "", nil))
	ctx := ctxWithIdentity(testTenantUUID, "svc-someagent")
	_, err := svc.GetM365Connection(ctx, &brokerv1.GetM365ConnectionRequest{})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("svc- subject: want PermissionDenied, got %v", err)
	}
}

func TestUpsertM365Connection_NonAdminDenied(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	svc := NewBrokerService(testProviderDeps(t, srv.URL, nil))

	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")
	_, err := svc.UpsertM365Connection(ctx, &brokerv1.UpsertM365ConnectionRequest{Connection: validM365Connection()})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-admin: want PermissionDenied, got %v", err)
	}
}

func TestUpsertM365Connection_RejectsSvcSubject(t *testing.T) {
	svc := NewBrokerService(testProviderDeps(t, "", nil))
	ctx := ctxWithIdentity(testTenantUUID, "svc-someagent")
	_, err := svc.UpsertM365Connection(ctx, &brokerv1.UpsertM365ConnectionRequest{Connection: validM365Connection()})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("svc- subject: want PermissionDenied, got %v", err)
	}
}

func TestDeleteM365Connection_NonAdminDenied(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	svc := NewBrokerService(testProviderDeps(t, srv.URL, nil))

	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")
	_, err := svc.DeleteM365Connection(ctx, &brokerv1.DeleteM365ConnectionRequest{})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-admin: want PermissionDenied, got %v", err)
	}
}

func TestDeleteM365Connection_RejectsSvcSubject(t *testing.T) {
	svc := NewBrokerService(testProviderDeps(t, "", nil))
	ctx := ctxWithIdentity(testTenantUUID, "svc-someagent")
	_, err := svc.DeleteM365Connection(ctx, &brokerv1.DeleteM365ConnectionRequest{})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("svc- subject: want PermissionDenied, got %v", err)
	}
}

func TestTestM365Connection_NonAdminDenied(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	svc := NewBrokerService(testProviderDeps(t, srv.URL, nil))

	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")
	_, err := svc.TestM365Connection(ctx, &brokerv1.TestM365ConnectionRequest{})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-admin: want PermissionDenied, got %v", err)
	}
}

func TestTestM365Connection_RejectsSvcSubject(t *testing.T) {
	svc := NewBrokerService(testProviderDeps(t, "", nil))
	ctx := ctxWithIdentity(testTenantUUID, "svc-someagent")
	_, err := svc.TestM365Connection(ctx, &brokerv1.TestM365ConnectionRequest{})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("svc- subject: want PermissionDenied, got %v", err)
	}
}

// ── GetM365Connection behavior ────────────────────────────────────────────────

func TestGetM365Connection_UnconfiguredReturnsZeroValue(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	svc := NewBrokerService(testProviderDeps(t, srv.URL, nil))

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	resp, err := svc.GetM365Connection(ctx, &brokerv1.GetM365ConnectionRequest{})
	if err != nil {
		t.Fatalf("GetM365Connection: %v", err)
	}
	if resp.Connection.EntraTenantId != "" || resp.Connection.ClientId != "" {
		t.Fatalf("unconfigured connection must be zero-value, got %+v", resp.Connection)
	}
	if resp.Connection.HasSecret {
		t.Fatal("want has_secret=false when nothing stored")
	}
}

func TestGetM365Connection_HasSecretFromVaultEvenWithoutOrgSettings(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	sec := newCapturingM365Secrets()
	sec.stored = "existing-secret"
	svc := NewBrokerService(testProviderDeps(t, srv.URL, sec))

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	resp, err := svc.GetM365Connection(ctx, &brokerv1.GetM365ConnectionRequest{})
	if err != nil {
		t.Fatalf("GetM365Connection: %v", err)
	}
	if !resp.Connection.HasSecret {
		t.Fatal("want has_secret=true when a secret is stored in Vault")
	}
	// api_key / client_secret must never travel on the wire — the response
	// type doesn't even have a field for it, but confirm no leakage indirectly
	// via has_secret being the only secret-derived signal.
	if resp.Connection.EntraTenantId != "" {
		t.Fatalf("metadata still zero-value with no org_settings row, got %+v", resp.Connection)
	}
}

// ── UpsertM365Connection validation + secret handling ────────────────────────

func TestUpsertM365Connection_RequiresTenantAndClientID(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	svc := NewBrokerService(testProviderDeps(t, srv.URL, nil))
	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")

	cases := []*brokerv1.M365Connection{
		nil,
		{ClientId: "client-1"},
		{EntraTenantId: "entra-1"},
	}
	for _, conn := range cases {
		_, err := svc.UpsertM365Connection(ctx, &brokerv1.UpsertM365ConnectionRequest{Connection: conn})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("connection=%+v: want InvalidArgument, got %v", conn, err)
		}
	}
}

func TestUpsertM365Connection_NoOrgSettingsConfiguredFailsClosedButWritesSecret(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	sec := newCapturingM365Secrets()
	svc := NewBrokerService(testProviderDeps(t, srv.URL, sec))
	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")

	_, err := svc.UpsertM365Connection(ctx, &brokerv1.UpsertM365ConnectionRequest{
		Connection:   validM365Connection(),
		ClientSecret: "super-secret",
	})
	// testProviderDeps never wires Deps.OrgSettings (no live Postgres in unit
	// tests) — the RPC must fail closed rather than silently drop the metadata.
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("want FailedPrecondition (org settings unconfigured), got %v", err)
	}
	if got := sec.writtenM365[testTenantUUID]; got != "super-secret" {
		t.Fatalf("secret must still be written to Vault before the metadata check, got %v", sec.writtenM365)
	}
}

// F-1 (reviewer feedback, iteration 1): the fallback-to-stored-secret behavior
// was only covered at the resolveM365TestApp helper level; this proves it at
// the UpsertM365Connection RPC boundary — a blank client_secret on an edit
// must never call WriteM365App, even though a secret is already stored.
func TestUpsertM365Connection_BlankSecretPreservesStoredNeverWrites(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	sec := newCapturingM365Secrets()
	sec.stored = "existing-secret"
	svc := NewBrokerService(testProviderDeps(t, srv.URL, sec))
	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")

	_, err := svc.UpsertM365Connection(ctx, &brokerv1.UpsertM365ConnectionRequest{
		Connection:   validM365Connection(),
		ClientSecret: "",
	})
	// testProviderDeps never wires Deps.OrgSettings (no live Postgres in unit
	// tests) — same fail-closed path as
	// TestUpsertM365Connection_NoOrgSettingsConfiguredFailsClosedButWritesSecret;
	// the point here is the read-only behavior below, not this status code.
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("want FailedPrecondition (org settings unconfigured), got %v", err)
	}
	if len(sec.writtenM365) != 0 {
		t.Fatalf("blank client_secret on an edit must never call WriteM365App, got writes: %v", sec.writtenM365)
	}
}

// emitM365Audit is the single place that shapes an M365 audit event; test it
// directly rather than through a full RPC round-trip (Upsert only reaches
// audit on a successful Merge, which needs a live OrgSettingsRepo unavailable
// in this unit-test harness — see TestUpsertM365Connection_NoOrgSettingsConfiguredFailsClosedButWritesSecret).
func TestEmitM365Audit_ChangedKeysOnlyNeverValues(t *testing.T) {
	sink := &capturingAuditSink{}
	svc := NewBrokerService(Deps{Audit: sink})

	svc.emitM365Audit(context.Background(), "trace-1", testTenantUUID, "admin@example.com",
		"m365.connection.upsert", []string{"client_id", "entra_tenant_id", "client_secret"})

	ev := sink.last()
	if ev == nil {
		t.Fatal("expected an audit event")
	}
	if ev.Context == nil {
		t.Fatal("expected a context struct")
	}
	changedVal, ok := ev.Context.Fields["changed_keys"]
	if !ok {
		t.Fatal("changed_keys missing from audit context")
	}
	list := changedVal.GetListValue()
	if list == nil {
		t.Fatal("changed_keys must be a list value")
	}
	wantKeys := map[string]bool{"client_id": true, "entra_tenant_id": true, "client_secret": true}
	for _, v := range list.Values {
		if !wantKeys[v.GetStringValue()] {
			t.Fatalf("unexpected/leaked value in changed_keys: %q", v.GetStringValue())
		}
	}
	if len(list.Values) != len(wantKeys) {
		t.Fatalf("want %d changed keys, got %d: %v", len(wantKeys), len(list.Values), list.Values)
	}
}

// ── DeleteM365Connection best-effort secret cleanup ──────────────────────────

func TestDeleteM365Connection_DeletesSecretBestEffort(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	sec := newCapturingM365Secrets()
	sec.stored = "to-remove"
	deps := testProviderDeps(t, srv.URL, sec)
	sink := &capturingAuditSink{}
	deps.Audit = sink
	svc := NewBrokerService(deps)
	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")

	if _, err := svc.DeleteM365Connection(ctx, &brokerv1.DeleteM365ConnectionRequest{}); err != nil {
		t.Fatalf("DeleteM365Connection: %v", err)
	}
	if len(sec.deletedM365) != 1 || sec.deletedM365[0] != testTenantUUID {
		t.Fatalf("want DeleteM365App called for %q, got %v", testTenantUUID, sec.deletedM365)
	}
	if sink.last() == nil {
		t.Fatal("expected an audit event even when org_settings is unconfigured")
	}
}

// ── invalidateM365Cache ────────────────────────────────────────────────────

// TestInvalidateM365Cache_InvalidatesWorkspacePrefResolver pins CP3 fix #2 at
// the m365_admin.go call site: invalidateM365Cache (called by
// Upsert/DeleteM365Connection) must drop the tenant's cached
// WorkspacePrefResolver entries, not only OrgSettingsM365Source's.
func TestInvalidateM365Cache_InvalidatesWorkspacePrefResolver(t *testing.T) {
	repo := newFakeWorkspacePrefsRepo()
	resolver := NewWorkspacePrefResolver(repo, fakeConfiguredChecker{configured: false})
	svc := NewBrokerService(Deps{Workspace: WorkspaceDeps{Prefs: resolver}})
	ctx := context.Background()

	if _, err := resolver.Effective(ctx, "t1", "u1"); err != nil {
		t.Fatal(err)
	}
	if repo.getCalls != 1 {
		t.Fatalf("expected 1 initial repo read, got %d", repo.getCalls)
	}

	svc.invalidateM365Cache("t1")

	if _, err := resolver.Effective(ctx, "t1", "u1"); err != nil {
		t.Fatal(err)
	}
	if repo.getCalls != 2 {
		t.Fatalf("expected invalidateM365Cache to force a re-read via WorkspacePrefResolver, got %d repo calls", repo.getCalls)
	}
}

func TestInvalidateM365Cache_NilPrefsIsNoop(t *testing.T) {
	svc := NewBrokerService(Deps{})
	svc.invalidateM365Cache("t1") // must not panic
}

// ── m365HasSecret ──────────────────────────────────────────────────────────────

func TestM365HasSecret(t *testing.T) {
	sec := newCapturingM365Secrets()
	svc := NewBrokerService(Deps{Secrets: sec})
	if svc.m365HasSecret(context.Background(), "t1") {
		t.Fatal("want false when nothing stored")
	}
	sec.stored = "abc"
	if !svc.m365HasSecret(context.Background(), "t1") {
		t.Fatal("want true once a secret is stored")
	}
}

func TestM365HasSecret_NilSecretsIsFalse(t *testing.T) {
	svc := NewBrokerService(Deps{})
	if svc.m365HasSecret(context.Background(), "t1") {
		t.Fatal("nil Secrets must resolve to false, not panic")
	}
}

// ── resolveM365TestApp ────────────────────────────────────────────────────────

func TestResolveM365TestApp_FailsClosedWhenUnconfigured(t *testing.T) {
	svc := NewBrokerService(Deps{Secrets: newCapturingM365Secrets()})
	_, err := svc.resolveM365TestApp(context.Background(), "t1", &brokerv1.TestM365ConnectionRequest{})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("want FailedPrecondition for a fully unconfigured tenant, got %v", err)
	}
}

func TestResolveM365TestApp_RequestSecretOverridesStored(t *testing.T) {
	sec := newCapturingM365Secrets()
	sec.stored = "stored-secret"
	svc := NewBrokerService(Deps{Secrets: sec})

	app, err := svc.resolveM365TestApp(context.Background(), "t1", &brokerv1.TestM365ConnectionRequest{
		Connection:   &brokerv1.M365Connection{EntraTenantId: "req-tenant", ClientId: "req-client"},
		ClientSecret: "req-secret",
	})
	if err != nil {
		t.Fatalf("resolveM365TestApp: %v", err)
	}
	if app.TenantID != "req-tenant" || app.ClientID != "req-client" || app.ClientSecret != "req-secret" {
		t.Fatalf("unexpected app: %+v", app)
	}
}

func TestResolveM365TestApp_BlankSecretFallsBackToStored(t *testing.T) {
	sec := newCapturingM365Secrets()
	sec.stored = "stored-secret"
	svc := NewBrokerService(Deps{Secrets: sec})

	app, err := svc.resolveM365TestApp(context.Background(), "t1", &brokerv1.TestM365ConnectionRequest{
		Connection: &brokerv1.M365Connection{EntraTenantId: "req-tenant", ClientId: "req-client"},
	})
	if err != nil {
		t.Fatalf("resolveM365TestApp: %v", err)
	}
	if app.ClientSecret != "stored-secret" {
		t.Fatalf("want fallback to stored secret, got %q", app.ClientSecret)
	}
}

// ── pure response/mapping helpers ─────────────────────────────────────────────

func TestM365TestResult(t *testing.T) {
	ok := m365TestResult(nil)
	if !ok.Ok || ok.Detail == "" {
		t.Fatalf("nil error must map to ok=true with a non-empty detail, got %+v", ok)
	}
	failed := m365TestResult(errors.New("invalid client secret — the configured client secret was rejected"))
	if failed.Ok {
		t.Fatal("non-nil error must map to ok=false")
	}
	if failed.Detail != "invalid client secret — the configured client secret was rejected" {
		t.Fatalf("detail must pass the classified error message through verbatim, got %q", failed.Detail)
	}
}

func TestM365ToProto(t *testing.T) {
	ts := time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC)
	enabled := true
	out := m365ToProto(db.OrgSettings{
		M365TenantID: "entra-1",
		M365ClientID: "client-1",
		M365Enabled:  &enabled,
		UpdatedBy:    "admin@x.com",
		UpdatedAt:    ts,
	}, true)
	if out.EntraTenantId != "entra-1" || out.ClientId != "client-1" || !out.Enabled || !out.HasSecret {
		t.Fatalf("unexpected mapping: %+v", out)
	}
	if out.UpdatedBy != "admin@x.com" || out.UpdatedAt != "2026-07-10T08:00:00Z" {
		t.Fatalf("provenance not mapped: %+v", out)
	}

	zero := m365ToProto(db.OrgSettings{}, false)
	if zero.Enabled || zero.HasSecret || zero.UpdatedAt != "" {
		t.Fatalf("zero-value settings must map to zero-value proto, got %+v", zero)
	}
}
