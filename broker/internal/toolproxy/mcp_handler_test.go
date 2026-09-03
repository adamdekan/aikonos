package toolproxy

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/google/uuid"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/secrets"
)

// fakeConnRepo is a minimal in-memory mcpConnectionLookup for tests.
type fakeConnRepo struct {
	conns map[string]*db.McpConnection
}

func (r *fakeConnRepo) Get(_ context.Context, _, id string) (*db.McpConnection, error) {
	c, ok := r.conns[id]
	if !ok {
		return nil, fmt.Errorf("connection %q not found", id)
	}
	return c, nil
}

// fakeBearerStore is an in-memory McpBearerStore.
type fakeBearerStore struct {
	tokens map[string]string // "<tenant>/<id>" → bearer
}

func (s *fakeBearerStore) WriteMcpBearer(_ context.Context, path, bearer string) error {
	if s.tokens == nil {
		s.tokens = map[string]string{}
	}
	s.tokens[path] = bearer
	return nil
}

func (s *fakeBearerStore) ReadMcpBearer(_ context.Context, tenant, id string) (string, error) {
	key := tenant + "/" + id
	if v, ok := s.tokens[key]; ok {
		return v, nil
	}
	return "", secrets.ErrMcpBearerNotFound
}

func (s *fakeBearerStore) DeleteMcpBearer(_ context.Context, path string) error {
	delete(s.tokens, path)
	return nil
}

