package broker

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/auth"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/gatewaygrant"
	"github.com/adamdekan/aikonos/broker/internal/revocation"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

type fakePublicKeyStore struct {
	mu   sync.Mutex
	keys []*db.StoredPublicKey
}

func (f *fakePublicKeyStore) Create(_ context.Context, k *db.StoredPublicKey) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range f.keys {
		if e.AgentID == k.AgentID && e.Kid == k.Kid {
			return db.ErrPublicKeyExists
		}
	}
	f.keys = append(f.keys, k)
	return nil
}

func (f *fakePublicKeyStore) ListByAgent(_ context.Context, tenantID, agentID string) ([]*db.StoredPublicKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*db.StoredPublicKey
	for _, k := range f.keys {
		if k.TenantID.String() == tenantID && k.AgentID.String() == agentID && k.Status == "active" {
			out = append(out, k)
		}
	}
	return out, nil
}

func (f *fakePublicKeyStore) Retire(_ context.Context, tenantID, agentID, keyID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, k := range f.keys {
		if k.ID.String() == keyID && k.TenantID.String() == tenantID && k.AgentID.String() == agentID && k.Status == "active" {
			k.Status = "retired"
			return nil
		}
	}
	return db.ErrPublicKeyNotFound
}

func (f *fakePublicKeyStore) Get(_ context.Context, tenantID, agentID, kid string) (*db.StoredPublicKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, k := range f.keys {
		if k.TenantID.String() == tenantID && k.AgentID.String() == agentID && k.Kid == kid {
			return k, nil
		}
	}
	return nil, db.ErrPublicKeyNotFound
}

func (f *fakePublicKeyStore) TouchLastUsed(string, string) {}

const testAudience = "https://aikonos.test/external"

// pkFixture is an admin, a registered ES256 key and the services around it.
type pkFixture struct {
	t      *testing.T
	north  *BrokerService
	south  *SandboxService
	aud    *fakeInvokeAudit
	priv   *ecdsa.PrivateKey
	keyRow *brokerv1.AgentPublicKey
	deps   Deps
}

func newPKFixture(t *testing.T) *pkFixture {
	t.Helper()
	agents := agentStoreWithAgent(t)
	agents.(*fakeAgentStore).agents[0].ServiceAgent = true
	deps := newApiKeyDeps(t, "admin@example.com", &fakeApiKeyStore{}, agents)
	deps.PublicKeys = &fakePublicKeyStore{}
	aud := &fakeInvokeAudit{}
	deps.Audit = aud
	f := &pkFixture{t: t, north: NewBrokerService(deps), south: NewSandboxService(deps), aud: aud, deps: deps}
	f.priv, _ = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	f.keyRow = f.register(pemOf(t, &f.priv.PublicKey), "k1")
	return f
}

