package toolproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeMCPServer is a minimal httptest server that speaks JSON-RPC 2.0 over
// streamable HTTP (POST to one endpoint). It records requests for assertions.
type fakeMCPServer struct {
	sessionID    string
	tools        []map[string]any
	toolResults  map[string]any // tool name → content string
	toolIsError  map[string]bool
	requestCount int32
	// lastBearer records the Authorization header value from the last request.
	lastBearer atomic.Value
}

func newFakeMCPServer(sessionID string) *fakeMCPServer {
	return &fakeMCPServer{
		sessionID:   sessionID,
		toolResults: map[string]any{},
		toolIsError: map[string]bool{},
	}
}

func (f *fakeMCPServer) addTool(name, description string, schema map[string]any) {
	f.tools = append(f.tools, map[string]any{
		"name":        name,
		"description": description,
		"inputSchema": schema,
	})
	f.toolResults[name] = "result of " + name
}

func (f *fakeMCPServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.requestCount, 1)
		f.lastBearer.Store(r.Header.Get("Authorization"))

		var req struct {
			JSONRPC string           `json:"jsonrpc"`
			ID      *json.RawMessage `json:"id"`
			Method  string           `json:"method"`
			Params  json.RawMessage  `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}

		// notifications/initialized has no id and no response needed.
		if req.Method == "notifications/initialized" {
			w.WriteHeader(http.StatusAccepted)
			return
		}

		w.Header().Set("Content-Type", "application/json")

		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", f.sessionID)
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]any{
					"protocolVersion": "2025-06-18",
					"capabilities":    map[string]any{},
					"serverInfo":      map[string]any{"name": "fake-mcp", "version": "0.1"},
				},
			})

		case "tools/list":
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result":  map[string]any{"tools": f.tools},
			})

		case "tools/call":
			var params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			json.Unmarshal(req.Params, &params) //nolint:errcheck

			if f.toolIsError[params.Name] {
				json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
					"jsonrpc": "2.0",
					"id":      req.ID,
					"error":   map[string]any{"code": -32000, "message": "tool execution failed"},
				})
				return
			}
			result := fmt.Sprintf("%v", f.toolResults[params.Name])
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]any{
					"content": []map[string]any{
						{"type": "text", "text": result},
					},
					"isError": false,
				},
			})

		default:
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				"jsonrpc": "2.0",
				"id":      req.ID,
				"error":   map[string]any{"code": -32601, "message": "method not found"},
			})
		}
	})
}

func TestMCPClient_Handshake_CapturesSessionID(t *testing.T) {
	fake := newFakeMCPServer("test-session-abc")
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	client := newMCPClient(srv.URL, "bearer-token-x", true /* allowPrivate */)
	if err := client.connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if client.sessionID != "test-session-abc" {
		t.Errorf("sessionID = %q, want %q", client.sessionID, "test-session-abc")
	}
}

func TestMCPClient_ListTools(t *testing.T) {
	fake := newFakeMCPServer("sid-1")
	fake.addTool("echo", "Echo text back", map[string]any{
		"type": "object",
		"properties": map[string]any{
			"text": map[string]any{"type": "string"},
		},
	})
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	client := newMCPClient(srv.URL, "", true)
	if err := client.connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	tools, err := client.listTools(context.Background())
	if err != nil {
		t.Fatalf("listTools: %v", err)
	}
	if len(tools) != 1 {
		t.Fatalf("want 1 tool, got %d", len(tools))
	}
	if tools[0].Name != "echo" {
		t.Errorf("Name = %q, want %q", tools[0].Name, "echo")
	}
	if tools[0].Description != "Echo text back" {
		t.Errorf("Description = %q", tools[0].Description)
	}
	if !strings.Contains(tools[0].InputSchemaJSON, "object") {
		t.Errorf("InputSchemaJSON missing 'object': %q", tools[0].InputSchemaJSON)
	}
}

func TestMCPClient_CallTool_ReturnsTextContent(t *testing.T) {
	fake := newFakeMCPServer("sid-2")
	fake.addTool("greet", "Greet someone", map[string]any{"type": "object"})
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	client := newMCPClient(srv.URL, "", true)
	if err := client.connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	content, isErr, err := client.callTool(context.Background(), "greet", map[string]any{"name": "alice"})
	if err != nil {
		t.Fatalf("callTool: %v", err)
	}
	if isErr {
		t.Errorf("isError should be false")
	}
	if content != "result of greet" {
		t.Errorf("content = %q, want %q", content, "result of greet")
	}
}

func TestMCPClient_CallTool_JSONRPCError_GoError(t *testing.T) {
	fake := newFakeMCPServer("sid-3")
	fake.addTool("bad", "A failing tool", map[string]any{"type": "object"})
	fake.toolIsError["bad"] = true
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	client := newMCPClient(srv.URL, "", true)
	if err := client.connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	_, _, err := client.callTool(context.Background(), "bad", nil)
	if err == nil {
		t.Fatal("want error for JSON-RPC error response, got nil")
	}
	if !strings.Contains(err.Error(), "tool execution failed") {
		t.Errorf("error %q does not mention 'tool execution failed'", err.Error())
	}
}

func TestMCPClient_BearerHeader_SentOnRequests(t *testing.T) {
	fake := newFakeMCPServer("sid-bearer")
	fake.addTool("t", "d", map[string]any{"type": "object"})
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	client := newMCPClient(srv.URL, "my-secret-token", true)
	if err := client.connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	// After connect, the initialize request should have sent the bearer.
	got, _ := fake.lastBearer.Load().(string)
	if got != "Bearer my-secret-token" {
		t.Errorf("Authorization header = %q, want %q", got, "Bearer my-secret-token")
	}
}

func TestMCPClient_CallTool_IsErrorTrue_NoGoError(t *testing.T) {
	// A successful JSON-RPC response (no error object) with isError=true in result
	// must be distinct from a JSON-RPC error: callTool returns (text, true, nil).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string           `json:"method"`
			ID     *json.RawMessage `json:"id"`
		}
		json.NewDecoder(r.Body).Decode(&req) //nolint:errcheck

		if req.Method == "notifications/initialized" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "sid-iserr")
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]any{
					"protocolVersion": "2025-06-18",
					"capabilities":    map[string]any{},
					"serverInfo":      map[string]any{"name": "fake-mcp", "version": "0.1"},
				},
			})
		case "tools/call":
			// Successful JSON-RPC response (no error field), but tool signals failure via isError.
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]any{
					"content": []map[string]any{
						{"type": "text", "text": "oops"},
					},
					"isError": true,
				},
			})
		default:
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				"jsonrpc": "2.0",
				"id":      req.ID,
				"error":   map[string]any{"code": -32601, "message": "method not found"},
			})
		}
	}))
	defer srv.Close()

	client := newMCPClient(srv.URL, "", true)
	if err := client.connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	content, isErr, err := client.callTool(context.Background(), "any-tool", nil)
	if err != nil {
		t.Fatalf("callTool must not return a Go error for isError=true result, got: %v", err)
	}
	if !isErr {
		t.Errorf("isError should be true")
	}
	if content != "oops" {
		t.Errorf("content = %q, want %q", content, "oops")
	}
}

func TestMCPClient_ListTools_ReadOnlyHint(t *testing.T) {
	// One tool with readOnlyHint=true, one without annotations at all.
	// Verifies the annotation decode: hint is preserved and missing defaults to false.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string           `json:"method"`
			ID     *json.RawMessage `json:"id"`
		}
		json.NewDecoder(r.Body).Decode(&req) //nolint:errcheck
		if req.Method == "notifications/initialized" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "sid-hint")
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]any{
					"protocolVersion": "2025-06-18",
					"capabilities":    map[string]any{},
					"serverInfo":      map[string]any{"name": "hint-mcp", "version": "0.1"},
				},
			})
		case "tools/list":
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]any{
					"tools": []map[string]any{
						{
							"name":        "get_article",
							"description": "Fetch an article",
							"inputSchema": map[string]any{"type": "object"},
							"annotations": map[string]any{"readOnlyHint": true},
						},
						{
							"name":        "create_doc",
							"description": "Create a document",
							"inputSchema": map[string]any{"type": "object"},
							// no annotations field
						},
					},
				},
			})
		default:
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				"jsonrpc": "2.0",
				"id":      req.ID,
				"error":   map[string]any{"code": -32601, "message": "method not found"},
			})
		}
	}))
	defer srv.Close()

	client := NewMCPClientAllowPrivate(srv.URL, "")
	if err := client.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	tools, err := client.ListTools(context.Background())
	if err != nil {
		t.Fatalf("listTools: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("want 2 tools, got %d", len(tools))
	}
	if !tools[0].ReadOnlyHint {
		t.Errorf("tools[0] (get_article): ReadOnlyHint want true, got false")
	}
	if tools[1].ReadOnlyHint {
		t.Errorf("tools[1] (create_doc): ReadOnlyHint want false, got true")
	}
}

func TestMCPClient_SSEFramedResponse(t *testing.T) {
	// A server that replies to initialize with SSE framing (text/event-stream,
	// data: <json> line). The client must parse the first data: line.
	sessionID := "sse-session-1"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string           `json:"method"`
			ID     *json.RawMessage `json:"id"`
		}
		json.NewDecoder(r.Body).Decode(&req) //nolint:errcheck

		if req.Method == "notifications/initialized" {
			w.WriteHeader(http.StatusAccepted)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Mcp-Session-Id", sessionID)
		payload, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"result": map[string]any{
				"protocolVersion": "2025-06-18",
				"capabilities":    map[string]any{},
				"serverInfo":      map[string]any{"name": "sse-mcp", "version": "0.1"},
			},
		})
		fmt.Fprintf(w, "data: %s\n\n", payload)
	}))
	defer srv.Close()

	client := newMCPClient(srv.URL, "", true)
	if err := client.connect(context.Background()); err != nil {
		t.Fatalf("connect with SSE response: %v", err)
	}
	if client.sessionID != sessionID {
		t.Errorf("sessionID = %q, want %q", client.sessionID, sessionID)
	}
}
