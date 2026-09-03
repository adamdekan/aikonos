// broker/internal/capability/capability.go
// Biscuit-based capability tokens for delegation.
//
// A capability token is a Biscuit that carries one `right("<scope>")` authority
// fact per scope its holder may exercise. Delegation attenuates the token by
// appending an offline block whose check constrains which scope a request may
// ask for — so the effective authority of any token in a delegation chain is
//
//	root rights  ∩  every attenuation subset  ∩  the requested scope.
//
// Attenuation needs no key: a holder narrows a token they already possess. Only
// minting (creating authority) needs the root private key, held by the broker
// (Vault in production). Verification needs only the root public key.
//
// This is the cryptographic counterpart to the OPA `envelope_send` scope-
// attenuation rule: OPA decides whether a delegation is permitted; the Biscuit
// makes the attenuation unforgeable and independently checkable at tool time.
package capability

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"time"

	biscuit "github.com/biscuit-auth/biscuit-go/v2"
	"github.com/biscuit-auth/biscuit-go/v2/datalog"
	"github.com/biscuit-auth/biscuit-go/v2/parser"
)

// authorizeMaxDuration bounds datalog evaluation. biscuit-go defaults to 2ms
// (datalog.defaultRunLimits) and implements the bound by racing a freshly
// spawned goroutine against a context timeout — so under CPU contention the
// goroutine may not be scheduled in time and evaluation "times out" regardless
// of how trivial it is. Our authorizer is a handful of facts and two rules, so
// a bound this generous never masks a real runaway; it only stops a scheduling
// hiccup from being reported as a failed authorization.
//
// A var, not a const, so the test can shrink it to force the runtime-limit path
// and prove a timeout is classified as ErrAuthorizeTimeout rather than a denial.
var authorizeMaxDuration = 500 * time.Millisecond

// ErrAuthorizeTimeout reports that datalog evaluation hit its runtime limit
// rather than reaching a decision. It is deliberately distinct from a denial:
// the caller must not record it as an authorization outcome, because no
// authorization outcome was computed.
var ErrAuthorizeTimeout = errors.New("capability evaluation timed out")

// Minter mints and authorizes capability tokens against a single root key pair.
type Minter struct {
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
}

// NewMinter builds a Minter from a root ed25519 private key.
func NewMinter(priv ed25519.PrivateKey) (*Minter, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("invalid ed25519 private key size %d", len(priv))
	}
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("could not derive public key")
	}
	return &Minter{priv: priv, pub: pub}, nil
}

// GenerateMinter creates a Minter with a fresh random root key (dev/tests).
func GenerateMinter() (*Minter, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return &Minter{priv: priv, pub: pub}, nil
}

// PublicKey returns the root public key (for out-of-band verifier distribution).
func (m *Minter) PublicKey() ed25519.PublicKey { return m.pub }

// Mint creates a root capability token granting `scopes` to `holder` for
// `taskID`. An empty scope list is rejected — a token that grants nothing is a
// programming error, not a valid capability.
func (m *Minter) Mint(holder, taskID string, scopes []string) ([]byte, error) {
	if len(scopes) == 0 {
		return nil, errors.New("cannot mint a capability with no scopes")
	}
	b := biscuit.NewBuilder(m.priv)

	if err := b.AddAuthorityFact(stringFact("holder", holder)); err != nil {
		return nil, fmt.Errorf("add holder fact: %w", err)
	}
	if err := b.AddAuthorityFact(stringFact("task", taskID)); err != nil {
		return nil, fmt.Errorf("add task fact: %w", err)
	}
	for _, s := range dedupe(scopes) {
		if err := b.AddAuthorityFact(stringFact("right", s)); err != nil {
			return nil, fmt.Errorf("add right fact: %w", err)
		}
	}

	tok, err := b.Build()
	if err != nil {
		return nil, fmt.Errorf("build token: %w", err)
	}
	return tok.Serialize()
}

// Attenuate appends a delegation block restricting the token to `subset`. It
// does not require the root key. The subset is intersected with whatever the
// parent already permitted, so attenuation can only ever narrow authority.
func Attenuate(parent []byte, subset []string) ([]byte, error) {
	if len(subset) == 0 {
		return nil, errors.New("cannot attenuate to an empty scope set")
	}
	tok, err := biscuit.Unmarshal(parent)
	if err != nil {
		return nil, fmt.Errorf("unmarshal parent token: %w", err)
	}

	bb := tok.CreateBlock()
	block, err := parser.FromStringBlockWithParams(
		`check if operation($s), {allowed}.contains($s);`,
		parser.ParametersMap{"allowed": scopeSet(subset)},
	)
	if err != nil {
		return nil, fmt.Errorf("parse attenuation block: %w", err)
	}
	if err := bb.AddBlock(block); err != nil {
		return nil, fmt.Errorf("add attenuation block: %w", err)
	}

	child, err := tok.Append(rand.Reader, bb.Build())
	if err != nil {
		return nil, fmt.Errorf("append attenuation: %w", err)
	}
	return child.Serialize()
}

// Authorize verifies `token` against the root key and returns nil iff it grants
// `requestedScope` (i.e. the token carries a matching `right` fact and every
// attenuation block in the chain permits this scope). A non-nil error means the
// request is denied — by failed signature, a missing right, or an attenuation —
// except for ErrAuthorizeTimeout, which means no decision was reached at all.
func (m *Minter) Authorize(token []byte, requestedScope string) error {
	if requestedScope == "" {
		return errors.New("empty requested scope")
	}
	tok, err := biscuit.Unmarshal(token)
	if err != nil {
		return fmt.Errorf("unmarshal token: %w", err)
	}

	authz, err := tok.Authorizer(m.pub, biscuit.WithWorldOptions(
		datalog.WithMaxDuration(authorizeMaxDuration),
	))
	if err != nil {
		return fmt.Errorf("create authorizer (bad signature?): %w", err)
	}

	parsed, err := parser.FromStringAuthorizerWithParams(
		`operation({op});
		 allow if right($s), operation($s);`,
		parser.ParametersMap{"op": biscuit.String(requestedScope)},
	)
	if err != nil {
		return fmt.Errorf("parse authorizer: %w", err)
	}
	authz.AddAuthorizer(parsed)

	if err := authz.Authorize(); err != nil {
		// A runtime-limit error is not a denial — the engine ran out of budget
		// before deciding. Reported separately so the caller can fail the call
		// without writing a policy decision that was never made.
		if errors.Is(err, datalog.ErrWorldRunLimitTimeout) ||
			errors.Is(err, datalog.ErrWorldRunLimitMaxFacts) ||
			errors.Is(err, datalog.ErrWorldRunLimitMaxIterations) {
			return fmt.Errorf("%w: scope %q: %v", ErrAuthorizeTimeout, requestedScope, err)
		}
		return fmt.Errorf("scope %q denied: %w", requestedScope, err)
	}
	return nil
}

// ── helpers ────────────────────────────────────────────────────────────────

func stringFact(name, value string) biscuit.Fact {
	return biscuit.Fact{Predicate: biscuit.Predicate{
		Name: name,
		IDs:  []biscuit.Term{biscuit.String(value)},
	}}
}

func scopeSet(scopes []string) biscuit.Set {
	set := make(biscuit.Set, 0, len(scopes))
	for _, s := range dedupe(scopes) {
		set = append(set, biscuit.String(s))
	}
	return set
}

func dedupe(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
