package audit

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.uber.org/zap"

	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// makeEvent builds a minimal AuditEvent for testing.
func makeEvent(id, actor, eventType string, decision auditv1.PolicyDecision, ts time.Time) *auditv1.AuditEvent {
	return &auditv1.AuditEvent{
		EventId:     id,
		TenantId:    "t1",
		ActorUserId: actor,
		EventType:   eventType,
		Decision:    decision,
		OccurredAt:  timestamppb.New(ts),
	}
}

var (
	t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t1 = t0.Add(1 * time.Hour)
	t2 = t0.Add(2 * time.Hour)
	t3 = t0.Add(3 * time.Hour)
	t4 = t0.Add(4 * time.Hour)
)

// five events in ascending time order; their event_ids are UUIDv7-ish but for
// sorting purposes these are just lexicographically ascending strings.
var sampleEvents = []*auditv1.AuditEvent{
	makeEvent("aaa", "alice", "task.created", auditv1.PolicyDecision_ALLOW, t0),
	makeEvent("bbb", "alice", "task.approved", auditv1.PolicyDecision_ALLOW, t1),
	makeEvent("ccc", "bob", "plan.validated", auditv1.PolicyDecision_DENY, t2),
	makeEvent("ddd", "alice", "task.created", auditv1.PolicyDecision_ALLOW, t3),
	makeEvent("eee", "bob", "task.created", auditv1.PolicyDecision_ALLOW, t4),
}

func TestFilterAndPage_NoFilter_AllEventsNewestFirst(t *testing.T) {
	f := QueryFilter{Limit: 10}
	page, next := filterAndPage(sampleEvents, f)
	if len(page) != 5 {
		t.Fatalf("want 5 events, got %d", len(page))
	}
	// newest first: eee > ddd > ccc > bbb > aaa
	wantOrder := []string{"eee", "ddd", "ccc", "bbb", "aaa"}
	for i, e := range page {
		if e.EventId != wantOrder[i] {
			t.Errorf("pos %d: want %s got %s", i, wantOrder[i], e.EventId)
		}
	}
	if next != "" {
		t.Errorf("want empty next_cursor, got %q", next)
	}
}

func TestFilterAndPage_ActorFilter(t *testing.T) {
	f := QueryFilter{ActorUserID: "alice", Limit: 10}
	page, next := filterAndPage(sampleEvents, f)
	if len(page) != 3 {
		t.Fatalf("want 3 alice events, got %d", len(page))
	}
	for _, e := range page {
		if e.ActorUserId != "alice" {
			t.Errorf("actor filter leaked non-alice event: %s", e.EventId)
		}
	}
	if next != "" {
		t.Errorf("want empty next_cursor")
	}
}

func TestFilterAndPage_EventTypeFilter(t *testing.T) {
	f := QueryFilter{EventType: "task.created", Limit: 10}
	page, next := filterAndPage(sampleEvents, f)
	if len(page) != 3 {
		t.Fatalf("want 3 task.created events, got %d: %v", len(page), page)
	}
	_ = next
}

func TestFilterAndPage_DecisionFilter(t *testing.T) {
	f := QueryFilter{Decision: auditv1.PolicyDecision_DENY, Limit: 10}
	page, next := filterAndPage(sampleEvents, f)
	if len(page) != 1 {
		t.Fatalf("want 1 DENY event, got %d", len(page))
	}
	if page[0].EventId != "ccc" {
		t.Errorf("want ccc, got %s", page[0].EventId)
	}
	_ = next
}

func TestFilterAndPage_TimeFilter(t *testing.T) {
	// [t1, t3] inclusive: bbb(t1), ccc(t2), ddd(t3)
	f := QueryFilter{StartTime: t1, EndTime: t3, Limit: 10}
	page, next := filterAndPage(sampleEvents, f)
	if len(page) != 3 {
		t.Fatalf("want 3 events in [t1,t3], got %d", len(page))
	}
	ids := map[string]bool{"bbb": true, "ccc": true, "ddd": true}
	for _, e := range page {
		if !ids[e.EventId] {
			t.Errorf("unexpected event in time window: %s", e.EventId)
		}
	}
	_ = next
}

func TestFilterAndPage_Limit_SetsNextCursor(t *testing.T) {
	f := QueryFilter{Limit: 3}
	page, next := filterAndPage(sampleEvents, f)
	if len(page) != 3 {
		t.Fatalf("want 3 events (limit), got %d", len(page))
	}
	// newest-first: eee, ddd, ccc; next cursor is the last returned id
	if next == "" {
		t.Error("want non-empty next_cursor when more events remain")
	}
	if next != "ccc" {
		t.Errorf("want next_cursor=ccc (last returned), got %q", next)
	}
}

