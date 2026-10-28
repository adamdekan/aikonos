package broker

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/apikey"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/revocation"
	"github.com/adamdekan/aikonos/broker/internal/scim"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// fakeScimStore keeps tokens and users; group methods come from the nil
// embedded Store and are not reached by these tests.
type fakeScimStore struct {
	scim.Store
	tokens  map[uuid.UUID]string // id → hash of an active token
	created []db.ScimToken
	users   []db.ScimUser
}

func newFakeScimStore() *fakeScimStore {
	return &fakeScimStore{tokens: map[uuid.UUID]string{}}
}

func (f *fakeScimStore) CreateToken(_ context.Context, _, hash, prefix, by string, replaces *uuid.UUID) (db.ScimToken, error) {
	if replaces != nil {
		if _, ok := f.tokens[*replaces]; !ok {
			return db.ScimToken{}, db.ErrNotFound
		}
		delete(f.tokens, *replaces)
	}
	t := db.ScimToken{ID: uuid.New(), Prefix: prefix, CreatedBy: by, CreatedAt: time.Now()}
	f.tokens[t.ID] = hash
	f.created = append(f.created, t)
	return t, nil
}

func (f *fakeScimStore) ListTokens(context.Context, string) ([]db.ScimToken, error) {
	var out []db.ScimToken
	for _, t := range f.created {
		if _, ok := f.tokens[t.ID]; ok {
			out = append(out, t)
		}
	}
	return out, nil
}

func (f *fakeScimStore) RevokeToken(_ context.Context, _ string, id uuid.UUID, _ string) error {
	if _, ok := f.tokens[id]; !ok {
		return db.ErrNotFound
	}
	delete(f.tokens, id)
	return nil
}

func (f *fakeScimStore) ResolveToken(_ context.Context, _, hash string) (uuid.UUID, error) {
	for id, h := range f.tokens {
		if h == hash {
			return id, nil
		}
	}
	return uuid.Nil, db.ErrNotFound
}

func (f *fakeScimStore) ListUsers(context.Context, string) ([]db.ScimUser, error) {
	return f.users, nil
}

func (f *fakeScimStore) CreateUser(_ context.Context, _ string, u *db.ScimUser) error {
	u.ID, u.CreatedAt, u.UpdatedAt = uuid.New(), time.Now(), time.Now()
	f.users = append(f.users, *u)
	return nil
}

