package workflowsvc

// Core-level direct-call tests relocated from
// broker/internal/broker/service_workflows_cp7_test.go (workflowsvc-extraction
// CP2 — see ). The rest of that file
// (PublishWorkflow south/north wrapper tests) stayed in broker — it drives
// svc.PublishWorkflow, a wrapper-level concern.

import (
	"testing"

	"github.com/adamdekan/aikonos/broker/internal/toolproxy"
	"github.com/adamdekan/aikonos/broker/internal/toolregistry"
	"github.com/adamdekan/aikonos/broker/internal/workflow"
)

// newTestToolRegistry returns a Registry seeded the way main.go seeds toolReg
// in production: NewRegistry() +
// LoadFromPlugins(toolproxy.RegisteredPluginBaselines()) — doc.read/web.fetch
// below are plugin-backed ids that no longer resolve from a bare NewRegistry().
func newTestToolRegistry() *toolregistry.Registry {
	reg := toolregistry.NewRegistry()
	reg.LoadFromPlugins(toolproxy.RegisteredPluginBaselines())
	return reg
}

func TestComputeRequires_DistinctSkillsAndScopes(t *testing.T) {
	reg := newTestToolRegistry()
	wf := &workflow.Workflow{
		APIVersion: "aikonos.com/v1",
		Kind:       "Workflow",
		Metadata: workflow.Metadata{
			Name:       "test",
			Visibility: workflow.Visibility{Kind: "private"},
		},
		Steps: []workflow.Step{
			{Skill: "doc.read"},
			{Skill: "web.fetch"},
			{Skill: "doc.read"}, // duplicate — should appear once
		},
	}

	req := ComputeRequires(wf, reg)

	// Skills: doc.read, web.fetch (deduplicated; order of first appearance)
	if len(req.Skills) != 2 {
		t.Fatalf("Skills: want 2 distinct, got %d: %v", len(req.Skills), req.Skills)
	}
	skillSet := map[string]bool{}
	for _, s := range req.Skills {
		skillSet[s] = true
	}
	if !skillSet["doc.read"] || !skillSet["web.fetch"] {
		t.Errorf("Skills: want doc.read and web.fetch, got %v", req.Skills)
	}

	// Scopes: doc:read and web:read (from static baseline)
	if len(req.Scopes) != 2 {
		t.Fatalf("Scopes: want 2 distinct, got %d: %v", len(req.Scopes), req.Scopes)
	}
	scopeSet := map[string]bool{}
	for _, s := range req.Scopes {
		scopeSet[s] = true
	}
	if !scopeSet["doc:read"] || !scopeSet["web:read"] {
		t.Errorf("Scopes: want doc:read and web:read, got %v", req.Scopes)
	}

	// Agents: always empty for v1
	if len(req.Agents) != 0 {
		t.Errorf("Agents: want empty, got %v", req.Agents)
	}
}

// TestComputeRequires_SkipsReasonSteps verifies that a mixed tool+reason
// definition's Requires lists only tool-step skills/scopes (CP-R1).
func TestComputeRequires_SkipsReasonSteps(t *testing.T) {
	reg := newTestToolRegistry()
	wf := &workflow.Workflow{
		APIVersion: "aikonos.com/v1",
		Kind:       "Workflow",
		Metadata: workflow.Metadata{
			Name:       "mixed",
			Visibility: workflow.Visibility{Kind: "private"},
		},
		Steps: []workflow.Step{
			{Skill: "doc.read"},
			{Kind: "reason", Instruction: "summarize ${steps.0.output}"},
			{Kind: "tool", Skill: "web.fetch"},
		},
	}

	req := ComputeRequires(wf, reg)

	if len(req.Skills) != 2 {
		t.Fatalf("Skills: want 2 tool-step skills, got %d: %v", len(req.Skills), req.Skills)
	}
	skillSet := map[string]bool{}
	for _, s := range req.Skills {
		skillSet[s] = true
	}
	if !skillSet["doc.read"] || !skillSet["web.fetch"] {
		t.Errorf("Skills: want doc.read and web.fetch, got %v", req.Skills)
	}

	scopeSet := map[string]bool{}
	for _, s := range req.Scopes {
		scopeSet[s] = true
	}
	if !scopeSet["doc:read"] || !scopeSet["web:read"] {
		t.Errorf("Scopes: want doc:read and web:read, got %v", req.Scopes)
	}
}

func TestComputeRequires_EmptySteps(t *testing.T) {
	reg := toolregistry.NewRegistry()
	wf := &workflow.Workflow{
		APIVersion: "aikonos.com/v1",
		Kind:       "Workflow",
		Metadata: workflow.Metadata{
			Name:       "empty",
			Visibility: workflow.Visibility{Kind: "private"},
		},
		Steps: nil,
	}
	req := ComputeRequires(wf, reg)
	if len(req.Skills) != 0 || len(req.Scopes) != 0 {
		t.Errorf("empty steps: want no skills/scopes, got %v / %v", req.Skills, req.Scopes)
	}
}