func TestFilterAndPage_Limit_EmptyNextCursorWhenExhausted(t *testing.T) {
	f := QueryFilter{Limit: 5}
	_, next := filterAndPage(sampleEvents, f)
	if next != "" {
		t.Errorf("want empty next_cursor when all events returned, got %q", next)
	}
}

func TestFilterAndPage_CursorContinuation_NoOverlap(t *testing.T) {
	// First page: limit 3, newest first → eee, ddd, ccc; cursor=ccc
	f1 := QueryFilter{Limit: 3}
	page1, cursor := filterAndPage(sampleEvents, f1)
	if len(page1) != 3 || cursor != "ccc" {
		t.Fatalf("setup: page1=%v cursor=%q", eventIDs(page1), cursor)
	}

	// Continuation: cursor=ccc → events with event_id < "ccc" → bbb, aaa
	f2 := QueryFilter{Limit: 3, Cursor: cursor}
	page2, next2 := filterAndPage(sampleEvents, f2)
	if len(page2) != 2 {
		t.Fatalf("want 2 remaining events, got %d: %v", len(page2), eventIDs(page2))
	}
	if next2 != "" {
		t.Errorf("want empty cursor on last page, got %q", next2)
	}
	// Verify no overlap: page2 IDs not in page1
	page1IDs := map[string]bool{}
	for _, e := range page1 {
		page1IDs[e.EventId] = true
	}
	for _, e := range page2 {
		if page1IDs[e.EventId] {
			t.Errorf("overlap: event %s appeared in both pages", e.EventId)
		}
	}
}

func TestFilterAndPage_ZeroLimit_DefaultsTo50(t *testing.T) {
	f := QueryFilter{Limit: 0}
	page, _ := filterAndPage(sampleEvents, f)
	// 5 events, all returned when limit 0 → default 50
	if len(page) != 5 {
		t.Fatalf("want 5 events with default limit, got %d", len(page))
	}
}

func TestNewReader_LoggingOnly_OkFalse(t *testing.T) {
	r := NewReader(Config{})
	if r.store != nil {
		t.Fatal("logging-only reader should have nil store")
	}
}

func eventIDs(es []*auditv1.AuditEvent) []string {
	ids := make([]string, len(es))
	for i, e := range es {
		ids[i] = e.EventId
	}
	return ids
}

// --- Reader.Query integration tests via fake objectStore ---

// fakeStore is an in-package fake objectStore for Reader tests.
type fakeStore struct {
	objects map[string][]byte // key → raw JSON
	listErr error
	getErrs map[string]error // key → forced get error
}

func (f *fakeStore) list(_ context.Context, prefix string) ([]objMeta, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []objMeta
	for k := range f.objects {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			out = append(out, objMeta{key: k})
		}
	}
	return out, nil
}

func (f *fakeStore) get(_ context.Context, key string) ([]byte, error) {
	if err, ok := f.getErrs[key]; ok {
		return nil, err
	}
	data, ok := f.objects[key]
	if !ok {
		return nil, errors.New("not found")
	}
	return data, nil
}

func mustMarshalEvent(e *auditv1.AuditEvent) []byte {
	b, err := json.Marshal(e)
	if err != nil {
		panic(err)
	}
	return b
}

func TestReader_Query_ListsAndDecodes(t *testing.T) {
	ev1 := makeEvent("aaa", "alice", "task.created", auditv1.PolicyDecision_ALLOW, t0)
	ev2 := makeEvent("bbb", "bob", "task.created", auditv1.PolicyDecision_DENY, t1)

	store := &fakeStore{
		objects: map[string][]byte{
			"tenant1/2026/01/01/aaa.json": mustMarshalEvent(ev1),
			"tenant1/2026/01/02/bbb.json": mustMarshalEvent(ev2),
		},
	}
	r := &Reader{store: store, cfg: Config{}, logger: nopLogger()}

	events, next, ok, err := r.Query(context.Background(), "tenant1", QueryFilter{Limit: 10})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("want ok=true with configured store")
	}
	if next != "" {
		t.Errorf("want empty next_cursor, got %q", next)
	}
	if len(events) != 2 {
		t.Fatalf("want 2 events, got %d", len(events))
	}
	ids := map[string]bool{}
	for _, e := range events {
		ids[e.EventId] = true
	}
	if !ids["aaa"] || !ids["bbb"] {
		t.Errorf("decoded event ids: %v", eventIDs(events))
	}
}

