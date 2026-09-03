// broker/internal/config/schema.go
// Allowlisted config keys. Every key here is routing/convenience-only —
// none may widen a capability scope, grant access, or lower an authorization
// bar. The Set path rejects anything not in this map.
package config

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var llmModelRE = regexp.MustCompile(`^[a-z0-9._-]+/[a-z0-9._:-]+$`)

// validLLMModel reports whether s is a well-formed provider/model string.
func validLLMModel(s string) bool { return llmModelRE.MatchString(s) }

// ValidLLMModel is the exported form of validLLMModel, used by agent validation.
func ValidLLMModel(s string) bool { return validLLMModel(s) }

// validRegoClasses is the exhaustive set of rego names for the 8 non-UNSPECIFIED
// effect classes. Used by the effect_class_routing validator.
var validRegoClasses = map[string]bool{
	"read_only":         true,
	"write_local":       true,
	"write_internal":    true,
	"write_external":    true,
	"network_egress":    true,
	"credential_access": true,
	"destructive":       true,
	"infrastructure":    true,
}

// validPostures is the set of posture values accepted by effect_class_routing.
var validPostures = map[string]bool{
	"approval": true,
	"step_up":  true,
	"deny":     true,
}

// Posture represents the routing posture configured for an effect class.
// A posture can only ADD restriction — it escalates, never relaxes.
type Posture string

const (
	PostureApproval Posture = "approval"
	PostureStepUp   Posture = "step_up"
	PostureDeny     Posture = "deny"
)

// Severity returns a numeric severity for comparison. Higher = stricter.
// Maps to the same bucket system as effectclass.Severity:
//
//	approval=2, step_up=3, deny=4
//
// These values are chosen so a config Posture severity can be compared against
// the effectclass severity of the current decision.
func (p Posture) Severity() int {
	switch p {
	case PostureApproval:
		return 2
	case PostureStepUp:
		return 3
	case PostureDeny:
		return 4
	default:
		return 0
	}
}

// ParseEffectClassRouting parses a comma-separated "class:posture" config string
// into a map. Malformed pairs are silently skipped (degraded, not fatal).
// Duplicate class keys: last one wins. Unknown class names and unknown postures
// are skipped — they cannot make this helper panic or error.
func ParseEffectClassRouting(s string) map[string]Posture {
	out := map[string]Posture{}
	if s == "" {
		return out
	}
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		idx := strings.IndexByte(pair, ':')
		if idx <= 0 || idx == len(pair)-1 {
			continue // malformed — skip
		}
		class := strings.TrimSpace(pair[:idx])
		posture := Posture(strings.TrimSpace(pair[idx+1:]))
		if !validRegoClasses[class] {
			continue // unknown class — skip
		}
		if !validPostures[string(posture)] {
			continue // unknown posture — skip
		}
		out[class] = posture
	}
	return out
}

// Kind is the value type for a schema key.
type Kind int

const (
	KindInt    Kind = iota
	KindBool        // reserved for future keys
	KindString      // reserved for future keys
)

// Key is the schema entry for one config knob.
type Key struct {
	Name     string
	Kind     Kind
	Default  string
	Doc      string
	Validate func(string) error
}

// Schema is the exhaustive allowlist of settable config keys. Adding a key
// here is the only way to make it settable. The guardrail test in
// store_test.go asserts this map contains exactly the MVP convenience keys.
var Schema = map[string]Key{
	"approval_expiry_hours": {
		Name:    "approval_expiry_hours",
		Kind:    KindInt,
		Default: "24",
		Doc:     "Hours a pending human-approval gate stays open before it expires",
		Validate: func(v string) error {
			n, err := strconv.Atoi(v)
			if err != nil {
				return fmt.Errorf("approval_expiry_hours: must be an integer, got %q", v)
			}
			if n < 1 || n > 720 {
				return fmt.Errorf("approval_expiry_hours: must be in [1, 720], got %d", n)
			}
			return nil
		},
	},
	"disabled_tools": {
		Name:    "disabled_tools",
		Kind:    KindString,
		Default: "",
		Doc:     "Comma-separated tool IDs denied at InvokeTool for this tenant; deny-only convenience knob, cannot grant or widen access",
		Validate: func(v string) error {
			if len(v) > 4096 {
				return fmt.Errorf("disabled_tools: value exceeds 4096 characters")
			}
			return nil
		},
	},
	"approval_required_n": {
		Name:    "approval_required_n",
		Kind:    KindInt,
		Default: "1",
		Doc:     "Minimum number of distinct approvers required for a human-approval gate. The policy floor still applies (config can only raise, never lower). Range [1, 16].",
		Validate: func(v string) error {
			n, err := strconv.Atoi(v)
			if err != nil {
				return fmt.Errorf("approval_required_n: must be an integer, got %q", v)
			}
			if n < 1 || n > 16 {
				return fmt.Errorf("approval_required_n: must be in [1, 16], got %d", n)
			}
			return nil
		},
	},
	"effect_class_routing": {
		Name:    "effect_class_routing",
		Kind:    KindString,
		Default: "",
		Doc:     "Comma-separated class:posture pairs that escalate routing for an effect class (e.g. write_local:approval). Postures: approval, step_up, deny. Monotonic-stricter only — config may escalate, never relax.",
		Validate: func(v string) error {
			if len(v) > 4096 {
				return fmt.Errorf("effect_class_routing: value exceeds 4096 characters")
			}
			// Each pair must be well-formed; malformed pairs are rejected at validate time.
			if v == "" {
				return nil
			}
			for _, pair := range strings.Split(v, ",") {
				pair = strings.TrimSpace(pair)
				if pair == "" {
					continue
				}
				idx := strings.IndexByte(pair, ':')
				if idx <= 0 || idx == len(pair)-1 {
					return fmt.Errorf("effect_class_routing: malformed pair %q (want class:posture)", pair)
				}
				class := strings.TrimSpace(pair[:idx])
				posture := strings.TrimSpace(pair[idx+1:])
				if !validRegoClasses[class] {
					return fmt.Errorf("effect_class_routing: unknown effect class %q", class)
				}
				if !validPostures[posture] {
					return fmt.Errorf("effect_class_routing: unknown posture %q (want approval|step_up|deny)", posture)
				}
			}
			return nil
		},
	},
	// llm_model selects the LLM provider/model for the agent-gateway when
	// non-empty. Routing/convenience-only — never affects a gate, scope, or
	// approval decision.
	"llm_model": {
		Name:    "llm_model",
		Kind:    KindString,
		Default: "",
		Doc:     "Per-tenant LLM model override (e.g. anthropic/claude-sonnet-4.6). Empty means the gateway uses its env default. Routing/convenience-only — does not affect any gate.",
		Validate: func(v string) error {
			if v == "" {
				return nil
			}
			if len(v) > 128 {
				return fmt.Errorf("llm_model: value exceeds 128 characters")
			}
			if !validLLMModel(v) {
				return fmt.Errorf("llm_model: must match provider/model (^[a-z0-9._-]+/[a-z0-9._:-]+$), got %q", v)
			}
			return nil
		},
	},
}

// ParseList splits a comma-separated config string into trimmed, non-empty
// tokens. Used by both plan-time and invoke-time disabled_tools consumers so
// parsing is consistent.
func ParseList(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		t := strings.TrimSpace(p)
		if t != "" {
			out = append(out, t)
		}
	}
	return out
}
