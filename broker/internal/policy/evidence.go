// broker/internal/policy/evidence.go
// Evidence: what an OPA decision was computed from, recorded with the decision
// so it can be re-run later against the same policy revision.
package policy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// Evidence describes one OPA evaluation. Together with the archived policy
// snapshot for Revision it is enough to re-run the decision with the stock
// opa binary (scripts/replay-decision.sh).
type Evidence struct {
	Query       string         `json:"query"`                 // OPA data path, e.g. "aikonos/tool_invocation"
	Revision    string         `json:"revision,omitempty"`    // policy bundle revision OPA evaluated with
	OPAVersion  string         `json:"opa_version,omitempty"` // OPA server version
	DecisionID  string         `json:"decision_id,omitempty"` // OPA's decision log id
	Input       map[string]any `json:"input"`                 // the input sent, with Redacted fields replaced by digests
	InputSHA256 string         `json:"input_sha256"`          // digest of the exact input sent, before redaction
	Redacted    []string       `json:"redacted,omitempty"`    // which input fields were replaced, and how
	Result      map[string]any `json:"result"`                // the decision document OPA returned
	Fields      []string       `json:"fields,omitempty"`      // the result fields the broker acts on; the rest are helpers
	Gates       []GateEvidence `json:"gates,omitempty"`       // extra tool gates, in evaluation order
}

// The result fields each decision reads (see the result structs in engine.go
// and GateResult).
var (
	planDecisionFields     = []string{"allow", "violations"}
	toolDecisionFields     = []string{"allow", "require_approval", "require_step_up", "deny", "deny_reasons"}
	envelopeDecisionFields = []string{"allow", "auto_accept", "require_manual_acceptance", "scope_violations", "deny_reasons"}
)

// GateEvidence records one extra tool gate (policy.tool_gates) evaluation.
type GateEvidence struct {
	Query      string         `json:"query"`
	Revision   string         `json:"revision,omitempty"`
	DecisionID string         `json:"decision_id,omitempty"`
	Result     map[string]any `json:"result,omitempty"`
	Error      string         `json:"error,omitempty"`
}

// opaMeta is the provenance OPA returns alongside a result.
type opaMeta struct {
	Revision   string
	OPAVersion string
	DecisionID string
}

// canonicalJSON marshals v with sorted map keys, no whitespace and no HTML
// escaping: the form `jq -cS` prints, so digests can be recomputed outside Go.
func canonicalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// digestJSON is "sha256:<hex>" of v's canonical JSON.
func digestJSON(v any) (string, error) {
	b, err := canonicalJSON(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// jsonDoc round-trips v through JSON into a generic document, so evidence
// holds exactly what OPA saw (numbers as JSON numbers, sets as arrays).
func jsonDoc(v any) (map[string]any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// redactPlanInput replaces each step's tool arguments and justification with
// their SHA-256 digests. Tool arguments can carry personal data (a patient
// name in a document request), and the audit store is write-once, so the raw
// values must not land there. aikonos.plan_validation only compares arguments
// for equality between steps and checks a justification is non-empty; both
// survive the replacement, so the redacted input replays to the same result.
// A policy that reads argument contents would not replay from this record;
// the input digest still commits to the original.
func redactPlanInput(input map[string]any) ([]string, error) {
	plan, _ := input["plan"].(map[string]any)
	if plan == nil {
		return nil, nil
	}
	steps, _ := plan["steps"].([]any)
	for i, raw := range steps {
		step, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("plan step %d is not an object", i)
		}
		if args, ok := step["args"]; ok && args != nil {
			d, err := digestJSON(args)
			if err != nil {
				return nil, err
			}
			step["args"] = d
		}
		if j, ok := step["justification"].(string); ok && j != "" {
			d, err := digestJSON(j)
			if err != nil {
				return nil, err
			}
			step["justification"] = d
		}
	}
	return []string{
		"plan.steps[].args: replaced by the sha256 of its canonical JSON (compared only for equality)",
		"plan.steps[].justification: a non-empty value replaced by its sha256 (checked only for presence)",
	}, nil
}
