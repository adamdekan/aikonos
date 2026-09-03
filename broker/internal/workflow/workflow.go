// Package workflow defines the Workflow document format (apiVersion: aikonos.com/v1,
// kind: Workflow) and provides YAML↔struct↔JSON conversion with field-named
// validation.
//
// Canonical storage form is JSON (for the later JSONB column). YAML is the
// import/export wire form. The same struct drives both via sigs.k8s.io/yaml
// (YAML→JSON via json struct tags), matching the skillmanifest pattern.
package workflow

import (
	"encoding/json"
	"fmt"

	"sigs.k8s.io/yaml"
)

const (
	APIVersion = "aikonos.com/v1"
	Kind       = "Workflow"
)

// MaxSteps bounds a workflow's step count. WHY: the gateway runs steps linearly
// within one fire, so an unbounded count is an unbounded-attempt loop inside a
// single scheduler timeout — the per-call rate-limit and spend-cap gates only
// slow it down, they never end it. 100 is an order-of-magnitude ceiling far
// above any legitimate workflow, not a tuning knob.
const MaxSteps = 100

// Visibility controls who can see a workflow.
type Visibility struct {
	// Kind is either "private" or "shared".
	Kind   string   `json:"kind"`
	Groups []string `json:"groups,omitempty"`
}

// Metadata holds the workflow's identifying and lifecycle fields.
type Metadata struct {
	Name        string     `json:"name"`
	Description string     `json:"description,omitempty"`
	Version     int        `json:"version,omitempty"`
	Owner       string     `json:"owner,omitempty"`
	Visibility  Visibility `json:"visibility"`
}

// Requires declares the access manifest: what scopes, skills, and agents a
// runner must hold before executing this workflow. Used for grey-out comparison.
type Requires struct {
	Scopes []string `json:"scopes,omitempty"`
	Skills []string `json:"skills,omitempty"`
	Agents []string `json:"agents,omitempty"`
}

// Input declares one parameterized input slot.
type Input struct {
	Name    string         `json:"name"`
	Schema  map[string]any `json:"schema,omitempty"`
	Default string         `json:"default,omitempty"`
}

// Step is one authored step in the linear step sequence: either a tool
// invocation (kind "" or "tool") or a bounded parent-side reasoning call
// (kind "reason").
type Step struct {
	Kind         string         `json:"kind,omitempty"`
	Skill        string         `json:"skill,omitempty"`
	Args         map[string]any `json:"args,omitempty"`
	Instruction  string         `json:"instruction,omitempty"`
	OutputSchema map[string]any `json:"output_schema,omitempty"`
}

// Workflow is the parsed, validated representation of a kind: Workflow document.
type Workflow struct {
	APIVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Metadata   Metadata `json:"metadata"`
	Requires   Requires `json:"requires,omitempty"`
	Inputs     []Input  `json:"inputs,omitempty"`
	Steps      []Step   `json:"steps,omitempty"`
}

// ParseYAML decodes a YAML (or JSON) Workflow document, validates it, and
// returns the typed struct. Returns a field-named error on any violation.
func ParseYAML(src []byte) (*Workflow, error) {
	// sigs.k8s.io/yaml converts YAML to JSON first, then uses encoding/json
	// to populate the struct — the same pattern used by skillmanifest.
	var wf Workflow
	if err := yaml.Unmarshal(src, &wf); err != nil {
		return nil, fmt.Errorf("workflow: parse: %w", err)
	}
	if err := validate(&wf); err != nil {
		return nil, err
	}
	return &wf, nil
}

// ToJSON encodes a validated Workflow to its canonical JSON form (the storage
// representation for the later JSONB column).
func ToJSON(wf *Workflow) ([]byte, error) {
	b, err := json.Marshal(wf)
	if err != nil {
		return nil, fmt.Errorf("workflow: encode json: %w", err)
	}
	return b, nil
}

// ToYAML encodes a validated Workflow to YAML (the import/export wire form).
// It converts via JSON first so struct json tags govern the field names,
// matching the sigs.k8s.io/yaml round-trip contract.
func ToYAML(wf *Workflow) ([]byte, error) {
	b, err := yaml.Marshal(wf)
	if err != nil {
		return nil, fmt.Errorf("workflow: encode yaml: %w", err)
	}
	return b, nil
}

// FromJSON decodes a canonical JSON blob back to a Workflow struct and
// re-validates it.
func FromJSON(src []byte) (*Workflow, error) {
	var wf Workflow
	if err := json.Unmarshal(src, &wf); err != nil {
		return nil, fmt.Errorf("workflow: decode json: %w", err)
	}
	if err := validate(&wf); err != nil {
		return nil, err
	}
	return &wf, nil
}

// validate checks required fields and constrained enumerations, returning an
// error that names the offending field.
func validate(wf *Workflow) error {
	if wf.APIVersion != APIVersion {
		return fmt.Errorf("workflow: apiVersion must be %q, got %q", APIVersion, wf.APIVersion)
	}
	if wf.Kind != Kind {
		return fmt.Errorf("workflow: kind must be %q, got %q", Kind, wf.Kind)
	}
	if wf.Metadata.Name == "" {
		return fmt.Errorf("workflow: metadata.name must not be empty")
	}
	switch wf.Metadata.Visibility.Kind {
	case "private", "shared":
		// valid
	case "":
		return fmt.Errorf("workflow: visibility.kind must not be empty")
	default:
		return fmt.Errorf("workflow: visibility.kind must be \"private\" or \"shared\", got %q", wf.Metadata.Visibility.Kind)
	}
	if len(wf.Steps) > MaxSteps {
		return fmt.Errorf("workflow: steps must not exceed %d, got %d", MaxSteps, len(wf.Steps))
	}
	for i, s := range wf.Steps {
		switch s.Kind {
		case "", "tool":
			if s.Skill == "" {
				return fmt.Errorf("workflow: steps[%d].skill must not be empty", i)
			}
			if s.Instruction != "" {
				return fmt.Errorf("workflow: steps[%d].instruction must be absent for a tool step", i)
			}
			if s.OutputSchema != nil {
				return fmt.Errorf("workflow: steps[%d].output_schema must be absent for a tool step", i)
			}
		case "reason":
			if s.Instruction == "" {
				return fmt.Errorf("workflow: steps[%d].instruction must not be empty for a reason step", i)
			}
			if s.Skill != "" {
				return fmt.Errorf("workflow: steps[%d].skill must be absent for a reason step", i)
			}
			if s.Args != nil {
				return fmt.Errorf("workflow: steps[%d].args must be absent for a reason step", i)
			}
		default:
			return fmt.Errorf("workflow: steps[%d].kind must be \"tool\" or \"reason\", got %q", i, s.Kind)
		}
	}
	return nil
}
