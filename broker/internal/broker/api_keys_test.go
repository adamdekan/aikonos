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
	"github.com/adamdekan/aikonos/broker/internal/revocation"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
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
		DevMode:         true, // south RPCs: any trust-domain peer, GatewaySpiffeID unset
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
		DevMode:         true, // south RPCs: any trust-domain peer, GatewaySpiffeID unset
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
		TtlDays:  30,
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
		TtlDays:  30,
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
		TtlDays:  30,
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
		TtlDays:  30,
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
		TtlDays:  30,
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
		TtlDays:  30,
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
		TtlDays:  30,
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
		TtlDays:  30,
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
		TtlDays:  30,
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
		TtlDays:  30,
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
	g, verifyErr := gatewaygrant.Verify(testGrantKey, resolveResp.OwnerGrant, false)
	if verifyErr != nil {
		t.Fatalf("Verify owner grant: %v", verifyErr)
	}
	if g.TenantID != apiKeyTestTenantUUID {
		t.Errorf("grant tenant = %q, want %q", g.TenantID, apiKeyTestTenantUUID)
	}
	wantOwner := "svc-" + apiKeyTestAgentUUID
	if g.Owner() != wantOwner || g.Subject != "" || g.Actor != apiKeyTestAgentUUID || g.Kind != gatewaygrant.KindExternal {
		t.Errorf("grant = %+v, want external, actor %s, owner %q", g, apiKeyTestAgentUUID, wantOwner)
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
		TtlDays:  30,
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

// ── AG-17 S5: a key must belong to a service agent ──────────────────────────

// resolveForAgent mints a key for the test agent (service flag as given) and
// resolves it under identity.obo=mode ("" = schema default, audit).
func resolveForAgent(t *testing.T, serviceAgent bool, mode string) (*brokerv1.ResolveAgentApiKeyResponse, *fakeInvokeAudit, error) {
	t.Helper()
	fga := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	agents := agentStoreWithAgent(t)
	agents.(*fakeAgentStore).agents[0].ServiceAgent = serviceAgent
	deps := newApiKeyDepsWithFGA(t, "admin@example.com", fga, &fakeApiKeyStore{}, agents)
	aud := &fakeInvokeAudit{}
	deps.Audit = aud
	if mode != "" {
		cfg := newFakeConfigStore()
		cfg.values[apiKeyTestTenantUUID+":identity.obo"] = mode
		deps.Config = cfg
	}
	mintResp, err := NewBrokerService(deps).MintAgentApiKey(ctxWithIdentity(apiKeyTestTenantUUID, "admin@example.com"), &brokerv1.MintAgentApiKeyRequest{
		TenantId: apiKeyTestTenantUUID, UserId: "admin@example.com", AgentId: apiKeyTestAgentUUID, TtlDays: 30,
	})
	if err != nil {
		t.Fatalf("MintAgentApiKey: %v", err)
	}
	resp, err := NewSandboxService(deps).ResolveAgentApiKey(sandboxCtx(), &brokerv1.ResolveAgentApiKeyRequest{
		KeyHashHex: apikey.HashForResolve(mintResp.RawKey), TenantId: apiKeyTestTenantUUID,
	})
	return resp, aud, err
}

func serviceAgentDenied(aud *fakeInvokeAudit) bool {
	for _, ev := range aud.events {
		if ev.EventType == "aikonos.broker.apikey.denied" && ev.Decision == auditv1.PolicyDecision_DENY &&
			ev.Context.Fields["reason"].GetStringValue() == "service_agent_required" {
			return true
		}
	}
	return false
}

func TestResolveAgentApiKey_NonServiceAgentRefusedUnderEnforce(t *testing.T) {
	resp, aud, err := resolveForAgent(t, false, "enforce")
	if status.Code(err) != codes.PermissionDenied || !strings.Contains(status.Convert(err).Message(), "service agent") {
		t.Fatalf("non-service agent under enforce: want PermissionDenied naming the cause, got resp=%+v err=%v", resp, err)
	}
	if !serviceAgentDenied(aud) {
		t.Fatalf("want service_agent_required DENY, got %v", aud.events)
	}
}

func TestResolveAgentApiKey_NonServiceAgentAuditOnly(t *testing.T) {
	resp, aud, err := resolveForAgent(t, false, "")
	if err != nil || !resp.Valid {
		t.Fatal("audit mode must keep pre-S5 keys working")
	}
	if serviceAgentDenied(aud) {
		t.Fatalf("audit mode must not write a DENY for a call it allows, got %v", aud.events)
	}
}

func TestResolveAgentApiKey_ServiceAgentAllowedUnderEnforce(t *testing.T) {
	resp, aud, err := resolveForAgent(t, true, "enforce")
	if err != nil || !resp.Valid {
		t.Fatal("service agent under enforce: want valid")
	}
	if serviceAgentDenied(aud) {
		t.Fatalf("service agent must not record a denial, got %v", aud.events)
	}
}

// ── AG-17 S6: external agent with a user assertion ──────────────────────────

// fakeAssertions stands in for the north OIDC validator: it returns id or err
// for any token (real audience/svc- checks live in auth/oidc_test.go).
type fakeAssertions struct {
	id  *auth.Identity
	err error
}

func (f fakeAssertions) Validate(context.Context, string) (*auth.Identity, error) { return f.id, f.err }

func TestResolveAgentApiKey_UserAssertion(t *testing.T) {
	const alice = "alice-sub"
	canUse := "user:" + alice + "|can_use|agent:" + apiKeyTestAgentUUID
	cases := []struct {
		name       string
		validator  *fakeAssertions // nil = Deps.UserAssertions unset
		canUse     bool
		wantReason string // "" = valid with Subject=alice
	}{
		{"good assertion, non-service agent under enforce", &fakeAssertions{id: &auth.Identity{Subject: alice, TenantID: apiKeyTestTenantUUID}}, true, ""},
		{"wrong audience", &fakeAssertions{err: errors.New("oidc: invalid audience")}, true, "assertion_invalid"},
		{"svc- subject", &fakeAssertions{err: &auth.SvcSubjectError{Subject: "svc-x"}}, true, "assertion_svc_subject"},
		{"no can_use", &fakeAssertions{id: &auth.Identity{Subject: alice, TenantID: apiKeyTestTenantUUID}}, false, "assertion_no_can_use"},
		{"no tenant claim", &fakeAssertions{id: &auth.Identity{Subject: alice}}, true, "assertion_tenant_mismatch"},
		{"other tenant", &fakeAssertions{id: &auth.Identity{Subject: alice, TenantID: "33333333-0000-0000-0000-000000000003"}}, true, "assertion_tenant_mismatch"},
		{"validator unset fails closed", nil, true, "assertion_unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fga := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}, checks: map[string]bool{canUse: tc.canUse}}
			deps := newApiKeyDepsWithFGA(t, "admin@example.com", fga, &fakeApiKeyStore{}, agentStoreWithAgent(t))
			aud := &fakeInvokeAudit{}
			deps.Audit = aud
			cfg := newFakeConfigStore()
			cfg.values[apiKeyTestTenantUUID+":identity.obo"] = "enforce"
			deps.Config = cfg
			if tc.validator != nil {
				deps.UserAssertions = tc.validator
			}
			mintResp, err := NewBrokerService(deps).MintAgentApiKey(ctxWithIdentity(apiKeyTestTenantUUID, "admin@example.com"), &brokerv1.MintAgentApiKeyRequest{
				TenantId: apiKeyTestTenantUUID, UserId: "admin@example.com", AgentId: apiKeyTestAgentUUID, TtlDays: 30,
			})
			if err != nil {
				t.Fatalf("MintAgentApiKey: %v", err)
			}
			resp, err := NewSandboxService(deps).ResolveAgentApiKey(sandboxCtx(), &brokerv1.ResolveAgentApiKeyRequest{
				KeyHashHex: apikey.HashForResolve(mintResp.RawKey), TenantId: apiKeyTestTenantUUID, UserAssertion: "token",
			})
			if tc.wantReason != "" {
				if status.Code(err) != codes.PermissionDenied || status.Convert(err).Message() != "user assertion rejected: "+tc.wantReason {
					t.Fatalf("want PermissionDenied %q, got resp=%+v err=%v", tc.wantReason, resp, err)
				}
				for _, ev := range aud.events {
					if ev.EventType == "aikonos.broker.apikey.denied" && ev.Context.Fields["reason"].GetStringValue() == tc.wantReason {
						return
					}
				}
				t.Fatalf("want %s DENY, got %v", tc.wantReason, aud.events)
			}
			if err != nil {
				t.Fatalf("ResolveAgentApiKey: %v", err)
			}
			if !resp.Valid || resp.Principal != alice {
				t.Fatalf("want valid with principal %q, got %+v", alice, resp)
			}
			g, err := gatewaygrant.Verify(testGrantKey, resp.OwnerGrant, false)
			if err != nil {
				t.Fatalf("Verify owner grant: %v", err)
			}
			if g.Subject != alice || g.Owner() != alice || g.Actor != apiKeyTestAgentUUID || g.Kind != gatewaygrant.KindExternal || g.GrantID != mintResp.Key.GetId() {
				t.Fatalf("grant = %+v, want subject %s, actor %s, external, key id", g, alice, apiKeyTestAgentUUID)
			}
			if serviceAgentDenied(aud) {
				t.Fatalf("an asserted subject must skip the service-agent gate, got %v", aud.events)
			}
		})
	}
}

