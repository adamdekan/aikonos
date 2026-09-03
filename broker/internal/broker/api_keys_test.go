package broker

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/apikey"
	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/auth"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/gatewaygrant"
	"github.com/adamdekan/aikonos/broker/internal/policy"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// ── fakeApiKeyStore ───────────────────────────────────────────────────────────

type fakeApiKeyStore struct {
	mu   sync.Mutex
	keys []*db.StoredApiKey
}

func (f *fakeApiKeyStore) Create(_ context.Context, k *db.StoredApiKey) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys = append(f.keys, k)
	return nil
}

func (f *fakeApiKeyStore) ListByAgent(_ context.Context, tenantID, agentID string) ([]*db.StoredApiKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*db.StoredApiKey
	for _, k := range f.keys {
		if k.TenantID.String() == tenantID && k.AgentID.String() == agentID && !k.Revoked() {
			out = append(out, k)
		}
	}
	return out, nil
}

func (f *fakeApiKeyStore) Revoke(_ context.Context, tenantID, agentID, keyID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, k := range f.keys {
		if k.ID.String() == keyID && k.TenantID.String() == tenantID && k.AgentID.String() == agentID && !k.Revoked() {
			now := time.Now()
			k.RevokedAt = &now
			return nil
		}
	}
	return db.ErrApiKeyNotFound
}

func (f *fakeApiKeyStore) Resolve(_ context.Context, keyHash string) (*db.StoredApiKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, k := range f.keys {
		if k.KeyHash == keyHash && !k.Revoked() {
			return k, nil
		}
	}
	return nil, db.ErrApiKeyNotFound
}

// explodingApiKeyStore fails the test if any method is called — used to prove
// a non-uuid agent_id is rejected before reaching the store.
type explodingApiKeyStore struct {
	t *testing.T
}

func (e *explodingApiKeyStore) Create(context.Context, *db.StoredApiKey) error {
	e.t.Fatal("Create must not be called for a non-uuid agent_id")
	return nil
}
func (e *explodingApiKeyStore) ListByAgent(context.Context, string, string) ([]*db.StoredApiKey, error) {
	e.t.Fatal("ListByAgent must not be called for a non-uuid agent_id")
	return nil, nil
}
func (e *explodingApiKeyStore) Revoke(context.Context, string, string, string) error {
	e.t.Fatal("Revoke must not be called for a non-uuid agent_id")
	return nil
}
func (e *explodingApiKeyStore) Resolve(context.Context, string) (*db.StoredApiKey, error) {
	e.t.Fatal("Resolve must not be called for a non-uuid agent_id")
	return nil, nil
}

// ── test helpers ─────────────────────────────────────────────────────────────

const apiKeyTestTenantUUID = "11111111-0000-0000-0000-000000000001"
const apiKeyTestAgentUUID = "22222222-0000-0000-0000-000000000002"

func newApiKeyDeps(t *testing.T, adminUser string, keyStore ApiKeyStore, agentStore AgentStore) Deps {
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
	return Deps{
		Logger:          zap.NewNop(),
		Policy:          eng,
		TenantID:        "aikonos-dev",
		Audit:           em,
		ApiKeys:         keyStore,
		Agents:          agentStore,
		ApiKeyPepper:    "test-pepper",
		GatewayGrantKey: testGrantKey,
		GatewayGrantTTL: testGrantTTL,
	}
}

func newApiKeyDepsWithFGA(t *testing.T, adminUser string, fga *fakeFGA, keyStore ApiKeyStore, agentStore AgentStore) Deps {
	t.Helper()
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
	return Deps{
		Logger:          zap.NewNop(),
		Policy:          eng,
		TenantID:        "aikonos-dev",
		Audit:           em,
		ApiKeys:         keyStore,
		Agents:          agentStore,
		ApiKeyPepper:    "test-pepper",
		GatewayGrantKey: testGrantKey,
		GatewayGrantTTL: testGrantTTL,
	}
}

func agentStoreWithAgent(t *testing.T) AgentStore {
	t.Helper()
	tid, _ := uuid.Parse(apiKeyTestTenantUUID)
	aid, _ := uuid.Parse(apiKeyTestAgentUUID)
	store := &fakeAgentStore{}
	store.agents = []*db.Agent{{
		ID:           aid,
		TenantID:     tid,
		Name:         "test-agent",
		ApprovalMode: "auto",
		Skills:       []string{"doc.read"},
	}}
	return store
}

