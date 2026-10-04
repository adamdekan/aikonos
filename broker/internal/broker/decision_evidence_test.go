package broker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/policy"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// fakeEvidenceReader is a ReaderIface that also serves single events and
// archived policy snapshots.
type fakeEvidenceReader struct {
	fakeAuditReader
	events    map[string]*auditv1.AuditEvent
	snapshots map[string][]byte
}

func (f *fakeEvidenceReader) GetEvent(_ context.Context, tenant, id string) (*auditv1.AuditEvent, []byte, bool, error) {
	ev, ok := f.events[id]
	if !ok || ev.TenantId != tenant {
		return nil, nil, true, audit.ErrNotFound
	}
	raw, _ := json.Marshal(ev)
	return ev, raw, true, nil
}

func (f *fakeEvidenceReader) GetPolicyBundle(_ context.Context, rev string) ([]byte, bool, error) {
	if s, ok := f.snapshots[rev]; ok {
		return s, true, nil
	}
	return nil, true, audit.ErrNotFound
}

func (f *fakeEvidenceReader) VerifyEventSignature(context.Context, *auditv1.AuditEvent) audit.SignatureStatus {
	return audit.SignatureValid
}

func testBundle(t *testing.T) *policy.Bundle {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tool_invocation.rego"), []byte("package aikonos.tool_invocation\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := policy.LoadBundle(dir, []string{"aikonos"})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func decisionEvent(t *testing.T, revision string) *auditv1.AuditEvent {
	t.Helper()
	ctx, err := structpb.NewStruct(map[string]any{
		"kind": decisionKindToolInvocation,
		"opa":  map[string]any{"query": "aikonos/tool_invocation", "revision": revision, "input": map[string]any{}, "result": map[string]any{"allow": true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &auditv1.AuditEvent{EventId: uuid.NewString(), TenantId: testTenantUUID, EventType: PolicyDecisionEvent, Context: ctx}
}

func evidenceSvc(t *testing.T, reader audit.ReaderIface) *BrokerService {
	t.Helper()
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	t.Cleanup(srv.Close)
	return NewBrokerService(testAdminDepsWithReader(t, srv.URL, reader))
}

func TestGetDecisionEvidence_ExportsTheEventAndItsVerifiedPolicy(t *testing.T) {
	b := testBundle(t)
	snap, _ := b.Snapshot()
	ev := decisionEvent(t, b.Revision)
	reader := &fakeEvidenceReader{events: map[string]*auditv1.AuditEvent{ev.EventId: ev}, snapshots: map[string][]byte{b.Revision: snap}}
	svc := evidenceSvc(t, reader)

	resp, err := svc.GetDecisionEvidence(ctxWithIdentity(testTenantUUID, "admin@example.com"), &brokerv1.GetDecisionEvidenceRequest{EventId: ev.EventId})
	if err != nil {
		t.Fatalf("GetDecisionEvidence: %v", err)
	}
	var doc struct {
		Kind      string          `json:"kind"`
		Event     json.RawMessage `json:"event"`
		EventHash string          `json:"event_hash"`
		Signature string          `json:"signature"`
		Policies  []struct {
			Revision string          `json:"revision"`
			Verified bool            `json:"verified"`
			Snapshot json.RawMessage `json:"snapshot"`
		} `json:"policies"`
	}
	if err := json.Unmarshal(resp.EvidenceJson, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Kind != EvidenceKind || doc.Signature != "valid" || doc.EventHash != audit.EventHash(ev) {
		t.Fatalf("evidence header: %+v", doc)
	}
	if len(doc.Policies) != 1 || doc.Policies[0].Revision != b.Revision || !doc.Policies[0].Verified {
		t.Fatalf("policies: %+v", doc.Policies)
	}
	if rev, err := policy.VerifySnapshot(doc.Policies[0].Snapshot); err != nil || rev != b.Revision {
		t.Fatalf("exported snapshot does not verify: %s %v", rev, err)
	}
}

func TestGetDecisionEvidence_FlagsATamperedSnapshot(t *testing.T) {
	b := testBundle(t)
	var s policy.BundleSnapshot
	raw, _ := b.Snapshot()
	_ = json.Unmarshal(raw, &s)
	s.Files[0].Content += "allow := true\n"
	tampered, _ := json.Marshal(s)
	ev := decisionEvent(t, b.Revision)
	svc := evidenceSvc(t, &fakeEvidenceReader{events: map[string]*auditv1.AuditEvent{ev.EventId: ev}, snapshots: map[string][]byte{b.Revision: tampered}})

	resp, err := svc.GetDecisionEvidence(ctxWithIdentity(testTenantUUID, "admin@example.com"), &brokerv1.GetDecisionEvidenceRequest{EventId: ev.EventId})
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Policies []struct {
			Verified bool   `json:"verified"`
			Error    string `json:"error"`
		} `json:"policies"`
	}
	_ = json.Unmarshal(resp.EvidenceJson, &doc)
	if len(doc.Policies) != 1 || doc.Policies[0].Verified || doc.Policies[0].Error == "" {
		t.Fatalf("a tampered snapshot must be flagged: %+v", doc.Policies)
	}
}

func TestGetDecisionEvidence_Refusals(t *testing.T) {
	ev := decisionEvent(t, "")
	reader := &fakeEvidenceReader{events: map[string]*auditv1.AuditEvent{ev.EventId: ev}}
	svc := evidenceSvc(t, reader)
	admin := ctxWithIdentity(testTenantUUID, "admin@example.com")

	if _, err := svc.GetDecisionEvidence(ctxWithIdentity(testTenantUUID, "alice@example.com"), &brokerv1.GetDecisionEvidenceRequest{EventId: ev.EventId}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("non-admin: %v", err)
	}
	if _, err := svc.GetDecisionEvidence(admin, &brokerv1.GetDecisionEvidenceRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("no event id: %v", err)
	}
	if _, err := svc.GetDecisionEvidence(admin, &brokerv1.GetDecisionEvidenceRequest{EventId: uuid.NewString()}); status.Code(err) != codes.NotFound {
		t.Errorf("unknown event: %v", err)
	}
	// Another tenant's admin cannot read this tenant's event.
	other := ctxWithIdentity("223e4567-e89b-12d3-a456-426614174000", "admin@example.com")
	if _, err := svc.GetDecisionEvidence(other, &brokerv1.GetDecisionEvidenceRequest{EventId: ev.EventId}); status.Code(err) != codes.NotFound && status.Code(err) != codes.PermissionDenied {
		t.Errorf("cross-tenant read: %v", err)
	}
	// A reader without single-event support (or no store) cannot export.
	plain := evidenceSvc(t, &fakeAuditReader{ok: true})
	if _, err := plain.GetDecisionEvidence(admin, &brokerv1.GetDecisionEvidenceRequest{EventId: ev.EventId}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("reader without evidence support: %v", err)
	}
}
