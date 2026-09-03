package broker

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// fakeAuditReader implements audit.ReaderIface for tests.
type fakeAuditReader struct {
	events     []*auditv1.AuditEvent
	nextCursor string
	ok         bool
	called     bool
}

func (f *fakeAuditReader) Query(_ context.Context, _ string, _ audit.QueryFilter) ([]*auditv1.AuditEvent, string, bool, error) {
	f.called = true
	return f.events, f.nextCursor, f.ok, nil
}

func (f *fakeAuditReader) VerifyChain(_ context.Context, _ string) (audit.ChainReport, bool, error) {
	return audit.ChainReport{}, f.ok, nil
}

func testAdminDepsWithReader(t *testing.T, fgaURL string, reader audit.ReaderIface) Deps {
	t.Helper()
	d := testAdminDeps(t, fgaURL)
	d.AuditReader = reader
	return d
}

func sampleAuditEvents() []*auditv1.AuditEvent {
	return []*auditv1.AuditEvent{
		{EventId: "evt-2", TenantId: testTenantUUID, EventType: "task.created",
			ActorUserId: "alice@example.com", Decision: auditv1.PolicyDecision_ALLOW},
		{EventId: "evt-1", TenantId: testTenantUUID, EventType: "task.created",
			ActorUserId: "alice@example.com", Decision: auditv1.PolicyDecision_ALLOW},
	}
}

func TestQueryAudit_NonAdminDenied_ReaderNotCalled(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	reader := &fakeAuditReader{events: sampleAuditEvents(), ok: true}
	svc := NewBrokerService(testAdminDepsWithReader(t, srv.URL, reader))

	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")
	_, err := svc.QueryAudit(ctx, &brokerv1.QueryAuditRequest{})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-admin should be denied, got %v", err)
	}
	if reader.called {
		t.Error("reader must not be called when admin check fails")
	}
}

func TestQueryAudit_AdminAllowed_EventsMapped(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	events := sampleAuditEvents()
	reader := &fakeAuditReader{events: events, nextCursor: "evt-1", ok: true}
	svc := NewBrokerService(testAdminDepsWithReader(t, srv.URL, reader))

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	resp, err := svc.QueryAudit(ctx, &brokerv1.QueryAuditRequest{})
	if err != nil {
		t.Fatalf("admin query failed: %v", err)
	}
	if !resp.StoreConfigured {
		t.Error("want store_configured=true when ok=true")
	}
	if len(resp.Events) != 2 {
		t.Fatalf("want 2 events, got %d", len(resp.Events))
	}
	if resp.Events[0].EventId != "evt-2" || resp.Events[1].EventId != "evt-1" {
		t.Errorf("events not mapped correctly: %v", resp.Events)
	}
	if resp.NextCursor != "evt-1" {
		t.Errorf("want next_cursor=evt-1, got %q", resp.NextCursor)
	}
}

func TestQueryAudit_LoggingOnly_EmptyResult_NoError(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	// ok=false means store not configured; the fake IS still invoked.
	reader := &fakeAuditReader{events: nil, ok: false}
	svc := NewBrokerService(testAdminDepsWithReader(t, srv.URL, reader))

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	resp, err := svc.QueryAudit(ctx, &brokerv1.QueryAuditRequest{})
	if err != nil {
		t.Fatalf("logging-only query must not error: %v", err)
	}
	if !reader.called {
		t.Error("reader must be called even when ok=false")
	}
	if resp.StoreConfigured {
		t.Error("want store_configured=false when ok=false")
	}
	if len(resp.Events) != 0 {
		t.Errorf("want empty events, got %d", len(resp.Events))
	}
}

func TestQueryAudit_NilReader_EmptyResult_NoError(t *testing.T) {
	// Nil reader (unconfigured) must be fail-safe.
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	svc := NewBrokerService(testAdminDepsWithReader(t, srv.URL, nil))

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	resp, err := svc.QueryAudit(ctx, &brokerv1.QueryAuditRequest{})
	if err != nil {
		t.Fatalf("nil reader must not error: %v", err)
	}
	if resp.StoreConfigured {
		t.Error("want store_configured=false with nil reader")
	}
	if len(resp.Events) != 0 {
		t.Errorf("want empty events with nil reader, got %d", len(resp.Events))
	}
}

func TestQueryAudit_FiltersForwarded(t *testing.T) {
	// Verify that the request fields are translated into a QueryFilter and
	// forwarded to the reader. Use a capturing fake.
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()

	var capturedFilter audit.QueryFilter
	capturingReader := &capturingAuditReader{
		fn: func(_ context.Context, _ string, fil audit.QueryFilter) ([]*auditv1.AuditEvent, string, bool, error) {
			capturedFilter = fil
			return nil, "", true, nil
		},
	}
	svc := NewBrokerService(testAdminDepsWithReader(t, srv.URL, capturingReader))

	wantActor := "alice@example.com"
	wantType := "task.created"
	wantDecision := auditv1.PolicyDecision_DENY
	wantLimit := int32(25)
	wantCursor := "some-cursor"
	wantStart := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	wantEnd := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	_, _ = svc.QueryAudit(ctx, &brokerv1.QueryAuditRequest{
		ActorUserId: wantActor,
		EventType:   wantType,
		Decision:    wantDecision,
		Limit:       wantLimit,
		Cursor:      wantCursor,
		StartTime:   timestamppb.New(wantStart),
		EndTime:     timestamppb.New(wantEnd),
	})

	if capturedFilter.ActorUserID != wantActor {
		t.Errorf("actor: want %q got %q", wantActor, capturedFilter.ActorUserID)
	}
	if capturedFilter.EventType != wantType {
		t.Errorf("event_type: want %q got %q", wantType, capturedFilter.EventType)
	}
	if capturedFilter.Decision != wantDecision {
		t.Errorf("decision: want %v got %v", wantDecision, capturedFilter.Decision)
	}
	if capturedFilter.Limit != int(wantLimit) {
		t.Errorf("limit: want %d got %d", wantLimit, capturedFilter.Limit)
	}
	if capturedFilter.Cursor != wantCursor {
		t.Errorf("cursor: want %q got %q", wantCursor, capturedFilter.Cursor)
	}
	if !capturedFilter.StartTime.Equal(wantStart) {
		t.Errorf("start_time: want %v got %v", wantStart, capturedFilter.StartTime)
	}
	if !capturedFilter.EndTime.Equal(wantEnd) {
		t.Errorf("end_time: want %v got %v", wantEnd, capturedFilter.EndTime)
	}
}

// capturingAuditReader lets tests inspect what filter was forwarded.
type capturingAuditReader struct {
	fn func(context.Context, string, audit.QueryFilter) ([]*auditv1.AuditEvent, string, bool, error)
}

func (c *capturingAuditReader) Query(ctx context.Context, tenant string, f audit.QueryFilter) ([]*auditv1.AuditEvent, string, bool, error) {
	return c.fn(ctx, tenant, f)
}

func (c *capturingAuditReader) VerifyChain(_ context.Context, _ string) (audit.ChainReport, bool, error) {
	return audit.ChainReport{}, true, nil
}