func sandboxCtx() context.Context {
	return auth.WithIdentity(context.Background(), &auth.Identity{
		SpiffeID: "spiffe://aikonos.com/agent-gateway",
	})
}

// ── tests ─────────────────────────────────────────────────────────────────────

// TestMintAgentApiKey_HappyPath: mint succeeds, raw key starts with tk_, FGA tuple written.
func TestMintAgentApiKey_HappyPath(t *testing.T) {
	fga := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	keyStore := &fakeApiKeyStore{}
	deps := newApiKeyDepsWithFGA(t, "admin@example.com", fga, keyStore, agentStoreWithAgent(t))
	svc := NewBrokerService(deps)

	ctx := ctxWithIdentity(apiKeyTestTenantUUID, "admin@example.com")
	resp, err := svc.MintAgentApiKey(ctx, &brokerv1.MintAgentApiKeyRequest{
		TenantId: apiKeyTestTenantUUID,
		UserId:   "admin@example.com",
		AgentId:  apiKeyTestAgentUUID,
		Label:    "my key",
	})
	if err != nil {
		t.Fatalf("MintAgentApiKey: %v", err)
	}
	if !strings.HasPrefix(resp.RawKey, "tk_") {
		t.Errorf("raw key must start with tk_, got %q", resp.RawKey[:min(len(resp.RawKey), 10)])
	}
	if resp.Key == nil {
		t.Fatal("response.Key must be non-nil")
	}
	if resp.Key.Label != "my key" {
		t.Errorf("label mismatch: want 'my key', got %q", resp.Key.Label)
	}
	// Raw key must NOT appear in any stored key hash.
	keyStore.mu.Lock()
	defer keyStore.mu.Unlock()
	for _, k := range keyStore.keys {
		if k.KeyHash == resp.RawKey {
			t.Error("stored key_hash must not equal the raw key")
		}
	}
}

// TestMintAgentApiKey_WritesUsableByTuple: first mint writes agent:<id> usable_by user:svc-<id>.
func TestMintAgentApiKey_WritesUsableByTuple(t *testing.T) {
	fga := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	keyStore := &fakeApiKeyStore{}
	deps := newApiKeyDepsWithFGA(t, "admin@example.com", fga, keyStore, agentStoreWithAgent(t))
	svc := NewBrokerService(deps)

	ctx := ctxWithIdentity(apiKeyTestTenantUUID, "admin@example.com")
	_, err := svc.MintAgentApiKey(ctx, &brokerv1.MintAgentApiKeyRequest{
		TenantId: apiKeyTestTenantUUID,
		UserId:   "admin@example.com",
		AgentId:  apiKeyTestAgentUUID,
	})
	if err != nil {
		t.Fatalf("MintAgentApiKey: %v", err)
	}
	fga.mu.Lock()
	writes := fga.writes
	fga.mu.Unlock()

	wantUser := "user:svc-" + apiKeyTestAgentUUID
	wantObject := "agent:" + apiKeyTestAgentUUID
	var found bool
	for _, w := range writes {
		if writes_contains(w, wantUser, "usable_by", wantObject) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected FGA write {user:%s, relation:usable_by, object:%s}, got %v",
			wantUser, wantObject, writes)
	}
}

// TestMintAgentApiKey_TenantAdminGated: non-admin gets PermissionDenied.
func TestMintAgentApiKey_TenantAdminGated(t *testing.T) {
	keyStore := &fakeApiKeyStore{}
	deps := newApiKeyDeps(t, "admin@example.com", keyStore, agentStoreWithAgent(t))
	svc := NewBrokerService(deps)

	ctx := ctxWithIdentity(apiKeyTestTenantUUID, "nonadmin@example.com")
	_, err := svc.MintAgentApiKey(ctx, &brokerv1.MintAgentApiKeyRequest{
		TenantId: apiKeyTestTenantUUID,
		UserId:   "nonadmin@example.com",
		AgentId:  apiKeyTestAgentUUID,
	})
	if err == nil {
		t.Fatal("want PermissionDenied for non-admin, got nil")
	}
	if code := status.Code(err); code != codes.PermissionDenied {
		t.Errorf("want PermissionDenied, got %v", code)
	}
}

