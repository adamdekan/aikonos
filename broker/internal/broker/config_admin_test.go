package broker

// CP2 tests: GetPlatformConfig / SetPlatformConfig RPCs.
//
// Non-admin gate tests use fakeConfigStore (no DB involved).
// Schema-validation and audit-event tests use a real config.Store wrapping a
// fakeConfigRepo so the guardrails come from the real code, not a copy.

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/config"
	"github.com/adamdekan/aikonos/broker/internal/notify"
	"github.com/adamdekan/aikonos/broker/internal/policy"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// ── fake Config store (for non-admin gate tests only) ─────────────────────────

type fakeConfigStore struct {
	mu       sync.Mutex
	values   map[string]string // "tenant:key" → value
	setCalls []configSetCall
}

type configSetCall struct {
	tenant, key, value, actor string
}

func newFakeConfigStore() *fakeConfigStore {
	return &fakeConfigStore{values: map[string]string{}}
}

func (f *fakeConfigStore) GetInt(_ context.Context, tenant, key string) int {
	f.mu.Lock()
	v, found := f.values[tenant+":"+key]
	f.mu.Unlock()
	if !found {
		k, ok := config.Schema[key]
		if !ok {
			return 0
		}
		n, _ := strconv.Atoi(k.Default)
		return n
	}
	n, _ := strconv.Atoi(v)
	return n
}

func (f *fakeConfigStore) GetString(_ context.Context, tenant, key string) string {
	f.mu.Lock()
	v, found := f.values[tenant+":"+key]
	f.mu.Unlock()
	if !found {
		k, ok := config.Schema[key]
		if !ok {
			return ""
		}
		return k.Default
	}
	return v
}

func (f *fakeConfigStore) List(_ context.Context, tenant string) ([]config.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]config.Entry, 0, len(config.Schema))
	for name, k := range config.Schema {
		val, ok := f.values[tenant+":"+name]
		if !ok {
			val = k.Default
		}
		out = append(out, config.Entry{
			Key:     name,
			Value:   val,
			Default: k.Default,
			Kind:    k.Kind,
			Doc:     k.Doc,
		})
	}
	return out, nil
}

func (f *fakeConfigStore) Set(_ context.Context, tenant, key, value, actor string) error {
	f.mu.Lock()
	f.values[tenant+":"+key] = value
	f.setCalls = append(f.setCalls, configSetCall{tenant, key, value, actor})
	f.mu.Unlock()
	return nil
}

// ── fakeConfigRepo satisfies config.Repo ─────────────────────────────────────

type fakeConfigRepo struct {
	mu      sync.Mutex
	data    map[string]string // key → value (tenant ignored for simplicity)
	written []configSetCall
	err     error // when non-nil, repo.Set returns this error
}

func newFakeConfigRepo() *fakeConfigRepo {
	return &fakeConfigRepo{data: map[string]string{}}
}

func (r *fakeConfigRepo) Get(_ context.Context, _, key string) (string, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.data[key]
	return v, ok, nil
}

func (r *fakeConfigRepo) Set(_ context.Context, tenant, key, value, actor string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.data[key] = value
	r.written = append(r.written, configSetCall{tenant, key, value, actor})
	return nil
}

func (r *fakeConfigRepo) List(_ context.Context, _ string) (map[string]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]string, len(r.data))
	for k, v := range r.data {
		out[k] = v
	}
	return out, nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

func testAdminDepsWithConfig(t *testing.T, fgaURL string, cfg Config) Deps {
	t.Helper()
	em, err := audit.NewEmitter(context.Background(), audit.Config{})
	if err != nil {
		t.Fatal(err)
	}
	pcfg := policy.Config{OPAEndpoint: "http://unused", OpenFGAEndpoint: fgaURL}
	if fgaURL != "" {
		pcfg.OpenFGAStoreID = "store-1"
	}
	eng, err := policy.NewEngine(context.Background(), pcfg)
	if err != nil {
		t.Fatal(err)
	}
	return Deps{
		Logger:     zap.NewNop(),
		Audit:      em,
		Policy:     eng,
		TenantID:   "aikonos-dev",
		KnownUsers: []string{"admin@example.com", "alice@example.com"},
		Config:     cfg,
	}
}

// testAdminDepsWithConfigAndBus is like testAdminDepsWithConfig but wires a
// MemoryBus so audit events can be captured by tests.
func testAdminDepsWithConfigAndBus(t *testing.T, fgaURL string, cfg Config) (Deps, notify.Subscription) {
	t.Helper()
	em, sub := buildAuditBus(t, testTenantUUID)
	pcfg := policy.Config{OPAEndpoint: "http://unused", OpenFGAEndpoint: fgaURL}
	if fgaURL != "" {
		pcfg.OpenFGAStoreID = "store-1"
	}
	eng, err := policy.NewEngine(context.Background(), pcfg)
	if err != nil {
		t.Fatal(err)
	}
	deps := Deps{
		Logger:     zap.NewNop(),
		Audit:      em,
		Policy:     eng,
		TenantID:   "aikonos-dev",
		KnownUsers: []string{"admin@example.com", "alice@example.com"},
		Config:     cfg,
	}
	return deps, sub
}

// ── test 1: non-admin → PermissionDenied; Config not touched ─────────────────

func TestGetPlatformConfig_NonAdminDenied(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()

	cfg := newFakeConfigStore()
	svc := NewBrokerService(testAdminDepsWithConfig(t, srv.URL, cfg))

	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")
	_, err := svc.GetPlatformConfig(ctx, &brokerv1.GetPlatformConfigRequest{})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-admin GetPlatformConfig: want PermissionDenied, got %v", err)
	}
	if len(cfg.setCalls) != 0 {
		t.Error("non-admin GetPlatformConfig must not touch Config")
	}
}

