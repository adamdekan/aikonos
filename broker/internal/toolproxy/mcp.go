// broker/internal/toolproxy/mcp.go
//
// Minimal hand-rolled MCP client over streamable HTTP (one POST endpoint,
// JSON-RPC 2.0). Mirrors the Vault client style: no heavy SDK, deliberate
// surface — initialize → notifications/initialized → tools/list / tools/call.
//
// The HTTP transport uses the same guarded dialer as web_fetch: loopback and
// private-IP targets are rejected at every TCP connect, covering the initial
// URL and any redirect. An allowPrivate flag lets tests reach httptest servers.
package toolproxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

const (
	mcpProtocolVersion = "2025-06-18"
	mcpClientName      = "aikonos-broker"
	mcpClientVersion   = "0.1"
	mcpTimeout         = 30 * time.Second
)

// McpToolSchema is the discovery result for one tool on an MCP server.
type McpToolSchema struct {
	Name            string
	Description     string
	InputSchemaJSON string // JSON-encoded inputSchema object
	ReadOnlyHint    bool
}

// MCPClient is the exported handle returned by NewMCPClientPublic.
// It wraps mcpClient for callers outside the toolproxy package (e.g. the
// broker MCP service handler for ListMcpServerTools).
type MCPClient struct{ inner *mcpClient }

// NewMCPClientPublic constructs an MCPClient with the SSRF guard enabled
// (allowPrivate=false). Intended for broker-internal callers that load the
// server URL from the admin-managed DB (trusted source), but still keep the
// guard on as defence-in-depth.
func NewMCPClientPublic(baseURL, bearer string) *MCPClient {
	return &MCPClient{inner: newMCPClient(baseURL, bearer, false)}
}

// NewMCPClientAllowPrivate constructs an MCPClient with the SSRF guard
// disabled. Only for unit tests that reach httptest servers on 127.0.0.1.
func NewMCPClientAllowPrivate(baseURL, bearer string) *MCPClient {
	return &MCPClient{inner: newMCPClient(baseURL, bearer, true)}
}

// Connect performs the MCP handshake (initialize + notifications/initialized).
func (c *MCPClient) Connect(ctx context.Context) error { return c.inner.connect(ctx) }

// ListTools retrieves the tool list (tools/list).
func (c *MCPClient) ListTools(ctx context.Context) ([]McpToolSchema, error) {
	return c.inner.listTools(ctx)
}

// CallTool invokes a named tool (tools/call) and returns text content.
func (c *MCPClient) CallTool(ctx context.Context, name string, args map[string]any) (content string, isError bool, err error) {
	return c.inner.callTool(ctx, name, args)
}

// mcpClient holds the connection state for one MCP server session.
type mcpClient struct {
	baseURL   string
	bearer    string
	httpc     *http.Client
	sessionID string // set after initialize; echoed on subsequent requests
	idCounter uint64 // JSON-RPC request id sequence
}

// newMCPClient constructs an mcpClient. allowPrivate should be false in
// production (enables the SSRF guard); true in tests to reach 127.0.0.1.
func newMCPClient(baseURL, bearer string, allowPrivate bool) *mcpClient {
	return &mcpClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		bearer:  bearer,
		httpc:   hardenedClient(mcpTimeout, allowPrivate, nil),
	}
}

// connect performs the MCP handshake: POST initialize, capture the
// Mcp-Session-Id header from the response, then POST notifications/initialized.
func (c *mcpClient) connect(ctx context.Context) error {
	result, respHeader, err := c.doRPCWithHeader(ctx, "initialize", map[string]any{
		"protocolVersion": mcpProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": mcpClientName, "version": mcpClientVersion},
	})
	if err != nil {
		return fmt.Errorf("mcp: initialize: %w", err)
	}
	_ = result
	c.sessionID = respHeader.Get("Mcp-Session-Id")

	// Notify the server that initialization is complete (no response expected).
	if err := c.sendNotification(ctx, "notifications/initialized", nil); err != nil {
		return fmt.Errorf("mcp: notifications/initialized: %w", err)
	}
	return nil
}