func pemOf(t *testing.T, pub any) string {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

func (f *pkFixture) register(key, kid string) *brokerv1.AgentPublicKey {
	f.t.Helper()
	resp, err := f.north.RegisterAgentPublicKey(ctxWithIdentity(apiKeyTestTenantUUID, "admin@example.com"), &brokerv1.RegisterAgentPublicKeyRequest{
		TenantId: apiKeyTestTenantUUID, AgentId: apiKeyTestAgentUUID, PublicKey: key, Kid: kid, TtlDays: 30,
	})
	if err != nil {
		f.t.Fatalf("RegisterAgentPublicKey: %v", err)
	}
	return resp.Key
}

// sign builds an assertion; mutate edits the claims before signing.
func sign(t *testing.T, key any, alg jose.SignatureAlgorithm, kid string, mutate func(*jwt.Claims)) string {
	t.Helper()
	now := time.Now()
	c := jwt.Claims{
		Issuer: apiKeyTestAgentUUID, Subject: apiKeyTestAgentUUID, Audience: jwt.Audience{testAudience},
		Expiry: jwt.NewNumericDate(now.Add(2 * time.Minute)), IssuedAt: jwt.NewNumericDate(now), ID: uuid.NewString(),
	}
	if mutate != nil {
		mutate(&c)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: alg, Key: key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", kid))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := jwt.Signed(signer).Claims(c).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func (f *pkFixture) resolve(assertion string) (*brokerv1.ResolveAgentApiKeyResponse, error) {
	return f.south.ResolveAgentApiKey(sandboxCtx(), &brokerv1.ResolveAgentApiKeyRequest{
		ClientAssertion: assertion, TenantId: apiKeyTestTenantUUID, Audience: testAudience,
	})
}

func (f *pkFixture) denied(reason string) bool {
	for _, ev := range f.aud.events {
		if ev.EventType == "aikonos.broker.agentkey.assertion_denied" && ev.Decision == auditv1.PolicyDecision_DENY &&
			ev.Context.Fields["reason"].GetStringValue() == reason {
			return true
		}
	}
	return false
}

func TestClientAssertion_Valid(t *testing.T) {
	f := newPKFixture(t)
	resp, err := f.resolve(sign(t, f.priv, jose.ES256, "k1", nil))
	if err != nil || !resp.Valid {
		t.Fatalf("want valid, got %+v %v", resp, err)
	}
	if resp.Principal != "svc-"+apiKeyTestAgentUUID || resp.AgentId != apiKeyTestAgentUUID {
		t.Fatalf("want the same principal as a tk_ key, got %+v", resp)
	}
	g, err := gatewaygrant.Verify(testGrantKey, resp.OwnerGrant, false)
	if err != nil || g.Actor != apiKeyTestAgentUUID || g.Kind != gatewaygrant.KindExternal || g.GrantID != f.keyRow.Id {
		t.Fatalf("grant = %+v err=%v, want external grant for the public key", g, err)
	}
}

func TestClientAssertion_Refusals(t *testing.T) {
	f := newPKFixture(t)
	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	cases := []struct {
		name, reason, assertion string
	}{
		{"bad aud", "bad_audience", sign(t, f.priv, jose.ES256, "k1", func(c *jwt.Claims) { c.Audience = jwt.Audience{"https://elsewhere"} })},
		{"expired", "expired", sign(t, f.priv, jose.ES256, "k1", func(c *jwt.Claims) {
			c.Expiry = jwt.NewNumericDate(time.Now().Add(-2 * time.Minute))
			c.IssuedAt = jwt.NewNumericDate(time.Now().Add(-4 * time.Minute))
		})},
		{"exp over five minutes", "exp_too_far", sign(t, f.priv, jose.ES256, "k1", func(c *jwt.Claims) { c.Expiry = jwt.NewNumericDate(time.Now().Add(time.Hour)) })},
		{"unknown kid", "unknown_kid", sign(t, f.priv, jose.ES256, "nope", nil)},
		{"wrong signer", "bad_signature", sign(t, other, jose.ES256, "k1", nil)},
		{"sub differs from iss", "bad_claims", sign(t, f.priv, jose.ES256, "k1", func(c *jwt.Claims) { c.Subject = "someone" })},
		{"no jti", "missing_claims", sign(t, f.priv, jose.ES256, "k1", func(c *jwt.Claims) { c.ID = "" })},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := f.resolve(tc.assertion)
			if err != nil || resp.Valid {
				t.Fatalf("want Valid:false, got %+v %v", resp, err)
			}
			if !f.denied(tc.reason) {
				t.Fatalf("want %s DENY audit, got %v", tc.reason, f.aud.events)
			}
		})
	}
}

func TestClientAssertion_ReplayedJTI(t *testing.T) {
	f := newPKFixture(t)
	a := sign(t, f.priv, jose.ES256, "k1", nil)
	if resp, err := f.resolve(a); err != nil || !resp.Valid {
		t.Fatalf("first use: want valid, got %+v %v", resp, err)
	}
	if resp, err := f.resolve(a); err != nil || resp.Valid || !f.denied("replayed_jti") {
		t.Fatalf("replay: want Valid:false with replayed_jti DENY, got %+v %v", resp, err)
	}
}

