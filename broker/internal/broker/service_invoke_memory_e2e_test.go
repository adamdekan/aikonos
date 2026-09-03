// broker/internal/broker/service_invoke_memory_e2e_test.go
//
// The agent-memory end-to-end criterion: memory.write then memory.read through InvokeTool dispatching into
// the REAL toolproxy memory plugins over a real tempdir workspacefs.Store —
// not the stand-in handler the CP3 wiring tests in service_invoke_test.go
// register. WHY it is worth its own test: the wiring tests would still pass if
// the plugins stopped being registered, if their scopes drifted from the
// capability token's, or if the bundle never reached disk.
package broker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"

	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"

	"github.com/adamdekan/aikonos/broker/internal/capability"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/toolproxy"
	"github.com/adamdekan/aikonos/broker/internal/workspacefs"
)

func TestInvokeTool_MemoryRoundTripThroughRealPlugin(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	m, err := capability.GenerateMinter()
	if err != nil {
		t.Fatalf("GenerateMinter: %v", err)
	}
	svc := &SandboxService{deps: Deps{
		Logger:     zap.NewNop(),
		Capability: m,
		ToolProxy:  toolproxy.New(toolproxy.Config{WorkspaceFS: workspacefs.New(root)}, zap.NewNop()),
		Tasks:      &fakeInvokeToolTaskStore{task: &db.Task{CostBudget: 1000}},
		Audit:      &fakeInvokeAudit{},
	}}

	res, err := svc.InvokeTool(ctx, &brokerv1.InvokeToolRequest{
		TaskId: "task-1", TenantId: "tenant-x", UserId: "user-1",
		ToolId: "memory.write", CapabilityToken: mintForScope(t, m, "memory:write"),
		Args: memoryArgs(t, map[string]any{
			"scope": "user", "id": "facts/sla", "type": "Fact",
			"title": "Orders freshness SLA", "description": "Orders must be fresh within 30 minutes.",
			"body": "The orders feed must be fresh within 30 minutes.",
		}),
	})
	if err != nil {
		t.Fatalf("memory.write: %v", err)
	}
	if !res.Success {
		t.Fatalf("memory.write failed: %q", res.Error)
	}
	if got := res.Result.AsMap()["path"]; got != ".agent/Memory/facts/sla.md" {
		t.Errorf("write reported path %v", got)
	}

	// The concept, the regenerated index and the log entry all landed on disk
	// under the caller's own segment.
	bundle := filepath.Join(root, "tenant-x", "user-1", ".agent", "Memory")
	for _, rel := range []string{"facts/sla.md", "index.md", "log.md"} {
		if _, err := os.Stat(filepath.Join(bundle, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("%s missing from the bundle: %v", rel, err)
		}
	}

	res, err = svc.InvokeTool(ctx, &brokerv1.InvokeToolRequest{
		TaskId: "task-1", TenantId: "tenant-x", UserId: "user-1",
		ToolId: "memory.read", CapabilityToken: mintForScope(t, m, "memory:read"),
		Args: memoryArgs(t, map[string]any{"scope": "user", "mode": "index"}),
	})
	if err != nil {
		t.Fatalf("memory.read: %v", err)
	}
	if !res.Success {
		t.Fatalf("memory.read failed: %q", res.Error)
	}
	out := res.Result.AsMap()
	index, _ := out["index"].(string)
	if !strings.Contains(index, "## Fact") || !strings.Contains(index, "`facts/sla`") ||
		!strings.Contains(index, "Orders freshness SLA") {
		t.Fatalf("index does not describe the written concept:\n%s", index)
	}
	if n, _ := out["concept_count"].(float64); n != 1 {
		t.Fatalf("concept_count = %v, want 1", out["concept_count"])
	}
}
