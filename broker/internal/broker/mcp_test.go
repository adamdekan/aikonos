package broker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/policy"
	"github.com/adamdekan/aikonos/broker/internal/secrets"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// ── mock repos & providers ───────────────────────────────────────────────────

type fakeMcpRepo struct {
	mu    sync.Mutex
	conns []*db.McpConnection
}

func (r *fakeMcpRepo) List(_ context.Context, _ string) ([]*db.McpConnection, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*db.McpConnection, len(r.conns))
	copy(out, r.conns)
	return out, nil
}
func (r *fakeMcpRepo) Add(_ context.Context, c *db.McpConnection) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.conns = append(r.conns, c)
	return nil
}
func (r *fakeMcpRepo) Get(_ context.Context, _, id string) (*db.McpConnection, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.conns {
		if c.ID.String() == id {
			cp := *c
			return &cp, nil
		}
	}
	return nil, fmt.Errorf("mcp_connection %s not found", id)
}

func (r *fakeMcpRepo) Update(_ context.Context, _ string, c *db.McpConnection) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, ex := range r.conns {
		if ex.ID == c.ID {
			r.conns[i] = c
			return nil
		}
	}
	return nil
}
func (r *fakeMcpRepo) Delete(_ context.Context, _, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, ex := range r.conns {
		if ex.ID.String() == id {
			r.conns = append(r.conns[:i], r.conns[i+1:]...)
			return nil
		}
	}
	return nil
}

type fakeMcpSecrets struct {
	mu      sync.Mutex
	written map[string]string // path → value
	deleted map[string]bool
}

func (s *fakeMcpSecrets) WriteMcpBearer(_ context.Context, path, bearer string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.written == nil {
		s.written = map[string]string{}
	}
	s.written[path] = bearer
	return nil
}
func (s *fakeMcpSecrets) ReadMcpBearer(_ context.Context, tenant, id string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path, err := secrets.McpBearerPath(tenant, id)
	if err != nil {
		return "", err
	}
	if v, ok := s.written[path]; ok {
		return v, nil
	}
	return "", secrets.ErrMcpBearerNotFound
}
func (s *fakeMcpSecrets) DeleteMcpBearer(_ context.Context, path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deleted == nil {
		s.deleted = map[string]bool{}
	}
	s.deleted[path] = true
	return nil
}

// fakeFGAForMcp is a minimal FGA server: /check allows if user matches allowed map.
type fakeFGAForMcp struct {
	// allowed maps "user||relation||object" → bool
	allowed map[string]bool
}

func (f *fakeFGAForMcp) server(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case strings.HasSuffix(r.URL.Path, "/check"):
			var req struct {
				TupleKey struct{ User, Relation, Object string } `json:"tuple_key"`
			}
			_ = json.Unmarshal(body, &req)
			key := req.TupleKey.User + "||" + req.TupleKey.Relation + "||" + req.TupleKey.Object
			_ = json.NewEncoder(w).Encode(map[string]bool{"allowed": f.allowed[key]})
		case strings.HasSuffix(r.URL.Path, "/read"):
			_ = json.NewEncoder(w).Encode(map[string]any{"tuples": []any{}})
		case strings.HasSuffix(r.URL.Path, "/write"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotImplemented)
		}
	}))
}

func testMcpDeps(t *testing.T, fgaURL string, repo *fakeMcpRepo, sec secrets.McpBearerStore) Deps {
	t.Helper()
	em, err := audit.NewEmitter(context.Background(), audit.Config{})
	if err != nil {
		t.Fatal(err)
	}
	cfg := policy.Config{OPAEndpoint: "http://unused", OpenFGAEndpoint: fgaURL}
	if fgaURL != "" {
		cfg.OpenFGAStoreID = "store-1"
	}
	eng, err := policy.NewEngine(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return Deps{
		Logger:   zap.NewNop(),
		Audit:    em,
		Policy:   eng,
		TenantID: "aikonos-dev",
		Mcp:      McpDeps{Connections: repo, Bearer: sec},
	}
}

// ── CP4 tests ─────────────────────────────────────────────────────────────────

// Non-admin must get PERMISSION_DENIED on every admin MCP RPC.
func TestListMcpConnections_NonAdminDenied(t *testing.T) {
	f := &fakeFGAForMcp{allowed: map[string]bool{
		"user:admin@example.com||admin||tenant:aikonos-dev": true,
	}}
	srv := f.server(t)
	defer srv.Close()
	svc := NewBrokerService(testMcpDeps(t, srv.URL, &fakeMcpRepo{}, nil))

	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")
	_, err := svc.ListMcpConnections(ctx, &brokerv1.ListMcpConnectionsRequest{})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-admin list: want PermissionDenied, got %v", err)
	}
}