// TestMintAgentApiKey_EmptyPepper: pepper not configured → FailedPrecondition.
func TestMintAgentApiKey_EmptyPepper(t *testing.T) {
	fga := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	fgaSrv := fga.server(t)
	t.Cleanup(fgaSrv.Close)
	em, _ := audit.NewEmitter(context.Background(), audit.Config{})
	eng, _ := policy.NewEngine(context.Background(), policy.Config{
		OPAEndpoint:     "http://unused",
		OpenFGAEndpoint: fgaSrv.URL,
		OpenFGAStoreID:  "store-1",
	})
	svc := NewBrokerService(Deps{
		Logger:       zap.NewNop(),
		Policy:       eng,
		TenantID:     "aikonos-dev",
		Audit:        em,
		ApiKeys:      &fakeApiKeyStore{},
		Agents:       agentStoreWithAgent(t),
		ApiKeyPepper: "", // intentionally empty
	})
	ctx := ctxWithIdentity(apiKeyTestTenantUUID, "admin@example.com")
	_, err := svc.MintAgentApiKey(ctx, &brokerv1.MintAgentApiKeyRequest{
		TenantId: apiKeyTestTenantUUID,
		UserId:   "admin@example.com",
		AgentId:  apiKeyTestAgentUUID,
	})
	if err == nil {
		t.Fatal("want FailedPrecondition when pepper empty, got nil")
	}
	if code := status.Code(err); code != codes.FailedPrecondition {
		t.Errorf("want FailedPrecondition, got %v", code)
	}
}

// TestMintAgentApiKey_NonUUIDAgentID: a syntactically non-uuid agent_id must
// be rejected as InvalidArgument before the Agents.Get store read (which
// would otherwise 500 with a raw 22P02 from Postgres).
func TestMintAgentApiKey_NonUUIDAgentID(t *testing.T) {
	fga := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	deps := newApiKeyDepsWithFGA(t, "admin@example.com", fga, &fakeApiKeyStore{}, &explodingAgentStore{t: t})
	svc := NewBrokerService(deps)

	ctx := ctxWithIdentity(apiKeyTestTenantUUID, "admin@example.com")
	_, err := svc.MintAgentApiKey(ctx, &brokerv1.MintAgentApiKeyRequest{
		TenantId: apiKeyTestTenantUUID,
		UserId:   "admin@example.com",
		AgentId:  "alice-agent",
	})
	if err == nil {
		t.Fatal("want InvalidArgument, got nil")
	}
	if code := status.Code(err); code != codes.InvalidArgument {
		t.Errorf("non-uuid agent_id: want InvalidArgument, got %v", code)
	}
}

// TestListAgentApiKeys_NonUUIDAgentID: a syntactically non-uuid agent_id must
// be rejected as NotFound before the store read.
func TestListAgentApiKeys_NonUUIDAgentID(t *testing.T) {
	fga := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	deps := newApiKeyDepsWithFGA(t, "admin@example.com", fga, &explodingApiKeyStore{t: t}, agentStoreWithAgent(t))
	svc := NewBrokerService(deps)

	ctx := ctxWithIdentity(apiKeyTestTenantUUID, "admin@example.com")
	_, err := svc.ListAgentApiKeys(ctx, &brokerv1.ListAgentApiKeysRequest{
		TenantId: apiKeyTestTenantUUID,
		UserId:   "admin@example.com",
		AgentId:  "alice-agent",
	})
	if err == nil {
		t.Fatal("want NotFound, got nil")
	}
	if code := status.Code(err); code != codes.NotFound {
		t.Errorf("non-uuid agent_id: want NotFound, got %v", code)
	}
}

