package broker

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/auth"
	"github.com/adamdekan/aikonos/broker/internal/db"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// fakeProvisioningStore is an in-memory provisioningAdminStore for tests.
type fakeProvisioningStore struct {
	rules    []db.ProvisioningRule
	subjects map[string][]string // email → subjects
	addErr   error
}

func (f *fakeProvisioningStore) ListRules(_ context.Context, _ string) ([]db.ProvisioningRule, error) {
	return f.rules, nil
}

func (f *fakeProvisioningStore) AddRule(_ context.Context, _ string, rule db.ProvisioningRule) error {
	if f.addErr != nil {
		return f.addErr
	}
	f.rules = append(f.rules, rule)
	return nil
}

func (f *fakeProvisioningStore) DeleteRule(_ context.Context, _, id string) error {
	for i, r := range f.rules {
		if r.ID.String() == id {
			f.rules = append(f.rules[:i], f.rules[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("rule not found: %s", id)
}

func (f *fakeProvisioningStore) SubjectsByEmail(_ context.Context, _, email string) ([]string, error) {
	return f.subjects[email], nil
}

// ── helpers ──────────────────────────────────────────────────────────────────

func testProvisioningDeps(t *testing.T, fgaURL string, store provisioningAdminStore) Deps {
	t.Helper()
	deps := testAdminDeps(t, fgaURL)
	deps.Provisioning = store
	return deps
}

// ctxWithSubject creates a context with a full Identity including Subject (for
// the forged-user-id guard in callerIdentity).
func ctxWithSubject(tenant, subject, email string) context.Context {
	return auth.WithIdentity(context.Background(), &auth.Identity{
		TenantID: tenant,
		Subject:  subject,
		Email:    email,
	})
}

// ── gate tests ────────────────────────────────────────────────────────────────

func TestListProvisioningRules_NonAdminDenied(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	store := &fakeProvisioningStore{}
	svc := NewBrokerService(testProvisioningDeps(t, srv.URL, store))

	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")
	_, err := svc.ListProvisioningRules(ctx, &brokerv1.ListProvisioningRulesRequest{})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-admin list: want PermissionDenied, got %v", err)
	}
}

func TestAddProvisioningRule_NonAdminDenied(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	store := &fakeProvisioningStore{}
	svc := NewBrokerService(testProvisioningDeps(t, srv.URL, store))

	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")
	_, err := svc.AddProvisioningRule(ctx, &brokerv1.AddProvisioningRuleRequest{
		Matcher: "alice@example.com", Groups: []string{"security-team"},
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-admin add: want PermissionDenied, got %v", err)
	}
}

func TestDeleteProvisioningRule_NonAdminDenied(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	id := uuid.New()
	store := &fakeProvisioningStore{rules: []db.ProvisioningRule{{ID: id, Matcher: "alice@example.com", Groups: []string{"security-team"}}}}
	svc := NewBrokerService(testProvisioningDeps(t, srv.URL, store))

	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")
	_, err := svc.DeleteProvisioningRule(ctx, &brokerv1.DeleteProvisioningRuleRequest{Id: id.String()})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-admin delete: want PermissionDenied, got %v", err)
	}
}

func TestAddProvisioningRule_ForgedUserIDRejected(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	store := &fakeProvisioningStore{}
	svc := NewBrokerService(testProvisioningDeps(t, srv.URL, store))

	// Identity has Subject "real-subject" but request tries to forge "admin@example.com"
	ctx := ctxWithSubject(testTenantUUID, "real-subject", "alice@example.com")
	_, err := svc.AddProvisioningRule(ctx, &brokerv1.AddProvisioningRuleRequest{
		UserId: "admin@example.com", // forged
		Matcher: "alice@example.com", Groups: []string{"security-team"},
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("forged user_id: want PermissionDenied, got %v", err)
	}
}

// ── happy-path tests ──────────────────────────────────────────────────────────

func TestListProvisioningRules_AdminHappyPath(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	ruleID := uuid.New()
	store := &fakeProvisioningStore{rules: []db.ProvisioningRule{
		{ID: ruleID, Matcher: "*@example.com", Groups: []string{"security-team"}, CreatedBy: "admin@example.com"},
	}}
	svc := NewBrokerService(testProvisioningDeps(t, srv.URL, store))

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	resp, err := svc.ListProvisioningRules(ctx, &brokerv1.ListProvisioningRulesRequest{})
	if err != nil {
		t.Fatalf("list: unexpected error %v", err)
	}
	if len(resp.Rules) != 1 {
		t.Fatalf("want 1 rule, got %d", len(resp.Rules))
	}
	if resp.Rules[0].Matcher != "*@example.com" {
		t.Errorf("matcher mismatch: %s", resp.Rules[0].Matcher)
	}
}

func TestAddProvisioningRule_AdminHappyPath(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	store := &fakeProvisioningStore{}
	svc := NewBrokerService(testProvisioningDeps(t, srv.URL, store))

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	resp, err := svc.AddProvisioningRule(ctx, &brokerv1.AddProvisioningRuleRequest{
		Matcher: "*@example.com", Groups: []string{"security-team"},
	})
	if err != nil {
		t.Fatalf("add: unexpected error %v", err)
	}
	if resp.Rule.Matcher != "*@example.com" {
		t.Errorf("matcher mismatch: %s", resp.Rule.Matcher)
	}
	if resp.AppliedCount != 0 {
		t.Errorf("wildcard: want applied_count=0, got %d", resp.AppliedCount)
	}
}

func TestDeleteProvisioningRule_AdminHappyPath(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	ruleID := uuid.New()
	store := &fakeProvisioningStore{rules: []db.ProvisioningRule{
		{ID: ruleID, Matcher: "*@example.com", Groups: []string{"security-team"}},
	}}
	svc := NewBrokerService(testProvisioningDeps(t, srv.URL, store))

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	_, err := svc.DeleteProvisioningRule(ctx, &brokerv1.DeleteProvisioningRuleRequest{Id: ruleID.String()})
	if err != nil {
		t.Fatalf("delete: unexpected error %v", err)
	}
	if len(store.rules) != 0 {
		t.Errorf("rule not deleted from store")
	}
}

// ── validation tests ──────────────────────────────────────────────────────────

func TestAddProvisioningRule_InvalidMatcherRejected(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	store := &fakeProvisioningStore{}
	svc := NewBrokerService(testProvisioningDeps(t, srv.URL, store))

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	badMatchers := []string{"*", "notanemail", "@nodomain", "svc-foo@example.com"}
	for _, m := range badMatchers {
		_, err := svc.AddProvisioningRule(ctx, &brokerv1.AddProvisioningRuleRequest{
			Matcher: m, Groups: []string{"security-team"},
		})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("matcher %q: want InvalidArgument, got %v", m, err)
		}
	}
}

func TestAddProvisioningRule_InvalidGroupsRejected(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	store := &fakeProvisioningStore{}
	svc := NewBrokerService(testProvisioningDeps(t, srv.URL, store))

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	_, err := svc.AddProvisioningRule(ctx, &brokerv1.AddProvisioningRuleRequest{
		Matcher: "alice@example.com", Groups: []string{}, // empty groups
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty groups: want InvalidArgument, got %v", err)
	}
}

// ── FGA-disabled tests ────────────────────────────────────────────────────────

func TestAddProvisioningRule_FGADisabledFailsPrecondition(t *testing.T) {
	// No FGA URL → FGA disabled (allow-all stub, FGAEnabled() = false).
	store := &fakeProvisioningStore{}
	deps := testProvisioningDeps(t, "", store)
	// Without FGA server the check returns allow-all but FGAEnabled()=false.
	svc := NewBrokerService(deps)

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	_, err := svc.AddProvisioningRule(ctx, &brokerv1.AddProvisioningRuleRequest{
		Matcher: "alice@example.com", Groups: []string{"security-team"},
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("FGA disabled add: want FailedPrecondition, got %v", err)
	}
}

func TestDeleteProvisioningRule_FGADisabledFailsPrecondition(t *testing.T) {
	store := &fakeProvisioningStore{}
	svc := NewBrokerService(testProvisioningDeps(t, "", store))

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	_, err := svc.DeleteProvisioningRule(ctx, &brokerv1.DeleteProvisioningRuleRequest{Id: uuid.New().String()})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("FGA disabled delete: want FailedPrecondition, got %v", err)
	}
}

func TestListProvisioningRules_FGADisabledStillWorks(t *testing.T) {
	// List must work even when FGA is disabled.
	store := &fakeProvisioningStore{rules: []db.ProvisioningRule{
		{ID: uuid.New(), Matcher: "*@example.com", Groups: []string{"security-team"}},
	}}
	svc := NewBrokerService(testProvisioningDeps(t, "", store))

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	resp, err := svc.ListProvisioningRules(ctx, &brokerv1.ListProvisioningRulesRequest{})
	if err != nil {
		t.Fatalf("list with FGA disabled: unexpected error %v", err)
	}
	if len(resp.Rules) != 1 {
		t.Errorf("want 1 rule, got %d", len(resp.Rules))
	}
}

// ── exact-rule immediate apply tests ─────────────────────────────────────────

func TestAddProvisioningRule_ExactRule_ImmediateApply(t *testing.T) {
	// exact matcher with 2 known subjects → applied_count=2 + 2×groups tuples written
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	store := &fakeProvisioningStore{
		subjects: map[string][]string{
			"alice@example.com": {"subj-aaa", "subj-bbb"},
		},
	}
	svc := NewBrokerService(testProvisioningDeps(t, srv.URL, store))

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	resp, err := svc.AddProvisioningRule(ctx, &brokerv1.AddProvisioningRuleRequest{
		Matcher: "alice@example.com", Groups: []string{"security-team"},
	})
	if err != nil {
		t.Fatalf("exact add: unexpected error %v", err)
	}
	if resp.AppliedCount != 2 {
		t.Errorf("want applied_count=2, got %d", resp.AppliedCount)
	}
	// 2 subjects × 1 group = 2 FGA writes
	f.mu.Lock()
	nWrites := len(f.writes)
	f.mu.Unlock()
	if nWrites != 2 {
		t.Errorf("want 2 FGA tuple writes, got %d", nWrites)
	}
}

func TestDeleteProvisioningRule_NotFound(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	store := &fakeProvisioningStore{} // empty — no rules
	svc := NewBrokerService(testProvisioningDeps(t, srv.URL, store))

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	_, err := svc.DeleteProvisioningRule(ctx, &brokerv1.DeleteProvisioningRuleRequest{Id: uuid.New().String()})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("delete non-existent rule: want NotFound, got %v", err)
	}
}

func TestAddProvisioningRule_WildcardRule_AppliedCountZero(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	store := &fakeProvisioningStore{
		// Even if subjects existed, wildcard should not resolve them.
		subjects: map[string][]string{
			"*@example.com": {"subj-aaa"},
		},
	}
	svc := NewBrokerService(testProvisioningDeps(t, srv.URL, store))

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	resp, err := svc.AddProvisioningRule(ctx, &brokerv1.AddProvisioningRuleRequest{
		Matcher: "*@example.com", Groups: []string{"security-team"},
	})
	if err != nil {
		t.Fatalf("wildcard add: unexpected error %v", err)
	}
	if resp.AppliedCount != 0 {
		t.Errorf("wildcard: want applied_count=0, got %d", resp.AppliedCount)
	}
	f.mu.Lock()
	nWrites := len(f.writes)
	f.mu.Unlock()
	if nWrites != 0 {
		t.Errorf("wildcard: want 0 FGA writes, got %d", nWrites)
	}
}
