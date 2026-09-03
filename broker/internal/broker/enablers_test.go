package broker

import (
	"context"
	"testing"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/adamdekan/aikonos/broker/internal/capability"
	"github.com/adamdekan/aikonos/broker/internal/toolproxy"
	"github.com/adamdekan/aikonos/broker/internal/toolregistry"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// Phase 7.0 enablers: per-step capability-token minting, the InvokeTool tenant
// fix, and the gateway-execution flag. The full SubmitPlan / ApproveTask flows
// need a Postgres-backed TaskRepo and are exercised by the in-cluster verify
// scripts; these tests cover the DB-free seams (minting + the InvokeTool gate),
// which carry the security-critical round-trip property.

// The minted token must pass InvokeTool's capability gate for the matching
// tool, and the request's tenant_id must reach the Tool Proxy (the 7.0c fix).
func TestMintStepTokens_RoundTripAndTenant(t *testing.T) {
	m, err := capability.GenerateMinter()
	if err != nil {
		t.Fatalf("GenerateMinter: %v", err)
	}

	var gotTenant string
	proxy := toolproxy.New(toolproxy.Config{}, zap.NewNop())
	proxy.Register("web.fetch", func(_ context.Context, req toolproxy.Request) (map[string]any, int64, error) {
		gotTenant = req.TenantID
		return map[string]any{"ok": true}, 1, nil
	})
	svc := NewSandboxService(Deps{Logger: zap.NewNop(), Capability: m, ToolProxy: proxy})

	tokens, err := mintStepTokens(m, toolregistry.PkgDefault(), "spiffe://aikonos.com/agent-gateway", "task-1",
		[]stepScope{{Seq: 1, ToolID: "web.fetch"}})
	if err != nil {
		t.Fatalf("mintStepTokens: %v", err)
	}
	tok, ok := tokens[1]
	if !ok || tok == "" {
		t.Fatalf("expected a token for seq 1, got %v", tokens)
	}

	args, _ := structpb.NewStruct(map[string]any{"url": "https://example.com"})
	resp, err := svc.InvokeTool(context.Background(), &brokerv1.InvokeToolRequest{
		TaskId: "task-1", ToolId: "web.fetch", CapabilityToken: tok, Args: args, TenantId: "tenant-xyz",
	})
	if err != nil {
		t.Fatalf("InvokeTool with minted token should pass the gate: %v", err)
	}
	if !resp.Success {
		t.Fatalf("expected success, got error %q", resp.Error)
	}
	if gotTenant != "tenant-xyz" {
		t.Errorf("InvokeTool tenant not propagated to proxy: got %q, want tenant-xyz", gotTenant)
	}
}

// A token minted for one tool's scope must not authorize a different tool.
func TestMintStepTokens_ScopeIsLeastPrivilege(t *testing.T) {
	m, err := capability.GenerateMinter()
	if err != nil {
		t.Fatalf("GenerateMinter: %v", err)
	}
	svc := NewSandboxService(Deps{Logger: zap.NewNop(), Capability: m})

	// Mint for doc.write (scope doc:write) but try to invoke siem.query (siem:read).
	tokens, err := mintStepTokens(m, toolregistry.PkgDefault(), "holder", "task-1", []stepScope{{Seq: 1, ToolID: "doc.write"}})
	if err != nil {
		t.Fatalf("mintStepTokens: %v", err)
	}
	_, err = svc.InvokeTool(context.Background(), &brokerv1.InvokeToolRequest{
		TaskId: "task-1", ToolId: "siem.query", CapabilityToken: tokens[1],
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("cross-tool token: want PermissionDenied, got %v", err)
	}
}

func TestMintStepTokens_NilMinterAndUnknownTool(t *testing.T) {
	if out, err := mintStepTokens(nil, toolregistry.PkgDefault(), "h", "t", []stepScope{{Seq: 1, ToolID: "web.fetch"}}); err != nil || out != nil {
		t.Fatalf("nil minter → nil map, got out=%v err=%v", out, err)
	}
	m, err := capability.GenerateMinter()
	if err != nil {
		t.Fatalf("GenerateMinter: %v", err)
	}
	out, err := mintStepTokens(m, toolregistry.PkgDefault(), "h", "t", []stepScope{{Seq: 1, ToolID: "totally.unregistered"}})
	if err != nil {
		t.Fatalf("mintStepTokens: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("unknown tool should be skipped (no scope), got %v", out)
	}
}

func TestHasGatewayHint(t *testing.T) {
	if !hasGatewayHint([]string{"web.fetch", gatewayExecutionHint}) {
		t.Error("should detect the gateway sentinel")
	}
	if hasGatewayHint([]string{"web.fetch", "doc.write"}) {
		t.Error("should not detect a gateway hint that is absent")
	}
	if hasGatewayHint(nil) {
		t.Error("nil hints → false")
	}
}
