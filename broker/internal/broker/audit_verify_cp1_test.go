package broker

import (
	"context"
	"testing"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// truncatedVerifyReader returns a ChainReport with Truncated=true and Skipped=2.
type truncatedVerifyReader struct{}

func (r *truncatedVerifyReader) Query(_ context.Context, _ string, _ audit.QueryFilter) ([]*auditv1.AuditEvent, string, bool, error) {
	return nil, "", true, nil
}

func (r *truncatedVerifyReader) VerifyChain(_ context.Context, _ string) (audit.ChainReport, bool, error) {
	return audit.ChainReport{
		Total:     5000,
		OK:        true,
		Signed:    false,
		Truncated: true,
		Skipped:   2,
	}, true, nil
}

func TestVerifyAuditChain_Truncated_Skipped_Mapped(t *testing.T) {
	fg := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := fg.server(t)
	defer srv.Close()

	d := testAdminDepsWithReader(t, srv.URL, &truncatedVerifyReader{})
	svc := NewBrokerService(d)

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	resp, err := svc.VerifyAuditChain(ctx, &brokerv1.VerifyAuditChainRequest{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !resp.Ok {
		t.Error("want ok=true (scanned subset was clean)")
	}
	if !resp.Truncated {
		t.Error("want Truncated=true in response")
	}
	if resp.Skipped != 2 {
		t.Errorf("want Skipped=2, got %d", resp.Skipped)
	}
}
