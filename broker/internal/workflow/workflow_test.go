package workflow_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/adamdekan/aikonos/broker/internal/workflow"
)

const validYAML = `
apiVersion: aikonos.com/v1
kind: Workflow
metadata:
  name: weekly-support-triage
  description: Pull last week's tickets, tag by severity, draft a team summary.
  version: 3
  owner: alice
  visibility:
    kind: shared
    groups:
      - support-leads
requires:
  scopes:
    - support:read
    - doc:write
  skills:
    - support.list_tickets
    - doc.write
  agents: []
inputs:
  - name: since
    schema:
      type: string
      format: date
    default: "-7d"
steps:
  - skill: support.list_tickets
    args:
      since: "${inputs.since}"
  - skill: doc.write
    args:
      path: "summaries/triage-${inputs.since}.md"
      body: "${steps[0].summary}"
`

// TestRoundTrip verifies the full lossless loop:
// YAML → struct → JSON → struct → YAML → struct, with semantic equality at
// every JSON boundary and explicit map-field assertions to catch int→float64
// drift that encoding/json can introduce on map[string]any round-trips.
func TestRoundTrip(t *testing.T) {
	wf, err := workflow.ParseYAML([]byte(validYAML))
	if err != nil {
		t.Fatalf("ParseYAML: %v", err)
	}

	// struct → canonical JSON
	canonical, err := workflow.ToJSON(wf)
	if err != nil {
		t.Fatalf("ToJSON: %v", err)
	}

	// JSON → struct
	wf2, err := workflow.FromJSON(canonical)
	if err != nil {
		t.Fatalf("FromJSON: %v", err)
	}

	// struct → YAML → struct (completes the SC#1 lossless YAML round-trip)
	yamlBytes, err := workflow.ToYAML(wf2)
	if err != nil {
		t.Fatalf("ToYAML: %v", err)
	}
	wf3, err := workflow.ParseYAML(yamlBytes)
	if err != nil {
		t.Fatalf("ParseYAML after ToYAML: %v", err)
	}

	// semantic equality: re-encode all three as JSON and compare
	canonical2, err := workflow.ToJSON(wf2)
	if err != nil {
		t.Fatalf("ToJSON wf2: %v", err)
	}
	canonical3, err := workflow.ToJSON(wf3)
	if err != nil {
		t.Fatalf("ToJSON wf3: %v", err)
	}

	cmpJSON := func(a, b []byte, label string) {
		t.Helper()
		var ma, mb map[string]any
		if err := json.Unmarshal(a, &ma); err != nil {
			t.Fatalf("%s: unmarshal a: %v", label, err)
		}
		if err := json.Unmarshal(b, &mb); err != nil {
			t.Fatalf("%s: unmarshal b: %v", label, err)
		}
		ba, _ := json.Marshal(ma)
		bb, _ := json.Marshal(mb)
		if string(ba) != string(bb) {
			t.Fatalf("%s not lossless:\nfirst:  %s\nsecond: %s", label, ba, bb)
		}
	}
	cmpJSON(canonical, canonical2, "JSON→JSON")
	cmpJSON(canonical, canonical3, "JSON→YAML→JSON")

	// spot-check scalar fields on the final struct
	if wf3.Metadata.Name != "weekly-support-triage" {
		t.Errorf("metadata.name: got %q", wf3.Metadata.Name)
	}
	if wf3.Metadata.Version != 3 {
		t.Errorf("metadata.version: got %d", wf3.Metadata.Version)
	}
	if wf3.Metadata.Visibility.Kind != "shared" {
		t.Errorf("visibility.kind: got %q", wf3.Metadata.Visibility.Kind)
	}
	if len(wf3.Metadata.Visibility.Groups) != 1 || wf3.Metadata.Visibility.Groups[0] != "support-leads" {
		t.Errorf("visibility.groups: got %v", wf3.Metadata.Visibility.Groups)
	}
	if len(wf3.Steps) != 2 {
		t.Errorf("steps len: got %d", len(wf3.Steps))
	}

	// assert map fields survive with correct types — catches int→float64 drift
	// that encoding/json introduces when decoding into map[string]any
	if wf3.Steps[0].Args == nil {
		t.Fatalf("steps[0].args is nil after round-trip")
	}
	sinceVal, ok := wf3.Steps[0].Args["since"]
	if !ok {
		t.Fatalf("steps[0].args missing 'since' key")
	}
	if sinceVal != "${inputs.since}" {
		t.Errorf("steps[0].args[since]: got %q (%T)", sinceVal, sinceVal)
	}

	if wf3.Inputs[0].Schema == nil {
		t.Fatalf("inputs[0].schema is nil after round-trip")
	}
	schemaType, ok := wf3.Inputs[0].Schema["type"]
	if !ok {
		t.Fatalf("inputs[0].schema missing 'type' key")
	}
	if schemaType != "string" {
		t.Errorf("inputs[0].schema.type: got %q (%T)", schemaType, schemaType)
	}
}

