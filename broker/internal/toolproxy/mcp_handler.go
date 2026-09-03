// broker/internal/toolproxy/mcp_handler.go
//
// invokeMCP executes an MCP tool call via the hand-rolled mcpClient (mcp.go).
// It mirrors the gdrive/onedrive handler pattern: load the connection record,
// read the bearer from Vault (if auth_type==bearer), newMCPClient → connect →
// callTool, return content as the handler output map.
//
// The handler is NOT registered in the static handlers map — Invoke dispatches
// to it by prefix check ("mcp:") for dynamically-named tool ids.
//
// Arg schema validation: after connect, before callTool, the handler calls
// tools/list, finds the target tool's inputSchema, compiles it (cached), and
// validates req.Args against it. An empty/unparseable schema or a listTools
// error is a pass-through (never blocks the call). Only a well-formed schema
// with a schema violation returns Result{Success:false} before the external call.
package toolproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/secrets"
	"github.com/adamdekan/aikonos/broker/internal/toolregistry"
)

// mcpSchemaCache holds compiled JSON-Schema objects keyed by the raw
// InputSchemaJSON string. A schema is compiled once and reused across calls.
type mcpSchemaCache struct {
	mu      sync.Mutex
	schemas map[string]*jsonschema.Schema // key = raw InputSchemaJSON
}

func newMcpSchemaCache() *mcpSchemaCache {
	return &mcpSchemaCache{schemas: map[string]*jsonschema.Schema{}}
}

// compileMcpSchema returns the compiled Schema for schemaJSON, or (nil, false)
// when the schema is empty, unparseable, or uncompilable. Errors are silently
// treated as pass-through — the MCP server remains the authority.
func (c *mcpSchemaCache) compileMcpSchema(schemaJSON string) (*jsonschema.Schema, bool) {
	if strings.TrimSpace(schemaJSON) == "" ||
		schemaJSON == "null" ||
		schemaJSON == "{}" {
		return nil, false
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if sch, ok := c.schemas[schemaJSON]; ok {
		return sch, true
	}

	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(schemaJSON))
	if err != nil {
		return nil, false
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("mem://tool.json", doc); err != nil {
		return nil, false
	}
	sch, err := compiler.Compile("mem://tool.json")
	if err != nil {
		return nil, false
	}

	c.schemas[schemaJSON] = sch
	return sch, true
}

// mcpProxyDeps holds what the MCP proxy path needs beyond the static handler set.
type mcpProxyDeps struct {
	connections mcpConnectionLookup
	bearer      secrets.McpBearerStore
	// allowPrivate disables the SSRF guard. False in production; true in tests
	// that reach httptest servers on 127.0.0.1.
	allowPrivate bool
}

// mcpConnectionLookup is the minimal interface the MCP proxy needs from the
// connection repo. Satisfied by *db.McpConnectionRepo; extracted for test fakes.
type mcpConnectionLookup interface {
	Get(ctx context.Context, tenantID, id string) (*db.McpConnection, error)
}

// invokeMCP handles a tool_id of the form "mcp:<connectorId>:<tool>".
// It is called from Proxy.Invoke when the static handler map yields no match
// and the tool_id carries the "mcp:" prefix.
func (p *Proxy) invokeMCP(ctx context.Context, req Request) (*Result, error) {
	if p.mcpDeps == nil {
		return &Result{Success: false, Error: "MCP tool proxy not configured"}, nil
	}

	connID, toolName, ok := toolregistry.ParseMcpToolID(req.ToolID)
	if !ok {
		// Should not happen: Invoke only calls invokeMCP when the prefix matched.
		return nil, fmt.Errorf("toolproxy: malformed mcp tool_id %q", req.ToolID)
	}

	conn, err := p.mcpDeps.connections.Get(ctx, req.TenantID, connID)
	if err != nil {
		return &Result{Success: false, Error: fmt.Sprintf("mcp: load connection %q: %v", connID, err)}, nil
	}

	var bearer string
	if conn.AuthType == "bearer" && p.mcpDeps.bearer != nil {
		b, rerr := p.mcpDeps.bearer.ReadMcpBearer(ctx, req.TenantID, connID)
		if rerr != nil && rerr != secrets.ErrMcpBearerNotFound {
			return &Result{Success: false, Error: fmt.Sprintf("mcp: read bearer: %v", rerr)}, nil
		}
		bearer = b // empty on ErrMcpBearerNotFound → unauthenticated (server tolerates)
	}

	client := newMCPClient(conn.URL, bearer, p.mcpDeps.allowPrivate)
	if err := client.connect(ctx); err != nil {
		return &Result{Success: false, Error: fmt.Sprintf("mcp: connect %q: %v", conn.URL, err)}, nil
	}

	// Arg schema validation: on any error from listTools, or when the tool or
	// its schema can't be found/compiled, fall through to callTool unchanged.
	if p.mcpSchemas != nil {
		if tools, lerr := client.listTools(ctx); lerr == nil {
			for _, t := range tools {
				if t.Name != toolName {
					continue
				}
				if sch, ok := p.mcpSchemas.compileMcpSchema(t.InputSchemaJSON); ok {
					// A no-args call is a valid empty object — nil would marshal
					// to JSON null and fail an `{"type":"object"}` schema.
					args := req.Args
					if args == nil {
						args = map[string]any{}
					}
					// JSON round-trip to normalize number types for jsonschema/v6.
					b, _ := json.Marshal(args)
					argsAny, _ := jsonschema.UnmarshalJSON(bytes.NewReader(b))
					if verr := sch.Validate(argsAny); verr != nil {
						return &Result{
							Success: false,
							Error:   fmt.Sprintf("mcp: invalid arguments for %q: %v", toolName, verr),
						}, nil
					}
				}
				break
			}
		}
	}

	content, isErr, err := client.callTool(ctx, toolName, req.Args)
	if err != nil {
		return &Result{Success: false, Error: fmt.Sprintf("mcp: callTool %q: %v", toolName, err)}, nil
	}

	out := map[string]any{
		"content":  content,
		"is_error": isErr,
		"tool":     toolName,
		"server":   conn.URL,
	}
	return &Result{Success: !isErr, Output: out, CostUnits: 1}, nil
}