func TestReader_Query_GetError_SkipsObjectContinuesRest(t *testing.T) {
	ev1 := makeEvent("aaa", "alice", "task.created", auditv1.PolicyDecision_ALLOW, t0)
	ev3 := makeEvent("ccc", "alice", "task.created", auditv1.PolicyDecision_ALLOW, t2)

	store := &fakeStore{
		objects: map[string][]byte{
			"tenant1/aaa.json": mustMarshalEvent(ev1),
			"tenant1/bbb.json": mustMarshalEvent(makeEvent("bbb", "x", "x", auditv1.PolicyDecision_ALLOW, t1)),
			"tenant1/ccc.json": mustMarshalEvent(ev3),
		},
		getErrs: map[string]error{
			"tenant1/bbb.json": errors.New("simulated get failure"),
		},
	}
	r := &Reader{store: store, cfg: Config{}, logger: nopLogger()}

	events, _, ok, err := r.Query(context.Background(), "tenant1", QueryFilter{Limit: 10})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("want ok=true")
	}
	// bbb must be skipped; aaa and ccc must be returned.
	if len(events) != 2 {
		t.Fatalf("want 2 events (skip failed get), got %d: %v", len(events), eventIDs(events))
	}
	ids := map[string]bool{}
	for _, e := range events {
		ids[e.EventId] = true
	}
	if ids["bbb"] {
		t.Error("bbb should have been skipped due to get failure")
	}
	if !ids["aaa"] || !ids["ccc"] {
		t.Errorf("want aaa+ccc, got %v", eventIDs(events))
	}
}

func TestReader_Query_ListError_ReturnsErrorOkTrue(t *testing.T) {
	store := &fakeStore{
		listErr: errors.New("simulated list failure"),
		objects: map[string][]byte{},
	}
	r := &Reader{store: store, cfg: Config{}, logger: nopLogger()}

	_, _, ok, err := r.Query(context.Background(), "tenant1", QueryFilter{Limit: 10})
	if !ok {
		t.Error("want ok=true (store configured) even on list error")
	}
	if err == nil {
		t.Error("want non-nil error when list fails — caller must propagate, not treat as empty result")
	}
}

func TestReader_Query_TenantIsolation(t *testing.T) {
	ev1 := makeEvent("aaa", "alice", "task.created", auditv1.PolicyDecision_ALLOW, t0)
	ev2 := makeEvent("bbb", "bob", "task.created", auditv1.PolicyDecision_ALLOW, t1)

	store := &fakeStore{
		objects: map[string][]byte{
			"tenant1/aaa.json": mustMarshalEvent(ev1),
			"tenant2/bbb.json": mustMarshalEvent(ev2),
		},
	}
	r := &Reader{store: store, cfg: Config{}, logger: nopLogger()}

	events, _, _, _ := r.Query(context.Background(), "tenant1", QueryFilter{Limit: 10})
	if len(events) != 1 || events[0].EventId != "aaa" {
		t.Errorf("tenant isolation: want only aaa for tenant1, got %v", eventIDs(events))
	}
}

func TestReader_VerifyChain_ListError_ReturnsError(t *testing.T) {
	store := &fakeStore{
		listErr: errors.New("simulated list failure"),
		objects: map[string][]byte{},
	}
	r := &Reader{store: store, cfg: Config{}, logger: nopLogger()}

	_, ok, err := r.VerifyChain(context.Background(), "tenant1")
	if !ok {
		t.Error("want ok=true (store is configured)")
	}
	if err == nil {
		t.Error("want non-nil error when list fails, got nil — list failure would silently read as intact chain")
	}
}

func TestReader_VerifyChain_Unconfigured_OkFalseNoError(t *testing.T) {
	r := &Reader{store: nil, cfg: Config{}, logger: nopLogger()}
	_, ok, err := r.VerifyChain(context.Background(), "tenant1")
	if ok {
		t.Error("want ok=false for unconfigured reader")
	}
	if err != nil {
		t.Errorf("want nil error for unconfigured reader, got %v", err)
	}
}

func nopLogger() *zap.Logger { return zap.NewNop() }

// --- Reader.VerifyChain unsigned-chain metric (CP5.3) ---

// newAuditTestMeterReader installs an in-memory manual reader as the global
// MeterProvider and restores the previous provider on cleanup — mirrors
// broker/internal/metrics/task_test.go's newTestReader.
func newAuditTestMeterReader(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	prev := otel.GetMeterProvider()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	otel.SetMeterProvider(mp)
	t.Cleanup(func() { otel.SetMeterProvider(prev) })
	return reader
}

func collectAuditMetrics(t *testing.T, reader *sdkmetric.ManualReader) metricdata.ResourceMetrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	return rm
}