// TestRejectMissingName verifies that a document with no metadata.name is
// rejected with an error that names the field.
func TestRejectMissingName(t *testing.T) {
	bad := strings.ReplaceAll(validYAML, "  name: weekly-support-triage\n", "")
	_, err := workflow.ParseYAML([]byte(bad))
	if err == nil {
		t.Fatal("expected error for missing metadata.name, got nil")
	}
	if !strings.Contains(err.Error(), "metadata.name") {
		t.Errorf("error should mention 'metadata.name', got: %v", err)
	}
}

// TestRejectInvalidVisibilityKind verifies that visibility.kind outside
// {private, shared} is rejected with a field-named error.
func TestRejectInvalidVisibilityKind(t *testing.T) {
	bad := strings.ReplaceAll(validYAML, "    kind: shared", "    kind: public")
	_, err := workflow.ParseYAML([]byte(bad))
	if err == nil {
		t.Fatal("expected error for invalid visibility.kind, got nil")
	}
	if !strings.Contains(err.Error(), "visibility.kind") {
		t.Errorf("error should mention 'visibility.kind', got: %v", err)
	}
}

// TestRejectWrongKind verifies that a document with kind != Workflow is rejected.
func TestRejectWrongKind(t *testing.T) {
	bad := strings.ReplaceAll(validYAML, "kind: Workflow", "kind: Skill")
	_, err := workflow.ParseYAML([]byte(bad))
	if err == nil {
		t.Fatal("expected error for wrong kind, got nil")
	}
}

// TestRejectEmptyStepSkill verifies that a step with an empty skill field is
// rejected with an error naming the field (steps[i].skill), so the field-named
// error contract holds end-to-end for CP4 step execution.
func TestRejectEmptyStepSkill(t *testing.T) {
	// replace the first skill value with an empty string
	bad := strings.ReplaceAll(validYAML, "  - skill: support.list_tickets", "  - skill: ")
	_, err := workflow.ParseYAML([]byte(bad))
	if err == nil {
		t.Fatal("expected error for empty steps[0].skill, got nil")
	}
	if !strings.Contains(err.Error(), "steps[0].skill") {
		t.Errorf("error should mention 'steps[0].skill', got: %v", err)
	}
}

// TestPrivateVisibility verifies that private visibility with no groups is valid.
func TestPrivateVisibility(t *testing.T) {
	private := strings.ReplaceAll(validYAML,
		"  visibility:\n    kind: shared\n    groups:\n      - support-leads\n",
		"  visibility:\n    kind: private\n")
	wf, err := workflow.ParseYAML([]byte(private))
	if err != nil {
		t.Fatalf("ParseYAML private: %v", err)
	}
	if wf.Metadata.Visibility.Kind != "private" {
		t.Errorf("expected private, got %q", wf.Metadata.Visibility.Kind)
	}
}

