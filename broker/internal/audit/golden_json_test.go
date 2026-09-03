package audit

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/adamdekan/aikonos/broker/internal/notify"
)

// TestGoldenJSON_CanonicalShape pins plain encoding/json (not protojson) as the
// canonical AuditEvent wire+storage shape — the format the emitter actually
// writes (emitter.go:206's marshal call) and the format the reader must
// decode. The bytes are captured off the emitter's real NATS-publish path (via
// a MemoryBus) rather than re-marshaled independently in the test, so a
// mutation of the emitter's marshal call (e.g. to protojson.Marshal) fails
// this test rather than going unnoticed.
func TestGoldenJSON_CanonicalShape(t *testing.T) {
	fixture := &auditv1.AuditEvent{
		EventId:        "01ARZ3NDEKTSV4RRFFQ69G5FAV",
		TraceId:        "trace-abc123",
		TenantId:       "tenant-1",
		OccurredAt:     timestamppb.New(t0),
		ActorSpiffeId:  "spiffe://aikonos.com/broker",
		ActorUserId:    "user-1",
		ActorEmail:     "user@example.com",
		EventType:      "aikonos.broker.task.created",
		ResourceRef:    "aikonos:task:abc",
		Decision:       auditv1.PolicyDecision_ALLOW,
		Context:        mustStruct(t, map[string]any{"tool_id": "web.fetch", "cost": 3}),
		PriorEventHash: "deadbeef",
		Signature:      "cafebabe",
	}
	// Emit mutates PriorEventHash/Signature/SigningKeyVersion (chain+sign);
	// take a copy for the round-trip assertion so we compare against what was
	// actually published.
	want := proto.Clone(fixture).(*auditv1.AuditEvent)

	// CP1.2: additive field. A fixed-version fake key source (empty key, so
	// the unsigned Signature behavior below is unchanged) proves
	// signing_key_version propagates through Emit into the golden JSON shape
	// and survives the round-trip, without coupling this test to Vault.
	e, err := NewEmitter(context.Background(), Config{SigningKeySource: fixedVersionSource{version: 7}})
	if err != nil {
		t.Fatalf("NewEmitter: %v", err)
	}
	// Seed the chain head so Emit doesn't overwrite prior_event_hash with the
	// empty first-event value — the fixture must exercise every string field,
	// including prior_event_hash, as non-empty.
	e.lastHash[fixture.TenantId] = "seed-hash"
	bus := notify.NewMemoryBus()
	e.AttachBus(bus)
	sub, err := bus.Subscribe(notify.AuditSubject(fixture.TenantId))
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Unsubscribe()

	if err := e.Emit(context.Background(), fixture); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	want.PriorEventHash = fixture.PriorEventHash
	want.Signature = fixture.Signature
	want.SigningKeyVersion = fixture.SigningKeyVersion

	var data []byte
	select {
	case data = <-sub.Events():
	default:
		t.Fatal("expected the emitter to publish the event to the bus")
	}
	s := string(data)

	// (a) snake_case field names, not protojson's lowerCamelCase.
	for _, field := range []string{
		`"event_id"`, `"trace_id"`, `"tenant_id"`, `"occurred_at"`,
		`"actor_spiffe_id"`, `"actor_user_id"`, `"actor_email"`,
		`"event_type"`, `"resource_ref"`, `"decision"`, `"context"`,
		`"prior_event_hash"`, `"signature"`, `"signing_key_version"`,
	} {
		if !strings.Contains(s, field) {
			t.Errorf("golden JSON missing snake_case field %s in: %s", field, s)
		}
	}
	// protojson would render these as camelCase — assert their absence too, so a
	// protojson mutation is caught even if a snake_case substring happened to
	// coincidentally match (e.g. via a JSON tag override).
	for _, camel := range []string{`"eventId"`, `"traceId"`, `"tenantId"`, `"actorUserId"`, `"eventType"`, `"resourceRef"`} {
		if strings.Contains(s, camel) {
			t.Errorf("golden JSON contains camelCase field %s — plain encoding/json should never render camelCase: %s", camel, s)
		}
	}

	// (b') signing_key_version renders as the plain number the emitter
	// recorded from the key source, proving CP1.2's field survives the wire
	// format unchanged (int32, not a nested/renamed shape).
	if !strings.Contains(s, `"signing_key_version":7`) {
		t.Errorf("want signing_key_version:7, got: %s", s)
	}

	// (b) decision renders as a number (proto enum), not protojson's string name.
	if !strings.Contains(s, `"decision":1`) {
		t.Errorf("want numeric decision rendering \"decision\":1 (ALLOW), got: %s", s)
	}
	if strings.Contains(s, `"decision":"ALLOW"`) {
		t.Errorf("decision rendered as protojson enum string, not the canonical number: %s", s)
	}

	// (c) occurred_at renders as a {seconds,nanos} object (proto struct field
	// tags), not protojson's RFC3339 string.
	if !strings.Contains(s, `"occurred_at":{"seconds":`) {
		t.Errorf("want occurred_at as a {seconds,nanos} object, got: %s", s)
	}

	// Sanity: reject accidental double-encoding / non-JSON output.
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		t.Fatalf("published payload is not a JSON object: %v", err)
	}

	// (d) round-trip through unmarshalAuditEvent yields a proto-equal event.
	got, err := unmarshalAuditEvent(data)
	if err != nil {
		t.Fatalf("unmarshalAuditEvent: %v", err)
	}
	if !proto.Equal(want, got) {
		t.Errorf("round-trip mismatch:\n  want=%v\n  got=%v", want, got)
	}
}

// fixedVersionSource is a minimal SigningKeySource that always signs with an
// empty key at a fixed version — used by the golden test to pin
// signing_key_version propagation without also exercising HMAC signing.
type fixedVersionSource struct{ version int32 }

func (f fixedVersionSource) Current() (string, int32) { return "", f.version }
func (f fixedVersionSource) ForVersion(_ context.Context, version int32) (string, error) {
	return "", nil
}

func mustStruct(t *testing.T, m map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatalf("structpb.NewStruct: %v", err)
	}
	return s
}
