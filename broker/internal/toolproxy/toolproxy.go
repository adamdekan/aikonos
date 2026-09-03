// broker/internal/toolproxy/toolproxy.go
//
// The Tool Proxy executes a registered tool on behalf of an authorized caller.
// It is the component InvokeTool routes to *after* the Biscuit capability gate
// passes — so by the time a handler runs, the call is already authorized for the
// tool's scope. The proxy's own job is the controlled side-effect: enforcing
// per-tool guards (egress allowlist / SSRF for network tools, path-traversal for
// writes) and shaping the result.
//
// This is an in-process proxy (a package the broker calls directly). The Proxy
// interface is the seam where a real out-of-process egress/DLP proxy would slot
// in later; handlers here are the MVP implementations of the skills/ manifests.
package toolproxy

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/adamdekan/aikonos/broker/internal/connector"
	"github.com/adamdekan/aikonos/broker/internal/secrets"
	"github.com/adamdekan/aikonos/broker/internal/workspacefs"
	planv1 "github.com/adamdekan/aikonos/gen/go/plan/v1"
)

// Request is one tool invocation. Args/EffectClass come straight off the plan
// step; the proxy does not re-run policy (that happened at SubmitPlan) but a
// handler may use EffectClass to pick a stricter posture.
type Request struct {
	ToolID   string
	Args     map[string]any
	TaskID   string
	TenantID string
	UserID   string
	// AgentID is the bound agent of the task this call belongs to, populated by
	// InvokeTool from the task row — never from the caller's request, since a
	// caller-supplied agent id would be a cross-agent memory read (memory.go's
	// scope=agent resolves the bundle segment from it). Empty for a personal,
	// non-agent-bound call.
	AgentID     string
	EffectClass planv1.EffectClass
}

// wsKey is the per-user workspace segment: the owning user when known (so a
// user's documents persist across tasks/conversations), else the task id (the
// server-side executor path, which has no user). Sanitised by the caller.
func (r Request) wsKey() string {
	if r.UserID != "" {
		return r.UserID
	}
	return r.TaskID
}

// Result is a tool outcome. A failed tool (bad URL, HTTP error, validation) is a
// normal Result with Success=false — only proxy-internal problems (unknown tool)
// return a Go error from Invoke.
type Result struct {
	Success   bool
	Output    map[string]any
	Error     string
	CostUnits int64
}

// Handler executes a single tool. It returns the tool output, a cost estimate in
// cost units, and an error for a tool-level failure (surfaced as Result.Error).
type Handler func(ctx context.Context, req Request) (output map[string]any, cost int64, err error)

// Proxy routes a Request to the handler registered for its tool id.
type Proxy struct {
	handlers map[string]Handler
	logger   *zap.Logger
	// mcpDeps backs the dynamic mcp:<C>:<tool> routing path. Nil disables MCP.
	mcpDeps *mcpProxyDeps
	// mcpSchemas is the compiled-schema cache for MCP arg validation.
	mcpSchemas *mcpSchemaCache
}

// ErrUnknownTool is returned by Invoke when no handler is registered for the
// tool id. InvokeTool already deny-by-defaults unknown tools via the registry,
// so reaching this means a registry/handler drift — a programming error.
var ErrUnknownTool = fmt.Errorf("toolproxy: no handler registered for tool")

// New builds a Proxy with the default MVP handler set.
func New(cfg Config, logger *zap.Logger) *Proxy {
	if logger == nil {
		logger = zap.NewNop()
	}
	p := &Proxy{handlers: map[string]Handler{}, logger: logger}
	// Each static tool self-registers a ToolPlugin via init() (see plugin.go);
	// New only asks "is cfg wired for this handler's deps" and builds it.
	for _, id := range RegisteredToolIDs() {
		plugin := plugins[id]
		if plugin.Available(cfg) {
			p.handlers[id] = plugin.Build(cfg)
		}
	}
	// MCP dynamic routing: wired when a connection repo is provided. The bearer
	// store is optional (nil → no Vault lookup; connections with auth_type=none
	// continue to work without it).
	if cfg.McpConnections != nil {
		p.mcpDeps = &mcpProxyDeps{
			connections:  cfg.McpConnections,
			bearer:       cfg.McpBearer,
			allowPrivate: cfg.AllowMcpPrivate,
		}
		p.mcpSchemas = newMcpSchemaCache()
	}
	return p
}