func TestClientAssertion_RotationRetiresOldKey(t *testing.T) {
	f := newPKFixture(t)
	edPub, edPriv, _ := ed25519.GenerateKey(rand.Reader)
	jwk, _ := json.Marshal(jose.JSONWebKey{Key: edPub, KeyID: "k2", Algorithm: string(jose.EdDSA)})
	f.register(string(jwk), "")
	if resp, err := f.resolve(sign(t, edPriv, jose.EdDSA, "k2", nil)); err != nil || !resp.Valid {
		t.Fatalf("new EdDSA key: want valid, got %+v %v", resp, err)
	}
	ctx := ctxWithIdentity(apiKeyTestTenantUUID, "admin@example.com")
	if _, err := f.north.RetireAgentPublicKey(ctx, &brokerv1.RetireAgentPublicKeyRequest{
		TenantId: apiKeyTestTenantUUID, AgentId: apiKeyTestAgentUUID, KeyId: f.keyRow.Id,
	}); err != nil {
		t.Fatalf("RetireAgentPublicKey: %v", err)
	}
	if resp, err := f.resolve(sign(t, f.priv, jose.ES256, "k1", nil)); err != nil || resp.Valid || !f.denied("key_retired") {
		t.Fatalf("retired key: want Valid:false with key_retired DENY, got %+v %v", resp, err)
	}
	list, err := f.north.ListAgentPublicKeys(ctx, &brokerv1.ListAgentPublicKeysRequest{TenantId: apiKeyTestTenantUUID, AgentId: apiKeyTestAgentUUID})
	if err != nil || len(list.Keys) != 1 || list.Keys[0].Kid != "k2" || list.Keys[0].Alg != "EdDSA" || list.MaxTtlDays != 90 {
		t.Fatalf("list after rotation = %+v %v, want only k2", list, err)
	}
	var registered, retired bool
	for _, ev := range f.aud.events {
		registered = registered || ev.EventType == "aikonos.broker.agentkey.registered"
		retired = retired || ev.EventType == "aikonos.broker.agentkey.retired"
	}
	if !registered || !retired {
		t.Fatalf("want registered and retired audit events, got %v", f.aud.events)
	}
}

func TestClientAssertion_AlgMismatchAndExpiredKey(t *testing.T) {
	f := newPKFixture(t)
	// An Ed25519 signature presented under the ES256 key's kid.
	_, edPriv, _ := ed25519.GenerateKey(rand.Reader)
	if resp, _ := f.resolve(sign(t, edPriv, jose.EdDSA, "k1", nil)); resp.Valid || !f.denied("alg_mismatch") {
		t.Fatalf("want alg_mismatch, got %+v", resp)
	}
	f.deps.PublicKeys.(*fakePublicKeyStore).keys[0].ExpiresAt = time.Now().Add(-time.Minute)
	if resp, _ := f.resolve(sign(t, f.priv, jose.ES256, "k1", nil)); resp.Valid || !f.denied("key_expired") {
		t.Fatalf("want key_expired, got %+v", resp)
	}
}

