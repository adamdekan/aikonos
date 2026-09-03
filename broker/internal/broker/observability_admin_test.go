package broker

import (
	"testing"

	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Non-admins may not read observability info.
func TestGetObservabilityInfo_NonAdminDenied(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()

	deps := testAdminDepsWithConfig(t, srv.URL, newFakeConfigStore())
	deps.OtelEndpoint = "otel-collector:4317"
	svc := NewBrokerService(deps)

	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")
	_, err := svc.GetObservabilityInfo(ctx, &brokerv1.GetObservabilityInfoRequest{})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-admin GetObservabilityInfo: want PermissionDenied, got %v", err)
	}
}

// A configured endpoint is reported enabled, with the exported_job label.
func TestGetObservabilityInfo_EndpointSet(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()

	deps := testAdminDepsWithConfig(t, srv.URL, newFakeConfigStore())
	deps.OtelEndpoint = "otel-collector:4317"
	svc := NewBrokerService(deps)

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	resp, err := svc.GetObservabilityInfo(ctx, &brokerv1.GetObservabilityInfoRequest{})
	if err != nil {
		t.Fatalf("admin GetObservabilityInfo: %v", err)
	}
	if resp.OtelEndpoint != "otel-collector:4317" {
		t.Errorf("OtelEndpoint = %q, want otel-collector:4317", resp.OtelEndpoint)
	}
	if !resp.ExportEnabled {
		t.Error("ExportEnabled = false, want true when endpoint set")
	}
	if resp.ExportedJob != "aikonos-broker" {
		t.Errorf("ExportedJob = %q, want aikonos-broker", resp.ExportedJob)
	}
}

// An empty endpoint reports export disabled (the default deploy state).
func TestGetObservabilityInfo_EndpointEmpty(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()

	deps := testAdminDepsWithConfig(t, srv.URL, newFakeConfigStore())
	// OtelEndpoint left empty.
	svc := NewBrokerService(deps)

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	resp, err := svc.GetObservabilityInfo(ctx, &brokerv1.GetObservabilityInfoRequest{})
	if err != nil {
		t.Fatalf("admin GetObservabilityInfo: %v", err)
	}
	if resp.ExportEnabled {
		t.Error("ExportEnabled = true, want false when endpoint empty")
	}
}
