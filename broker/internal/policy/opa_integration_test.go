package policy

// End-to-end check of decision replay against a real OPA: the broker serves
// policies/opa as a bundle, OPA loads it, decisions come back stamped with the
// revision, and scripts/replay-decision.sh re-runs them from an export.
//
// Runs only when AIKONOS_TEST_OPA names an opa binary (CI's broker-test job
// sets it; the version should match compose.yaml's OPA). Also needs bash and
// jq for the replay script.

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	planv1 "github.com/adamdekan/aikonos/gen/go/plan/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

type memArchive struct {
	mu    sync.Mutex
	snaps map[string][]byte
}

func (a *memArchive) ArchivePolicyBundle(_ context.Context, rev string, snap []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.snaps == nil {
		a.snaps = map[string][]byte{}
	}
	a.snaps[rev] = snap
	return nil
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestDecisionReplayAgainstRealOPA(t *testing.T) {
	opaBin := os.Getenv("AIKONOS_TEST_OPA")
	if opaBin == "" {
		t.Skip("AIKONOS_TEST_OPA not set; skipping the OPA integration test")
	}
	root := repoRoot(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The broker side: serve policies/opa as the "aikonos" bundle.
	archive := &memArchive{}
	srv := NewBundleServer(filepath.Join(root, "policies", "opa"), []string{"aikonos"}, archive, nil)
	if _, err := srv.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	bundle := srv.Current()
	bundlePort := freePort(t)
	mux := http.NewServeMux()
	mux.Handle("/opa/bundles/aikonos.tar.gz", srv)
	hs := &http.Server{Addr: fmt.Sprintf("127.0.0.1:%d", bundlePort), Handler: mux}
	go func() { _ = hs.ListenAndServe() }()
	defer hs.Close()

	// OPA, configured as in deploy/compose/opa.yaml but pointed at the test server.
	tmp := t.TempDir()
	cfg := fmt.Sprintf(`services:
  broker:
    url: http://127.0.0.1:%d
bundles:
  aikonos:
    service: broker
    resource: /opa/bundles/aikonos.tar.gz
    persist: false
    polling: {min_delay_seconds: 1, max_delay_seconds: 2}
decision_logs:
  console: true
`, bundlePort)
	cfgPath := filepath.Join(tmp, "opa.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	opaPort := freePort(t)
	opaURL := fmt.Sprintf("http://127.0.0.1:%d", opaPort)
	cmd := exec.CommandContext(ctx, opaBin, "run", "--server", "--addr", fmt.Sprintf("127.0.0.1:%d", opaPort), "--config-file", cfgPath, "--log-level", "error")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start opa: %v", err)
	}
	defer func() { cancel(); _ = cmd.Wait() }()

	// Wait for OPA to activate the bundle.
	deadline := time.Now().Add(30 * time.Second)
	for {
		resp, err := http.Get(opaURL + "/health?bundles")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("OPA did not activate the bundle within 30s")
		}
		time.Sleep(200 * time.Millisecond)
	}

	eng, err := NewEngine(ctx, Config{OPAEndpoint: opaURL, PolicyBundle: "aikonos"})
	if err != nil {
		t.Fatal(err)
	}

	// A routing decision carries the revision OPA evaluated with.
	dec, err := eng.DecideToolCall(ctx, ToolQuery{ToolID: "web.fetch", EffectClass: planv1.EffectClass_NETWORK_EGRESS,
		FGADecision: "allow", Now: time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatalf("DecideToolCall: %v", err)
	}
	if !dec.NeedsApproval {
		t.Fatalf("network_egress should need approval, got %+v", dec)
	}
	if dec.Evidence == nil || dec.Evidence.Revision != bundle.Revision {
		t.Fatalf("decision revision = %+v, want %s", dec.Evidence, bundle.Revision)
	}

	// A plan decision: arguments and justification are redacted in the
	// record, yet replay still reproduces the duplicate-step violation.
	args, _ := structpb.NewStruct(map[string]any{"path": "patients/jane-doe.pdf"})
	plan := &planv1.Plan{PlanId: "p1", Steps: []*planv1.PlanStep{
		{Seq: 1, ToolId: "doc.read", EffectClass: planv1.EffectClass_READ_ONLY, Args: args, Justification: "summarise the referral"},
		{Seq: 2, ToolId: "doc.read", EffectClass: planv1.EffectClass_READ_ONLY, Args: args, Justification: "again"},
	}}
	pdec, err := eng.DecidePlan(ctx, plan, PlanContext{CostBudget: 100})
	if err != nil {
		t.Fatalf("DecidePlan: %v", err)
	}
	if pdec.Allow || len(pdec.Violations) == 0 {
		t.Fatalf("identical steps should violate, got %+v", pdec)
	}
	recorded, _ := json.Marshal(pdec.Evidence.Input)
	if strings.Contains(string(recorded), "jane-doe") || strings.Contains(string(recorded), "referral") {
		t.Fatalf("plan record leaks raw arguments or justification: %s", recorded)
	}

	// OPA refuses REST writes under the bundle's root.
	req, _ := http.NewRequest(http.MethodPut, opaURL+"/v1/policies/evil", strings.NewReader("package aikonos.tool_invocation\nallow := true\n"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("OPA accepted a policy write under the bundle's root")
	}

	// Export both decisions as the GetDecisionEvidence RPC would, and replay.
	snap := archive.snaps[bundle.Revision]
	for name, ev := range map[string]*Evidence{"tool": dec.Evidence, "plan": pdec.Evidence} {
		path := writeEvidence(t, tmp, name, ev, snap)
		runReplay(t, root, opaBin, path, 0)

		// Tampering is caught: an edited policy file, and an edited result.
		runReplay(t, root, opaBin, writeEvidence(t, tmp, name+"-tampered-policy", ev, tamperSnapshot(t, snap)), 1)
		runReplay(t, root, opaBin, writeEvidence(t, tmp, name+"-tampered-result", flipResult(ev), snap), 1)
	}
}

// tamperSnapshot changes one policy file's content but keeps its recorded hash.
func tamperSnapshot(t *testing.T, snap []byte) []byte {
	t.Helper()
	var s BundleSnapshot
	if err := json.Unmarshal(snap, &s); err != nil {
		t.Fatal(err)
	}
	s.Files[0].Content += "\n# edited after archiving\n"
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func flipResult(ev *Evidence) *Evidence {
	c := *ev
	c.Result = map[string]any{}
	for k, v := range ev.Result {
		c.Result[k] = v
	}
	c.Result["allow"] = !(ev.Result["allow"] == true)
	return &c
}

// writeEvidence builds an export the way the broker does: the decision record
// becomes the event's context Struct, and the event is stored with
// encoding/json exactly as the audit emitter writes it.
func writeEvidence(t *testing.T, dir, name string, ev *Evidence, snap []byte) string {
	t.Helper()
	recJSON, _ := json.Marshal(map[string]any{"kind": "test", "outcome": map[string]any{"decision": "x", "decided_by": "opa:" + ev.Query}, "opa": ev})
	var rec map[string]any
	if err := json.Unmarshal(recJSON, &rec); err != nil {
		t.Fatal(err)
	}
	ctxStruct, err := structpb.NewStruct(rec)
	if err != nil {
		t.Fatal(err)
	}
	event, err := json.Marshal(&auditv1.AuditEvent{EventId: name, TenantId: "t", EventType: "aikonos.broker.policy.decision", Context: ctxStruct})
	if err != nil {
		t.Fatal(err)
	}
	doc := map[string]any{
		"kind":     "aikonos.decision-evidence/v1",
		"event":    json.RawMessage(event),
		"policies": []any{map[string]any{"revision": ev.Revision, "verified": true, "snapshot": json.RawMessage(snap)}},
	}
	b, _ := json.MarshalIndent(doc, "", "  ")
	return writeFile(t, dir, name+".json", string(b))
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func runReplay(t *testing.T, root, opaBin, evidence string, wantExit int) {
	t.Helper()
	cmd := exec.Command("bash", filepath.Join(root, "scripts", "replay-decision.sh"), evidence, "--opa", opaBin)
	out, err := cmd.CombinedOutput()
	got := 0
	if ee, ok := err.(*exec.ExitError); ok {
		got = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run replay: %v", err)
	}
	if got != wantExit {
		t.Fatalf("replay %s: exit %d, want %d\n%s", filepath.Base(evidence), got, wantExit, out)
	}
	if wantExit == 0 && !strings.Contains(string(out), "MATCH") {
		t.Fatalf("replay %s did not report a match:\n%s", filepath.Base(evidence), out)
	}
	t.Logf("replay %s (exit %d):\n%s", filepath.Base(evidence), got, out)
}
