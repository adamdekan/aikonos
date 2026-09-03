package workflowsvc

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/toolregistry"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// oneToolStepWorkflowJSON builds a schema-valid Workflow whose single tool step
// references skill. Used to exercise the broker-side invented-tool guard.
func oneToolStepWorkflowJSON(skill string) string {
	m := map[string]any{
		"apiVersion": "aikonos.com/v1",
		"kind":       "Workflow",
		"metadata": map[string]any{
			"name":       "skill-guard-wf",
			"visibility": map[string]any{"kind": "private"},
		},
		"steps": []any{map[string]any{"skill": skill}},
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// reasonStepWorkflowJSON builds a schema-valid Workflow with a single reason
// step (no skill), which the guard must skip.
func reasonStepWorkflowJSON() string {
	m := map[string]any{
		"apiVersion": "aikonos.com/v1",
		"kind":       "Workflow",
		"metadata": map[string]any{
			"name":       "reason-wf",
			"visibility": map[string]any{"kind": "private"},
		},
		"steps": []any{map[string]any{"kind": "reason", "instruction": "summarize"}},
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// saveWith runs Save on a brand-new lineage with the given definition + registry.
func saveWith(t *testing.T, reg *toolregistry.Registry, defJSON string) error {
	t.Helper()
	req := &brokerv1.SaveWorkflowRequest{
		TenantId:       testWFTenant,
		DefinitionJson: defJSON,
		Name:           "skill-guard",
	}
	_, err := Save(context.Background(), testWFTenant, testWFOwner, req,
		&fakeWorkflowStore{}, reg, newEmitter(t), zap.NewNop())
	return err
}

// currentApprovedRow is a minimal approved lineage row owned by testWFOwner,
// enough for Propose's owner-verify + metadata copy.
func currentApprovedRow() *db.WorkflowRow {
	return &db.WorkflowRow{
		ID:               uuid.New(),
		LineageID:        uuid.New(),
		Version:          1,
		TenantID:         uuid.MustParse(testWFTenant),
		OwnerUserID:      testWFOwner,
		Name:             "skill-guard",
		Status:           "private",
		VisibilityKind:   "private",
		VisibilityGroups: json.RawMessage(`[]`),
		Definition:       json.RawMessage(minimalValidWorkflowJSON()),
		ApprovalState:    "approved",
	}
}

// TestSave_RegisteredSkill_Passes: a tool step naming a registered skill is
// accepted. siem.query is in the static baseline (toolregistry.NewRegistry).
func TestSave_RegisteredSkill_Passes(t *testing.T) {
	if err := saveWith(t, toolregistry.NewRegistry(), oneToolStepWorkflowJSON("siem.query")); err != nil {
		t.Fatalf("registered skill must pass: %v", err)
	}
}

// TestSave_UnknownSkill_InvalidArgument: a tool step naming a skill absent from
// the registry (and not mcp:-prefixed) is rejected with InvalidArgument.
func TestSave_UnknownSkill_InvalidArgument(t *testing.T) {
	err := saveWith(t, toolregistry.NewRegistry(), oneToolStepWorkflowJSON("nonexistent.tool"))
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("unknown skill: want InvalidArgument, got %v", err)
	}
}

// TestSave_McpSkill_Passes: an mcp:-prefixed skill passes the broker guard —
// MCP ids are dynamic and covered by the gateway-side check instead.
func TestSave_McpSkill_Passes(t *testing.T) {
	if err := saveWith(t, toolregistry.NewRegistry(), oneToolStepWorkflowJSON("mcp:foo:bar")); err != nil {
		t.Fatalf("mcp: skill must pass: %v", err)
	}
}

// TestSave_ReasonStep_Passes: a reason step carries no skill and is skipped by
// the guard even with a registry injected.
func TestSave_ReasonStep_Passes(t *testing.T) {
	if err := saveWith(t, toolregistry.NewRegistry(), reasonStepWorkflowJSON()); err != nil {
		t.Fatalf("reason step must pass: %v", err)
	}
}

// TestSave_NilRegistry_SkipsGuard: a nil registry fails open — the guard is
// skipped so an unknown skill still persists (the gateway guard remains).
func TestSave_NilRegistry_SkipsGuard(t *testing.T) {
	if err := saveWith(t, nil, oneToolStepWorkflowJSON("nonexistent.tool")); err != nil {
		t.Fatalf("nil registry must skip the guard: %v", err)
	}
}

// TestPropose_UnknownSkill_InvalidArgument: the same guard runs on the Propose
// entrance. Uses fakeProposeStore so the owner-verify + metadata copy succeed
// before the guard would (but the guard runs first, before any store call).
func TestPropose_UnknownSkill_InvalidArgument(t *testing.T) {
	store := &fakeProposeStore{current: currentApprovedRow()}
	_, err := Propose(context.Background(), testWFTenant, testWFOwner,
		&brokerv1.ProposeWorkflowVersionRequest{
			TenantId:       testWFTenant,
			LineageId:      store.current.LineageID.String(),
			DefinitionJson: oneToolStepWorkflowJSON("nonexistent.tool"),
		},
		store, store, nil, toolregistry.NewRegistry(), newEmitter(t), zap.NewNop())
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("Propose unknown skill: want InvalidArgument, got %v", err)
	}
}
