package broker

// connectors_managed_test.go — tenant-managed OneDrive behavior on the three
// connector RPCs. New file rather than
// edits to connectors_test.go so the existing unconfigured-tenant coverage
// stays byte-for-byte unmodified.

import (
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/connector"
	"github.com/adamdekan/aikonos/broker/internal/connectorstore"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// managedTestDeps builds connector Deps with a Google+Microsoft-configured
// OAuth registry (so onedrive would otherwise be advertised) plus a
// fakeOBOBroker reporting the tenant as M365-managed.
func managedTestDeps(t *testing.T, cs *connectorstore.Store) Deps {
	t.Helper()
	deps := testConnectorDeps(t)
	if deps.OAuth.Credentials == nil {
		deps.OAuth.Credentials = map[connector.Provider]connector.AppCredentials{}
	}
	deps.OAuth.Credentials[connector.ProviderOneDrive] = connector.AppCredentials{ClientID: "m-id", ClientSecret: "m-secret"}
	deps.OBOBroker = &fakeOBOBroker{configured: true}
	if cs != nil {
		deps.Connectors = cs
	}
	return deps
}

func TestListConnectorProviders_OmitsOnedriveWhenTenantManaged(t *testing.T) {
	svc := NewBrokerService(managedTestDeps(t, nil))
	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")

	resp, err := svc.ListConnectorProviders(ctx, &brokerv1.ListConnectorProvidersRequest{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.Providers) != 1 || resp.Providers[0].Key != "google_drive" {
		t.Fatalf("want only google_drive when onedrive is tenant-managed, got %+v", resp.Providers)
	}
}

func TestListConnectorProviders_UnmanagedStillAdvertisesOnedrive(t *testing.T) {
	// Both providers configured, but OBOBroker reports the tenant unmanaged —
	// legacy per-user onedrive stays available.
	deps := testConnectorDeps(t)
	deps.OAuth.Credentials[connector.ProviderOneDrive] = connector.AppCredentials{ClientID: "m-id", ClientSecret: "m-secret"}
	deps.OBOBroker = &fakeOBOBroker{configured: false}
	svc := NewBrokerService(deps)
	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")

	resp, err := svc.ListConnectorProviders(ctx, &brokerv1.ListConnectorProvidersRequest{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.Providers) != 2 {
		t.Fatalf("want both providers when unmanaged, got %+v", resp.Providers)
	}
}

func TestBeginConnectorAuth_OneDriveManagedFailsPrecondition(t *testing.T) {
	svc := NewBrokerService(managedTestDeps(t, nil))
	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")

	_, err := svc.BeginConnectorAuth(ctx, &brokerv1.BeginConnectorAuthRequest{
		UserId:   "alice@example.com",
		Provider: brokerv1.ConnectorProvider_ONEDRIVE,
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("want FailedPrecondition for managed onedrive, got %v", err)
	}
}

func TestBeginConnectorAuth_GoogleDriveUnaffectedByOneDriveManaged(t *testing.T) {
	svc := NewBrokerService(managedTestDeps(t, nil))
	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")

	_, err := svc.BeginConnectorAuth(ctx, &brokerv1.BeginConnectorAuthRequest{
		UserId:   "alice@example.com",
		Provider: brokerv1.ConnectorProvider_GOOGLE_DRIVE,
	})
	if err != nil {
		t.Fatalf("google drive must be unaffected by onedrive being managed: %v", err)
	}
}

func TestListConnectors_ManagedFlagSetOnlyOnOnedriveEntry(t *testing.T) {
	cs := connectorstore.New(t.TempDir())
	if err := cs.Upsert(testTenantUUID, "alice@example.com", connectorstore.Entry{
		ConnectorID: connector.ProviderGoogleDrive.ConnectorID(), Provider: string(connector.ProviderGoogleDrive), Status: "connected",
	}); err != nil {
		t.Fatal(err)
	}
	if err := cs.Upsert(testTenantUUID, "alice@example.com", connectorstore.Entry{
		ConnectorID: connector.ProviderOneDrive.ConnectorID(), Provider: string(connector.ProviderOneDrive), Status: "connected",
	}); err != nil {
		t.Fatal(err)
	}
	svc := NewBrokerService(managedTestDeps(t, cs))
	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")

	resp, err := svc.ListConnectors(ctx, &brokerv1.ListConnectorsRequest{UserId: "alice@example.com"})
	if err != nil {
		t.Fatalf("ListConnectors: %v", err)
	}
	if len(resp.Connectors) != 2 {
		t.Fatalf("want both entries, got %+v", resp.Connectors)
	}
	for _, c := range resp.Connectors {
		wantManaged := c.Provider == brokerv1.ConnectorProvider_ONEDRIVE
		if c.Managed != wantManaged {
			t.Fatalf("connector %q: managed=%v, want %v", c.ConnectorId, c.Managed, wantManaged)
		}
	}
}

func TestListConnectors_UnconfiguredTenantNeverManaged(t *testing.T) {
	// No OBOBroker wired at all (the unconfigured-tenant precedent every other
	// connectors_test.go case exercises) — managed must always be false.
	cs := connectorstore.New(t.TempDir())
	if err := cs.Upsert(testTenantUUID, "alice@example.com", connectorstore.Entry{
		ConnectorID: connector.ProviderOneDrive.ConnectorID(), Provider: string(connector.ProviderOneDrive), Status: "connected",
	}); err != nil {
		t.Fatal(err)
	}
	deps := testConnectorDeps(t)
	deps.Connectors = cs
	svc := NewBrokerService(deps)
	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")

	resp, err := svc.ListConnectors(ctx, &brokerv1.ListConnectorsRequest{UserId: "alice@example.com"})
	if err != nil {
		t.Fatalf("ListConnectors: %v", err)
	}
	if len(resp.Connectors) != 1 || resp.Connectors[0].Managed {
		t.Fatalf("want managed=false for an unconfigured tenant, got %+v", resp.Connectors)
	}
}
