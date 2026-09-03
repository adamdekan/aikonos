package provisioning_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/adamdekan/aikonos/broker/internal/auth"
	"github.com/adamdekan/aikonos/broker/internal/db"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"

	"github.com/adamdekan/aikonos/broker/internal/provisioning"
)

// ── fakes ─────────────────────────────────────────────────────────────────────

type fakeRuleStore struct {
	mu      sync.Mutex
	rules   []db.ProvisioningRule
	claimed map[string]bool // key = tenant+"/"+subject
}

func (f *fakeRuleStore) ListRules(_ context.Context, _ string) ([]db.ProvisioningRule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]db.ProvisioningRule(nil), f.rules...), nil
}

func (f *fakeRuleStore) ClaimProvisioning(_ context.Context, tenant, subject, _, _ string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.claimed == nil {
		f.claimed = make(map[string]bool)
	}
	key := tenant + "/" + subject
	if f.claimed[key] {
		return false, nil
	}
	f.claimed[key] = true
	return true, nil
}

type fakeTupleWrite struct {
	mu      sync.Mutex
	written []string // "user:X member group:Y"
	failOn  string   // if non-empty, return a write_failed error for this group
}

func (f *fakeTupleWrite) WriteTuple(_ context.Context, user, relation, object string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	entry := user + " " + relation + " " + object
	if f.failOn != "" && object == "group:"+f.failOn {
		// simulate OpenFGA duplicate-tuple error (already_exists is the code that
		// escapes fga.write(); write_failed_due_to_invalid_input is absorbed internally).
		return errors.New("fga write rejected: already_exists")
	}
	f.written = append(f.written, entry)
	return nil
}

type fakeAudit struct {
	mu     sync.Mutex
	events []*auditv1.AuditEvent
}

func (f *fakeAudit) Emit(_ context.Context, ev *auditv1.AuditEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, ev)
	return nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

func identity(subject, email string) *auth.Identity {
	return &auth.Identity{Subject: subject, Email: email}
}

func newProvisioner(store provisioning.RuleStore, tw *fakeTupleWrite, aud *fakeAudit) *provisioning.Provisioner {
	return provisioning.NewProvisioner(store, tw, aud, zap.NewNop())
}

// ── tests ─────────────────────────────────────────────────────────────────────

func TestProvisioner_ClaimFalse_NoTuples(t *testing.T) {
	// ClaimProvisioning returns false → already provisioned; no tuples written.
	store := &fakeRuleStore{
		rules:   []db.ProvisioningRule{{Matcher: "*@contoso.com", Groups: []string{"devs"}}},
		claimed: map[string]bool{"tenant1/sub1": true}, // pre-claimed
	}
	tw := &fakeTupleWrite{}
	aud := &fakeAudit{}

	p := newProvisioner(store, tw, aud)
	p.Seen(context.Background(), "tenant1", identity("sub1", "alice@contoso.com"))

	// allow goroutine to finish
	time.Sleep(50 * time.Millisecond)

	tw.mu.Lock()
	defer tw.mu.Unlock()
	if len(tw.written) != 0 {
		t.Errorf("expected no tuples written; got %v", tw.written)
	}
}

func TestProvisioner_Claimed_TuplesWritten(t *testing.T) {
	store := &fakeRuleStore{
		rules: []db.ProvisioningRule{
			{Matcher: "*@contoso.com", Groups: []string{"devs", "security-team"}},
		},
	}
	tw := &fakeTupleWrite{}
	aud := &fakeAudit{}

	p := newProvisioner(store, tw, aud)
	p.Seen(context.Background(), "tenant1", identity("sub1", "alice@contoso.com"))
	time.Sleep(50 * time.Millisecond)

	tw.mu.Lock()
	defer tw.mu.Unlock()
	if len(tw.written) != 2 {
		t.Fatalf("expected 2 tuples; got %v", tw.written)
	}
	want := map[string]bool{
		"user:sub1 member group:devs":          true,
		"user:sub1 member group:security-team": true,
	}
	for _, w := range tw.written {
		if !want[w] {
			t.Errorf("unexpected tuple %q", w)
		}
	}
}

func TestProvisioner_SvcSubjectSkipped(t *testing.T) {
	store := &fakeRuleStore{
		rules: []db.ProvisioningRule{{Matcher: "*@contoso.com", Groups: []string{"devs"}}},
	}
	tw := &fakeTupleWrite{}
	aud := &fakeAudit{}

	p := newProvisioner(store, tw, aud)
	p.Seen(context.Background(), "tenant1", identity("svc-agent", "svc-agent@contoso.com"))
	time.Sleep(50 * time.Millisecond)

	tw.mu.Lock()
	defer tw.mu.Unlock()
	if len(tw.written) != 0 {
		t.Errorf("svc- subject must be skipped; got tuples %v", tw.written)
	}
}