// listTools retrieves the tool list from the server (POST tools/list).
func (c *mcpClient) listTools(ctx context.Context) ([]McpToolSchema, error) {
	result, err := c.doRPC(ctx, "tools/list", map[string]any{})
	if err != nil {
		return nil, fmt.Errorf("mcp: tools/list: %w", err)
	}

	var resp struct {
		Tools []struct {
			Name        string         `json:"name"`
			Description string         `json:"description"`
			InputSchema map[string]any `json:"inputSchema"`
			Annotations struct {
				ReadOnlyHint bool `json:"readOnlyHint"`
			} `json:"annotations"`
		} `json:"tools"`
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("mcp: marshal tools/list result: %w", err)
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("mcp: decode tools/list: %w", err)
	}

	out := make([]McpToolSchema, 0, len(resp.Tools))
	for _, t := range resp.Tools {
		schemaJSON, err := json.Marshal(t.InputSchema)
		if err != nil {
			return nil, fmt.Errorf("mcp: marshal inputSchema for %q: %w", t.Name, err)
		}
		out = append(out, McpToolSchema{
			Name:            t.Name,
			Description:     t.Description,
			InputSchemaJSON: string(schemaJSON),
			ReadOnlyHint:    t.Annotations.ReadOnlyHint,
		})
	}
	return out, nil
}

// callTool invokes a tool and returns the concatenated text content. A
// JSON-RPC error object returns a Go error. result.isError=true sets isError.
func (c *mcpClient) callTool(ctx context.Context, name string, args map[string]any) (content string, isError bool, err error) {
	if args == nil {
		args = map[string]any{}
	}
	result, rpcErr := c.doRPC(ctx, "tools/call", map[string]any{
		"name":      name,
		"arguments": args,
	})
	if rpcErr != nil {
		return "", false, fmt.Errorf("mcp: tools/call %q: %w", name, rpcErr)
	}

	var resp struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	raw, _ := json.Marshal(result)
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", false, fmt.Errorf("mcp: decode tools/call result: %w", err)
	}

	var sb strings.Builder
	for _, c := range resp.Content {
		if c.Type == "text" {
			sb.WriteString(c.Text)
		}
	}
	return sb.String(), resp.IsError, nil
}

// doRPC sends a JSON-RPC 2.0 request and returns the parsed result object.
// JSON-RPC errors are returned as Go errors.
func (c *mcpClient) doRPC(ctx context.Context, method string, params any) (map[string]any, error) {
	result, _, err := c.doRPCWithHeader(ctx, method, params)
	return result, err
}

// doRPCWithHeader is like doRPC but also returns the HTTP response headers so
// connect() can capture Mcp-Session-Id from the initialize response.
func (c *mcpClient) doRPCWithHeader(ctx context.Context, method string, params any) (map[string]any, http.Header, error) {
	id := atomic.AddUint64(&c.idCounter, 1)
	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	if c.sessionID != "" {
		req.Header.Set("Mcp-Session-Id", c.sessionID)
	}

	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("mcp: POST %s: %w", method, err)
	}
	defer resp.Body.Close()

	ct := resp.Header.Get("Content-Type")

	// Parse: either a direct JSON body or an SSE stream (first data: line).
	var rpcResp struct {
		Result map[string]any `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}

	if strings.Contains(ct, "text/event-stream") {
		// Streamable HTTP: read lines until we find a "data:" event.
		scanner := bufio.NewScanner(resp.Body)
		found := false
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "data:") {
				data := strings.TrimPrefix(line, "data:")
				data = strings.TrimSpace(data)
				if err := json.Unmarshal([]byte(data), &rpcResp); err != nil {
					return nil, resp.Header, fmt.Errorf("mcp: decode SSE data: %w", err)
				}
				found = true
				break
			}
		}
		if !found {
			return nil, resp.Header, fmt.Errorf("mcp: no data: line in SSE response for %s", method)
		}
	} else {
		if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
			return nil, resp.Header, fmt.Errorf("mcp: decode response for %s: %w", method, err)
		}
	}

	if rpcResp.Error != nil {
		return nil, resp.Header, fmt.Errorf("mcp: JSON-RPC error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
	}
	return rpcResp.Result, resp.Header, nil
}

// sendNotification POSTs a JSON-RPC notification (no id, no response expected).
// A non-2xx status is ignored — the server may return 202 Accepted or 200.
func (c *mcpClient) sendNotification(ctx context.Context, method string, params any) error {
	if params == nil {
		params = map[string]any{}
	}
	payload := map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	if c.sessionID != "" {
		req.Header.Set("Mcp-Session-Id", c.sessionID)
	}
	resp, err := c.httpc.Do(req)
	if err != nil {
		return fmt.Errorf("mcp: notification %s: %w", method, err)
	}
	// Drain a small cap before closing; prevents the server from stalling on a
	// write if the buffer is full, without letting a misbehaving server stream
	// an unbounded response body (non-2xx is still ignored per the contract).
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096)) //nolint:errcheck
	resp.Body.Close()
	return nil
}
