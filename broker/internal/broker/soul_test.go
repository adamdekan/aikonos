package broker

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/policy"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// ── sanitizeSoul table tests ──────────────────────────────────────────────────

func TestSanitizeSoul(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		want    string
		wantErr codes.Code // codes.OK means no error expected
	}{
		{
			name:    "plain text passes unchanged",
			input:   "You are a helpful assistant.",
			want:    "You are a helpful assistant.",
			wantErr: codes.OK,
		},
		{
			name:    "script tag stripped",
			input:   "<script>x</script>",
			want:    "x",
			wantErr: codes.OK,
		},
		{
			name:    "leading and trailing whitespace trimmed",
			input:   "  hello world  ",
			want:    "hello world",
			wantErr: codes.OK,
		},
		{
			name:    "5000-byte input rejected",
			input:   strings.Repeat("a", 5000),
			wantErr: codes.InvalidArgument,
		},
		{
			name:    "empty input is valid and clears soul",
			input:   "",
			want:    "",
			wantErr: codes.OK,
		},
		{
			name:    "whitespace-only clears soul",
			input:   "   \t\n  ",
			want:    "",
			wantErr: codes.OK,
		},
		{
			name:    "html tags in middle stripped",
			input:   "hello <b>world</b>",
			want:    "hello world",
			wantErr: codes.OK,
		},
		{
			name:    "exactly 4096 bytes passes",
			input:   strings.Repeat("a", 4096),
			want:    strings.Repeat("a", 4096),
			wantErr: codes.OK,
		},
		{
			name:    "4097 bytes rejected",
			input:   strings.Repeat("a", 4097),
			wantErr: codes.InvalidArgument,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := sanitizeSoul(tc.input)
			if tc.wantErr == codes.OK {
				if err != nil {
					t.Fatalf("want no error, got %v", err)
				}
				if got != tc.want {
					t.Errorf("want %q, got %q", tc.want, got)
				}
				return
			}
			if err == nil {
				t.Fatal("want error, got nil")
			}
			if code := status.Code(err); code != tc.wantErr {
				t.Errorf("want code %v, got %v: %v", tc.wantErr, code, err)
			}
		})
	}
}

// ── requireAgentOwnerOrAdmin tests ───────────────────────────────────────────

const (
	soulTestTenantUUID = "00000000-0000-0000-0000-000000000099"
	soulTestTenantName = "aikonos-dev"
	soulTestAgentID    = "cccccccc-0000-0000-0000-000000000099"
)

func newSoulDeps(t *testing.T, fga *fakeFGA, store AgentStore) *BrokerService {
	t.Helper()
	srv := fga.server(t)
	t.Cleanup(srv.Close)

	em, err := audit.NewEmitter(context.Background(), audit.Config{})
	if err != nil {
		t.Fatalf("audit.NewEmitter: %v", err)
	}
	eng, err := policy.NewEngine(context.Background(), policy.Config{
		OPAEndpoint:     "http://unused",
		OpenFGAEndpoint: srv.URL,
		OpenFGAStoreID:  "store-1",
	})
	if err != nil {
		t.Fatalf("policy.NewEngine: %v", err)
	}
	return NewBrokerService(Deps{
		Logger:   zap.NewNop(),
		Policy:   eng,
		TenantID: soulTestTenantName,
		Audit:    em,
		Agents:   store,
	})
}

func makeSoulAgent(t *testing.T) *db.Agent {
	t.Helper()
	tid, _ := uuid.Parse(soulTestTenantUUID)
	aid, _ := uuid.Parse(soulTestAgentID)
	return &db.Agent{
		ID:           aid,
		TenantID:     tid,
		Name:         "soul-agent",
		ApprovalMode: "needs_approval",
		Skills:       []string{},
		McpServers:   []string{},
	}
}

func TestRequireAgentOwnerOrAdmin_AdminAllows(t *testing.T) {
	fga := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	svc := newSoulDeps(t, fga, &fakeAgentStore{})
	ctx := context.Background()
	err := svc.requireAgentOwnerOrAdmin(ctx, "admin@example.com", soulTestAgentID)
	if err != nil {
		t.Fatalf("admin should be allowed, got: %v", err)
	}
}

