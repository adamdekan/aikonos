package toolproxy

import (
	"context"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/adamdekan/aikonos/broker/internal/db"
)

// TestInvoke_LogsChain pins AG-17 S7: every downstream call (web.search and
// mcp:* alike) logs subject, actor and task id, and never the args.
func TestInvoke_LogsChain(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	p := New(Config{}, zap.New(core))
	p.Register("web.search", func(context.Context, Request) (map[string]any, int64, error) { return nil, 0, nil })

	for _, id := range []string{"web.search", "mcp:c1:tool"} {
		logs.TakeAll()
		_, _ = p.Invoke(context.Background(), Request{
			ToolID: id, TaskID: "task-1", Args: map[string]any{"q": "secret-query"},
			OBO: db.OnBehalfOf{Subject: "bob@example.com", Actor: "agent-1", DelegatedBy: "alice@example.com"},
		})
		calls := logs.FilterMessage("toolproxy: call").AllUntimed()
		if len(calls) != 1 {
			t.Fatalf("%s: want 1 call log, got %d", id, len(calls))
		}
		f := calls[0].ContextMap()
		for k, want := range map[string]any{"tool_id": id, "task_id": "task-1", "subject": "bob@example.com", "actor": "agent-1", "delegated_by": "alice@example.com"} {
			if f[k] != want {
				t.Errorf("%s: %s = %v, want %v", id, k, f[k], want)
			}
		}
		if _, ok := f["args"]; ok {
			t.Errorf("%s: args must not be logged", id)
		}
	}
}
