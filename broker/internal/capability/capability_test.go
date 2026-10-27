package capability

import (
	"crypto/rand"
	"errors"
	"sync"
	"testing"
	"time"

	biscuit "github.com/biscuit-auth/biscuit-go/v2"
	"github.com/biscuit-auth/biscuit-go/v2/parser"
)

func newMinter(t *testing.T) *Minter {
	t.Helper()
	m, err := GenerateMinter()
	if err != nil {
		t.Fatalf("GenerateMinter: %v", err)
	}
	return m
}

func TestMintAndAuthorize(t *testing.T) {
	m := newMinter(t)
	tok, err := m.Mint("alice@example.com", "task-1", []string{"siem:read", "slack:write"})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	if err := m.Authorize(tok, "siem:read", "task-1"); err != nil {
		t.Errorf("granted scope siem:read should be allowed: %v", err)
	}
	if err := m.Authorize(tok, "slack:write", "task-1"); err != nil {
		t.Errorf("granted scope slack:write should be allowed: %v", err)
	}
	if err := m.Authorize(tok, "db:drop", "task-1"); err == nil {
		t.Error("ungranted scope db:drop must be denied")
	}
}

// TestAuthorizeUnderConcurrencyDoesNotTimeOut is the regression guard for the
// spurious-denial bug.
//
// biscuit-go bounds datalog evaluation by racing a freshly spawned goroutine
// against a context timeout, and its default bound is 2ms. Evaluation here is a
// handful of facts, so the work is never the problem — but when the box is busy
// the goroutine may not be scheduled inside 2ms, the timeout branch wins, and
// Authorize reports a failure. On on-prem that surfaced as web.search and
// web.fetch being denied mid-run with "world runtime limit: timeout", each time
// right after several concurrent tool calls.
//
// Many concurrent Authorize calls reproduce that scheduling pressure. Every one
// must reach a real decision.
func TestAuthorizeUnderConcurrencyDoesNotTimeOut(t *testing.T) {
	m := newMinter(t)
	tok, err := m.Mint("alice@example.com", "task-1", []string{"web:read"})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	const parallel = 64
	var wg sync.WaitGroup
	errs := make([]error, parallel)
	start := make(chan struct{})
	for i := 0; i < parallel; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release together, so the calls genuinely contend
			errs[i] = m.Authorize(tok, "web:read", "task-1")
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent Authorize[%d] failed on a granted scope: %v", i, err)
		}
	}
}

// TestAuthorizeTimeoutIsNotADenial pins the classification. A runtime-limit
// error means no decision was reached, so it must be reported as
// ErrAuthorizeTimeout — the caller uses that to fail the call without writing a
// tool.denied audit event, which would otherwise record a policy decision that
// was never computed into the hash-chained WORM trail.
func TestAuthorizeTimeoutIsNotADenial(t *testing.T) {
	m := newMinter(t)
	tok, err := m.Mint("alice@example.com", "task-1", []string{"web:read"})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	orig := authorizeMaxDuration
	authorizeMaxDuration = time.Nanosecond // no evaluation can finish in 1ns
	t.Cleanup(func() { authorizeMaxDuration = orig })

	err = m.Authorize(tok, "web:read", "task-1")
	if err == nil {
		t.Fatal("expected a failure when the datalog budget is exhausted")
	}
	if !errors.Is(err, ErrAuthorizeTimeout) {
		t.Fatalf("a runtime-limit error must classify as ErrAuthorizeTimeout, got %v", err)
	}
}

// TestAuthorizeDenialIsNotATimeout is the other half: a genuine "scope not
// granted" must never be mistaken for a transient fault, or a real denial would
// be reported to the caller as retryable and skip its audit event.
func TestAuthorizeDenialIsNotATimeout(t *testing.T) {
	m := newMinter(t)
	tok, err := m.Mint("alice@example.com", "task-1", []string{"web:read"})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	err = m.Authorize(tok, "db:drop", "task-1")
	if err == nil {
		t.Fatal("ungranted scope must be denied")
	}
	if errors.Is(err, ErrAuthorizeTimeout) {
		t.Fatalf("a genuine denial must not classify as a timeout: %v", err)
	}
}

func TestMintRejectsEmptyScopes(t *testing.T) {
	m := newMinter(t)
	if _, err := m.Mint("alice", "task-1", nil); err == nil {
		t.Error("minting with no scopes must fail")
	}
}