// Register adds or replaces a handler (used by tests).
func (p *Proxy) Register(toolID string, h Handler) { p.handlers[toolID] = h }

// Tools returns the registered tool ids (for diagnostics).
func (p *Proxy) Tools() []string {
	out := make([]string, 0, len(p.handlers))
	for id := range p.handlers {
		out = append(out, id)
	}
	return out
}

// Invoke runs the handler for req.ToolID. Dynamic mcp:<C>:<tool> ids are
// routed to invokeMCP when no static handler is found. ErrUnknownTool is
// returned for any other unregistered id.
func (p *Proxy) Invoke(ctx context.Context, req Request) (*Result, error) {
	h, ok := p.handlers[req.ToolID]
	if !ok {
		// Dynamic MCP routing: any mcp:-prefixed id that has no static handler.
		if strings.HasPrefix(req.ToolID, "mcp:") {
			return p.invokeMCP(ctx, req)
		}
		return nil, fmt.Errorf("%w: %q", ErrUnknownTool, req.ToolID)
	}
	out, cost, err := h(ctx, req)
	if err != nil {
		p.logger.Info("toolproxy: tool failed",
			zap.String("tool_id", req.ToolID), zap.String("task_id", req.TaskID), zap.Error(err))
		return &Result{Success: false, Error: err.Error(), CostUnits: cost}, nil
	}
	return &Result{Success: true, Output: out, CostUnits: cost}, nil
}

// ResultStruct converts a Result's output map into a protobuf Struct for the
// InvokeToolResponse. Returns nil on a nil/empty map.
func ResultStruct(out map[string]any) (*structpb.Struct, error) {
	if len(out) == 0 {
		return nil, nil
	}
	return structpb.NewStruct(out)
}

// Config configures the default handler set.
type Config struct {
	WebFetch  WebFetchConfig
	WebSearch WebSearchConfig
	Workspace WorkspaceConfig
	// Secrets + OAuth enable the cloud-storage connector tools. Both nil → those
	// tools are not registered (InvokeTool then denies them via the registry).
	Secrets secrets.Provider
	OAuth   *connector.Registry
	// McpConnections enables the dynamic mcp:<C>:<tool> routing path. Nil → MCP
	// tool calls return a not-configured error (the capability gate would have
	// already denied an unknown tool, so this path is defence-in-depth).
	McpConnections mcpConnectionLookup
	// McpBearer reads per-connector bearer tokens from Vault. Nil → no Vault
	// lookup; connections with auth_type=none continue to work.
	McpBearer secrets.McpBearerStore
	// AllowMcpPrivate disables the SSRF guard on the MCP client. False in
	// production; set true only in unit tests reaching httptest servers.
	AllowMcpPrivate bool
	// OfficeWorkerURL is the office-worker base URL (docx/xlsx/pptx/pdf tools,
	// ). Empty → the office plugins report
	// unavailable, so a deployment without office-worker degrades cleanly.
	OfficeWorkerURL string
	// OfficeTimeout bounds each office-worker HTTP call. Zero → defaultOfficeTimeout.
	OfficeTimeout time.Duration
	// OfficeHTTPClient is injectable for tests; nil builds a default client.
	OfficeHTTPClient *http.Client
	// WorkspaceFS is the injected per-user workspace storage seam
	//.
	// doc.write/doc.read/workspace.read — and every office/cloudfile tool via
	// officeReadInput/officeWriteOutput, which they call directly — route
	// through it instead of the legacy Workspace.Root path when the request
	// carries a known user (req.UserID != ""). Nil (the default until main.go
	// wires one) means every handler behaves exactly as before this
	// checkpoint: the legacy safeJoin/os.* path against the per-(tenant,
	// task-or-user) local directory. A task-only request (no user — the
	// server-side executor path) always takes the legacy branch regardless.
	WorkspaceFS workspacefs.Backend
	// OBO resolves a tenant-managed OneDrive access token via on-behalf-of
	// exchange, preferred over the legacy
	// per-user OAuth connector (OAuth/Secrets above) for onedrive.* calls when
	// the caller's tenant has a configured M365 connection. Nil ⇒ legacy-only
	// (the default until main.go wires one). gdrive.* never consults this.
	OBO oboTokenSource
}
