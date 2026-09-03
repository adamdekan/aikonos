// Package skillmanifest loads and validates skills/<name>/skill.yaml manifests.
// A manifest is the typed, versioned declaration of a skill's capability scope,
// effect class, and tool list. The loader is fail-closed: a single invalid or
// malformed manifest aborts the entire load.
package skillmanifest

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/adamdekan/aikonos/broker/internal/effectclass"
	"sigs.k8s.io/yaml"
)

// Tool is one tool entry within a skill manifest's spec.tools list.
type Tool struct {
	Name         string         `json:"name"`
	EffectClass  string         `json:"effect_class"`
	Description  string         `json:"description"`
	InputSchema  map[string]any `json:"input_schema,omitempty"`
	OutputSchema map[string]any `json:"output_schema,omitempty"`
}

// Metadata holds the manifest's identifying fields.
type Metadata struct {
	Name        string `json:"name"`
	Version     string `json:"version,omitempty"`
	Description string `json:"description,omitempty"`
}

// Spec holds the operational contract for the skill.
type Spec struct {
	CapabilityScope string `json:"capability_scope"`
	EffectClass     string `json:"effect_class"`
	Tools           []Tool `json:"tools,omitempty"`
}

// Manifest is the parsed, validated representation of a skill.yaml file.
type Manifest struct {
	APIVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Metadata   Metadata `json:"metadata"`
	Spec       Spec     `json:"spec"`
}

// ToolID returns the skill's identifier (metadata.name).
func (m Manifest) ToolID() string { return m.Metadata.Name }

// Scope returns the capability scope required to invoke this skill
// (spec.capability_scope).
func (m Manifest) Scope() string { return m.Spec.CapabilityScope }

// EffectClass returns the effect-class rego string for this skill
// (spec.effect_class).
func (m Manifest) EffectClass() string { return m.Spec.EffectClass }

// knownEffectClasses is built once from the effectclass registry so the
// validation check here never duplicates the authoritative list.
var knownEffectClasses map[string]struct{}

func init() {
	entries := effectclass.All()
	knownEffectClasses = make(map[string]struct{}, len(entries))
	for _, e := range entries {
		knownEffectClasses[e.Rego] = struct{}{}
	}
}

// Load globs dir/*/skill.yaml, parses and validates each file, and returns all
// manifests sorted by ToolID. An empty or absent dir returns an empty slice
// without error. Any invalid or malformed manifest causes Load to return an
// error immediately (fail-closed).
func Load(dir string) ([]Manifest, error) {
	pattern := filepath.Join(dir, "*", "skill.yaml")
	paths, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("skillmanifest: glob %q: %w", pattern, err)
	}

	manifests := make([]Manifest, 0, len(paths))
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("skillmanifest: read %s: %w", p, err)
		}
		var m Manifest
		if err := yaml.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("skillmanifest: parse %s: %w", p, err)
		}
		if err := validate(m, p); err != nil {
			return nil, err
		}
		manifests = append(manifests, m)
	}

	sort.Slice(manifests, func(i, j int) bool {
		return manifests[i].ToolID() < manifests[j].ToolID()
	})
	return manifests, nil
}

func validate(m Manifest, path string) error {
	if m.APIVersion == "" {
		return fmt.Errorf("skillmanifest: %s: apiVersion must not be empty", path)
	}
	if m.Kind != "Skill" {
		return fmt.Errorf("skillmanifest: %s: kind must be \"Skill\", got %q", path, m.Kind)
	}
	if m.Metadata.Name == "" {
		return fmt.Errorf("skillmanifest: %s: metadata.name must not be empty", path)
	}
	if m.Spec.CapabilityScope == "" {
		return fmt.Errorf("skillmanifest: %s: spec.capability_scope must not be empty", path)
	}
	if _, ok := knownEffectClasses[m.Spec.EffectClass]; !ok {
		return fmt.Errorf("skillmanifest: %s: spec.effect_class %q is unknown", path, m.Spec.EffectClass)
	}
	return nil
}