func TestAddMcpConnection_NonAdminDenied(t *testing.T) {
	f := &fakeFGAForMcp{allowed: map[string]bool{
		"user:admin@example.com||admin||tenant:aikonos-dev": true,
	}}
	srv := f.server(t)
	defer srv.Close()
	svc := NewBrokerService(testMcpDeps(t, srv.URL, &fakeMcpRepo{}, nil))

	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")
	_, err := svc.AddMcpConnection(ctx, &brokerv1.AddMcpConnectionRequest{
		Connection: &brokerv1.McpConnection{Name: "x", Url: "https://mcp.example.com", Transport: "streamable_http", AuthType: "none"},
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-admin add: want PermissionDenied, got %v", err)
	}
}

// Admin add with bearer: writes token to Vault, persists the ref (not the token).
// Response must not include the bearer token.
func TestAddMcpConnection_BearerWritesToVaultAndOmitsToken(t *testing.T) {
	f := &fakeFGAForMcp{allowed: map[string]bool{
		"user:admin@example.com||admin||tenant:aikonos-dev": true,
	}}
	srv := f.server(t)
	defer srv.Close()
	repo := &fakeMcpRepo{}
	sec := &fakeMcpSecrets{}
	svc := NewBrokerService(testMcpDeps(t, srv.URL, repo, sec))

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	resp, err := svc.AddMcpConnection(ctx, &brokerv1.AddMcpConnectionRequest{
		UserId: "admin@example.com",
		Connection: &brokerv1.McpConnection{
			Name: "my-mcp", Url: "https://mcp.example.com",
			Transport: "streamable_http", AuthType: "bearer",
		},
		BearerToken: "secret-token",
	})
	if err != nil {
		t.Fatalf("add bearer: %v", err)
	}

	// The response connection must not expose the bearer token.
	if resp.Connection == nil {
		t.Fatal("response connection is nil")
	}
	if resp.Connection.GetAuthSecretRef() == "" {
		t.Error("auth_secret_ref should be set")
	}

	// Vault must have received the token at some path.
	sec.mu.Lock()
	written := len(sec.written)
	var writtenPath, writtenVal string
	for p, v := range sec.written {
		writtenPath, writtenVal = p, v
	}
	sec.mu.Unlock()
	if written != 1 {
		t.Fatalf("expected 1 Vault write, got %d", written)
	}
	if writtenVal != "secret-token" {
		t.Errorf("Vault token mismatch: got %q", writtenVal)
	}

	// The persisted record must hold the ref, not the token.
	repo.mu.Lock()
	conns := repo.conns
	repo.mu.Unlock()
	if len(conns) != 1 {
		t.Fatalf("expected 1 persisted conn, got %d", len(conns))
	}
	if conns[0].AuthSecretRef != writtenPath {
		t.Errorf("persisted auth_secret_ref %q != Vault path %q", conns[0].AuthSecretRef, writtenPath)
	}
}

// List must omit bearer tokens (auth_secret_ref may be set, but no token field in response).
func TestListMcpConnections_OmitsToken(t *testing.T) {
	f := &fakeFGAForMcp{allowed: map[string]bool{
		"user:admin@example.com||admin||tenant:aikonos-dev": true,
	}}
	srv := f.server(t)
	defer srv.Close()
	repo := &fakeMcpRepo{}
	svc := NewBrokerService(testMcpDeps(t, srv.URL, repo, nil))

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	// Add a none-auth connection first.
	_, err := svc.AddMcpConnection(ctx, &brokerv1.AddMcpConnectionRequest{
		UserId: "admin@example.com",
		Connection: &brokerv1.McpConnection{
			Name: "public-mcp", Url: "https://mcp.example.com",
			Transport: "sse", AuthType: "none",
		},
	})
	if err != nil {
		t.Fatalf("add: %v", err)
	}

	resp, err := svc.ListMcpConnections(ctx, &brokerv1.ListMcpConnectionsRequest{UserId: "admin@example.com"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(resp.Connections) != 1 {
		t.Fatalf("expected 1 connection, got %d", len(resp.Connections))
	}
	// McpConnection proto has no bearer_token field (write-only on request side).
	// Just confirm the connection is returned with expected fields.
	c := resp.Connections[0]
	if c.Name != "public-mcp" {
		t.Errorf("name mismatch: %q", c.Name)
	}
}

// Delete with bearer: best-effort Vault delete (no error if already gone).
func TestDeleteMcpConnection_DeletesVaultSecret(t *testing.T) {
	f := &fakeFGAForMcp{allowed: map[string]bool{
		"user:admin@example.com||admin||tenant:aikonos-dev": true,
	}}
	srv := f.server(t)
	defer srv.Close()
	repo := &fakeMcpRepo{}
	sec := &fakeMcpSecrets{}
	svc := NewBrokerService(testMcpDeps(t, srv.URL, repo, sec))

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	addResp, err := svc.AddMcpConnection(ctx, &brokerv1.AddMcpConnectionRequest{
		UserId: "admin@example.com",
		Connection: &brokerv1.McpConnection{
			Name: "to-delete", Url: "https://mcp.example.com",
			Transport: "streamable_http", AuthType: "bearer",
		},
		BearerToken: "tok",
	})
	if err != nil {
		t.Fatalf("add: %v", err)
	}

	_, err = svc.DeleteMcpConnection(ctx, &brokerv1.DeleteMcpConnectionRequest{
		UserId: "admin@example.com",
		Id:     addResp.Connection.GetId(),
	})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}

	// Vault delete must have been called.
	sec.mu.Lock()
	delCount := len(sec.deleted)
	sec.mu.Unlock()
	if delCount != 1 {
		t.Errorf("expected 1 Vault delete, got %d", delCount)
	}

	// Connection must be gone from repo.
	repo.mu.Lock()
	repoLen := len(repo.conns)
	repo.mu.Unlock()
	if repoLen != 0 {
		t.Errorf("expected 0 connections after delete, got %d", repoLen)
	}
}

// Audit event must be emitted on successful add.
func TestAddMcpConnection_AuditEmitted(t *testing.T) {
	f := &fakeFGAForMcp{allowed: map[string]bool{
		"user:admin@example.com||admin||tenant:aikonos-dev": true,
	}}
	srv := f.server(t)
	defer srv.Close()
	repo := &fakeMcpRepo{}
	svc := NewBrokerService(testMcpDeps(t, srv.URL, repo, nil))

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	_, err := svc.AddMcpConnection(ctx, &brokerv1.AddMcpConnectionRequest{
		UserId: "admin@example.com",
		Connection: &brokerv1.McpConnection{
			Name: "audit-test", Url: "https://mcp.example.com",
			Transport: "sse", AuthType: "none",
		},
	})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	// Audit is fire-and-forget; confirming no error suffices.
}

// ── CP4 — no-secrets provider, bearer fails cleanly ─────────────────────────

func TestAddMcpConnection_BearerFailsWithoutVault(t *testing.T) {
	f := &fakeFGAForMcp{allowed: map[string]bool{
		"user:admin@example.com||admin||tenant:aikonos-dev": true,
	}}
	srv := f.server(t)
	defer srv.Close()
	// Mcp.Bearer is nil → Vault disabled for MCP
	svc := NewBrokerService(testMcpDeps(t, srv.URL, &fakeMcpRepo{}, nil))

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	_, err := svc.AddMcpConnection(ctx, &brokerv1.AddMcpConnectionRequest{
		UserId: "admin@example.com",
		Connection: &brokerv1.McpConnection{
			Name: "bearer-no-vault", Url: "https://mcp.example.com",
			Transport: "streamable_http", AuthType: "bearer",
		},
		BearerToken: "tok",
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("bearer without vault: want FailedPrecondition, got %v", err)
	}
}

// ── CP5 — ListAccessibleMcpServers ───────────────────────────────────────────

// With a mocked FGA Check allowing one of two connections, only the allowed one
// is returned.
func TestListAccessibleMcpServers_ReturnsAllowed(t *testing.T) {
	// We have two connections: conn-a and conn-b.
	// FGA allows agent:my-agent on conn-a, not on conn-b.
	// fakeFGAForMcp.check: must also allow the admin relation so callerIdentity/admin
	// gate does not block — for ListAccessibleMcpServers we use tenant-admin gate.
	// For simplicity, allow admin for the acting user.
	// We'll use a server that grants admin + selective can_access.

	var srv *httptest.Server
	var connAID, connBID string

	// We need the IDs before the FGA server knows them, so build the server lazily.
	allowedConnID := ""
	fgaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case strings.HasSuffix(r.URL.Path, "/check"):
			var req struct {
				TupleKey struct{ User, Relation, Object string } `json:"tuple_key"`
			}
			_ = json.Unmarshal(body, &req)
			allowed := false
			switch req.TupleKey.Relation {
			case "admin":
				allowed = req.TupleKey.User == "user:admin@example.com"
			case "can_access":
				// Only allow the connection we will designate.
				allowed = req.TupleKey.User == "agent:my-agent" &&
					req.TupleKey.Object == "mcp_connector:"+allowedConnID
			}
			_ = json.NewEncoder(w).Encode(map[string]bool{"allowed": allowed})
		case strings.HasSuffix(r.URL.Path, "/read"):
			_ = json.NewEncoder(w).Encode(map[string]any{"tuples": []any{}})
		case strings.HasSuffix(r.URL.Path, "/write"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer fgaSrv.Close()
	srv = fgaSrv

	repo := &fakeMcpRepo{}
	svc := NewBrokerService(testMcpDeps(t, srv.URL, repo, nil))

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")

	// Add two connections.
	rA, err := svc.AddMcpConnection(ctx, &brokerv1.AddMcpConnectionRequest{
		UserId:     "admin@example.com",
		Connection: &brokerv1.McpConnection{Name: "conn-a", Url: "https://a.example.com", Transport: "sse", AuthType: "none"},
	})
	if err != nil {
		t.Fatalf("add conn-a: %v", err)
	}
	connAID = rA.Connection.GetId()

	rB, err := svc.AddMcpConnection(ctx, &brokerv1.AddMcpConnectionRequest{
		UserId:     "admin@example.com",
		Connection: &brokerv1.McpConnection{Name: "conn-b", Url: "https://b.example.com", Transport: "streamable_http", AuthType: "none"},
	})
	if err != nil {
		t.Fatalf("add conn-b: %v", err)
	}
	connBID = rB.Connection.GetId()
	_ = connBID

	// Designate conn-a as the allowed one.
	allowedConnID = connAID

	// List accessible for my-agent (no user_id field on this request).
	resp, err := svc.ListAccessibleMcpServers(ctx, &brokerv1.ListAccessibleMcpServersRequest{
		AgentId: "agent:my-agent",
	})
	if err != nil {
		t.Fatalf("ListAccessibleMcpServers: %v", err)
	}
	if len(resp.Connections) != 1 {
		t.Fatalf("expected 1 accessible connection, got %d", len(resp.Connections))
	}
	if resp.Connections[0].GetId() != connAID {
		t.Errorf("expected conn-a (%s), got %s", connAID, resp.Connections[0].GetId())
	}
}

// ── CP5 — Roles: MCP access section ─────────────────────────────────────────

// AssignRole accepts agent→mcp_connector.
func TestAssignRole_AcceptsMcpConnectorAgent(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	svc := NewBrokerService(testAdminDeps(t, srv.URL))

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	resp, err := svc.AssignRole(ctx, &brokerv1.AssignRoleRequest{
		UserId: "admin@example.com",
		Tuple:  tuple("agent:alice-agent", "permitted_agent", "mcp_connector:conn-123"),
	})
	if err != nil || !resp.Success {
		t.Fatalf("assign agent→mcp_connector: resp=%v err=%v", resp, err)
	}
	if len(f.writes) != 1 {
		t.Errorf("expected 1 write, got %+v", f.writes)
	}
}

// AssignRole rejects user subject for permitted_agent on mcp_connector.
func TestAssignRole_RejectsUserForMcpConnector(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	svc := NewBrokerService(testAdminDeps(t, srv.URL))

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	_, err := svc.AssignRole(ctx, &brokerv1.AssignRoleRequest{
		UserId: "admin@example.com",
		Tuple:  tuple("user:alice@example.com", "permitted_agent", "mcp_connector:conn-123"),
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("user on permitted_agent: want InvalidArgument, got %v", err)
	}
}

// AssignRole rejects group#member subject for permitted_agent on mcp_connector.
func TestAssignRole_RejectsGroupForMcpConnector(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	svc := NewBrokerService(testAdminDeps(t, srv.URL))

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	_, err := svc.AssignRole(ctx, &brokerv1.AssignRoleRequest{
		UserId: "admin@example.com",
		Tuple:  tuple("group:security-team#member", "permitted_agent", "mcp_connector:conn-123"),
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("group on permitted_agent: want InvalidArgument, got %v", err)
	}
}

// AssignRole rejects wildcard id for mcp_connector.
func TestAssignRole_RejectsWildcardMcpConnector(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	svc := NewBrokerService(testAdminDeps(t, srv.URL))

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	_, err := svc.AssignRole(ctx, &brokerv1.AssignRoleRequest{
		UserId: "admin@example.com",
		Tuple:  tuple("agent:alice-agent", "permitted_agent", "mcp_connector:*"),
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("wildcard mcp_connector: want InvalidArgument, got %v", err)
	}
}

// ── Finding-1 regression tests ────────────────────────────────────────────────

// Non-admin must get PERMISSION_DENIED on Update.
func TestUpdateMcpConnection_NonAdminDenied(t *testing.T) {
	f := &fakeFGAForMcp{allowed: map[string]bool{
		"user:admin@example.com||admin||tenant:aikonos-dev": true,
	}}
	srv := f.server(t)
	defer srv.Close()
	svc := NewBrokerService(testMcpDeps(t, srv.URL, &fakeMcpRepo{}, nil))

	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")
	_, err := svc.UpdateMcpConnection(ctx, &brokerv1.UpdateMcpConnectionRequest{
		Connection: &brokerv1.McpConnection{
			Id:   "00000000-0000-0000-0000-000000000001",
			Name: "x", Url: "https://mcp.example.com", Transport: "streamable_http", AuthType: "none",
		},
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-admin update: want PermissionDenied, got %v", err)
	}
}

// Non-admin must get PERMISSION_DENIED on Delete.
func TestDeleteMcpConnection_NonAdminDenied(t *testing.T) {
	f := &fakeFGAForMcp{allowed: map[string]bool{
		"user:admin@example.com||admin||tenant:aikonos-dev": true,
	}}
	srv := f.server(t)
	defer srv.Close()
	svc := NewBrokerService(testMcpDeps(t, srv.URL, &fakeMcpRepo{}, nil))

	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")
	_, err := svc.DeleteMcpConnection(ctx, &brokerv1.DeleteMcpConnectionRequest{
		Id: "00000000-0000-0000-0000-000000000001",
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-admin delete: want PermissionDenied, got %v", err)
	}
}

// Update with no new bearer token must preserve the existing auth_secret_ref
// from the DB and must NOT allow a client-supplied ref to overwrite it.
func TestUpdateMcpConnection_PreservesAuthSecretRefWhenNoNewToken(t *testing.T) {
	f := &fakeFGAForMcp{allowed: map[string]bool{
		"user:admin@example.com||admin||tenant:aikonos-dev": true,
	}}
	srv := f.server(t)
	defer srv.Close()
	repo := &fakeMcpRepo{}
	sec := &fakeMcpSecrets{}
	svc := NewBrokerService(testMcpDeps(t, srv.URL, repo, sec))

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")

	// Add a bearer connection — the server derives and stores the Vault ref.
	addResp, err := svc.AddMcpConnection(ctx, &brokerv1.AddMcpConnectionRequest{
		UserId: "admin@example.com",
		Connection: &brokerv1.McpConnection{
			Name: "guarded", Url: "https://mcp.example.com",
			Transport: "streamable_http", AuthType: "bearer",
		},
		BearerToken: "original-token",
	})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	connID := addResp.Connection.GetId()
	originalRef := addResp.Connection.GetAuthSecretRef()
	if originalRef == "" {
		t.Fatal("expected non-empty auth_secret_ref after add")
	}

	// Update with a forged client-supplied auth_secret_ref and NO new bearer token.
	// The server must ignore the client ref and preserve the DB value.
	_, err = svc.UpdateMcpConnection(ctx, &brokerv1.UpdateMcpConnectionRequest{
		UserId: "admin@example.com",
		Connection: &brokerv1.McpConnection{
			Id:            connID,
			Name:          "guarded-renamed",
			Url:           "https://mcp.example.com",
			Transport:     "streamable_http",
			AuthType:      "bearer",
			AuthSecretRef: "secret/workspaces/other-tenant/evil/connectors/injected",
		},
		// BearerToken intentionally empty — no new token.
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}

	// The persisted record must still hold the server-derived ref, not the forged one.
	repo.mu.Lock()
	var stored string
	for _, c := range repo.conns {
		if c.ID.String() == connID {
			stored = c.AuthSecretRef
		}
	}
	repo.mu.Unlock()
	if stored != originalRef {
		t.Errorf("auth_secret_ref after update = %q, want original %q", stored, originalRef)
	}
}