// TestResolveAgentApiKey_UserAssertionRevoked pins F3/G45: a user assertion
// issued before the user's revoke-all epoch (or for a blocked user) mints no
// owner grant, like the north RevocationInterceptor; one issued after it does.
func TestResolveAgentApiKey_UserAssertionRevoked(t *testing.T) {
	const alice = "alice-sub"
	epoch := time.Now().Add(-time.Minute)
	cases := []struct {
		name       string
		iat        time.Time
		blocked    bool
		wantReason string
	}{
		{"issued before revoke-all", epoch.Add(-time.Hour), false, "assertion_revoked"},
		{"no iat once an epoch is set", time.Time{}, false, "assertion_revoked"},
		{"blocked user", time.Now(), true, "assertion_revoked"},
		{"issued after revoke-all", time.Now(), false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fga := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true},
				checks: map[string]bool{"user:" + alice + "|can_use|agent:" + apiKeyTestAgentUUID: true}}
			deps := newApiKeyDepsWithFGA(t, "admin@example.com", fga, &fakeApiKeyStore{}, agentStoreWithAgent(t))
			aud := &fakeInvokeAudit{}
			deps.Audit = aud
			deps.Config = newFakeConfigStore()
			deps.UserAssertions = fakeAssertions{id: &auth.Identity{Subject: alice, TenantID: apiKeyTestTenantUUID, IssuedAt: tc.iat}}
			rev := newFakeRevStore()
			rev.userNB[alice] = epoch
			if tc.blocked {
				rev.blocks[alice] = db.UserBlock{}
			}
			deps.Revocation = revocation.New(rev)
			mintResp, err := NewBrokerService(deps).MintAgentApiKey(ctxWithIdentity(apiKeyTestTenantUUID, "admin@example.com"), &brokerv1.MintAgentApiKeyRequest{
				TenantId: apiKeyTestTenantUUID, UserId: "admin@example.com", AgentId: apiKeyTestAgentUUID, TtlDays: 30,
			})
			if err != nil {
				t.Fatalf("MintAgentApiKey: %v", err)
			}
			resp, err := NewSandboxService(deps).ResolveAgentApiKey(sandboxCtx(), &brokerv1.ResolveAgentApiKeyRequest{
				KeyHashHex: apikey.HashForResolve(mintResp.RawKey), TenantId: apiKeyTestTenantUUID, UserAssertion: "token",
			})
			if tc.wantReason == "" {
				if err != nil || !resp.Valid {
					t.Fatalf("want valid, got resp=%+v err=%v", resp, err)
				}
				return
			}
			if status.Code(err) != codes.PermissionDenied || status.Convert(err).Message() != "user assertion rejected: "+tc.wantReason {
				t.Fatalf("want PermissionDenied %q, got resp=%+v err=%v", tc.wantReason, resp, err)
			}
			for _, ev := range aud.events {
				if ev.EventType == "aikonos.broker.apikey.denied" && ev.Context.Fields["reason"].GetStringValue() == tc.wantReason {
					return
				}
			}
			t.Fatalf("want %s DENY, got %v", tc.wantReason, aud.events)
		})
	}
}