func TestRequireAgentOwnerOrAdmin_OwnerAllows(t *testing.T) {
	// admin check → false, owner_user check → true via checks map.
	ownerKey := "user:owner@example.com|owner_user|agent:" + soulTestAgentID
	fga := &fakeFGA{
		admins: map[string]bool{},
		checks: map[string]bool{ownerKey: true},
	}
	svc := newSoulDeps(t, fga, &fakeAgentStore{})
	ctx := context.Background()
	err := svc.requireAgentOwnerOrAdmin(ctx, "owner@example.com", soulTestAgentID)
	if err != nil {
		t.Fatalf("owner should be allowed, got: %v", err)
	}
}

func TestRequireAgentOwnerOrAdmin_BothFalsePermissionDenied(t *testing.T) {
	fga := &fakeFGA{admins: map[string]bool{}}
	svc := newSoulDeps(t, fga, &fakeAgentStore{})
	ctx := context.Background()
	err := svc.requireAgentOwnerOrAdmin(ctx, "stranger@example.com", soulTestAgentID)
	if err == nil {
		t.Fatal("want PermissionDenied, got nil")
	}
	if code := status.Code(err); code != codes.PermissionDenied {
		t.Errorf("want PermissionDenied, got %v", code)
	}
}

func TestRequireAgentOwnerOrAdmin_FGAErrorInternal(t *testing.T) {
	fga := &fakeFGA{admins: map[string]bool{}, checkErr: true}
	svc := newSoulDeps(t, fga, &fakeAgentStore{})
	ctx := context.Background()
	err := svc.requireAgentOwnerOrAdmin(ctx, "user@example.com", soulTestAgentID)
	if err == nil {
		t.Fatal("want Internal on FGA error, got nil")
	}
	if code := status.Code(err); code != codes.Internal {
		t.Errorf("want codes.Internal (fail-closed), got %v", code)
	}
}

// ── GetAgentSoul handler tests ────────────────────────────────────────────────

func TestGetAgentSoul_HappyPath(t *testing.T) {
	store := &fakeAgentStore{}
	a := makeSoulAgent(t)
	a.Soul = "You are a pirate."
	store.agents = []*db.Agent{a}

	fga := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	svc := newSoulDeps(t, fga, store)

	ctx := ctxWithIdentity(soulTestTenantUUID, "admin@example.com")
	resp, err := svc.GetAgentSoul(ctx, &brokerv1.GetAgentSoulRequest{
		TenantId: soulTestTenantUUID,
		UserId:   "admin@example.com",
		AgentId:  soulTestAgentID,
	})
	if err != nil {
		t.Fatalf("GetAgentSoul: %v", err)
	}
	if resp.Soul != "You are a pirate." {
		t.Errorf("want %q, got %q", "You are a pirate.", resp.Soul)
	}
}

func TestGetAgentSoul_OwnerAllowed(t *testing.T) {
	store := &fakeAgentStore{}
	a := makeSoulAgent(t)
	a.Soul = "owner soul"
	store.agents = []*db.Agent{a}

	ownerKey := "user:owner@example.com|owner_user|agent:" + soulTestAgentID
	fga := &fakeFGA{
		admins: map[string]bool{},
		checks: map[string]bool{ownerKey: true},
	}
	svc := newSoulDeps(t, fga, store)

	ctx := ctxWithIdentity(soulTestTenantUUID, "owner@example.com")
	resp, err := svc.GetAgentSoul(ctx, &brokerv1.GetAgentSoulRequest{
		TenantId: soulTestTenantUUID,
		UserId:   "owner@example.com",
		AgentId:  soulTestAgentID,
	})
	if err != nil {
		t.Fatalf("GetAgentSoul as owner: %v", err)
	}
	if resp.Soul != "owner soul" {
		t.Errorf("want %q, got %q", "owner soul", resp.Soul)
	}
}

