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

// erroringAuditReader is a ReaderIface whose Query always returns an error.
type erroringAuditReader struct{}

func (e *erroringAuditReader) Query(_ context.Context, _ string, _ audit.QueryFilter) ([]*auditv1.AuditEvent, string, bool, error) {
	return nil, "", true, errors.New("minio: connection refused")
}

func (e *erroringAuditReader) VerifyChain(_ context.Context, _ string) (audit.ChainReport, bool, error) {
	return audit.ChainReport{}, true, nil
}

func TestQueryAudit_StoreListError_ReturnsInternal(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()

	reader := &erroringAuditReader{}
	svc := NewBrokerService(testAdminDepsWithReader(t, srv.URL, reader))

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	_, err := svc.QueryAudit(ctx, &brokerv1.QueryAuditRequest{})
	if status.Code(err) != codes.Internal {
		t.Fatalf("want codes.Internal on Query list error, got %v", err)
	}
}
