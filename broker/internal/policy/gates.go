package policy

import (
	"context"
	"fmt"
	"strings"

	"go.uber.org/zap"
)

// ParseToolGates parses a comma-separated "name=path" config string into a
// slice of ToolGate values. Malformed entries (empty name or path, missing "=")
// are silently skipped — degraded, not fatal, mirroring the alerting opt-in
// posture. Duplicate paths are preserved here; RegisterToolGate deduplicates.
func ParseToolGates(s string) []ToolGate {
	if s == "" {
		return nil
	}
	var out []ToolGate
	for _, entry := range strings.Split(s, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		idx := strings.IndexByte(entry, '=')
		if idx <= 0 || idx == len(entry)-1 {
			continue // missing "=" or empty name/path — skip
		}
		name := strings.TrimSpace(entry[:idx])
		path := strings.TrimSpace(entry[idx+1:])
		if name == "" || path == "" {
			continue
		}
		out = append(out, ToolGate{Name: name, Path: path})
	}
	return out
}

// GateResult is the parsed output of a single OPA gate evaluation. The json
// tags match the field names the tool_invocation rego (and any future gate
// regos) emit, so the same struct can be reused across gate paths.
type GateResult struct {
	Allow           bool     `json:"allow"`
	RequireApproval bool     `json:"require_approval"`
	RequireStepUp   bool     `json:"require_step_up"`
	Deny            bool     `json:"deny"`
	DenyReasons     []string `json:"deny_reasons"`
}

// evaluateGate queries OPA at path with input and deserialises the response
// into a GateResult. An undefined (empty) OPA document leaves all fields at
// their zero values — the caller interprets that as "no opinion".
func (e *Engine) evaluateGate(ctx context.Context, path string, input any) (GateResult, error) {
	var res GateResult
	if err := e.queryOPA(ctx, path, input, &res); err != nil {
		return GateResult{}, err
	}
	return res, nil
}

// ToolGate is an additional OPA policy path evaluated alongside the core
// aikonos/tool_invocation gate. Extra gates can only restrict — they cannot
// widen a decision already made by the core gate or another extra gate.
type ToolGate struct {
	Name string
	Path string
}

// RegisterToolGate appends an extra OPA gate to the engine. It is deduplicated
// by Path so operators can call it idempotently during startup. Not concurrency-
// safe: call before the server begins serving requests.
//
// To add a gate in production: ship the Rego policy in the opa-policies
// ConfigMap (key: <name>.rego, package aikonos/<name>) and call
// RegisterToolGate("<name>", "aikonos/<name>") on the engine returned by
// NewEngine, before registering the broker gRPC service.
//
// Example (commented-out — no extra gates are registered by default):
//
//	// policyEngine.RegisterToolGate("tenant_quota", "aikonos/tenant_quota")
func (e *Engine) RegisterToolGate(name, path string) {
	for _, g := range e.extraToolGates {
		if g.Path == path {
			return // dedupe by path
		}
	}
	e.extraToolGates = append(e.extraToolGates, ToolGate{Name: name, Path: path})
}

// gateIsDeny returns true if a GateResult represents an active denial
// (explicit deny flag or at least one deny reason).
func gateIsDeny(r GateResult) bool {
	return r.Deny || len(r.DenyReasons) > 0
}

// aggregateExtraGates merges results from all extra gates into the core result,
// applying monotonic-stricter semantics: extras can only escalate, never permit.
// On an evaluateGate error the gate is treated as a deny (fail-closed); the
// error is not propagated so one broken optional gate does not 500 every call.
func (e *Engine) aggregateExtraGates(ctx context.Context, input any, core GateResult) (GateResult, string) {
	if len(e.extraToolGates) == 0 {
		return core, "opa:tool_invocation"
	}

	anyDeny := gateIsDeny(core)
	needsApproval := core.RequireApproval
	needsStepUp := core.RequireStepUp
	reasons := append([]string(nil), core.DenyReasons...)
	decidingPath := "opa:tool_invocation"

	for _, gate := range e.extraToolGates {
		gr, err := e.evaluateGate(ctx, gate.Path, input)
		if err != nil {
			// Fail-closed: treat broken extra gate as a deny.
			reason := fmt.Sprintf("policy gate %q evaluation failed", gate.Name)
			reasons = append(reasons, reason)
			anyDeny = true
			if decidingPath == "opa:tool_invocation" {
				decidingPath = "opa:" + gate.Path
			}
			e.logger.Warn("extra tool gate error — failing closed",
				zap.String("gate", gate.Name),
				zap.String("path", gate.Path),
				zap.Error(err),
			)
			continue
		}
		if gateIsDeny(gr) {
			anyDeny = true
			reasons = append(reasons, gr.DenyReasons...)
			// why: first-escalating-gate-wins attribution — preserves the
			// earliest gate that tightened the decision for the audit trace.
			if decidingPath == "opa:tool_invocation" {
				decidingPath = "opa:" + gate.Path
			}
		}
		if gr.RequireApproval {
			needsApproval = true
			if decidingPath == "opa:tool_invocation" {
				decidingPath = "opa:" + gate.Path
			}
		}
		if gr.RequireStepUp {
			needsStepUp = true
		}
		// gr.Allow is intentionally ignored — extras only restrict.
	}

	return GateResult{
		Allow:           core.Allow && !anyDeny,
		RequireApproval: needsApproval,
		RequireStepUp:   needsStepUp,
		Deny:            anyDeny,
		DenyReasons:     reasons,
	}, decidingPath
}