func TestGetAgentSoul_NonOwnerNonAdminPermissionDenied(t *testing.T) {
	store := &fakeAgentStore{}
	store.agents = []*db.Agent{makeSoulAgent(t)}

	fga := &fakeFGA{admins: map[string]bool{}}
	svc := newSoulDeps(t, fga, store)

	ctx := ctxWithIdentity(soulTestTenantUUID, "stranger@example.com")
	_, err := svc.GetAgentSoul(ctx, &brokerv1.GetAgentSoulRequest{
		TenantId: soulTestTenantUUID,
		UserId:   "stranger@example.com",
		AgentId:  soulTestAgentID,
	})
	if err == nil {
		t.Fatal("want PermissionDenied, got nil")
	}
	if code := status.Code(err); code != codes.PermissionDenied {
		t.Errorf("want PermissionDenied, got %v", code)
	}
}

func TestGetAgentSoul_UnknownAgentNotFound(t *testing.T) {
	store := &fakeAgentStore{}
	fga := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	svc := newSoulDeps(t, fga, store)

	ctx := ctxWithIdentity(soulTestTenantUUID, "admin@example.com")
	_, err := svc.GetAgentSoul(ctx, &brokerv1.GetAgentSoulRequest{
		TenantId: soulTestTenantUUID,
		UserId:   "admin@example.com",
		AgentId:  soulTestAgentID,
	})
	if err == nil {
		t.Fatal("want NotFound, got nil")
	}
	if code := status.Code(err); code != codes.NotFound {
		t.Errorf("want NotFound, got %v", code)
	}
}