// TestRevokeAgentApiKey_NonUUIDAgentID: a syntactically non-uuid agent_id
// must be rejected as NotFound before the store read.
func TestRevokeAgentApiKey_NonUUIDAgentID(t *testing.T) {
	fga := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	deps := newApiKeyDepsWithFGA(t, "admin@example.com", fga, &explodingApiKeyStore{t: t}, agentStoreWithAgent(t))
	svc := NewBrokerService(deps)

	ctx := ctxWithIdentity(apiKeyTestTenantUUID, "admin@example.com")
	_, err := svc.RevokeAgentApiKey(ctx, &brokerv1.RevokeAgentApiKeyRequest{
		TenantId: apiKeyTestTenantUUID,
		UserId:   "admin@example.com",
		AgentId:  "alice-agent",
		KeyId:    "some-key-id",
	})
	if err == nil {
		t.Fatal("want NotFound, got nil")
	}
	if code := status.Code(err); code != codes.NotFound {
		t.Errorf("non-uuid agent_id: want NotFound, got %v", code)
	}
}

// TestRevokeAgentApiKey_EmptyKeyIdPrecedesUUIDGuard: an empty key_id combined
// with a non-uuid agent_id must report the key_id requiredness error, not the
// agent-not-found uuid guard — key_id validation runs first.
func TestRevokeAgentApiKey_EmptyKeyIdPrecedesUUIDGuard(t *testing.T) {
	fga := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	deps := newApiKeyDepsWithFGA(t, "admin@example.com", fga, &explodingApiKeyStore{t: t}, agentStoreWithAgent(t))
	svc := NewBrokerService(deps)

	ctx := ctxWithIdentity(apiKeyTestTenantUUID, "admin@example.com")
	_, err := svc.RevokeAgentApiKey(ctx, &brokerv1.RevokeAgentApiKeyRequest{
		TenantId: apiKeyTestTenantUUID,
		UserId:   "admin@example.com",
		AgentId:  "alice-agent",
		KeyId:    "",
	})
	if err == nil {
		t.Fatal("want InvalidArgument, got nil")
	}
	if code := status.Code(err); code != codes.InvalidArgument {
		t.Errorf("empty key_id + non-uuid agent_id: want InvalidArgument, got %v", code)
	}
}

// TestResolveAgentApiKey_HappyPath: mint then resolve returns valid+agentId+svc- principal.
func TestResolveAgentApiKey_HappyPath(t *testing.T) {
	fga := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	keyStore := &fakeApiKeyStore{}
	deps := newApiKeyDepsWithFGA(t, "admin@example.com", fga, keyStore, agentStoreWithAgent(t))
	northSvc := NewBrokerService(deps)
	southSvc := NewSandboxService(deps)

	ctx := ctxWithIdentity(apiKeyTestTenantUUID, "admin@example.com")
	mintResp, err := northSvc.MintAgentApiKey(ctx, &brokerv1.MintAgentApiKeyRequest{
		TenantId: apiKeyTestTenantUUID,
		UserId:   "admin@example.com",
		AgentId:  apiKeyTestAgentUUID,
	})
	if err != nil {
		t.Fatalf("MintAgentApiKey: %v", err)
	}

	hashHex := apikey.HashForResolve(mintResp.RawKey)
	sCtx := sandboxCtx()
	sCtx = auth.WithIdentity(sCtx, &auth.Identity{
		SpiffeID: "spiffe://aikonos.com/agent-gateway",
	})
	resolveResp, err := southSvc.ResolveAgentApiKey(sCtx, &brokerv1.ResolveAgentApiKeyRequest{
		KeyHashHex: hashHex,
		TenantId:   apiKeyTestTenantUUID,
	})
	if err != nil {
		t.Fatalf("ResolveAgentApiKey: %v", err)
	}
	if !resolveResp.Valid {
		t.Fatal("expected valid=true")
	}
	if resolveResp.AgentId != apiKeyTestAgentUUID {
		t.Errorf("agentId: want %s, got %s", apiKeyTestAgentUUID, resolveResp.AgentId)
	}
	wantPrincipal := "svc-" + apiKeyTestAgentUUID
	if resolveResp.Principal != wantPrincipal {
		t.Errorf("principal: want %s, got %s", wantPrincipal, resolveResp.Principal)
	}
}

