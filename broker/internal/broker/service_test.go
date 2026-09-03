package broker

import (
	"context"
	"encoding/base64"
	"testing"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/adamdekan/aikonos/broker/internal/capability"
	"github.com/adamdekan/aikonos/broker/internal/toolproxy"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// newSandboxSvc builds a SandboxService with only the deps the InvokeTool
// capability gate touches (no DB).
func newSandboxSvc(t *testing.T) (*SandboxService, *capability.Minter) {
	t.Helper()
	m, err := capability.GenerateMinter()
	if err != nil {
		t.Fatalf("GenerateMinter: %v", err)
	}
	return NewSandboxService(Deps{Logger: zap.NewNop(), Capability: m}), m
}

func mintFor(t *testing.T, m *capability.Minter, scopes ...string) string {
	t.Helper()
	raw, err := m.Mint("agent@example.com", "task-1", scopes)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func TestInvokeTool_RequiresCapabilityToken(t *testing.T) {
	svc, _ := newSandboxSvc(t)
	_, err := svc.InvokeTool(context.Background(), &brokerv1.InvokeToolRequest{
		TaskId: "task-1", ToolId: "siem.query",
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("missing token: want PermissionDenied, got %v", err)
	}
}

func TestInvokeTool_MalformedToken(t *testing.T) {
	svc, _ := newSandboxSvc(t)
	_, err := svc.InvokeTool(context.Background(), &brokerv1.InvokeToolRequest{
		TaskId: "task-1", ToolId: "siem.query", CapabilityToken: "!!!not-base64!!!",
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("malformed token: want InvalidArgument, got %v", err)
	}
}

func TestInvokeTool_GrantedScopeButNoProxyConfigured(t *testing.T) {
	svc, m := newSandboxSvc(t) // Deps has no ToolProxy
	// toolregistry maps siem.query → siem:read, so the token must grant that scope.
	tok := mintFor(t, m, "siem:read", "slack:write")

	resp, err := svc.InvokeTool(context.Background(), &brokerv1.InvokeToolRequest{
		TaskId: "task-1", ToolId: "siem.query", CapabilityToken: tok,
	})
	if err != nil {
		t.Fatalf("granted scope should pass the gate: %v", err)
	}
	if resp.Success || resp.Error != "tool proxy not configured" {
		t.Errorf("with no proxy wired, expected a clean not-configured result, got %+v", resp)
	}
}

func TestInvokeTool_RoutesToProxyOnSuccess(t *testing.T) {
	m, err := capability.GenerateMinter()
	if err != nil {
		t.Fatalf("GenerateMinter: %v", err)
	}
	// Real proxy with a stub handler for web.fetch (registry scope web:read).
	proxy := toolproxy.New(toolproxy.Config{}, zap.NewNop())
	proxy.Register("web.fetch", func(_ context.Context, req toolproxy.Request) (map[string]any, int64, error) {
		return map[string]any{"status_code": 200, "url": req.Args["url"]}, 7, nil
	})
	svc := NewSandboxService(Deps{Logger: zap.NewNop(), Capability: m, ToolProxy: proxy})

	tok := mintFor(t, m, "web:read")
	args, _ := structpb.NewStruct(map[string]any{"url": "https://example.com"})
	resp, err := svc.InvokeTool(context.Background(), &brokerv1.InvokeToolRequest{
		TaskId: "task-1", ToolId: "web.fetch", CapabilityToken: tok, Args: args,
	})
	if err != nil {
		t.Fatalf("InvokeTool: %v", err)
	}
	if !resp.Success {
		t.Fatalf("expected success, got error %q", resp.Error)
	}
	if resp.CostUnitsConsumed != 7 {
		t.Errorf("cost not propagated: %d", resp.CostUnitsConsumed)
	}
	if resp.Result == nil || resp.Result.Fields["status_code"].GetNumberValue() != 200 {
		t.Errorf("tool result not propagated: %+v", resp.Result)
	}
}

func TestInvokeTool_UngrantedScopeDenied(t *testing.T) {
	svc, m := newSandboxSvc(t)
	tok := mintFor(t, m, "doc:write") // does not grant siem:read (what siem.query needs)

	_, err := svc.InvokeTool(context.Background(), &brokerv1.InvokeToolRequest{
		TaskId: "task-1", ToolId: "siem.query", CapabilityToken: tok,
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("ungranted scope: want PermissionDenied, got %v", err)
	}
}

func TestInvokeTool_UnknownToolDenied(t *testing.T) {
	svc, m := newSandboxSvc(t)
	tok := mintFor(t, m, "siem:read")

	_, err := svc.InvokeTool(context.Background(), &brokerv1.InvokeToolRequest{
		TaskId: "task-1", ToolId: "totally.unregistered", CapabilityToken: tok,
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("unknown tool: want PermissionDenied, got %v", err)
	}
}

func TestInvokeTool_ForeignTokenDenied(t *testing.T) {
	svc, _ := newSandboxSvc(t)
	// A token signed by a different root must not verify.
	other, _ := capability.GenerateMinter()
	tok := mintFor(t, other, "siem.query")

	_, err := svc.InvokeTool(context.Background(), &brokerv1.InvokeToolRequest{
		TaskId: "task-1", ToolId: "siem.query", CapabilityToken: tok,
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("foreign-root token: want PermissionDenied, got %v", err)
	}
}
