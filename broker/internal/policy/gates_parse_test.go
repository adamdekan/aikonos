package policy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	planv1 "github.com/adamdekan/aikonos/gen/go/plan/v1"
)

// ── ParseToolGates ────────────────────────────────────────────────────────────

func TestParseToolGates_Empty(t *testing.T) {
	got := ParseToolGates("")
	if len(got) != 0 {
		t.Fatalf("empty string: want 0 gates, got %d", len(got))
	}
}

func TestParseToolGates_Single(t *testing.T) {
	got := ParseToolGates("tenant_quota=aikonos/tenant_quota")
	if len(got) != 1 {
		t.Fatalf("want 1 gate, got %d: %v", len(got), got)
	}
	if got[0].Name != "tenant_quota" || got[0].Path != "aikonos/tenant_quota" {
		t.Errorf("unexpected gate: %+v", got[0])
	}
}

func TestParseToolGates_Multiple(t *testing.T) {
	got := ParseToolGates("gate_a=aikonos/a, gate_b=aikonos/b")
	if len(got) != 2 {
		t.Fatalf("want 2 gates, got %d: %v", len(got), got)
	}
	if got[0].Name != "gate_a" || got[0].Path != "aikonos/a" {
		t.Errorf("got[0] = %+v", got[0])
	}
	if got[1].Name != "gate_b" || got[1].Path != "aikonos/b" {
		t.Errorf("got[1] = %+v", got[1])
	}
}

func TestParseToolGates_MalformedSkipped(t *testing.T) {
	// no "=", empty name, empty path — all skipped; one good entry survives.
	got := ParseToolGates("no_equals,=emptyname,goodname=,valid=aikonos/valid")
	if len(got) != 1 {
		t.Fatalf("want 1 gate (malformed skipped), got %d: %v", len(got), got)
	}
	if got[0].Name != "valid" || got[0].Path != "aikonos/valid" {
		t.Errorf("unexpected gate: %+v", got[0])
	}
}

func TestParseToolGates_TrailingComma(t *testing.T) {
	got := ParseToolGates("g=aikonos/g,")
	if len(got) != 1 {
		t.Fatalf("want 1, got %d", len(got))
	}
}

func TestParseToolGates_Whitespace(t *testing.T) {
	got := ParseToolGates("  g1 = aikonos/g1 , g2=aikonos/g2  ")
	if len(got) != 2 {
		t.Fatalf("want 2, got %d: %v", len(got), got)
	}
	if got[0].Name != "g1" || got[0].Path != "aikonos/g1" {
		t.Errorf("got[0] = %+v", got[0])
	}
}

// ── Registered gates from ParseToolGates produce same aggregate behaviour ────

func TestRegisterParsedGates_AggregatesBehavior(t *testing.T) {
	// Simulate: core allows, extra gate denies — result must be deny.
	// This proves that gates registered via ParseToolGates+RegisterToolGate
	// produce the same aggregate as programmatic registration.
	srv := fakeOPA(t, map[string]map[string]any{
		"tool_invocation": {"allow": true},
		"parsed_gate":     {"deny": true, "deny_reasons": []any{"parsed gate violated"}},
	}, nil)
	defer srv.Close()

	e := newTestEngine(t, srv.URL)

	// Register via the parsed list, mirroring what main.go does.
	for _, g := range ParseToolGates("parsed=aikonos/parsed_gate") {
		e.RegisterToolGate(g.Name, g.Path)
	}

	dec, err := e.DecideToolCall(context.Background(), ToolQuery{
		ToolID:      "web.fetch",
		EffectClass: planv1.EffectClass_READ_ONLY,
	})
	if err != nil {
		t.Fatalf("DecideToolCall: %v", err)
	}
	if dec.Allow {
		t.Error("want Allow=false (parsed gate denies)")
	}
	if !dec.Deny {
		t.Error("want Deny=true")
	}
}

func TestRegisterParsedGates_EmptyRegistersNothing(t *testing.T) {
	// Empty config → no extra gates → behaviour unchanged from core alone.
	srv := fakeOPA(t, map[string]map[string]any{
		"tool_invocation": {"allow": true},
	}, nil)
	defer srv.Close()

	e := newTestEngine(t, srv.URL)
	for _, g := range ParseToolGates("") {
		e.RegisterToolGate(g.Name, g.Path)
	}

	dec, err := e.DecideToolCall(context.Background(), ToolQuery{
		ToolID:      "web.fetch",
		EffectClass: planv1.EffectClass_READ_ONLY,
	})
	if err != nil {
		t.Fatalf("DecideToolCall: %v", err)
	}
	if !dec.Allow {
		t.Error("want Allow=true — no extra gates")
	}
}

func TestRegisterParsedGates_MalformedSkippedMeansNoop(t *testing.T) {
	// A fully malformed config string produces 0 gates — no registration →
	// behaviour identical to the no-gates case.
	callCount := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/data/aikonos/tool_invocation", func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":{"allow":true}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	e := newTestEngine(t, srv.URL)
	for _, g := range ParseToolGates("no_equals,=bad,good_name=") {
		e.RegisterToolGate(g.Name, g.Path)
	}
	if len(e.extraToolGates) != 0 {
		t.Errorf("malformed config must register 0 gates, got %d", len(e.extraToolGates))
	}
}
