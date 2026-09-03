package workflowsvc

import (
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/toolregistry"
	"github.com/adamdekan/aikonos/broker/internal/workflow"
)

// validateStepSkills rejects a workflow whose tool steps reference a skill the
// broker cannot resolve. It is the broker-side chokepoint half of the
// invented-tool guard the gateway's invalidSkillError already enforces for
// gateway-authored workflows — without it a direct-RPC caller could persist a
// workflow referencing a nonexistent skill.
//
// A tool step's skill (kind "" or "tool") is accepted when it is registered in
// reg or carries the "mcp:" prefix — MCP tool ids are dynamic, so the broker
// cannot enumerate them at save time and the gateway-side check remains their
// sole guard. Reason steps carry no skill and are skipped.
//
// ponytail: fail-open when reg is nil — the gateway guard still covers this
// path, so skipping (rather than defaulting to toolregistry.PkgDefault) is
// acceptable; nil reg only occurs in FGA-less dev wiring and tests.
func validateStepSkills(def *workflow.Workflow, reg *toolregistry.Registry) error {
	if reg == nil {
		return nil
	}
	var unknown []string
	for _, step := range def.Steps {
		if step.Kind == "reason" {
			continue
		}
		if strings.HasPrefix(step.Skill, "mcp:") {
			continue
		}
		if _, ok := reg.RequiredScope(step.Skill); !ok {
			unknown = append(unknown, step.Skill)
		}
	}
	if len(unknown) > 0 {
		return status.Errorf(codes.InvalidArgument,
			"workflow references unknown skill(s): %s", strings.Join(unknown, ", "))
	}
	return nil
}