// TestStepKindValidation table-drives the reason-step schema rules (CP-R1):
// kind enum, per-kind required/forbidden fields, and back-compat for
// kind-absent steps.
func TestStepKindValidation(t *testing.T) {
	base := func(steps []workflow.Step) *workflow.Workflow {
		return &workflow.Workflow{
			APIVersion: workflow.APIVersion,
			Kind:       workflow.Kind,
			Metadata: workflow.Metadata{
				Name:       "t",
				Visibility: workflow.Visibility{Kind: "private"},
			},
			Steps: steps,
		}
	}
	roundTrip := func(wf *workflow.Workflow) (*workflow.Workflow, error) {
		b, err := workflow.ToJSON(wf)
		if err != nil {
			t.Fatalf("ToJSON: %v", err)
		}
		return workflow.FromJSON(b)
	}

	cases := []struct {
		name       string
		steps      []workflow.Step
		wantErr    bool
		errContain string
	}{
		{
			name:  "kind absent is back-compat tool",
			steps: []workflow.Step{{Skill: "doc.read"}},
		},
		{
			name:  "explicit kind tool",
			steps: []workflow.Step{{Kind: "tool", Skill: "doc.read", Args: map[string]any{"path": "x"}}},
		},
		{
			name:  "valid reason step",
			steps: []workflow.Step{{Kind: "reason", Instruction: "summarize ${steps.0.output}"}},
		},
		{
			name: "valid reason step with output_schema",
			steps: []workflow.Step{{
				Kind:        "reason",
				Instruction: "extract fields",
				OutputSchema: map[string]any{
					"type":       "object",
					"properties": map[string]any{"email": map[string]any{"type": "string"}},
				},
			}},
		},
		{
			name:       "unknown kind rejected",
			steps:      []workflow.Step{{Kind: "loop", Skill: "doc.read"}},
			wantErr:    true,
			errContain: "steps[0].kind",
		},
		{
			name:       "tool step with instruction rejected",
			steps:      []workflow.Step{{Kind: "tool", Skill: "doc.read", Instruction: "do it"}},
			wantErr:    true,
			errContain: "steps[0].instruction",
		},
		{
			name:       "reason step missing instruction rejected",
			steps:      []workflow.Step{{Kind: "reason"}},
			wantErr:    true,
			errContain: "steps[0].instruction",
		},
		{
			name:       "reason step with skill rejected",
			steps:      []workflow.Step{{Kind: "reason", Instruction: "x", Skill: "doc.read"}},
			wantErr:    true,
			errContain: "steps[0].skill",
		},
		{
			name:       "reason step with args rejected",
			steps:      []workflow.Step{{Kind: "reason", Instruction: "x", Args: map[string]any{"a": 1}}},
			wantErr:    true,
			errContain: "steps[0].args",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wf := base(c.steps)
			b, err := json.Marshal(wf)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			_, err = workflow.FromJSON(b)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if !strings.Contains(err.Error(), c.errContain) {
					t.Errorf("error should mention %q, got: %v", c.errContain, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("FromJSON: %v", err)
			}
			if _, err := roundTrip(wf); err != nil {
				t.Fatalf("round-trip FromJSON(ToJSON): %v", err)
			}
		})
	}
}

const mixedStepsYAML = `
apiVersion: aikonos.com/v1
kind: Workflow
metadata:
  name: ip-alert
  visibility:
    kind: private
steps:
  - skill: doc.read
    args:
      path: registry.csv
  - kind: reason
    instruction: >
      Find the row whose CIDR contains ${inputs.ip_address}: ${steps.0.output}
    output_schema:
      type: object
      properties:
        email: { type: string }
      required: [email]
  - kind: tool
    skill: doc.write
    args:
      path: alert.txt
      content: "Dear ${steps.1.output.name}"
`