func findAuditMetric(rm metricdata.ResourceMetrics, name string) (metricdata.Metrics, bool) {
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				return m, true
			}
		}
	}
	return metricdata.Metrics{}, false
}

func storeFor(tenant string, events []*auditv1.AuditEvent) *fakeStore {
	store := &fakeStore{objects: map[string][]byte{}}
	for _, e := range events {
		store.objects[tenant+"/"+e.EventId+".json"] = mustMarshalEvent(e)
	}
	return store
}

func TestReader_VerifyChain_RecordsUnsignedMetric_OnAllUnsignedChain(t *testing.T) {
	reader := newAuditTestMeterReader(t)

	// No SigningKey/SigningKeySource configured — every event resolves to an
	// empty key, so the whole (non-empty) chain is unsigned.
	r := &Reader{store: storeFor("t1", sampleEvents), cfg: Config{}, logger: nopLogger()}

	report, ok, err := r.VerifyChain(context.Background(), "t1")
	if err != nil || !ok {
		t.Fatalf("VerifyChain failed: ok=%v err=%v", ok, err)
	}
	if report.Signed {
		t.Fatal("setup invariant broken: expected an unsigned chain")
	}

	rm := collectAuditMetrics(t, reader)
	m, found := findAuditMetric(rm, "audit_chain_unsigned_total")
	if !found {
		t.Fatal("audit_chain_unsigned_total not emitted for an all-unsigned chain — the unsigned-chain signal must fire beyond the boot log")
	}
	sum, ok := m.Data.(metricdata.Sum[int64])
	if !ok || len(sum.DataPoints) != 1 || sum.DataPoints[0].Value != 1 {
		t.Fatalf("unexpected data points for audit_chain_unsigned_total: %+v", m.Data)
	}
}

func TestReader_VerifyChain_NoUnsignedMetric_OnSignedChain(t *testing.T) {
	reader := newAuditTestMeterReader(t)

	// A configured SigningKey resolves non-empty for every event's (default,
	// version-0) SigningKeyVersion, so the chain is signed.
	r := &Reader{store: storeFor("t1", sampleEvents), cfg: Config{SigningKey: "test-signing-key"}, logger: nopLogger()}

	report, ok, err := r.VerifyChain(context.Background(), "t1")
	if err != nil || !ok {
		t.Fatalf("VerifyChain failed: ok=%v err=%v", ok, err)
	}
	if !report.Signed {
		t.Fatal("setup invariant broken: expected a signed chain")
	}

	rm := collectAuditMetrics(t, reader)
	if _, found := findAuditMetric(rm, "audit_chain_unsigned_total"); found {
		t.Fatal("audit_chain_unsigned_total unexpectedly emitted for a fully signed chain")
	}
}

func TestReader_VerifyChain_NoUnsignedMetric_OnPartiallySignedChain(t *testing.T) {
	reader := newAuditTestMeterReader(t)

	// Mixed chain: one event's signing_key_version resolves to a real key,
	// the rest resolve empty — mirrors the fixture in
	// TestVerifyChainVersioned_SignatureDowngradeRejected (cp1_2_test.go).
	// anySigned=true classifies the empty-key events as SignatureFailures
	// (verify.go's downgrade-rejection), not a benign legacy pass, so
	// report.Signed is true and the unsigned-chain counter must not fire.
	events := []*auditv1.AuditEvent{
		makeEvent("aaa", "alice", "task.created", auditv1.PolicyDecision_ALLOW, t0),
		makeEvent("bbb", "alice", "task.approved", auditv1.PolicyDecision_ALLOW, t1),
	}
	events[0].SigningKeyVersion = 1

	src := &fakeKeySource{byVersion: map[int32]string{0: "", 1: "test-signing-key"}}
	r := &Reader{store: storeFor("t1", events), cfg: Config{SigningKeySource: src}, logger: nopLogger()}

	report, ok, err := r.VerifyChain(context.Background(), "t1")
	if err != nil || !ok {
		t.Fatalf("VerifyChain failed: ok=%v err=%v", ok, err)
	}
	if !report.Signed {
		t.Fatal("setup invariant broken: expected anySigned=true (one event resolves a real key)")
	}
	if len(report.SignatureFailures) == 0 {
		t.Fatal("setup invariant broken: expected the version-0 events to be flagged as signature failures in a mixed chain")
	}

	rm := collectAuditMetrics(t, reader)
	if _, found := findAuditMetric(rm, "audit_chain_unsigned_total"); found {
		t.Fatal("audit_chain_unsigned_total unexpectedly emitted for a partially-signed chain")
	}
}