func TestProvisioner_EmptyEmailSkipped(t *testing.T) {
	store := &fakeRuleStore{
		rules: []db.ProvisioningRule{{Matcher: "*@contoso.com", Groups: []string{"devs"}}},
	}
	tw := &fakeTupleWrite{}
	aud := &fakeAudit{}

	p := newProvisioner(store, tw, aud)
	p.Seen(context.Background(), "tenant1", identity("sub1", ""))
	time.Sleep(50 * time.Millisecond)

	tw.mu.Lock()
	defer tw.mu.Unlock()
	if len(tw.written) != 0 {
		t.Errorf("empty email must be skipped; got tuples %v", tw.written)
	}
}

func TestProvisioner_SeenSetPreventsDoubleClaim(t *testing.T) {
	var claimCalls int
	store := &fakeRuleStore{
		rules: []db.ProvisioningRule{{Matcher: "*@contoso.com", Groups: []string{"devs"}}},
	}
	// Wrap ClaimProvisioning to count calls — use a custom store.
	countingStore := &countingRuleStore{inner: store, onClaim: func() { claimCalls++ }}

	tw := &fakeTupleWrite{}
	aud := &fakeAudit{}

	p := newProvisioner(countingStore, tw, aud)
	p.Seen(context.Background(), "tenant1", identity("sub1", "alice@contoso.com"))
	p.Seen(context.Background(), "tenant1", identity("sub1", "alice@contoso.com")) // second call
	time.Sleep(100 * time.Millisecond)

	if claimCalls != 1 {
		t.Errorf("ClaimProvisioning called %d times; want 1 (seen-set must block second call)", claimCalls)
	}
}

// countingRuleStore wraps a fakeRuleStore and counts ClaimProvisioning calls.
type countingRuleStore struct {
	inner   *fakeRuleStore
	onClaim func()
}

func (c *countingRuleStore) ListRules(ctx context.Context, tenant string) ([]db.ProvisioningRule, error) {
	return c.inner.ListRules(ctx, tenant)
}

func (c *countingRuleStore) ClaimProvisioning(ctx context.Context, tenant, subject, email, name string) (bool, error) {
	c.onClaim()
	return c.inner.ClaimProvisioning(ctx, tenant, subject, email, name)
}

func TestProvisioner_DuplicateTupleErrorTolerated(t *testing.T) {
	// failOn "devs" simulates OpenFGA duplicate-tuple error for that group.
	// The provisioner must continue and still write "security-team".
	store := &fakeRuleStore{
		rules: []db.ProvisioningRule{
			{Matcher: "*@contoso.com", Groups: []string{"devs", "security-team"}},
		},
	}
	tw := &fakeTupleWrite{failOn: "devs"}
	aud := &fakeAudit{}

	p := newProvisioner(store, tw, aud)
	p.Seen(context.Background(), "tenant1", identity("sub1", "alice@contoso.com"))
	time.Sleep(50 * time.Millisecond)

	tw.mu.Lock()
	defer tw.mu.Unlock()
	if len(tw.written) != 1 || tw.written[0] != "user:sub1 member group:security-team" {
		t.Errorf("expected security-team tuple written after devs error; got %v", tw.written)
	}
}

func TestProvisioner_FGADisabledNoop(t *testing.T) {
	// nil tupleWriter → Seen is a no-op.
	store := &fakeRuleStore{
		rules: []db.ProvisioningRule{{Matcher: "*@contoso.com", Groups: []string{"devs"}}},
	}
	aud := &fakeAudit{}

	p := provisioning.NewProvisioner(store, nil, aud, zap.NewNop())
	p.Seen(context.Background(), "tenant1", identity("sub1", "alice@contoso.com"))
	time.Sleep(50 * time.Millisecond)

	// No panic, no claim.
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.claimed["tenant1/sub1"] {
		t.Error("FGA disabled: ClaimProvisioning must not be called")
	}
}

func TestProvisioner_AuditEventEmitted(t *testing.T) {
	store := &fakeRuleStore{
		rules: []db.ProvisioningRule{
			{Matcher: "*@contoso.com", Groups: []string{"devs"}},
		},
	}
	tw := &fakeTupleWrite{}
	aud := &fakeAudit{}

	p := newProvisioner(store, tw, aud)
	p.Seen(context.Background(), "tenant1", identity("sub1", "alice@contoso.com"))
	time.Sleep(50 * time.Millisecond)

	aud.mu.Lock()
	defer aud.mu.Unlock()
	if len(aud.events) == 0 {
		t.Fatal("expected at least one audit event")
	}
	ev := aud.events[0]
	if ev.EventType != "user.provisioned" {
		t.Errorf("event type = %q; want %q", ev.EventType, "user.provisioned")
	}
}

// ── error-path / seen-set ordering tests ─────────────────────────────────────

