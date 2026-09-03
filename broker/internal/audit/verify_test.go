package audit

import (
	"context"
	"testing"

	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
)

// buildChain emits n events through a real Emitter (logging-only) so
// PriorEventHash/Signature are populated exactly as in production.
func buildChain(t *testing.T, n int, signingKey string) ([]*auditv1.AuditEvent, *Emitter) {
	t.Helper()
	e, err := NewEmitter(context.Background(), Config{SigningKey: signingKey})
	if err != nil {
		t.Fatalf("NewEmitter: %v", err)
	}
	events := make([]*auditv1.AuditEvent, n)
	for i := 0; i < n; i++ {
		events[i] = &auditv1.AuditEvent{
			EventId:   ids_for_test(i),
			TenantId:  "tenant1",
			EventType: "test.event",
		}
		if err := e.Emit(context.Background(), events[i]); err != nil {
			t.Fatalf("Emit[%d]: %v", i, err)
		}
	}
	return events, e
}

// ids_for_test generates a sortable id for position i. We want the ids to sort
// ascending in string order so VerifyChain's sort produces the emission order.
func ids_for_test(i int) string {
	// Zero-pad to 4 digits so lexical sort == numeric sort.
	return "evt-" + string([]byte{
		byte('0' + (i/1000)%10),
		byte('0' + (i/100)%10),
		byte('0' + (i/10)%10),
		byte('0' + i%10),
	})
}

func TestVerifyChain_IntactSigned(t *testing.T) {
	events, _ := buildChain(t, 4, "test-key")
	report := VerifyChain(events, "test-key")

	if !report.OK {
		t.Errorf("intact chain: want ok=true, got false; breaks=%v sig_failures=%v", report.Breaks, report.SignatureFailures)
	}
	if !report.Signed {
		t.Error("want signed=true when signing key is set")
	}
	if report.Total != 4 {
		t.Errorf("want total=4, got %d", report.Total)
	}
	if len(report.Breaks) != 0 {
		t.Errorf("want 0 breaks, got %d: %v", len(report.Breaks), report.Breaks)
	}
	if len(report.SignatureFailures) != 0 {
		t.Errorf("want 0 sig failures, got %d: %v", len(report.SignatureFailures), report.SignatureFailures)
	}
	if report.HeadID != "evt-0000" {
		t.Errorf("want head=evt-0000, got %q", report.HeadID)
	}
	if report.TailID != "evt-0003" {
		t.Errorf("want tail=evt-0003, got %q", report.TailID)
	}
}

func TestVerifyChain_MutatedEventType_BreakAtNext(t *testing.T) {
	// Emit 3 events, then mutate events[1].EventType post-emission.
	// events[2].PriorEventHash was computed over the original events[1], so it
	// will no longer match → continuity break at events[2].
	events, _ := buildChain(t, 3, "test-key")
	events[1].EventType = "tampered.event"

	report := VerifyChain(events, "test-key")

	if report.OK {
		t.Error("mutated event: want ok=false")
	}
	if len(report.Breaks) == 0 {
		t.Error("want at least one continuity break")
	}
}

func TestVerifyChain_DroppedMiddleEvent_ContinuityBreak(t *testing.T) {
	events, _ := buildChain(t, 4, "")
	// Drop events[1]; events[2].PriorEventHash points at events[1]'s hash, not
	// events[0]'s hash, so there will be a continuity break at events[2].
	reduced := []*auditv1.AuditEvent{events[0], events[2], events[3]}

	report := VerifyChain(reduced, "")

	if report.OK {
		t.Error("dropped event: want ok=false")
	}
	if len(report.Breaks) == 0 {
		t.Error("want at least one continuity break for the gap")
	}
}

func TestVerifyChain_TamperedSignature_SignatureFailure(t *testing.T) {
	events, _ := buildChain(t, 3, "test-key")
	// Tamper the signature on the middle event.
	events[1].Signature = "deadbeef"

	report := VerifyChain(events, "test-key")

	if report.OK {
		t.Error("tampered signature: want ok=false")
	}
	if len(report.SignatureFailures) != 1 {
		t.Errorf("want 1 signature failure, got %d: %v", len(report.SignatureFailures), report.SignatureFailures)
	}
	if report.SignatureFailures[0].EventID != "evt-0001" {
		t.Errorf("want failure at evt-0001, got %q", report.SignatureFailures[0].EventID)
	}
}

func TestVerifyChain_UnsignedChain(t *testing.T) {
	// No signing key at emission or at verify time.
	events, _ := buildChain(t, 3, "")

	report := VerifyChain(events, "")

	if report.Signed {
		t.Error("want signed=false for unsigned chain")
	}
	if !report.OK {
		t.Errorf("unsigned intact chain: want ok=true; breaks=%v", report.Breaks)
	}
	if len(report.Breaks) != 0 {
		t.Errorf("want 0 breaks, got %d", len(report.Breaks))
	}
}