func TestScimTokens_CreateRotateRevoke(t *testing.T) {
	const admin = "admin@example.com"
	deps := newApiKeyDeps(t, admin, &fakeApiKeyStore{}, agentStoreWithAgent(t))
	aud := &fakeInvokeAudit{}
	deps.Audit = aud
	store := newFakeScimStore()
	deps.Scim = store
	svc := NewBrokerService(deps)
	ctx := ctxWithIdentity(apiKeyTestTenantUUID, admin)

	if _, err := svc.CreateScimToken(ctxWithIdentity(apiKeyTestTenantUUID, "bob@example.com"), &brokerv1.CreateScimTokenRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-admin create: want PermissionDenied, got %v", err)
	}

	first, err := svc.CreateScimToken(ctx, &brokerv1.CreateScimTokenRequest{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !strings.HasPrefix(first.RawToken, "scim_") || len(first.RawToken) < 25 {
		t.Fatalf("raw token %q", first.RawToken)
	}
	// Only the peppered hash is kept.
	want, _ := apikey.PepperedHash("test-pepper", first.RawToken)
	if store.tokens[uuid.MustParse(first.Token.Id)] != want {
		t.Fatal("store does not hold the peppered hash")
	}
	list, err := svc.ListScimTokens(ctx, &brokerv1.ListScimTokensRequest{})
	if err != nil || len(list.Tokens) != 1 || list.Mode != "jit" {
		t.Fatalf("list: %v %v", list, err)
	}
	if b, _ := json.Marshal(list); strings.Contains(string(b), first.RawToken) {
		t.Fatal("list leaks the raw token")
	}

	second, err := svc.CreateScimToken(ctx, &brokerv1.CreateScimTokenRequest{ReplacesId: first.Token.Id})
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if _, ok := store.tokens[uuid.MustParse(first.Token.Id)]; ok {
		t.Fatal("rotation left the old token active")
	}
	if _, err := svc.CreateScimToken(ctx, &brokerv1.CreateScimTokenRequest{ReplacesId: first.Token.Id}); status.Code(err) != codes.NotFound {
		t.Fatalf("rotate a dead token: want NotFound, got %v", err)
	}
	if _, err := svc.RevokeScimToken(ctx, &brokerv1.RevokeScimTokenRequest{Id: second.Token.Id}); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if list, _ = svc.ListScimTokens(ctx, &brokerv1.ListScimTokensRequest{}); len(list.Tokens) != 0 {
		t.Fatalf("after revoke: %v", list.Tokens)
	}
	for _, ev := range []string{"aikonos.broker.scim.token.created", "aikonos.broker.scim.token.revoked"} {
		if e := findEvent(aud.events, ev); e == nil || e.ActorUserId != admin {
			t.Errorf("%s: %v", ev, e)
		}
	}
}

// ScimCall: a bad token is 401 and audited, a good token is refused under
// provisioning.mode=jit, and under scim an active=false user is blocked.
func TestScimCall_TokenModeAndBlock(t *testing.T) {
	const admin = "admin@example.com"
	deps := newApiKeyDeps(t, admin, &fakeApiKeyStore{}, agentStoreWithAgent(t))
	aud := &fakeInvokeAudit{}
	deps.Audit = aud
	cfg := newFakeConfigStore()
	deps.Config = cfg
	deps.Scim = newFakeScimStore()
	deps.Revocation = revocation.New(newFakeRevStore())
	north := NewBrokerService(deps)
	south := NewSandboxService(deps)

	tok, err := north.CreateScimToken(ctxWithIdentity(apiKeyTestTenantUUID, admin), &brokerv1.CreateScimTokenRequest{})
	if err != nil {
		t.Fatal(err)
	}
	call := func(token, method, path, body string) *brokerv1.ScimCallResponse {
		t.Helper()
		resp, err := south.ScimCall(sandboxCtx(), &brokerv1.ScimCallRequest{
			TenantId: apiKeyTestTenantUUID, TokenHashHex: apikey.HashForResolve(token),
			Method: method, Path: path, Body: []byte(body),
		})
		if err != nil {
			t.Fatalf("ScimCall: %v", err)
		}
		return resp
	}

	if r := call("scim_wrong", "GET", "/Users", ""); r.Status != 401 {
		t.Fatalf("bad token: %d", r.Status)
	}
	if e := findEvent(aud.events, "aikonos.broker.scim.auth.denied"); e == nil || e.Decision.String() != "DENY" {
		t.Fatalf("bad token not audited: %v", e)
	}
	if r := call(tok.RawToken, "GET", "/Users", ""); r.Status != 403 {
		t.Fatalf("jit mode: want 403, got %d %s", r.Status, r.Body)
	}

	cfg.values[apiKeyTestTenantUUID+":provisioning.mode"] = "scim"
	if r := call(tok.RawToken, "POST", "/Users", `{"userName":"carol@example.com","externalId":"oid-carol","active":false}`); r.Status != 201 {
		t.Fatalf("create: %d %s", r.Status, r.Body)
	}
	st, err := deps.Revocation.User(context.Background(), apiKeyTestTenantUUID, "oid-carol")
	if err != nil || !st.Blocked {
		t.Fatalf("active=false did not block: %v %v", st, err)
	}
	e := findEvent(aud.events, "aikonos.broker.scim.user.created")
	if e == nil || e.ActorUserId != "scim-token:"+tok.Token.Id {
		t.Fatalf("create audit: %v", e)
	}

	// A revoked token stops working at once.
	if _, err := north.RevokeScimToken(ctxWithIdentity(apiKeyTestTenantUUID, admin), &brokerv1.RevokeScimTokenRequest{Id: tok.Token.Id}); err != nil {
		t.Fatal(err)
	}
	if r := call(tok.RawToken, "GET", "/Users", ""); r.Status != 401 {
		t.Fatalf("revoked token: %d", r.Status)
	}
}

// Under provisioning.mode=scim the email-rule surface refuses new rules.
func TestAddProvisioningRule_RefusedUnderScim(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	deps := testProvisioningDeps(t, srv.URL, &fakeProvisioningStore{})
	cfg := newFakeConfigStore()
	cfg.values[testTenantUUID+":provisioning.mode"] = "scim"
	deps.Config = cfg
	svc := NewBrokerService(deps)
	_, err := svc.AddProvisioningRule(ctxWithIdentity(testTenantUUID, "admin@example.com"), &brokerv1.AddProvisioningRuleRequest{
		Matcher: "alice@example.com", Groups: []string{"security-team"},
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("want FailedPrecondition, got %v", err)
	}
}
