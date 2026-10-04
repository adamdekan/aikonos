package policy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	planv1 "github.com/adamdekan/aikonos/gen/go/plan/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestDigestJSON_IsTheJqCanonicalForm(t *testing.T) {
	d1, _ := digestJSON(map[string]any{"b": 1, "a": "<x> & y"})
	d2, _ := digestJSON(map[string]any{"a": "<x> & y", "b": 1})
	if d1 != d2 {
		t.Fatal("key order must not change the digest")
	}
	if want := "sha256:" + sha(`{"a":"<x> & y","b":1}`); d1 != want {
		t.Fatalf("digest = %s, want %s (sorted keys, no HTML escaping)", d1, want)
	}
}

func TestRedactPlanInput_KeepsWhatThePolicyCompares(t *testing.T) {
	input, err := jsonDoc(map[string]any{"plan": map[string]any{"steps": []any{
		map[string]any{"seq": 1, "args": map[string]any{"path": "patients/jane.pdf"}, "justification": "referral"},
		map[string]any{"seq": 2, "args": map[string]any{"path": "patients/jane.pdf"}, "justification": ""},
		map[string]any{"seq": 3, "args": map[string]any{"path": "other.pdf"}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	notes, err := redactPlanInput(input)
	if err != nil || len(notes) == 0 {
		t.Fatalf("redactPlanInput = %v, %v", notes, err)
	}
	raw, _ := json.Marshal(input)
	if strings.Contains(string(raw), "jane") || strings.Contains(string(raw), "referral") {
		t.Fatalf("redacted input still carries raw values: %s", raw)
	}
	steps := input["plan"].(map[string]any)["steps"].([]any)
	s1, s2, s3 := steps[0].(map[string]any), steps[1].(map[string]any), steps[2].(map[string]any)
	if s1["args"] != s2["args"] || s1["args"] == s3["args"] {
		t.Fatalf("argument digests must preserve equality: %v %v %v", s1["args"], s2["args"], s3["args"])
	}
	if j, _ := s1["justification"].(string); !strings.HasPrefix(j, "sha256:") {
		t.Fatalf("a non-empty justification becomes a digest, got %q", j)
	}
	if s2["justification"] != "" {
		t.Fatalf("an empty justification stays empty, got %q", s2["justification"])
	}
	if _, ok := s3["justification"]; ok {
		t.Fatal("a missing justification stays missing")
	}
}

// fakeProvenanceOPA answers every query with result and, unless revision is
// empty, provenance naming the "aikonos" bundle.
func fakeProvenanceOPA(t *testing.T, result map[string]any, revision string, lastInput *map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("provenance") != "true" {
			t.Errorf("query %s did not ask for provenance", r.URL)
		}
		var body struct {
			Input map[string]any `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if lastInput != nil {
			*lastInput = body.Input
		}
		resp := map[string]any{"result": result, "decision_id": "dec-1"}
		prov := map[string]any{"version": "0.70.0"}
		if revision != "" {
			prov["bundles"] = map[string]any{"aikonos": map[string]any{"revision": revision}}
		}
		resp["provenance"] = prov
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

func TestDecideToolCall_RecordsWhatOPAEvaluated(t *testing.T) {
	var sent map[string]any
	srv := fakeProvenanceOPA(t, map[string]any{"allow": false, "require_approval": true, "deny_reasons": []any{}}, "sha256:abc", &sent)
	defer srv.Close()
	eng, _ := NewEngine(context.Background(), Config{OPAEndpoint: srv.URL, PolicyBundle: "aikonos"})

	dec, err := eng.DecideToolCall(context.Background(), ToolQuery{ToolID: "web.fetch", EffectClass: planv1.EffectClass_NETWORK_EGRESS})
	if err != nil {
		t.Fatal(err)
	}
	ev := dec.Evidence
	if ev == nil || ev.Query != "aikonos/tool_invocation" || ev.Revision != "sha256:abc" || ev.OPAVersion != "0.70.0" || ev.DecisionID != "dec-1" {
		t.Fatalf("evidence = %+v", ev)
	}
	if want, _ := digestJSON(sent); ev.InputSHA256 != want {
		t.Fatalf("input digest %s does not match the input sent (%s)", ev.InputSHA256, want)
	}
	if got, _ := json.Marshal(ev.Input); string(got) != mustJSON(t, sent) {
		t.Fatalf("recorded input %s, sent %s", got, mustJSON(t, sent))
	}
	if ev.Result["require_approval"] != true || len(ev.Fields) == 0 {
		t.Fatalf("result %v, fields %v", ev.Result, ev.Fields)
	}
}

func TestDecide_FailsClosedWithoutTheBundle(t *testing.T) {
	srv := fakeProvenanceOPA(t, map[string]any{"allow": true}, "", nil)
	defer srv.Close()
	eng, _ := NewEngine(context.Background(), Config{OPAEndpoint: srv.URL, PolicyBundle: "aikonos"})

	if _, err := eng.DecideToolCall(context.Background(), ToolQuery{ToolID: "doc.read"}); !errors.Is(err, ErrPolicyNotLoaded) {
		t.Fatalf("tool call without the bundle active: err = %v, want ErrPolicyNotLoaded", err)
	}
	if _, err := eng.DecidePlan(context.Background(), &planv1.Plan{}, PlanContext{}); !errors.Is(err, ErrPolicyNotLoaded) {
		t.Fatalf("plan without the bundle active: err = %v, want ErrPolicyNotLoaded", err)
	}

	// Without a configured bundle, OPA loads its own policy: no revision, no error.
	plain, _ := NewEngine(context.Background(), Config{OPAEndpoint: srv.URL})
	dec, err := plain.DecideToolCall(context.Background(), ToolQuery{ToolID: "doc.read"})
	if err != nil || dec.Evidence.Revision != "" {
		t.Fatalf("unbundled decision = %+v, %v", dec, err)
	}
}

func TestDecidePlan_RecordsARedactedInput(t *testing.T) {
	srv := fakeProvenanceOPA(t, map[string]any{"allow": true, "violations": []any{}}, "sha256:abc", nil)
	defer srv.Close()
	eng, _ := NewEngine(context.Background(), Config{OPAEndpoint: srv.URL, PolicyBundle: "aikonos"})
	args, _ := structpb.NewStruct(map[string]any{"query": "diagnosis for jane"})
	pdec, err := eng.DecidePlan(context.Background(), &planv1.Plan{Steps: []*planv1.PlanStep{{Seq: 1, ToolId: "web.search", Args: args, Justification: "triage"}}}, PlanContext{})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(pdec.Evidence)
	if strings.Contains(string(raw), "jane") || strings.Contains(string(raw), "triage") {
		t.Fatalf("plan evidence carries raw arguments: %s", raw)
	}
	if len(pdec.Evidence.Redacted) == 0 || pdec.Evidence.InputSHA256 == "" {
		t.Fatalf("plan evidence must say what it redacted and commit to the original: %+v", pdec.Evidence)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
