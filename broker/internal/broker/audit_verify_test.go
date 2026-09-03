package broker

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// fakeVerifyReader implements audit.ReaderIface with a controllable VerifyChain.
type fakeVerifyReader struct {
	report           audit.ChainReport
	verifyCalled     bool
	verifyConfigured bool // whether VerifyChain returns ok=true
	verifyErr        error
}

func (f *fakeVerifyReader) Query(_ context.Context, _ string, _ audit.QueryFilter) ([]*auditv1.AuditEvent, string, bool, error) {
	return nil, "", true, nil
}

func (f *fakeVerifyReader) VerifyChain(_ context.Context, _ string) (audit.ChainReport, bool, error) {
	f.verifyCalled = true
	return f.report, f.verifyConfigured, f.verifyErr
}

func TestVerifyAuditChain_NonAdminDenied_ReaderNotCalled(t *testing.T) {
	fg := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := fg.server(t)
	defer srv.Close()

	reader := &fakeVerifyReader{verifyConfigured: true}
	d := testAdminDepsWithReader(t, srv.URL, reader)
	svc := NewBrokerService(d)

	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")
	_, err := svc.VerifyAuditChain(ctx, &brokerv1.VerifyAuditChainRequest{})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-admin should be denied, got %v", err)
	}
	if reader.verifyCalled {
		t.Error("reader must not be called when admin check fails")
	}
}

func TestVerifyAuditChain_AdminAllowed_CleanReport(t *testing.T) {
	fg := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := fg.server(t)
	defer srv.Close()

	reader := &fakeVerifyReader{
		verifyConfigured: true,
		report: audit.ChainReport{
			Total:  3,
			OK:     true,
			Signed: true,
			HeadID: "evt-0000",
			TailID: "evt-0002",
		},
	}
	d := testAdminDepsWithReader(t, srv.URL, reader)
	svc := NewBrokerService(d)

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	resp, err := svc.VerifyAuditChain(ctx, &brokerv1.VerifyAuditChainRequest{})
	if err != nil {
		t.Fatalf("admin verify failed: %v", err)
	}
	if !resp.Ok {
		t.Error("want ok=true for clean report")
	}
	if !resp.Signed {
		t.Error("want signed=true")
	}
	if resp.Total != 3 {
		t.Errorf("want total=3, got %d", resp.Total)
	}
	if resp.StoreConfigured != true {
		t.Error("want store_configured=true")
	}
	if resp.HeadEventId != "evt-0000" {
		t.Errorf("want head evt-0000, got %q", resp.HeadEventId)
	}
	if resp.TailEventId != "evt-0002" {
		t.Errorf("want tail evt-0002, got %q", resp.TailEventId)
	}
	if !reader.verifyCalled {
		t.Error("reader.VerifyChain must be called")
	}
}

func TestVerifyAuditChain_StoreReadError_ReturnsInternal(t *testing.T) {
	fg := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := fg.server(t)
	defer srv.Close()

	reader := &fakeVerifyReader{
		verifyConfigured: true,
		verifyErr:        errors.New("minio: connection refused"),
	}
	d := testAdminDepsWithReader(t, srv.URL, reader)
	svc := NewBrokerService(d)

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	_, err := svc.VerifyAuditChain(ctx, &brokerv1.VerifyAuditChainRequest{})
	if status.Code(err) != codes.Internal {
		t.Fatalf("want codes.Internal on store read error, got %v", err)
	}
}

func TestVerifyAuditChain_NilReader_StoreNotConfigured(t *testing.T) {
	fg := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := fg.server(t)
	defer srv.Close()

	// AuditReader == nil → MinIO not configured; handler must short-circuit
	// after the admin check and return store_configured=false with no error.
	d := testAdminDepsWithReader(t, srv.URL, nil)
	svc := NewBrokerService(d)

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	resp, err := svc.VerifyAuditChain(ctx, &brokerv1.VerifyAuditChainRequest{})
	if err != nil {
		t.Fatalf("nil reader must not error: %v", err)
	}
	if resp.StoreConfigured {
		t.Error("want store_configured=false when AuditReader is nil")
	}
}

func TestVerifyAuditChain_LoggingOnly_StoreNotConfigured(t *testing.T) {
	fg := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := fg.server(t)
	defer srv.Close()

	// ok=false from VerifyChain means store not configured.
	reader := &fakeVerifyReader{verifyConfigured: false}
	d := testAdminDepsWithReader(t, srv.URL, reader)
	svc := NewBrokerService(d)

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	resp, err := svc.VerifyAuditChain(ctx, &brokerv1.VerifyAuditChainRequest{})
	if err != nil {
		t.Fatalf("logging-only verify must not error: %v", err)
	}
	if resp.StoreConfigured {
		t.Error("want store_configured=false when store not configured")
	}
}
