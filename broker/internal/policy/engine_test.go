package policy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	planv1 "github.com/adamdekan/aikonos/gen/go/plan/v1"
)

// fakeOPA returns a test server that responds to /v1/data/<path> with the
// supplied result objects keyed by the trailing path segment, and records the
// last request body it received.
func fakeOPA(t *testing.T, results map[string]map[string]any, lastBody *map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if lastBody != nil {
			var parsed map[string]any
			_ = json.Unmarshal(body, &parsed)
			*lastBody = parsed
		}
		// key on the package name (last path segment)
		seg := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		res, ok := results[seg]
		if !ok {
			// no document for this query — OPA returns {} with no "result"
			w.Write([]byte(`{}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"result": res})
	}))
}

func newTestEngine(t *testing.T, url string) *Engine {
	t.Helper()
	e, err := NewEngine(context.Background(), Config{OPAEndpoint: url})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	return e
}

func TestEffectClassString(t *testing.T) {
	cases := map[planv1.EffectClass]string{
		planv1.EffectClass_READ_ONLY:                "read_only",
		planv1.EffectClass_WRITE_EXTERNAL:           "write_external",
		planv1.EffectClass_DESTRUCTIVE:              "destructive",
		planv1.EffectClass_INFRASTRUCTURE:           "infrastructure",
		planv1.EffectClass_EFFECT_CLASS_UNSPECIFIED: "unspecified",
	}
	for ec, want := range cases {
		if got := effectClassString(ec); got != want {
			t.Errorf("effectClassString(%v) = %q, want %q", ec, got, want)
		}
	}
}

func TestDecidePlan_Violations(t *testing.T) {
	srv := fakeOPA(t, map[string]map[string]any{
		"plan_validation": {"allow": false, "violations": []any{"Plan has 51 steps, exceeding the maximum of 50."}},
	}, nil)
	defer srv.Close()
	e := newTestEngine(t, srv.URL)

	plan := &planv1.Plan{PlanId: "p1", Steps: []*planv1.PlanStep{{Seq: 1, ToolId: "x", EffectClass: planv1.EffectClass_READ_ONLY}}}
	dec, err := e.DecidePlan(context.Background(), plan, PlanContext{CostBudget: 1000})
	if err != nil {
		t.Fatalf("DecidePlan: %v", err)
	}
	if dec.Allow {
		t.Error("expected Allow=false")
	}
	if len(dec.Violations) != 1 || !strings.Contains(dec.Violations[0], "exceeding the maximum") {
		t.Errorf("unexpected violations: %v", dec.Violations)
	}
}

func TestDecidePlan_AllowAndInputShape(t *testing.T) {
	var body map[string]any
	srv := fakeOPA(t, map[string]map[string]any{
		"plan_validation": {"allow": true},
	}, &body)
	defer srv.Close()
	e := newTestEngine(t, srv.URL)

	plan := &planv1.Plan{
		PlanId:             "p1",
		HasDlpAttestation:  true,
		EffectClassCeiling: planv1.EffectClass_WRITE_LOCAL,
		Steps: []*planv1.PlanStep{
			{Seq: 1, ToolId: "siem.query", EffectClass: planv1.EffectClass_READ_ONLY, EstimatedCost: 50},
		},
	}
	dec, err := e.DecidePlan(context.Background(), plan, PlanContext{
		CostBudget:         1000,
		EffectClassCeiling: planv1.EffectClass_WRITE_LOCAL,
		ActorSpiffeID:      "spiffe://aikonos.com/sandbox/abc",
	})
	if err != nil {
		t.Fatalf("DecidePlan: %v", err)
	}
	if !dec.Allow {
		t.Error("expected Allow=true")
	}

	// Verify the OPA input matches what the rego expects.
	input, _ := body["input"].(map[string]any)
	if input == nil {
		t.Fatal("request had no input")
	}
	planIn, _ := input["plan"].(map[string]any)
	if planIn["has_dlp_attestation"] != true {
		t.Errorf("plan.has_dlp_attestation not passed through: %v", planIn["has_dlp_attestation"])
	}
	steps, _ := planIn["steps"].([]any)
	if len(steps) != 1 {
		t.Fatalf("expected 1 step in input, got %d", len(steps))
	}
	step0 := steps[0].(map[string]any)
	if step0["effect_class"] != "read_only" {
		t.Errorf("step effect_class = %v, want read_only", step0["effect_class"])
	}
	task, _ := input["task"].(map[string]any)
	if task["effect_class_ceiling"] != "write_local" {
		t.Errorf("task.effect_class_ceiling = %v, want write_local", task["effect_class_ceiling"])
	}
}

func TestDecideToolCall_Outcomes(t *testing.T) {
	tests := []struct {
		name       string
		result     map[string]any
		wantAllow  bool
		wantAppr   bool
		wantStepUp bool
		wantDeny   bool
		reasonHas  string
	}{
		{"allow", map[string]any{"allow": true}, true, false, false, false, ""},
		{"approval", map[string]any{"require_approval": true}, false, true, false, false, ""},
		{"step_up", map[string]any{"require_approval": true, "require_step_up": true}, false, true, true, false, ""},
		{"deny", map[string]any{"deny": true, "deny_reasons": []any{"risk too high", "not business hours"}}, false, false, false, true, "risk too high; not business hours"},
		{"empty_denies_by_default", map[string]any{}, false, false, false, false, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := fakeOPA(t, map[string]map[string]any{"tool_invocation": tc.result}, nil)
			defer srv.Close()
			e := newTestEngine(t, srv.URL)

			dec, err := e.DecideToolCall(context.Background(), ToolQuery{
				ToolID:      "siem.query",
				EffectClass: planv1.EffectClass_WRITE_EXTERNAL,
			})
			if err != nil {
				t.Fatalf("DecideToolCall: %v", err)
			}
			if dec.Allow != tc.wantAllow || dec.NeedsApproval != tc.wantAppr ||
				dec.NeedsStepUp != tc.wantStepUp || dec.Deny != tc.wantDeny {
				t.Errorf("got allow=%v approval=%v stepup=%v deny=%v",
					dec.Allow, dec.NeedsApproval, dec.NeedsStepUp, dec.Deny)
			}
			if tc.reasonHas != "" && dec.Reason != tc.reasonHas {
				t.Errorf("reason = %q, want %q", dec.Reason, tc.reasonHas)
			}
		})
	}
}

func TestDecideEnvelopeSend(t *testing.T) {
	tests := []struct {
		name        string
		result      map[string]any
		wantAllow   bool
		wantAuto    bool
		wantScopeVs int
	}{
		{"allow", map[string]any{"allow": true}, true, false, 0},
		{"auto_accept", map[string]any{"allow": true, "auto_accept": true}, true, true, 0},
		{"scope_violation", map[string]any{"allow": false, "scope_violations": []any{"Scope 'db:drop' is not in sender's capability set and cannot be delegated."}}, false, false, 1},
		{"deny_budget", map[string]any{"allow": false, "deny_reasons": []any{"Delegation cost budget exceeds sender's remaining budget."}}, false, false, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := fakeOPA(t, map[string]map[string]any{"envelope_send": tc.result}, nil)
			defer srv.Close()
			e := newTestEngine(t, srv.URL)

			dec, err := e.DecideEnvelopeSend(context.Background(), 50, []string{"siem:read"}, EnvelopeSendContext{
				SenderScopes: []string{"siem:read", "slack:write"},
			})
			if err != nil {
				t.Fatalf("DecideEnvelopeSend: %v", err)
			}
			if dec.Allow != tc.wantAllow || dec.AutoAccept != tc.wantAuto || len(dec.ScopeViolations) != tc.wantScopeVs {
				t.Errorf("got allow=%v auto=%v scopeViolations=%d", dec.Allow, dec.AutoAccept, len(dec.ScopeViolations))
			}
		})
	}
}

func TestDecideEnvelopeSend_InputShape(t *testing.T) {
	var body map[string]any
	srv := fakeOPA(t, map[string]map[string]any{"envelope_send": {"allow": true}}, &body)
	defer srv.Close()
	e := newTestEngine(t, srv.URL)

	_, err := e.DecideEnvelopeSend(context.Background(), 200, []string{"siem:read", "db:drop"}, EnvelopeSendContext{
		SenderScopes: []string{"siem:read"},
		EffectClass:  "write_external",
	})
	if err != nil {
		t.Fatalf("DecideEnvelopeSend: %v", err)
	}
	in, _ := body["input"].(map[string]any)
	if in["fga_send_decision"] != "allow" {
		t.Errorf("fga_send_decision default = %v, want allow", in["fga_send_decision"])
	}
	envIn, _ := in["envelope"].(map[string]any)
	deleg, _ := envIn["delegation"].(map[string]any)
	if deleg["depth"].(float64) != 1 {
		t.Errorf("depth default = %v, want 1", deleg["depth"])
	}
	scopes, _ := deleg["attenuated_scopes"].([]any)
	if len(scopes) != 2 {
		t.Errorf("attenuated_scopes len = %d, want 2", len(scopes))
	}
	sender, _ := in["sender"].(map[string]any)
	if ss, _ := sender["capability_scopes"].([]any); len(ss) != 1 {
		t.Errorf("sender.capability_scopes len = %d, want 1", len(ss))
	}
}

func TestEvaluateGate_Direct(t *testing.T) {
	tests := []struct {
		name        string
		result      map[string]any
		wantAllow   bool
		wantApprove bool
		wantStepUp  bool
		wantDeny    bool
		wantReasons []string
	}{
		{
			name:      "allow",
			result:    map[string]any{"allow": true},
			wantAllow: true,
		},
		{
			name:        "require_approval",
			result:      map[string]any{"require_approval": true},
			wantApprove: true,
		},
		{
			name:        "require_step_up",
			result:      map[string]any{"require_approval": true, "require_step_up": true},
			wantApprove: true,
			wantStepUp:  true,
		},
		{
			name:        "deny_with_reasons",
			result:      map[string]any{"deny": true, "deny_reasons": []any{"risk too high", "off hours"}},
			wantDeny:    true,
			wantReasons: []string{"risk too high", "off hours"},
		},
		{
			name:   "empty_doc_zero_value",
			result: map[string]any{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := fakeOPA(t, map[string]map[string]any{"test_gate": tc.result}, nil)
			defer srv.Close()
			e := newTestEngine(t, srv.URL)

			gr, err := e.evaluateGate(context.Background(), "aikonos/test_gate", map[string]any{"x": 1})
			if err != nil {
				t.Fatalf("evaluateGate: %v", err)
			}
			if gr.Allow != tc.wantAllow {
				t.Errorf("Allow = %v, want %v", gr.Allow, tc.wantAllow)
			}
			if gr.RequireApproval != tc.wantApprove {
				t.Errorf("RequireApproval = %v, want %v", gr.RequireApproval, tc.wantApprove)
			}
			if gr.RequireStepUp != tc.wantStepUp {
				t.Errorf("RequireStepUp = %v, want %v", gr.RequireStepUp, tc.wantStepUp)
			}
			if gr.Deny != tc.wantDeny {
				t.Errorf("Deny = %v, want %v", gr.Deny, tc.wantDeny)
			}
			if len(tc.wantReasons) > 0 {
				if len(gr.DenyReasons) != len(tc.wantReasons) {
					t.Fatalf("DenyReasons len = %d, want %d: %v", len(gr.DenyReasons), len(tc.wantReasons), gr.DenyReasons)
				}
				for i, r := range tc.wantReasons {
					if gr.DenyReasons[i] != r {
						t.Errorf("DenyReasons[%d] = %q, want %q", i, gr.DenyReasons[i], r)
					}
				}
			}
		})
	}
}

// ── CP2: extra tool-gate tests ────────────────────────────────────────────────

func TestRegisterToolGate_ExtraDenies(t *testing.T) {
	// Extra gate denies while core allows → aggregate must deny.
	srv := fakeOPA(t, map[string]map[string]any{
		"tool_invocation": {"allow": true},
		"custom_gate":     {"deny": true, "deny_reasons": []any{"custom policy violation"}},
	}, nil)
	defer srv.Close()
	e := newTestEngine(t, srv.URL)
	e.RegisterToolGate("custom", "aikonos/custom_gate")

	dec, err := e.DecideToolCall(context.Background(), ToolQuery{
		ToolID:      "web.fetch",
		EffectClass: planv1.EffectClass_READ_ONLY,
	})
	if err != nil {
		t.Fatalf("DecideToolCall: %v", err)
	}
	if dec.Allow {
		t.Error("expected Allow=false (extra gate denies)")
	}
	if !dec.Deny {
		t.Error("expected Deny=true")
	}
	if len(dec.Reasons) == 0 || dec.Reasons[0] != "custom policy violation" {
		t.Errorf("expected deny reason from extra gate, got %v", dec.Reasons)
	}
}

func TestRegisterToolGate_ExtraRequiresApproval(t *testing.T) {
	// Extra gate requires approval while core allows → NeedsApproval=true, Allow=true.
	// Allow = core.Allow && !anyDeny = true; downstream classifyDecision routes on
	// NeedsApproval independently, so Allow and NeedsApproval can coexist.
	srv := fakeOPA(t, map[string]map[string]any{
		"tool_invocation": {"allow": true},
		"custom_gate":     {"require_approval": true},
	}, nil)
	defer srv.Close()
	e := newTestEngine(t, srv.URL)
	e.RegisterToolGate("custom", "aikonos/custom_gate")

	dec, err := e.DecideToolCall(context.Background(), ToolQuery{
		ToolID:      "web.fetch",
		EffectClass: planv1.EffectClass_READ_ONLY,
	})
	if err != nil {
		t.Fatalf("DecideToolCall: %v", err)
	}
	if !dec.Allow {
		t.Error("expected Allow=true (core allowed, no deny; NeedsApproval is orthogonal)")
	}
	if !dec.NeedsApproval {
		t.Error("expected NeedsApproval=true")
	}
}

func TestRegisterToolGate_ExtraAllowCannotClearNeedsApproval(t *testing.T) {
	// Core requires approval (allow:false); extra gate returns allow:true.
	// Extra allow must not clear NeedsApproval and must not widen Allow.
	// Allow = core.Allow && !anyDeny = false; NeedsApproval stays true.
	srv := fakeOPA(t, map[string]map[string]any{
		"tool_invocation": {"require_approval": true},
		"custom_gate":     {"allow": true},
	}, nil)
	defer srv.Close()
	e := newTestEngine(t, srv.URL)
	e.RegisterToolGate("custom", "aikonos/custom_gate")

	dec, err := e.DecideToolCall(context.Background(), ToolQuery{
		ToolID:      "web.fetch",
		EffectClass: planv1.EffectClass_WRITE_EXTERNAL,
	})
	if err != nil {
		t.Fatalf("DecideToolCall: %v", err)
	}
	if dec.Allow {
		t.Error("expected Allow=false — extra allow cannot widen core NeedsApproval")
	}
	if !dec.NeedsApproval {
		t.Error("expected NeedsApproval=true — extra allow must not clear it")
	}
}

func TestRegisterToolGate_ExtraAllowCannotWiden(t *testing.T) {
	// Extra gate returns allow:true while core denies → aggregate must stay denied.
	srv := fakeOPA(t, map[string]map[string]any{
		"tool_invocation": {"deny": true, "deny_reasons": []any{"core denied"}},
		"custom_gate":     {"allow": true},
	}, nil)
	defer srv.Close()
	e := newTestEngine(t, srv.URL)
	e.RegisterToolGate("custom", "aikonos/custom_gate")

	dec, err := e.DecideToolCall(context.Background(), ToolQuery{
		ToolID:      "web.fetch",
		EffectClass: planv1.EffectClass_READ_ONLY,
	})
	if err != nil {
		t.Fatalf("DecideToolCall: %v", err)
	}
	if dec.Allow {
		t.Error("expected Allow=false — extra allow cannot widen a core deny")
	}
	if !dec.Deny {
		t.Error("expected Deny=true")
	}
}

func TestRegisterToolGate_ExtraPathError_FailsClosed(t *testing.T) {
	// Extra gate path returns a non-200 (simulated error) → fail-closed: deny.
	// fakeOPA returns {} (no result doc) for unknown paths; we need a 500 for the
	// extra gate path, so we use a custom server.
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/data/aikonos/tool_invocation", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"allow": true}})
	})
	mux.HandleFunc("/v1/data/aikonos/broken_gate", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	e := newTestEngine(t, srv.URL)
	e.RegisterToolGate("broken", "aikonos/broken_gate")

	dec, err := e.DecideToolCall(context.Background(), ToolQuery{
		ToolID:      "web.fetch",
		EffectClass: planv1.EffectClass_READ_ONLY,
	})
	if err != nil {
		t.Fatalf("DecideToolCall must not return error for extra gate failure: %v", err)
	}
	if dec.Allow {
		t.Error("expected Allow=false (fail-closed on extra gate error)")
	}
	if len(dec.Reasons) == 0 {
		t.Error("expected a deny reason describing the failed gate")
	}
	found := false
	for _, r := range dec.Reasons {
		if strings.Contains(r, "broken") || strings.Contains(r, "evaluation failed") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("deny reasons did not mention gate name or 'evaluation failed': %v", dec.Reasons)
	}
}

func TestRegisterToolGate_CorePathError_StillReturnsError(t *testing.T) {
	// Core gate error must still propagate as an error (mandatory).
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/data/aikonos/tool_invocation", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	e := newTestEngine(t, srv.URL)

	_, err := e.DecideToolCall(context.Background(), ToolQuery{
		ToolID:      "web.fetch",
		EffectClass: planv1.EffectClass_READ_ONLY,
	})
	if err == nil {
		t.Error("expected error for core gate failure")
	}
}

func TestRegisterToolGate_NoExtras_Unchanged(t *testing.T) {
	// No extra gates → Decision identical to CP1/core behavior (regression guard).
	tests := []struct {
		name      string
		result    map[string]any
		wantAllow bool
		wantAppr  bool
		wantDeny  bool
	}{
		{"allow", map[string]any{"allow": true}, true, false, false},
		{"approval", map[string]any{"require_approval": true}, false, true, false},
		{"allow_and_approval", map[string]any{"allow": true, "require_approval": true}, true, true, false},
		{"deny", map[string]any{"deny": true, "deny_reasons": []any{"risk"}}, false, false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := fakeOPA(t, map[string]map[string]any{"tool_invocation": tc.result}, nil)
			defer srv.Close()
			e := newTestEngine(t, srv.URL)
			// no RegisterToolGate calls

			dec, err := e.DecideToolCall(context.Background(), ToolQuery{
				ToolID:      "siem.query",
				EffectClass: planv1.EffectClass_READ_ONLY,
			})
			if err != nil {
				t.Fatalf("DecideToolCall: %v", err)
			}
			if dec.Allow != tc.wantAllow || dec.NeedsApproval != tc.wantAppr || dec.Deny != tc.wantDeny {
				t.Errorf("got allow=%v approval=%v deny=%v", dec.Allow, dec.NeedsApproval, dec.Deny)
			}
		})
	}
}

func TestRegisterToolGate_Dedupe(t *testing.T) {
	// Registering the same path twice must not double-evaluate it.
	callCount := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/data/aikonos/tool_invocation", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"allow": true}})
	})
	mux.HandleFunc("/v1/data/aikonos/dup_gate", func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"allow": true}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	e := newTestEngine(t, srv.URL)
	e.RegisterToolGate("dup1", "aikonos/dup_gate")
	e.RegisterToolGate("dup2", "aikonos/dup_gate") // same path — must be deduped

	_, err := e.DecideToolCall(context.Background(), ToolQuery{ToolID: "x", EffectClass: planv1.EffectClass_READ_ONLY})
	if err != nil {
		t.Fatalf("DecideToolCall: %v", err)
	}
	if callCount != 1 {
		t.Errorf("dup_gate called %d times, want 1 (dedupe by path)", callCount)
	}
}

func TestBuildToolInput_Defaults(t *testing.T) {
	e := newTestEngine(t, "http://unused")
	in := e.buildToolInput(ToolQuery{EffectClass: planv1.EffectClass_READ_ONLY})

	if in["fga_decision"] != "allow" {
		t.Errorf("fga_decision default = %v, want allow", in["fga_decision"])
	}
	env, _ := in["env"].(map[string]any)
	if env == nil {
		t.Fatal("env missing")
	}
	if _, ok := env["hour"]; !ok {
		t.Error("env.hour missing")
	}
	if _, ok := env["day"]; !ok {
		t.Error("env.day missing")
	}
	tool, _ := in["tool"].(map[string]any)
	if tool["effect_class"] != "read_only" {
		t.Errorf("tool.effect_class = %v, want read_only", tool["effect_class"])
	}
}

// TestBuildToolInput_ContainsToolID pins the contract between buildToolInput and
// the rego rule at tool_invocation.rego:86 which references input.tool.id for the
// skill-gate deny message. Without tool.id in the map the message renders as "".
func TestBuildToolInput_ContainsToolID(t *testing.T) {
	e := newTestEngine(t, "http://unused")
	in := e.buildToolInput(ToolQuery{
		ToolID:      "email.draft",
		EffectClass: planv1.EffectClass_WRITE_EXTERNAL,
		FGADecision: "deny",
	})

	tool, _ := in["tool"].(map[string]any)
	if tool == nil {
		t.Fatal("tool map missing from buildToolInput output")
	}
	if tool["id"] != "email.draft" {
		t.Errorf("tool.id = %v, want email.draft — required by rego skill-gate deny message", tool["id"])
	}
	// fga_decision must pass through unchanged (not defaulted to "allow").
	if in["fga_decision"] != "deny" {
		t.Errorf("fga_decision = %v, want deny", in["fga_decision"])
	}
}