func TestSetPlatformConfig_NonAdminDenied(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()

	cfg := newFakeConfigStore()
	svc := NewBrokerService(testAdminDepsWithConfig(t, srv.URL, cfg))

	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")
	_, err := svc.SetPlatformConfig(ctx, &brokerv1.SetPlatformConfigRequest{
		Key: "approval_expiry_hours", Value: "48",
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-admin SetPlatformConfig: want PermissionDenied, got %v", err)
	}
	if len(cfg.setCalls) != 0 {
		t.Error("non-admin SetPlatformConfig must not write Config")
	}
}

// ── test 2: admin GetPlatformConfig → entries include approval_expiry_hours ──

func TestGetPlatformConfig_AdminSeesEntries(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()

	cfg := newFakeConfigStore()
	svc := NewBrokerService(testAdminDepsWithConfig(t, srv.URL, cfg))

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	resp, err := svc.GetPlatformConfig(ctx, &brokerv1.GetPlatformConfigRequest{})
	if err != nil {
		t.Fatalf("admin GetPlatformConfig: %v", err)
	}
	if len(resp.Entries) == 0 {
		t.Fatal("expected at least one entry")
	}
	var found *brokerv1.ConfigEntry
	for _, e := range resp.Entries {
		if e.Key == "approval_expiry_hours" {
			found = e
		}
	}
	if found == nil {
		t.Fatal("approval_expiry_hours not in entries")
	}
	if found.DefaultValue != "24" {
		t.Errorf("DefaultValue = %q, want 24", found.DefaultValue)
	}
	if found.Value != "24" {
		t.Errorf("Value (effective) = %q, want 24 (unset → default)", found.Value)
	}
}

// ── test 3a: admin SetPlatformConfig unknown key → InvalidArgument ────────────
// Uses real config.Store so ErrUnknownKey comes from the real guardrail.

func TestSetPlatformConfig_UnknownKey(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()

	repo := newFakeConfigRepo()
	store := config.New(repo)
	svc := NewBrokerService(testAdminDepsWithConfig(t, srv.URL, store))

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	_, err := svc.SetPlatformConfig(ctx, &brokerv1.SetPlatformConfigRequest{
		Key: "nonexistent_key", Value: "99",
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("unknown key: want InvalidArgument, got %v", err)
	}
	if len(repo.written) != 0 {
		t.Error("unknown key must not write repo")
	}
}

// ── test 3b: admin SetPlatformConfig valid → Set called + audit event emitted ─
// Uses real config.Store so validation comes from the real guardrail.
// Also asserts a "aikonos.broker.config.changed" audit event is published.

func TestSetPlatformConfig_ValidKeySetCalledAndAuditEmitted(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()

	repo := newFakeConfigRepo()
	store := config.New(repo)
	deps, sub := testAdminDepsWithConfigAndBus(t, srv.URL, store)
	svc := NewBrokerService(deps)

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	_, err := svc.SetPlatformConfig(ctx, &brokerv1.SetPlatformConfigRequest{
		Key: "approval_expiry_hours", Value: "48",
	})
	if err != nil {
		t.Fatalf("admin SetPlatformConfig valid: %v", err)
	}
	if len(repo.written) != 1 {
		t.Fatalf("expected 1 repo write, got %d", len(repo.written))
	}
	w := repo.written[0]
	if w.key != "approval_expiry_hours" || w.value != "48" {
		t.Errorf("repo write = %+v, want key=approval_expiry_hours value=48", w)
	}
	if w.actor != "admin@example.com" {
		t.Errorf("actor = %q, want admin@example.com", w.actor)
	}

	ev := receiveAuditEvent(t, sub, "aikonos.broker.config.changed", 2*time.Second)
	if ev.ActorUserId != "admin@example.com" {
		t.Errorf("audit actor = %q, want admin@example.com", ev.ActorUserId)
	}
	if ev.Context == nil {
		t.Fatal("audit Context must not be nil")
	}
	if k := ev.Context.Fields["key"].GetStringValue(); k != "approval_expiry_hours" {
		t.Errorf("audit context.key = %q, want approval_expiry_hours", k)
	}
}

// ── test 3c: admin SetPlatformConfig out-of-range value → InvalidArgument ─────
// Uses real config.Store so the range-check Validate comes from the real schema.

func TestSetPlatformConfig_OutOfRangeValue(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()

	repo := newFakeConfigRepo()
	store := config.New(repo)
	svc := NewBrokerService(testAdminDepsWithConfig(t, srv.URL, store))

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	_, err := svc.SetPlatformConfig(ctx, &brokerv1.SetPlatformConfigRequest{
		Key: "approval_expiry_hours", Value: "9999",
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("out-of-range: want InvalidArgument, got %v", err)
	}
	if len(repo.written) != 0 {
		t.Error("out-of-range must not write repo")
	}
}

// ── test 3d: repo error → codes.Internal (not InvalidArgument) ───────────────

func TestSetPlatformConfig_RepoError_ReturnsInternal(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()

	repo := newFakeConfigRepo()
	repo.err = errors.New("db connection reset")
	store := config.New(repo)
	svc := NewBrokerService(testAdminDepsWithConfig(t, srv.URL, store))

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	_, err := svc.SetPlatformConfig(ctx, &brokerv1.SetPlatformConfigRequest{
		Key: "approval_expiry_hours", Value: "48",
	})
	if status.Code(err) != codes.Internal {
		t.Fatalf("repo error: want Internal, got %v", err)
	}
	// driver error must not leak into the status message
	msg := status.Convert(err).Message()
	if msg == "db connection reset" {
		t.Errorf("repo error detail leaked into gRPC status message: %q", msg)
	}
}