// TestNewSandboxService_WarnsWithoutConfig guards the south wiring: a missing
// Config made identity.obo=enforce silently read as audit.
func TestNewSandboxService_WarnsWithoutConfig(t *testing.T) {
	core, recorded := observer.New(zapcore.WarnLevel)
	NewSandboxService(Deps{Logger: zap.New(core)})
	if recorded.FilterMessageSnippet("Deps.Config is nil").Len() != 1 {
		t.Fatalf("want a Config-missing warning, got %v", recorded.All())
	}
	NewSandboxService(Deps{Logger: zap.New(core), Config: newFakeConfigStore()})
	if recorded.Len() != 1 {
		t.Fatalf("wired Config must not warn, got %v", recorded.All())
	}
}

// ── AG-15: mandatory key expiry ──────────────────────────────────────────────

// TestMintAgentApiKey_TTLBounds: a missing ttl or one above the tenant maximum
// is InvalidArgument; the default maximum is 90 days, a tenant can lower it.
func TestMintAgentApiKey_TTLBounds(t *testing.T) {
	cases := []struct {
		name   string
		maxTTL string // "" = schema default (90)
		ttl    int32
		want   codes.Code
	}{
		{"missing", "", 0, codes.InvalidArgument},
		{"negative", "", -1, codes.InvalidArgument},
		{"above default max", "", 91, codes.InvalidArgument},
		{"at default max", "", 90, codes.OK},
		{"above tenant max", "7", 8, codes.InvalidArgument},
		{"at tenant max", "7", 7, codes.OK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fga := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
			keyStore := &fakeApiKeyStore{}
			deps := newApiKeyDepsWithFGA(t, "admin@example.com", fga, keyStore, agentStoreWithAgent(t))
			if tc.maxTTL != "" {
				cfg := newFakeConfigStore()
				cfg.values[apiKeyTestTenantUUID+":api_keys.max_ttl_days"] = tc.maxTTL
				deps.Config = cfg
			}
			resp, err := NewBrokerService(deps).MintAgentApiKey(ctxWithIdentity(apiKeyTestTenantUUID, "admin@example.com"), &brokerv1.MintAgentApiKeyRequest{
				TenantId: apiKeyTestTenantUUID, UserId: "admin@example.com", AgentId: apiKeyTestAgentUUID, TtlDays: tc.ttl,
			})
			if got := status.Code(err); got != tc.want {
				t.Fatalf("code = %v, want %v (err %v)", got, tc.want, err)
			}
			if tc.want != codes.OK {
				if len(keyStore.keys) != 0 {
					t.Fatal("a refused mint must not store a key")
				}
				return
			}
			want := time.Now().AddDate(0, 0, int(tc.ttl))
			if d := resp.Key.ExpiresAt.AsTime().Sub(want); d > time.Minute || d < -time.Minute {
				t.Fatalf("expires_at = %v, want ~%v", resp.Key.ExpiresAt.AsTime(), want)
			}
		})
	}
}