// TestResolveAgentApiKey_UnknownKey: unknown hash → valid=false, no error.
func TestResolveAgentApiKey_UnknownKey(t *testing.T) {
	fga := &fakeFGA{admins: map[string]bool{}}
	keyStore := &fakeApiKeyStore{}
	deps := newApiKeyDepsWithFGA(t, "admin@example.com", fga, keyStore, agentStoreWithAgent(t))
	southSvc := NewSandboxService(deps)

	sCtx := auth.WithIdentity(context.Background(), &auth.Identity{
		SpiffeID: "spiffe://aikonos.com/agent-gateway",
	})
	resp, err := southSvc.ResolveAgentApiKey(sCtx, &brokerv1.ResolveAgentApiKeyRequest{
		KeyHashHex: "aabbcc",
		TenantId:   apiKeyTestTenantUUID,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Valid {
		t.Error("expected valid=false for unknown key")
	}
}

// TestResolveAgentApiKey_RevokedKey: revoke then resolve → valid=false.
func TestResolveAgentApiKey_RevokedKey(t *testing.T) {
	fga := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	keyStore := &fakeApiKeyStore{}
	deps := newApiKeyDepsWithFGA(t, "admin@example.com", fga, keyStore, agentStoreWithAgent(t))
	northSvc := NewBrokerService(deps)
	southSvc := NewSandboxService(deps)

	ctx := ctxWithIdentity(apiKeyTestTenantUUID, "admin@example.com")
	mintResp, err := northSvc.MintAgentApiKey(ctx, &brokerv1.MintAgentApiKeyRequest{
		TenantId: apiKeyTestTenantUUID,
		UserId:   "admin@example.com",
		AgentId:  apiKeyTestAgentUUID,
	})
	if err != nil {
		t.Fatalf("MintAgentApiKey: %v", err)
	}

	_, err = northSvc.RevokeAgentApiKey(ctx, &brokerv1.RevokeAgentApiKeyRequest{
		TenantId: apiKeyTestTenantUUID,
		UserId:   "admin@example.com",
		AgentId:  apiKeyTestAgentUUID,
		KeyId:    mintResp.Key.Id,
	})
	if err != nil {
		t.Fatalf("RevokeAgentApiKey: %v", err)
	}

	hashHex := apikey.HashForResolve(mintResp.RawKey)
	sCtx := auth.WithIdentity(context.Background(), &auth.Identity{
		SpiffeID: "spiffe://aikonos.com/agent-gateway",
	})
	resp, err := southSvc.ResolveAgentApiKey(sCtx, &brokerv1.ResolveAgentApiKeyRequest{
		KeyHashHex: hashHex,
		TenantId:   apiKeyTestTenantUUID,
	})
	if err != nil {
		t.Fatalf("unexpected error after revoke: %v", err)
	}
	if resp.Valid {
		t.Error("expected valid=false for revoked key")
	}
}

// TestResolveAgentApiKey_CrossTenant: key exists but wrong tenant_id → valid=false.
func TestResolveAgentApiKey_CrossTenant(t *testing.T) {
	fga := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	keyStore := &fakeApiKeyStore{}
	deps := newApiKeyDepsWithFGA(t, "admin@example.com", fga, keyStore, agentStoreWithAgent(t))
	northSvc := NewBrokerService(deps)
	southSvc := NewSandboxService(deps)

	ctx := ctxWithIdentity(apiKeyTestTenantUUID, "admin@example.com")
	mintResp, err := northSvc.MintAgentApiKey(ctx, &brokerv1.MintAgentApiKeyRequest{
		TenantId: apiKeyTestTenantUUID,
		UserId:   "admin@example.com",
		AgentId:  apiKeyTestAgentUUID,
	})
	if err != nil {
		t.Fatalf("MintAgentApiKey: %v", err)
	}

	hashHex := apikey.HashForResolve(mintResp.RawKey)
	sCtx := auth.WithIdentity(context.Background(), &auth.Identity{
		SpiffeID: "spiffe://aikonos.com/agent-gateway",
	})
	// Present a *different* tenant_id in the resolve request.
	resp, err := southSvc.ResolveAgentApiKey(sCtx, &brokerv1.ResolveAgentApiKeyRequest{
		KeyHashHex: hashHex,
		TenantId:   "99999999-0000-0000-0000-000000000000",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Valid {
		t.Error("cross-tenant resolve must return valid=false")
	}
}

// TestResolveAgentApiKey_NonGatewayPeer: non-SPIFFE caller → PermissionDenied.
func TestResolveAgentApiKey_NonGatewayPeer(t *testing.T) {
	fga := &fakeFGA{admins: map[string]bool{}}
	keyStore := &fakeApiKeyStore{}
	deps := newApiKeyDepsWithFGA(t, "admin@example.com", fga, keyStore, agentStoreWithAgent(t))
	southSvc := NewSandboxService(deps)

	// No SpiffeID → requireGatewayPeer fails.
	_, err := southSvc.ResolveAgentApiKey(context.Background(), &brokerv1.ResolveAgentApiKeyRequest{
		KeyHashHex: "aabbcc",
		TenantId:   apiKeyTestTenantUUID,
	})
	if err == nil {
		t.Fatal("want PermissionDenied for non-gateway peer, got nil")
	}
	if code := status.Code(err); code != codes.PermissionDenied {
		t.Errorf("want PermissionDenied, got %v", code)
	}
}

// TestMintAgentApiKey_RawKeyNotLogged: the raw key must not appear in any
// captured log entry (message or field values). Uses zaptest/observer to
// intercept all log output at DebugLevel so no accidental log line is missed.
func TestMintAgentApiKey_RawKeyNotLogged(t *testing.T) {
	core, recorded := observer.New(zapcore.DebugLevel)
	obsLogger := zap.New(core)

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
	keyStore := &fakeApiKeyStore{}
	deps := Deps{
		Logger:       obsLogger,
		Policy:       eng,
		TenantID:     "aikonos-dev",
		Audit:        em,
		ApiKeys:      keyStore,
		Agents:       agentStoreWithAgent(t),
		ApiKeyPepper: "test-pepper",
	}
	svc := NewBrokerService(deps)

	ctx := ctxWithIdentity(apiKeyTestTenantUUID, "admin@example.com")
	resp, err := svc.MintAgentApiKey(ctx, &brokerv1.MintAgentApiKeyRequest{
		TenantId: apiKeyTestTenantUUID,
		UserId:   "admin@example.com",
		AgentId:  apiKeyTestAgentUUID,
	})
	if err != nil {
		t.Fatalf("MintAgentApiKey: %v", err)
	}

	rawKey := resp.RawKey
	for _, entry := range recorded.All() {
		if strings.Contains(entry.Message, rawKey) {
			t.Errorf("raw key found in log message: %q", entry.Message)
		}
		for _, f := range entry.Context {
			if strings.Contains(f.Key, rawKey) {
				t.Errorf("raw key found in log field key: %q", f.Key)
			}
			if f.Type == zapcore.StringType && strings.Contains(f.String, rawKey) {
				t.Errorf("raw key found in log field %q value: %q", f.Key, f.String)
			}
		}
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

// writes_contains checks whether a single fakeFGA writes entry (a map[string]any
// with keys "user", "relation", "object") matches the given triple.
func writes_contains(w map[string]any, user, relation, object string) bool {
	return w["user"] == user && w["relation"] == relation && w["object"] == object
}

// TestCallerIdentity_SvcUserIdRejected: callerIdentity must reject a request
// whose user_id starts with "svc-" — service principals must not impersonate
// human callers on the north-bound surface.
func TestCallerIdentity_SvcUserIdRejected(t *testing.T) {
	keyStore := &fakeApiKeyStore{}
	deps := newApiKeyDeps(t, "admin@example.com", keyStore, agentStoreWithAgent(t))
	svc := NewBrokerService(deps)

	// Send a request body user_id that starts with svc- but no authenticated identity.
	// In dev-mode (no OIDC interceptor) callerIdentity falls through to the reqUser branch.
	_, err := svc.ListAgentApiKeys(context.Background(), &brokerv1.ListAgentApiKeysRequest{
		TenantId: apiKeyTestTenantUUID,
		UserId:   "svc-some-agent",
		AgentId:  apiKeyTestAgentUUID,
	})
	if err == nil {
		t.Fatal("want error for svc- user_id, got nil")
	}
	if code := status.Code(err); code != codes.PermissionDenied {
		t.Errorf("want PermissionDenied for svc- user_id, got %v", code)
	}
}

// ── CP2: ResolveAgentApiKey mints an owner grant ─────────────────────────────

// TestResolveAgentApiKey_GrantVerifiesToSvcPrincipal proves that a successful
// resolve returns an owner-grant that Verify→"svc-<agentId>".
// This is the transport-level proof that the external API path satisfies the
// CP2 mint contract (gateway can thread the grant into CreateGatewayTask).
func TestResolveAgentApiKey_GrantVerifiesToSvcPrincipal(t *testing.T) {
	fga := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	keyStore := &fakeApiKeyStore{}
	deps := newApiKeyDepsWithFGA(t, "admin@example.com", fga, keyStore, agentStoreWithAgent(t))
	// Wire the grant key so Mint works.
	deps.GatewayGrantKey = testGrantKey
	deps.GatewayGrantTTL = testGrantTTL
	northSvc := NewBrokerService(deps)
	southSvc := NewSandboxService(deps)

	ctx := ctxWithIdentity(apiKeyTestTenantUUID, "admin@example.com")
	mintResp, err := northSvc.MintAgentApiKey(ctx, &brokerv1.MintAgentApiKeyRequest{
		TenantId: apiKeyTestTenantUUID,
		UserId:   "admin@example.com",
		AgentId:  apiKeyTestAgentUUID,
	})
	if err != nil {
		t.Fatalf("MintAgentApiKey: %v", err)
	}

	hashHex := apikey.HashForResolve(mintResp.RawKey)
	sCtx := auth.WithIdentity(context.Background(), &auth.Identity{
		SpiffeID: "spiffe://aikonos.com/agent-gateway",
	})
	resolveResp, err := southSvc.ResolveAgentApiKey(sCtx, &brokerv1.ResolveAgentApiKeyRequest{
		KeyHashHex: hashHex,
		TenantId:   apiKeyTestTenantUUID,
	})
	if err != nil {
		t.Fatalf("ResolveAgentApiKey: %v", err)
	}
	if !resolveResp.Valid {
		t.Fatal("expected valid=true")
	}
	if resolveResp.OwnerGrant == "" {
		t.Fatal("OwnerGrant must be non-empty on a valid resolve")
	}
	grantTenant, grantOwner, verifyErr := gatewaygrant.Verify(testGrantKey, resolveResp.OwnerGrant)
	if verifyErr != nil {
		t.Fatalf("Verify owner grant: %v", verifyErr)
	}
	if grantTenant != apiKeyTestTenantUUID {
		t.Errorf("grant tenant = %q, want %q", grantTenant, apiKeyTestTenantUUID)
	}
	wantOwner := "svc-" + apiKeyTestAgentUUID
	if grantOwner != wantOwner {
		t.Errorf("grant owner = %q, want %q", grantOwner, wantOwner)
	}
}

// TestMintAgentApiKey_FGAWriteErr: FGA /write returns 500 → FailedPrecondition,
// no key persisted.
func TestMintAgentApiKey_FGAWriteErr(t *testing.T) {
	fga := &fakeFGA{
		admins:   map[string]bool{"user:admin@example.com": true},
		writeErr: true,
	}
	keyStore := &fakeApiKeyStore{}
	deps := newApiKeyDepsWithFGA(t, "admin@example.com", fga, keyStore, agentStoreWithAgent(t))
	svc := NewBrokerService(deps)

	ctx := ctxWithIdentity(apiKeyTestTenantUUID, "admin@example.com")
	_, err := svc.MintAgentApiKey(ctx, &brokerv1.MintAgentApiKeyRequest{
		TenantId: apiKeyTestTenantUUID,
		UserId:   "admin@example.com",
		AgentId:  apiKeyTestAgentUUID,
		Label:    "should not persist",
	})
	if err == nil {
		t.Fatal("want error when FGA write fails, got nil")
	}
	if code := status.Code(err); code != codes.FailedPrecondition {
		t.Errorf("want FailedPrecondition, got %v", code)
	}

	// No key must have been persisted.
	keyStore.mu.Lock()
	n := len(keyStore.keys)
	keyStore.mu.Unlock()
	if n != 0 {
		t.Errorf("want 0 persisted keys after FGA failure, got %d", n)
	}
}

// ── sentinel to satisfy compiler ─────────────────────────────────────────────
var _ = errors.New // ensure errors imported

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