// TestGetAgentSoul_NonUUIDAgentID: a syntactically non-uuid agent_id must be
// rejected as NotFound at the handler boundary, never reaching the store
// (which would otherwise 500 with a raw 22P02 from Postgres).
func TestGetAgentSoul_NonUUIDAgentID(t *testing.T) {
	store := &explodingAgentStore{t: t}
	fga := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	svc := newSoulDeps(t, fga, store)

	ctx := ctxWithIdentity(soulTestTenantUUID, "admin@example.com")
	_, err := svc.GetAgentSoul(ctx, &brokerv1.GetAgentSoulRequest{
		TenantId: soulTestTenantUUID,
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

func TestGetAgentSoul_ForgedUserIDRejected(t *testing.T) {
	store := &fakeAgentStore{}
	store.agents = []*db.Agent{makeSoulAgent(t)}
	fga := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	svc := newSoulDeps(t, fga, store)

	// Authenticated as alice, but user_id claims to be admin.
	ctx := ctxWithIdentity(soulTestTenantUUID, "alice@example.com")
	_, err := svc.GetAgentSoul(ctx, &brokerv1.GetAgentSoulRequest{
		TenantId: soulTestTenantUUID,
		UserId:   "admin@example.com",
		AgentId:  soulTestAgentID,
	})
	if err == nil {
		t.Fatal("want error for forged user_id, got nil")
	}
	if code := status.Code(err); code != codes.PermissionDenied {
		t.Errorf("want PermissionDenied for forged user_id, got %v", code)
	}
}

// ── SetAgentSoul handler tests ────────────────────────────────────────────────

func TestSetAgentSoul_HappyPathPersistsCleanedSoul(t *testing.T) {
	store := &fakeAgentStore{}
	a := makeSoulAgent(t)
	store.agents = []*db.Agent{a}

	fga := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	svc := newSoulDeps(t, fga, store)

	ctx := ctxWithIdentity(soulTestTenantUUID, "admin@example.com")
	resp, err := svc.SetAgentSoul(ctx, &brokerv1.SetAgentSoulRequest{
		TenantId: soulTestTenantUUID,
		UserId:   "admin@example.com",
		AgentId:  soulTestAgentID,
		Soul:     "  <b>Be helpful.</b>  ",
	})
	if err != nil {
		t.Fatalf("SetAgentSoul: %v", err)
	}
	// HTML stripped + whitespace trimmed.
	if resp.Soul != "Be helpful." {
		t.Errorf("response soul: want %q, got %q", "Be helpful.", resp.Soul)
	}
	// Persisted in store.
	store.mu.Lock()
	stored := store.agents[0].Soul
	store.mu.Unlock()
	if stored != "Be helpful." {
		t.Errorf("stored soul: want %q, got %q", "Be helpful.", stored)
	}
}

func TestSetAgentSoul_OwnerCanSet(t *testing.T) {
	store := &fakeAgentStore{}
	a := makeSoulAgent(t)
	store.agents = []*db.Agent{a}

	ownerKey := "user:owner@example.com|owner_user|agent:" + soulTestAgentID
	fga := &fakeFGA{
		admins: map[string]bool{},
		checks: map[string]bool{ownerKey: true},
	}
	svc := newSoulDeps(t, fga, store)

	ctx := ctxWithIdentity(soulTestTenantUUID, "owner@example.com")
	resp, err := svc.SetAgentSoul(ctx, &brokerv1.SetAgentSoulRequest{
		TenantId: soulTestTenantUUID,
		UserId:   "owner@example.com",
		AgentId:  soulTestAgentID,
		Soul:     "pirate vibes",
	})
	if err != nil {
		t.Fatalf("SetAgentSoul as owner: %v", err)
	}
	if resp.Soul != "pirate vibes" {
		t.Errorf("want %q, got %q", "pirate vibes", resp.Soul)
	}
}

func TestSetAgentSoul_NonOwnerNonAdminPermissionDenied(t *testing.T) {
	store := &fakeAgentStore{}
	store.agents = []*db.Agent{makeSoulAgent(t)}
	fga := &fakeFGA{admins: map[string]bool{}}
	svc := newSoulDeps(t, fga, store)

	ctx := ctxWithIdentity(soulTestTenantUUID, "stranger@example.com")
	_, err := svc.SetAgentSoul(ctx, &brokerv1.SetAgentSoulRequest{
		TenantId: soulTestTenantUUID,
		UserId:   "stranger@example.com",
		AgentId:  soulTestAgentID,
		Soul:     "bad soul",
	})
	if err == nil {
		t.Fatal("want PermissionDenied, got nil")
	}
	if code := status.Code(err); code != codes.PermissionDenied {
		t.Errorf("want PermissionDenied, got %v", code)
	}
}

func TestSetAgentSoul_UnknownAgentNotFound(t *testing.T) {
	store := &fakeAgentStore{}
	fga := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	svc := newSoulDeps(t, fga, store)

	ctx := ctxWithIdentity(soulTestTenantUUID, "admin@example.com")
	_, err := svc.SetAgentSoul(ctx, &brokerv1.SetAgentSoulRequest{
		TenantId: soulTestTenantUUID,
		UserId:   "admin@example.com",
		AgentId:  soulTestAgentID,
		Soul:     "irrelevant",
	})
	if err == nil {
		t.Fatal("want NotFound, got nil")
	}
	if code := status.Code(err); code != codes.NotFound {
		t.Errorf("want NotFound, got %v", code)
	}
}

// TestSetAgentSoul_NonUUIDAgentID: a syntactically non-uuid agent_id must be
// rejected as NotFound at the handler boundary, never reaching the store
// (which would otherwise 500 with a raw 22P02 from Postgres).
func TestSetAgentSoul_NonUUIDAgentID(t *testing.T) {
	store := &explodingAgentStore{t: t}
	fga := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	svc := newSoulDeps(t, fga, store)

	ctx := ctxWithIdentity(soulTestTenantUUID, "admin@example.com")
	_, err := svc.SetAgentSoul(ctx, &brokerv1.SetAgentSoulRequest{
		TenantId: soulTestTenantUUID,
		UserId:   "admin@example.com",
		AgentId:  "alice-agent",
		Soul:     "irrelevant",
	})
	if err == nil {
		t.Fatal("want NotFound, got nil")
	}
	if code := status.Code(err); code != codes.NotFound {
		t.Errorf("non-uuid agent_id: want NotFound, got %v", code)
	}
}

func TestSetAgentSoul_ForgedUserIDRejected(t *testing.T) {
	store := &fakeAgentStore{}
	store.agents = []*db.Agent{makeSoulAgent(t)}
	fga := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	svc := newSoulDeps(t, fga, store)

	// Authenticated as alice, but user_id claims to be admin.
	ctx := ctxWithIdentity(soulTestTenantUUID, "alice@example.com")
	_, err := svc.SetAgentSoul(ctx, &brokerv1.SetAgentSoulRequest{
		TenantId: soulTestTenantUUID,
		UserId:   "admin@example.com",
		AgentId:  soulTestAgentID,
		Soul:     "injected",
	})
	if err == nil {
		t.Fatal("want error for forged user_id, got nil")
	}
	if code := status.Code(err); code != codes.PermissionDenied {
		t.Errorf("want PermissionDenied for forged user_id, got %v", code)
	}
}

func TestSetAgentSoul_OversizedSoulRejected(t *testing.T) {
	store := &fakeAgentStore{}
	a := makeSoulAgent(t)
	store.agents = []*db.Agent{a}

	fga := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	svc := newSoulDeps(t, fga, store)

	ctx := ctxWithIdentity(soulTestTenantUUID, "admin@example.com")
	_, err := svc.SetAgentSoul(ctx, &brokerv1.SetAgentSoulRequest{
		TenantId: soulTestTenantUUID,
		UserId:   "admin@example.com",
		AgentId:  soulTestAgentID,
		Soul:     strings.Repeat("x", 5000),
	})
	if err == nil {
		t.Fatal("want InvalidArgument for oversized soul, got nil")
	}
	if code := status.Code(err); code != codes.InvalidArgument {
		t.Errorf("want InvalidArgument, got %v", code)
	}
}

func TestSetAgentSoul_AgentsDisabled(t *testing.T) {
	// Agents == nil → agentsEnabled() == false.
	em, _ := audit.NewEmitter(context.Background(), audit.Config{})
	eng, _ := policy.NewEngine(context.Background(), policy.Config{OPAEndpoint: "http://unused"})
	svc := NewBrokerService(Deps{
		Logger:   zap.NewNop(),
		Policy:   eng,
		TenantID: soulTestTenantName,
		Audit:    em,
	})
	ctx := ctxWithIdentity(soulTestTenantUUID, "admin@example.com")
	_, err := svc.SetAgentSoul(ctx, &brokerv1.SetAgentSoulRequest{
		TenantId: soulTestTenantUUID,
		UserId:   "admin@example.com",
		AgentId:  soulTestAgentID,
		Soul:     "x",
	})
	if code := status.Code(err); code != codes.FailedPrecondition {
		t.Errorf("want FailedPrecondition when agents disabled, got %v", code)
	}
}

func TestGetAgentSoul_AgentsDisabled(t *testing.T) {
	// Agents == nil → agentsEnabled() == false; the read handler must also gate.
	em, _ := audit.NewEmitter(context.Background(), audit.Config{})
	eng, _ := policy.NewEngine(context.Background(), policy.Config{OPAEndpoint: "http://unused"})
	svc := NewBrokerService(Deps{
		Logger:   zap.NewNop(),
		Policy:   eng,
		TenantID: soulTestTenantName,
		Audit:    em,
	})
	ctx := ctxWithIdentity(soulTestTenantUUID, "admin@example.com")
	_, err := svc.GetAgentSoul(ctx, &brokerv1.GetAgentSoulRequest{
		TenantId: soulTestTenantUUID,
		UserId:   "admin@example.com",
		AgentId:  soulTestAgentID,
	})
	if code := status.Code(err); code != codes.FailedPrecondition {
		t.Errorf("want FailedPrecondition when agents disabled, got %v", code)
	}
}

func TestRequireAgentOwnerOrAdmin_OwnerCheckErrorInternal(t *testing.T) {
	// Admin check returns false (no error); the owner_user check on agent:<id>
	// errors. The owner-branch error path must fail closed → codes.Internal.
	fga := &fakeFGA{
		admins:      map[string]bool{},
		errOnObject: "agent:" + soulTestAgentID,
	}
	svc := newSoulDeps(t, fga, &fakeAgentStore{})
	err := svc.requireAgentOwnerOrAdmin(context.Background(), "user@example.com", soulTestAgentID)
	if code := status.Code(err); code != codes.Internal {
		t.Errorf("want Internal on owner-check FGA error (fail-closed), got %v", code)
	}
}

// ── audit-emission coverage (emit only on success) ────────────────────────────

// recordingEmitter captures emitted audit events. Satisfies the auditEmitter
// seam on Deps.Audit so a test can assert emission / non-emission.
type recordingEmitter struct {
	mu     sync.Mutex
	events []*auditv1.AuditEvent
}

func (r *recordingEmitter) Emit(_ context.Context, ev *auditv1.AuditEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
	return nil
}

func (r *recordingEmitter) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events)
}

func newRecordingSoulDeps(t *testing.T, fga *fakeFGA, store AgentStore, em auditEmitter) *BrokerService {
	t.Helper()
	srv := fga.server(t)
	t.Cleanup(srv.Close)
	eng, err := policy.NewEngine(context.Background(), policy.Config{
		OPAEndpoint:     "http://unused",
		OpenFGAEndpoint: srv.URL,
		OpenFGAStoreID:  "store-1",
	})
	if err != nil {
		t.Fatalf("policy.NewEngine: %v", err)
	}
	return NewBrokerService(Deps{
		Logger:   zap.NewNop(),
		Policy:   eng,
		TenantID: soulTestTenantName,
		Audit:    em,
		Agents:   store,
	})
}

func TestSetAgentSoul_HappyPathEmitsAudit(t *testing.T) {
	store := &fakeAgentStore{}
	store.agents = []*db.Agent{makeSoulAgent(t)}
	fga := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	rec := &recordingEmitter{}
	svc := newRecordingSoulDeps(t, fga, store, rec)

	ctx := ctxWithIdentity(soulTestTenantUUID, "admin@example.com")
	if _, err := svc.SetAgentSoul(ctx, &brokerv1.SetAgentSoulRequest{
		TenantId: soulTestTenantUUID,
		UserId:   "admin@example.com",
		AgentId:  soulTestAgentID,
		Soul:     "hi",
	}); err != nil {
		t.Fatalf("SetAgentSoul: %v", err)
	}
	if rec.count() != 1 {
		t.Fatalf("want exactly 1 audit event on success, got %d", rec.count())
	}
	if got := rec.events[0].EventType; got != "aikonos.broker.agent.soul.updated" {
		t.Errorf("event type: want aikonos.broker.agent.soul.updated, got %q", got)
	}
	if got := rec.events[0].ActorUserId; got != "admin@example.com" {
		t.Errorf("audit actor: want admin@example.com, got %q", got)
	}
}

func TestSetAgentSoul_StoreErrorNoAudit(t *testing.T) {
	// Gate passes (admin), but SetSoul fails at the store → handler returns
	// Internal and MUST NOT emit an audit event (emit only on success).
	store := &fakeAgentStore{setSoulErr: errors.New("db down")}
	store.agents = []*db.Agent{makeSoulAgent(t)}
	fga := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	rec := &recordingEmitter{}
	svc := newRecordingSoulDeps(t, fga, store, rec)

	ctx := ctxWithIdentity(soulTestTenantUUID, "admin@example.com")
	_, err := svc.SetAgentSoul(ctx, &brokerv1.SetAgentSoulRequest{
		TenantId: soulTestTenantUUID,
		UserId:   "admin@example.com",
		AgentId:  soulTestAgentID,
		Soul:     "ok",
	})
	if code := status.Code(err); code != codes.Internal {
		t.Errorf("want Internal on store error, got %v", code)
	}
	if rec.count() != 0 {
		t.Errorf("audit must NOT emit on store error, got %d events", rec.count())
	}
}