// TestMintAgentApiKey_AuditsExpiry: the minted event carries the expiry.
func TestMintAgentApiKey_AuditsExpiry(t *testing.T) {
	fga := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	deps := newApiKeyDepsWithFGA(t, "admin@example.com", fga, &fakeApiKeyStore{}, agentStoreWithAgent(t))
	aud := &fakeInvokeAudit{}
	deps.Audit = aud
	resp, err := NewBrokerService(deps).MintAgentApiKey(ctxWithIdentity(apiKeyTestTenantUUID, "admin@example.com"), &brokerv1.MintAgentApiKeyRequest{
		TenantId: apiKeyTestTenantUUID, UserId: "admin@example.com", AgentId: apiKeyTestAgentUUID, TtlDays: 30,
	})
	if err != nil {
		t.Fatalf("MintAgentApiKey: %v", err)
	}
	for _, e := range aud.events {
		if e.EventType == "aikonos.broker.apikey.minted" {
			got := e.GetContext().GetFields()["expires_at"].GetStringValue()
			if want := resp.Key.ExpiresAt.AsTime().Format(time.RFC3339); got != want {
				t.Fatalf("audit expires_at = %q, want %q", got, want)
			}
			return
		}
	}
	t.Fatal("no aikonos.broker.apikey.minted event")
}