// TestInvokeMCP_CallsToolAndReturnsContent verifies that Proxy.Invoke with an
// mcp:<C>:<tool> id connects to the fake MCP server and returns the tool's text.
func TestInvokeMCP_CallsToolAndReturnsContent(t *testing.T) {
	fake := newFakeMCPServer("sid-handler")
	fake.addTool("echo", "Echo tool", map[string]any{"type": "object"})
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	connID := uuid.New().String()
	repo := &fakeConnRepo{
		conns: map[string]*db.McpConnection{
			connID: {
				ID:       uuid.MustParse(connID),
				URL:      srv.URL,
				AuthType: "none",
			},
		},
	}

	proxy := New(Config{
		McpConnections:  repo,
		AllowMcpPrivate: true,
	}, zap.NewNop())

	res, err := proxy.Invoke(context.Background(), Request{
		ToolID:   "mcp:" + connID + ":echo",
		Args:     map[string]any{"input": "hello"},
		TenantID: "tenant-1",
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !res.Success {
		t.Fatalf("expected Success=true, got error: %s", res.Error)
	}
	if res.Output["content"] != "result of echo" {
		t.Errorf("content = %q, want %q", res.Output["content"], "result of echo")
	}
}

// TestInvokeMCP_BearerAttached verifies the bearer token is sent to the server
// when auth_type=bearer and the store holds a token.
func TestInvokeMCP_BearerAttached(t *testing.T) {
	fake := newFakeMCPServer("sid-bearer-h")
	fake.addTool("t", "d", map[string]any{"type": "object"})
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	connID := uuid.New().String()
	tenantID := "tenant-bearer"
	bearerToken := "my-secret-bearer"

	repo := &fakeConnRepo{
		conns: map[string]*db.McpConnection{
			connID: {
				ID:       uuid.MustParse(connID),
				URL:      srv.URL,
				AuthType: "bearer",
			},
		},
	}
	store := &fakeBearerStore{
		tokens: map[string]string{tenantID + "/" + connID: bearerToken},
	}

	proxy := New(Config{
		McpConnections:  repo,
		McpBearer:       store,
		AllowMcpPrivate: true,
	}, zap.NewNop())

	res, err := proxy.Invoke(context.Background(), Request{
		ToolID:   "mcp:" + connID + ":t",
		Args:     nil,
		TenantID: tenantID,
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !res.Success {
		t.Fatalf("expected success, got: %s", res.Error)
	}
	// Verify the server received the bearer.
	got, _ := fake.lastBearer.Load().(string)
	if got != "Bearer "+bearerToken {
		t.Errorf("Authorization header = %q, want %q", got, "Bearer "+bearerToken)
	}
}

// TestInvokeMCP_NoMcpDeps_ReturnsNotConfigured verifies invokeMCP is a no-op
// when no MCP deps are wired.
func TestInvokeMCP_NoMcpDeps_ReturnsNotConfigured(t *testing.T) {
	proxy := New(Config{}, zap.NewNop())
	res, err := proxy.Invoke(context.Background(), Request{
		ToolID:   "mcp:conn-123:any-tool",
		TenantID: "t",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Success {
		t.Error("want Success=false when MCP not configured")
	}
}

// TestInvokeMCP_MissingConnection_ReturnsError verifies a missing connector ID
// produces a non-fatal tool error (not a proxy-level error).
func TestInvokeMCP_MissingConnection_ReturnsError(t *testing.T) {
	repo := &fakeConnRepo{conns: map[string]*db.McpConnection{}}
	proxy := New(Config{McpConnections: repo}, zap.NewNop())

	res, err := proxy.Invoke(context.Background(), Request{
		ToolID:   "mcp:does-not-exist:tool",
		TenantID: "t",
	})
	if err != nil {
		t.Fatalf("unexpected proxy-level error: %v", err)
	}
	if res.Success {
		t.Error("want Success=false for missing connection")
	}
	if res.Error == "" {
		t.Error("want non-empty Error string")
	}
}

// TestInvokeMCP_ArgSchemaValidation_InvalidArgs verifies that when a tool
// advertises an inputSchema with a required property, passing args that violate
// it returns Success=false with "invalid arguments" in the error, and does NOT
// hit the tools/call endpoint.
func TestInvokeMCP_ArgSchemaValidation_InvalidArgs(t *testing.T) {
	fake := newFakeMCPServer("sid-schema-invalid")
	// Tool requires string property "q"; passing empty args should fail validation.
	fake.addTool("search", "Search tool", map[string]any{
		"type": "object",
		"properties": map[string]any{
			"q": map[string]any{"type": "string"},
		},
		"required": []any{"q"},
	})
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	connID := uuid.New().String()
	repo := &fakeConnRepo{
		conns: map[string]*db.McpConnection{
			connID: {
				ID:       uuid.MustParse(connID),
				URL:      srv.URL,
				AuthType: "none",
			},
		},
	}
	proxy := New(Config{
		McpConnections:  repo,
		AllowMcpPrivate: true,
	}, zap.NewNop())

	// Capture request count before the call (after connect, which sends
	// initialize + notifications/initialized = 2 requests).
	// We'll compare after: tools/list adds 1 more; tools/call must NOT be hit.
	res, err := proxy.Invoke(context.Background(), Request{
		ToolID:   "mcp:" + connID + ":search",
		Args:     map[string]any{}, // missing required "q"
		TenantID: "tenant-schema",
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.Success {
		t.Fatal("want Success=false for invalid args, got true")
	}
	if !strings.Contains(res.Error, "invalid arguments") {
		t.Errorf("want 'invalid arguments' in error, got: %s", res.Error)
	}
	// tools/call must not have been hit: request sequence is
	// initialize(1) + notifications/initialized(2) + tools/list(3) = 3 total.
	got := int(fake.requestCount)
	if got != 3 {
		t.Errorf("want 3 requests (init+notif+list, no tools/call), got %d", got)
	}
}

// TestInvokeMCP_ArgSchemaValidation_ValidArgs verifies that valid args pass
// schema validation and the call proceeds to tools/call normally.
func TestInvokeMCP_ArgSchemaValidation_ValidArgs(t *testing.T) {
	fake := newFakeMCPServer("sid-schema-valid")
	fake.addTool("search", "Search tool", map[string]any{
		"type": "object",
		"properties": map[string]any{
			"q": map[string]any{"type": "string"},
		},
		"required": []any{"q"},
	})
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	connID := uuid.New().String()
	repo := &fakeConnRepo{
		conns: map[string]*db.McpConnection{
			connID: {
				ID:       uuid.MustParse(connID),
				URL:      srv.URL,
				AuthType: "none",
			},
		},
	}
	proxy := New(Config{
		McpConnections:  repo,
		AllowMcpPrivate: true,
	}, zap.NewNop())

	res, err := proxy.Invoke(context.Background(), Request{
		ToolID:   "mcp:" + connID + ":search",
		Args:     map[string]any{"q": "hello"},
		TenantID: "tenant-schema",
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !res.Success {
		t.Fatalf("want Success=true for valid args, got error: %s", res.Error)
	}
	// tools/call must have been hit: init(1) + notif(2) + list(3) + call(4) = 4.
	got := int(fake.requestCount)
	if got != 4 {
		t.Errorf("want 4 requests (init+notif+list+call), got %d", got)
	}
}

// TestInvokeMCP_ArgSchemaValidation_EmptySchema verifies that a tool with an
// empty/missing inputSchema passes through to tools/call unchanged (no
// validation = no block).
func TestInvokeMCP_ArgSchemaValidation_EmptySchema(t *testing.T) {
	fake := newFakeMCPServer("sid-schema-empty")
	// addTool with nil/empty schema: inputSchema will marshal to "null" or "{}".
	fake.addTool("noop", "No schema tool", map[string]any{})
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	connID := uuid.New().String()
	repo := &fakeConnRepo{
		conns: map[string]*db.McpConnection{
			connID: {
				ID:       uuid.MustParse(connID),
				URL:      srv.URL,
				AuthType: "none",
			},
		},
	}
	proxy := New(Config{
		McpConnections:  repo,
		AllowMcpPrivate: true,
	}, zap.NewNop())

	res, err := proxy.Invoke(context.Background(), Request{
		ToolID:   "mcp:" + connID + ":noop",
		Args:     map[string]any{},
		TenantID: "tenant-schema",
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !res.Success {
		t.Fatalf("want Success=true (pass-through for empty schema), got error: %s", res.Error)
	}
	// tools/call must have been hit: init(1) + notif(2) + list(3) + call(4) = 4.
	got := int(fake.requestCount)
	if got != 4 {
		t.Errorf("want 4 requests (init+notif+list+call), got %d", got)
	}
}