// The assertion runs the same agent checks as a tk_ key: AG-03 status and the
// AG-17 user assertion.
func TestClientAssertion_SharesAgentChecks(t *testing.T) {
	f := newPKFixture(t)
	rev := newFakeRevStore()
	f.south.deps.Revocation = revocation.New(rev)
	f.south.deps.UserAssertions = fakeAssertions{id: &auth.Identity{Subject: "alice", TenantID: apiKeyTestTenantUUID}}
	f.south.deps.Policy = f.deps.Policy

	// The fake FGA in newApiKeyDeps answers can_use with false: subject refused.
	_, err := f.south.ResolveAgentApiKey(sandboxCtx(), &brokerv1.ResolveAgentApiKeyRequest{
		ClientAssertion: sign(t, f.priv, jose.ES256, "k1", nil), TenantId: apiKeyTestTenantUUID, Audience: testAudience, UserAssertion: "tok",
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("user assertion without can_use: want PermissionDenied, got %v", err)
	}

	rev.agentSt[apiKeyTestAgentUUID] = db.AgentSuspended
	f.south.deps.Revocation = revocation.New(rev) // fresh cache
	if _, err := f.resolve(sign(t, f.priv, jose.ES256, "k1", nil)); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("suspended agent: want PermissionDenied, got %v", err)
	}
}

func TestClientAssertion_UnconfiguredAudienceRefuses(t *testing.T) {
	f := newPKFixture(t)
	resp, err := f.south.ResolveAgentApiKey(sandboxCtx(), &brokerv1.ResolveAgentApiKeyRequest{
		ClientAssertion: sign(t, f.priv, jose.ES256, "k1", nil), TenantId: apiKeyTestTenantUUID,
	})
	if err != nil || resp.Valid {
		t.Fatalf("empty audience: want Valid:false, got %+v %v", resp, err)
	}
}

func TestRegisterAgentPublicKey_Validation(t *testing.T) {
	f := newPKFixture(t)
	ctx := ctxWithIdentity(apiKeyTestTenantUUID, "admin@example.com")
	ecPriv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	privJWK, _ := json.Marshal(jose.JSONWebKey{Key: ecPriv})
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	p384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	good := pemOf(t, &ecPriv.PublicKey)
	cases := []struct {
		name string
		key  string
		kid  string
		ttl  int32
		want codes.Code
	}{
		{"private JWK", string(privJWK), "", 30, codes.InvalidArgument},
		{"RSA", pemOf(t, &rsaKey.PublicKey), "", 30, codes.InvalidArgument},
		{"P-384", pemOf(t, &p384.PublicKey), "", 30, codes.InvalidArgument},
		{"garbage", "hello", "", 30, codes.InvalidArgument},
		{"ttl over max", good, "", 91, codes.InvalidArgument},
		{"ttl zero", good, "", 0, codes.InvalidArgument},
		{"kid taken", good, "k1", 30, codes.AlreadyExists},
		{"default kid is the thumbprint", good, "", 30, codes.OK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := f.north.RegisterAgentPublicKey(ctx, &brokerv1.RegisterAgentPublicKeyRequest{
				TenantId: apiKeyTestTenantUUID, AgentId: apiKeyTestAgentUUID, PublicKey: tc.key, Kid: tc.kid, TtlDays: tc.ttl,
			})
			if status.Code(err) != tc.want {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
			if tc.want == codes.OK && (len(resp.Key.Kid) != 43 || resp.Key.Alg != "ES256") {
				t.Fatalf("want a 43-char thumbprint kid and ES256, got %+v", resp.Key)
			}
		})
	}
	// Non-admins may not register keys.
	if _, err := f.north.RegisterAgentPublicKey(ctxWithIdentity(apiKeyTestTenantUUID, "eve@example.com"), &brokerv1.RegisterAgentPublicKeyRequest{
		TenantId: apiKeyTestTenantUUID, AgentId: apiKeyTestAgentUUID, PublicKey: good, Kid: "eve", TtlDays: 30,
	}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-admin: want PermissionDenied, got %v", err)
	}
}

func TestJTICache_ExpiresEntries(t *testing.T) {
	var c jtiCache
	now := time.Now()
	if !c.claim("a", now.Add(time.Minute), now) || c.claim("a", now.Add(time.Minute), now) {
		t.Fatal("first claim new, second a replay")
	}
	if !c.claim("a", now.Add(3*time.Minute), now.Add(2*time.Minute)) {
		t.Fatal("an expired entry may be claimed again")
	}
}