// TestListAgentApiKeys_ShowsExpiryAndLastUsed: the list carries expires_at,
// last_used_at and the tenant maximum for the mint form.
func TestListAgentApiKeys_ShowsExpiryAndLastUsed(t *testing.T) {
	fga := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	used := time.Now().Add(-time.Hour)
	exp := time.Now().Add(48 * time.Hour)
	keyStore := &fakeApiKeyStore{keys: []*db.StoredApiKey{{
		ID: uuid.New(), TenantID: uuid.MustParse(apiKeyTestTenantUUID), AgentID: uuid.MustParse(apiKeyTestAgentUUID),
		KeyPrefix: "tk_abcde", CreatedAt: time.Now(), LastUsedAt: &used, ExpiresAt: exp,
	}}}
	deps := newApiKeyDepsWithFGA(t, "admin@example.com", fga, keyStore, agentStoreWithAgent(t))
	resp, err := NewBrokerService(deps).ListAgentApiKeys(ctxWithIdentity(apiKeyTestTenantUUID, "admin@example.com"), &brokerv1.ListAgentApiKeysRequest{
		TenantId: apiKeyTestTenantUUID, UserId: "admin@example.com", AgentId: apiKeyTestAgentUUID,
	})
	if err != nil {
		t.Fatalf("ListAgentApiKeys: %v", err)
	}
	if resp.MaxTtlDays != 90 {
		t.Errorf("max_ttl_days = %d, want 90", resp.MaxTtlDays)
	}
	if len(resp.Keys) != 1 {
		t.Fatalf("keys = %d, want 1", len(resp.Keys))
	}
	k := resp.Keys[0]
	if !k.ExpiresAt.AsTime().Equal(exp) || !k.LastUsedAt.AsTime().Equal(used) {
		t.Errorf("expires_at=%v last_used_at=%v, want %v / %v", k.ExpiresAt.AsTime(), k.LastUsedAt.AsTime(), exp, used)
	}
}

// TestResolveAgentApiKey_ExpiredKey: an expired key resolves exactly like a
// revoked one (Valid:false, no error), which the REST path maps to 401.
func TestResolveAgentApiKey_ExpiredKey(t *testing.T) {
	fga := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	keyStore := &fakeApiKeyStore{}
	deps := newApiKeyDepsWithFGA(t, "admin@example.com", fga, keyStore, agentStoreWithAgent(t))
	mintResp, err := NewBrokerService(deps).MintAgentApiKey(ctxWithIdentity(apiKeyTestTenantUUID, "admin@example.com"), &brokerv1.MintAgentApiKeyRequest{
		TenantId: apiKeyTestTenantUUID, UserId: "admin@example.com", AgentId: apiKeyTestAgentUUID, TtlDays: 1,
	})
	if err != nil {
		t.Fatalf("MintAgentApiKey: %v", err)
	}
	keyStore.keys[0].ExpiresAt = time.Now().Add(-time.Second)
	resp, err := NewSandboxService(deps).ResolveAgentApiKey(sandboxCtx(), &brokerv1.ResolveAgentApiKeyRequest{
		KeyHashHex: apikey.HashForResolve(mintResp.RawKey), TenantId: apiKeyTestTenantUUID,
	})
	if err != nil {
		t.Fatalf("unexpected error for expired key: %v", err)
	}
	if resp.Valid || resp.OwnerGrant != "" {
		t.Errorf("expired key: want Valid=false and no grant, got %+v", resp)
	}
}