// TestMixedStepsYAMLRoundTrip verifies a definition mixing tool and reason
// steps round-trips through ParseYAML/FromJSON/ToYAML unchanged.
func TestMixedStepsYAMLRoundTrip(t *testing.T) {
	wf, err := workflow.ParseYAML([]byte(mixedStepsYAML))
	if err != nil {
		t.Fatalf("ParseYAML: %v", err)
	}
	if len(wf.Steps) != 3 {
		t.Fatalf("steps len: got %d", len(wf.Steps))
	}
	if wf.Steps[0].Kind != "" || wf.Steps[0].Skill != "doc.read" {
		t.Errorf("steps[0]: got %+v", wf.Steps[0])
	}
	if wf.Steps[1].Kind != "reason" || wf.Steps[1].Instruction == "" {
		t.Errorf("steps[1]: got %+v", wf.Steps[1])
	}
	if wf.Steps[1].OutputSchema == nil || wf.Steps[1].OutputSchema["type"] != "object" {
		t.Errorf("steps[1].output_schema: got %+v", wf.Steps[1].OutputSchema)
	}
	if wf.Steps[2].Kind != "tool" || wf.Steps[2].Skill != "doc.write" {
		t.Errorf("steps[2]: got %+v", wf.Steps[2])
	}

	yamlBytes, err := workflow.ToYAML(wf)
	if err != nil {
		t.Fatalf("ToYAML: %v", err)
	}
	wf2, err := workflow.ParseYAML(yamlBytes)
	if err != nil {
		t.Fatalf("ParseYAML after ToYAML: %v", err)
	}

	b1, err := workflow.ToJSON(wf)
	if err != nil {
		t.Fatalf("ToJSON wf: %v", err)
	}
	b2, err := workflow.ToJSON(wf2)
	if err != nil {
		t.Fatalf("ToJSON wf2: %v", err)
	}
	var m1, m2 map[string]any
	if err := json.Unmarshal(b1, &m1); err != nil {
		t.Fatalf("unmarshal b1: %v", err)
	}
	if err := json.Unmarshal(b2, &m2); err != nil {
		t.Fatalf("unmarshal b2: %v", err)
	}
	r1, _ := json.Marshal(m1)
	r2, _ := json.Marshal(m2)
	if string(r1) != string(r2) {
		t.Fatalf("mixed-steps round-trip not lossless:\nfirst:  %s\nsecond: %s", r1, r2)
	}
}

// TestMaxStepsCeiling verifies the step-count ceiling holds at both write-path
// entry points (FromJSON backs Save/Propose, ParseYAML backs YAML import), and
// that the error names the limit so an author knows what they hit. WHY the
// ceiling exists: the gateway runs steps linearly in one fire, so an unbounded
// count is an unbounded-attempt loop the per-call rate/spend gates only slow.
func TestMaxStepsCeiling(t *testing.T) {
	build := func(n int) *workflow.Workflow {
		steps := make([]workflow.Step, n)
		for i := range steps {
			steps[i] = workflow.Step{Kind: "reason", Instruction: "think"}
		}
		return &workflow.Workflow{
			APIVersion: workflow.APIVersion,
			Kind:       workflow.Kind,
			Metadata: workflow.Metadata{
				Name:       "t",
				Visibility: workflow.Visibility{Kind: "private"},
			},
			Steps: steps,
		}
	}

	for _, tc := range []struct {
		name    string
		steps   int
		wantErr bool
	}{
		{"at the limit", workflow.MaxSteps, false},
		{"one over the limit", workflow.MaxSteps + 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			jsonBytes, err := workflow.ToJSON(build(tc.steps))
			if err != nil {
				t.Fatalf("ToJSON: %v", err)
			}
			yamlBytes, err := workflow.ToYAML(build(tc.steps))
			if err != nil {
				t.Fatalf("ToYAML: %v", err)
			}

			// Both write paths must agree — Save/Propose enter via FromJSON,
			// YAML import via ParseYAML.
			for path, gotErr := range map[string]error{
				"FromJSON":  second(workflow.FromJSON(jsonBytes)),
				"ParseYAML": second(workflow.ParseYAML(yamlBytes)),
			} {
				if !tc.wantErr {
					if gotErr != nil {
						t.Errorf("%s: %d steps must be accepted, got: %v", path, tc.steps, gotErr)
					}
					continue
				}
				if gotErr == nil {
					t.Fatalf("%s: expected error for %d steps, got nil", path, tc.steps)
				}
				if !strings.Contains(gotErr.Error(), "100") {
					t.Errorf("%s: error must name the limit (100), got: %v", path, gotErr)
				}
			}
		})
	}
}

// second discards a two-value result's first element, so the table above can
// compare FromJSON and ParseYAML side by side in one map literal.
func second(_ *workflow.Workflow, err error) error { return err }