func TestAttenuationNarrows(t *testing.T) {
	m := newMinter(t)
	root, err := m.Mint("alice@example.com", "task-1", []string{"siem:read", "slack:write", "doc:write"})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	// Delegate only siem:read to bob.
	child, err := Attenuate(root, []string{"siem:read"})
	if err != nil {
		t.Fatalf("Attenuate: %v", err)
	}

	if err := m.Authorize(child, "siem:read", "task-1"); err != nil {
		t.Errorf("attenuated scope siem:read should still be allowed: %v", err)
	}
	// slack:write was in the root but dropped by attenuation.
	if err := m.Authorize(child, "slack:write", "task-1"); err == nil {
		t.Error("attenuated-away scope slack:write must be denied on the child")
	}
}

func TestAttenuationCannotWiden(t *testing.T) {
	m := newMinter(t)
	root, err := m.Mint("alice@example.com", "task-1", []string{"siem:read"})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	// Attempt to "add" a scope the root never granted. The attenuation block's
	// check only constrains the request; the authorizer policy still requires a
	// root `right` fact, so db:drop cannot be authorized.
	child, err := Attenuate(root, []string{"siem:read", "db:drop"})
	if err != nil {
		t.Fatalf("Attenuate: %v", err)
	}
	if err := m.Authorize(child, "db:drop", "task-1"); err == nil {
		t.Error("attenuation must never grant a scope absent from the root token")
	}
	if err := m.Authorize(child, "siem:read", "task-1"); err != nil {
		t.Errorf("siem:read should remain allowed: %v", err)
	}
}

func TestChainedAttenuationIntersects(t *testing.T) {
	m := newMinter(t)
	root, _ := m.Mint("alice", "task-1", []string{"a", "b", "c"})
	step1, err := Attenuate(root, []string{"a", "b"})
	if err != nil {
		t.Fatalf("attenuate 1: %v", err)
	}
	step2, err := Attenuate(step1, []string{"b", "c"})
	if err != nil {
		t.Fatalf("attenuate 2: %v", err)
	}

	// Effective = {a,b,c} ∩ {a,b} ∩ {b,c} = {b}.
	if err := m.Authorize(step2, "b", "task-1"); err != nil {
		t.Errorf("b survives both attenuations: %v", err)
	}
	for _, s := range []string{"a", "c"} {
		if err := m.Authorize(step2, s, "task-1"); err == nil {
			t.Errorf("scope %q dropped by an attenuation must be denied", s)
		}
	}
}

func TestForeignRootKeyRejected(t *testing.T) {
	m := newMinter(t)
	tok, _ := m.Mint("alice", "task-1", []string{"siem:read"})

	other := newMinter(t)
	if err := other.Authorize(tok, "siem:read", "task-1"); err == nil {
		t.Error("a token must not verify under a different root key")
	}
}

func TestTamperedTokenRejected(t *testing.T) {
	m := newMinter(t)
	tok, _ := m.Mint("alice", "task-1", []string{"siem:read"})
	tok[len(tok)-1] ^= 0xFF // flip bits in the signature region
	if err := m.Authorize(tok, "siem:read", "task-1"); err == nil {
		t.Error("a tampered token must be rejected")
	}
}

func TestRoundTripMinterFromKey(t *testing.T) {
	gen := newMinter(t)
	m, err := NewMinter(gen.priv)
	if err != nil {
		t.Fatalf("NewMinter: %v", err)
	}
	tok, err := m.Mint("alice", "task-1", []string{"siem:read"})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if err := m.Authorize(tok, "siem:read", "task-1"); err != nil {
		t.Errorf("Authorize: %v", err)
	}
}

// fixedClock pins the minter's clock and returns a setter to move it.
func fixedClock(m *Minter) func(time.Time) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	m.Now = func() time.Time { return now }
	return func(t time.Time) { now = t }
}

// H-03: a token past its TTL is denied as ErrExpired, a fresh one is allowed.
func TestAuthorizeExpiredTokenDenied(t *testing.T) {
	m := newMinter(t)
	m.TTL = time.Minute
	set := fixedClock(m)
	start := m.Now()
	tok, err := m.Mint("alice", "task-1", []string{"web:read"})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	set(start.Add(59 * time.Second))
	if err := m.Authorize(tok, "web:read", "task-1"); err != nil {
		t.Fatalf("token inside its TTL must authorize: %v", err)
	}
	set(start.Add(time.Minute))
	err = m.Authorize(tok, "web:read", "task-1")
	if !errors.Is(err, ErrExpired) {
		t.Fatalf("want ErrExpired at exp, got %v", err)
	}
}