// errorRuleStore lets tests inject a ListRules or ClaimProvisioning error.
type errorRuleStore struct {
	inner        *fakeRuleStore
	listRulesErr error
	claimErr     error
}

func (e *errorRuleStore) ListRules(ctx context.Context, tenant string) ([]db.ProvisioningRule, error) {
	if e.listRulesErr != nil {
		return nil, e.listRulesErr
	}
	return e.inner.ListRules(ctx, tenant)
}

func (e *errorRuleStore) ClaimProvisioning(ctx context.Context, tenant, subject, email, name string) (bool, error) {
	if e.claimErr != nil {
		return false, e.claimErr
	}
	return e.inner.ClaimProvisioning(ctx, tenant, subject, email, name)
}

func TestProvisioner_ListRulesError_ClearsSeenSet(t *testing.T) {
	// ListRules fails → seen-set entry must be removed so the next Seen call
	// retries the full flow (no claim consumed yet).
	inner := &fakeRuleStore{
		rules: []db.ProvisioningRule{{Matcher: "*@contoso.com", Groups: []string{"devs"}}},
	}
	store := &errorRuleStore{inner: inner, listRulesErr: errors.New("db timeout")}
	tw := &fakeTupleWrite{}
	aud := &fakeAudit{}

	p := newProvisioner(store, tw, aud)
	p.Seen(context.Background(), "tenant1", identity("sub1", "alice@contoso.com"))
	time.Sleep(50 * time.Millisecond)

	// No claim should have been consumed.
	inner.mu.Lock()
	claimed := inner.claimed["tenant1/sub1"]
	inner.mu.Unlock()
	if claimed {
		t.Error("ClaimProvisioning must not have been called when ListRules fails")
	}

	// Seen-set must be cleared: a second Seen call with a working store must proceed.
	store.listRulesErr = nil
	p.Seen(context.Background(), "tenant1", identity("sub1", "alice@contoso.com"))
	time.Sleep(50 * time.Millisecond)

	tw.mu.Lock()
	written := append([]string(nil), tw.written...)
	tw.mu.Unlock()
	if len(written) != 1 || written[0] != "user:sub1 member group:devs" {
		t.Errorf("expected tuple written on retry; got %v", written)
	}
}

func TestProvisioner_ClaimError_ClearsSeenSet(t *testing.T) {
	// ClaimProvisioning fails → seen-set entry must be removed so the next
	// call can retry.
	inner := &fakeRuleStore{
		rules: []db.ProvisioningRule{{Matcher: "*@contoso.com", Groups: []string{"devs"}}},
	}
	store := &errorRuleStore{inner: inner, claimErr: errors.New("connection reset")}
	tw := &fakeTupleWrite{}
	aud := &fakeAudit{}

	p := newProvisioner(store, tw, aud)
	p.Seen(context.Background(), "tenant1", identity("sub1", "alice@contoso.com"))
	time.Sleep(50 * time.Millisecond)

	tw.mu.Lock()
	wlen := len(tw.written)
	tw.mu.Unlock()
	if wlen != 0 {
		t.Error("no tuples must be written when Claim errors")
	}

	// Remove the injected error; seen-set must be clear so retry succeeds.
	store.claimErr = nil
	p.Seen(context.Background(), "tenant1", identity("sub1", "alice@contoso.com"))
	time.Sleep(50 * time.Millisecond)

	tw.mu.Lock()
	written := append([]string(nil), tw.written...)
	tw.mu.Unlock()
	if len(written) != 1 || written[0] != "user:sub1 member group:devs" {
		t.Errorf("expected tuple written on retry; got %v", written)
	}
}

func TestProvisioner_EmptyMatch_ClaimConsumedNoTuplesNoAudit(t *testing.T) {
	// No rule matches the email → claim is still consumed (once-only semantics)
	// but no tuples are written and no audit event is emitted.
	store := &fakeRuleStore{
		rules: []db.ProvisioningRule{{Matcher: "*@other.com", Groups: []string{"devs"}}},
	}
	tw := &fakeTupleWrite{}
	aud := &fakeAudit{}

	p := newProvisioner(store, tw, aud)
	p.Seen(context.Background(), "tenant1", identity("sub1", "alice@contoso.com"))
	time.Sleep(50 * time.Millisecond)

	store.mu.Lock()
	claimed := store.claimed["tenant1/sub1"]
	store.mu.Unlock()
	if !claimed {
		t.Error("claim must be consumed even when no rules match")
	}

	tw.mu.Lock()
	wlen := len(tw.written)
	tw.mu.Unlock()
	if wlen != 0 {
		t.Errorf("no tuples must be written for empty match; got %d", wlen)
	}

	aud.mu.Lock()
	elen := len(aud.events)
	aud.mu.Unlock()
	if elen != 0 {
		t.Errorf("no audit events must be emitted for empty match; got %d", elen)
	}
}