// H-03: an attenuated child inherits the parent's expiry.
func TestAttenuatedTokenKeepsExpiry(t *testing.T) {
	m := newMinter(t)
	set := fixedClock(m)
	root, _ := m.Mint("alice", "task-1", []string{"a", "b"})
	child, err := Attenuate(root, []string{"a"})
	if err != nil {
		t.Fatalf("Attenuate: %v", err)
	}
	set(m.Now().Add(DefaultTTL + time.Second))
	if err := m.Authorize(child, "a", "task-1"); !errors.Is(err, ErrExpired) {
		t.Fatalf("want ErrExpired, got %v", err)
	}
}

// H-03: a token minted for T1 is denied on T2; an empty task id or a task-less
// (delegation) token is never accepted.
func TestAuthorizeTaskBinding(t *testing.T) {
	m := newMinter(t)
	tok, _ := m.Mint("alice", "T1", []string{"web:read"})
	if err := m.Authorize(tok, "web:read", "T1"); err != nil {
		t.Fatalf("matching task must authorize: %v", err)
	}
	if err := m.Authorize(tok, "web:read", "T2"); !errors.Is(err, ErrTaskMismatch) {
		t.Fatalf("want ErrTaskMismatch for T2, got %v", err)
	}
	if err := m.Authorize(tok, "web:read", ""); !errors.Is(err, ErrTaskMismatch) {
		t.Fatalf("want ErrTaskMismatch for empty task id, got %v", err)
	}

	deleg, _ := m.MintTTL("bob", "", []string{"web:read"}, 72*time.Hour)
	if err := m.Authorize(deleg, "web:read", "T1"); !errors.Is(err, ErrTaskMismatch) {
		t.Fatalf("task-less delegation token must be denied, got %v", err)
	}
}

// H-03: a holder cannot rebind a token by appending a block with its own
// task() / expires() facts — authority checks never see block facts.
func TestAppendedBlockCannotForgeTaskOrExpiry(t *testing.T) {
	m := newMinter(t)
	set := fixedClock(m)
	tok, _ := m.Mint("alice", "T1", []string{"web:read"})
	parsed, err := biscuit.Unmarshal(tok)
	if err != nil {
		t.Fatal(err)
	}
	bb := parsed.CreateBlock()
	blk, err := parser.FromStringBlock(`task("T2"); expires(2099-01-01T00:00:00Z);`)
	if err != nil {
		t.Fatal(err)
	}
	if err := bb.AddBlock(blk); err != nil {
		t.Fatal(err)
	}
	forged, err := parsed.Append(rand.Reader, bb.Build())
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := forged.Serialize()
	if err := m.Authorize(raw, "web:read", "T2"); !errors.Is(err, ErrTaskMismatch) {
		t.Fatalf("forged task fact must not bind, got %v", err)
	}
	set(m.Now().Add(DefaultTTL + time.Second))
	if err := m.Authorize(raw, "web:read", "T1"); !errors.Is(err, ErrExpired) {
		t.Fatalf("forged expires fact must not extend, got %v", err)
	}
}

// H-03: a pre-TTL token (no expires fact) is refused even though it carries a
// valid signature, task and right.
func TestLegacyTokenWithoutExpiryDenied(t *testing.T) {
	m := newMinter(t)
	b := biscuit.NewBuilder(m.priv)
	for _, f := range []biscuit.Fact{stringFact("task", "T1"), stringFact("right", "web:read")} {
		if err := b.AddAuthorityFact(f); err != nil {
			t.Fatal(err)
		}
	}
	tok, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := tok.Serialize()
	if err := m.Authorize(raw, "web:read", "T1"); !errors.Is(err, ErrExpired) {
		t.Fatalf("want ErrExpired for a token with no lifetime, got %v", err)
	}
}

// A plain scope denial on a live, correctly bound token keeps its generic
// classification — not expired, not task mismatch.
func TestScopeDenialNotMislabelled(t *testing.T) {
	m := newMinter(t)
	tok, _ := m.Mint("alice", "T1", []string{"web:read"})
	err := m.Authorize(tok, "db:drop", "T1")
	if err == nil || errors.Is(err, ErrExpired) || errors.Is(err, ErrTaskMismatch) {
		t.Fatalf("want a plain denial, got %v", err)
	}
}
