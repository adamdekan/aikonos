package broker

import (
	"context"
	"fmt"
	"net/url"
	"testing"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/auth"
	"github.com/adamdekan/aikonos/broker/internal/connector"
	"github.com/adamdekan/aikonos/broker/internal/connectorstore"
	"github.com/adamdekan/aikonos/broker/internal/secrets"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// stubSecrets satisfies secrets.Provider without Vault (Begin never touches it).
type stubSecrets struct{}

func (stubSecrets) ReadConnector(context.Context, string, string, string) (*secrets.ConnectorToken, error) {
	return nil, secrets.ErrNotFound
}
func (stubSecrets) WriteConnector(context.Context, string, string, string, *secrets.ConnectorToken) error {
	return nil
}
func (stubSecrets) DeleteConnector(context.Context, string, string, string) error { return nil }
func (stubSecrets) ProviderKeyPath(tenant, providerID string) (string, error) {
	return secrets.ProviderKeyPath(tenant, providerID)
}
func (stubSecrets) ReadProviderKey(context.Context, string, string) (string, bool, error) {
	return "", false, nil
}
func (stubSecrets) WriteProviderKey(context.Context, string, string, string) error { return nil }
func (stubSecrets) DeleteProviderKey(context.Context, string, string) error        { return nil }
func (stubSecrets) ReadM365App(context.Context, string) (string, bool, error)      { return "", false, nil }
func (stubSecrets) WriteM365App(context.Context, string, string) error             { return nil }
func (stubSecrets) DeleteM365App(context.Context, string) error                    { return nil }

func testConnectorDeps(t *testing.T) Deps {
	t.Helper()
	em, err := audit.NewEmitter(context.Background(), audit.Config{})
	if err != nil {
		t.Fatal(err)
	}
	return Deps{
		Logger:  zap.NewNop(),
		Audit:   em,
		Secrets: stubSecrets{},
		OAuth: &connector.Registry{
			RedirectURI: "http://localhost:3000/connect/callback",
			Credentials: map[connector.Provider]connector.AppCredentials{
				connector.ProviderGoogleDrive: {ClientID: "g-id", ClientSecret: "g-secret"},
			},
		},
		ConnAuth:   connector.NewStateStore(connectorStateTTL),
		Connectors: connectorstore.New(t.TempDir()),
	}
}

const testTenantUUID = "123e4567-e89b-12d3-a456-426614174000"

func ctxWithIdentity(tenant, user string) context.Context {
	return auth.WithIdentity(context.Background(), &auth.Identity{TenantID: tenant, Email: user})
}

func TestBeginConnectorAuth_DisabledFailsClosed(t *testing.T) {
	svc := NewBrokerService(Deps{Logger: zap.NewNop()})
	_, err := svc.BeginConnectorAuth(context.Background(), &brokerv1.BeginConnectorAuthRequest{
		Provider: brokerv1.ConnectorProvider_GOOGLE_DRIVE,
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("want FailedPrecondition, got %v", err)
	}
}

func TestBeginConnectorAuth_HappyPath(t *testing.T) {
	svc := NewBrokerService(testConnectorDeps(t))
	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")

	resp, err := svc.BeginConnectorAuth(ctx, &brokerv1.BeginConnectorAuthRequest{
		UserId:   "alice@example.com",
		Provider: brokerv1.ConnectorProvider_GOOGLE_DRIVE,
	})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if resp.State == "" {
		t.Fatal("expected non-empty state")
	}
	u, err := url.Parse(resp.AuthorizeUrl)
	if err != nil {
		t.Fatal(err)
	}
	if u.Query().Get("code_challenge") == "" {
		t.Fatal("authorize url missing PKCE challenge")
	}
	// The state must be recoverable (single-use) from the store with the verifier.
	pending, ok, err := svc.deps.ConnAuth.Take(ctx, testTenantUUID, resp.State)
	if err != nil || !ok || pending.Verifier == "" || pending.User != "alice@example.com" {
		t.Fatalf("state not stored correctly: %+v ok=%v err=%v", pending, ok, err)
	}
}

func TestBeginConnectorAuth_UnconfiguredProvider(t *testing.T) {
	svc := NewBrokerService(testConnectorDeps(t)) // only Google configured
	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")
	_, err := svc.BeginConnectorAuth(ctx, &brokerv1.BeginConnectorAuthRequest{
		UserId:   "alice@example.com",
		Provider: brokerv1.ConnectorProvider_ONEDRIVE,
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("want FailedPrecondition for unconfigured onedrive, got %v", err)
	}
}

func TestListConnectorProviders_DisabledReturnsEmpty(t *testing.T) {
	svc := NewBrokerService(Deps{Logger: zap.NewNop()})
	resp, err := svc.ListConnectorProviders(context.Background(), &brokerv1.ListConnectorProvidersRequest{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.Providers) != 0 {
		t.Fatalf("disabled connectors must advertise nothing, got %d", len(resp.Providers))
	}
}

// Only providers with OAuth creds are advertised — the UI must never offer a
// provider that would fail closed on BeginConnectorAuth. testConnectorDeps wires
// Google creds only, so OneDrive must be absent.
func TestListConnectorProviders_OnlyConfiguredAdvertised(t *testing.T) {
	svc := NewBrokerService(testConnectorDeps(t))
	resp, err := svc.ListConnectorProviders(context.Background(), &brokerv1.ListConnectorProvidersRequest{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.Providers) != 1 {
		t.Fatalf("want only the configured provider, got %d: %+v", len(resp.Providers), resp.Providers)
	}
	p := resp.Providers[0]
	if p.Provider != brokerv1.ConnectorProvider_GOOGLE_DRIVE || p.Key != "google_drive" ||
		p.DisplayName != "Google Drive" || p.ConnectorId != "gdrive" {
		t.Fatalf("unexpected provider metadata: %+v", p)
	}
}

// Setting Microsoft creds makes OneDrive appear; result is sorted by key for
// deterministic output (google_drive < onedrive).
func TestListConnectorProviders_BothConfiguredSorted(t *testing.T) {
	deps := testConnectorDeps(t)
	if deps.OAuth.Credentials == nil {
		deps.OAuth.Credentials = map[connector.Provider]connector.AppCredentials{}
	}
	deps.OAuth.Credentials[connector.ProviderOneDrive] = connector.AppCredentials{ClientID: "m-id", ClientSecret: "m-secret"}
	svc := NewBrokerService(deps)
	resp, err := svc.ListConnectorProviders(context.Background(), &brokerv1.ListConnectorProvidersRequest{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.Providers) != 2 {
		t.Fatalf("want both providers, got %d: %+v", len(resp.Providers), resp.Providers)
	}
	if resp.Providers[0].Key != "google_drive" || resp.Providers[1].Key != "onedrive" {
		t.Fatalf("providers not sorted by key: %+v", resp.Providers)
	}
}

func TestCompleteConnectorAuth_UnknownStateRejected(t *testing.T) {
	svc := NewBrokerService(testConnectorDeps(t))
	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")
	_, err := svc.CompleteConnectorAuth(ctx, &brokerv1.CompleteConnectorAuthRequest{
		UserId: "alice@example.com",
		Code:   "abc",
		State:  "does-not-exist",
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument for unknown state, got %v", err)
	}
}

func TestConnectorIdentity_RejectsNonUUIDTenant(t *testing.T) {
	svc := NewBrokerService(testConnectorDeps(t))
	ctx := ctxWithIdentity("not-a-uuid", "alice@example.com")
	_, err := svc.ListConnectors(ctx, &brokerv1.ListConnectorsRequest{UserId: "alice@example.com"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument for non-uuid tenant, got %v", err)
	}
}

// A connector call may only act on the authenticated user's own Vault path: a
// token holder claiming a different user_id is rejected, so OAuth tokens can't
// be written to or read from another user's connector path.
func TestConnectorIdentity_RejectsUserIdMismatch(t *testing.T) {
	svc := NewBrokerService(testConnectorDeps(t))
	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")
	_, err := svc.ListConnectors(ctx, &brokerv1.ListConnectorsRequest{UserId: "bob@example.com"})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("mismatched user_id should be PermissionDenied, got %v", err)
	}
}

// ctxWithSubjectIdentity builds a context carrying an Entra-shaped identity:
// Subject=oid (the immutable authz principal), Email=email (display), TenantID=tenant.
// This mirrors what the OIDC interceptor populates when AIKONOS_OIDC_SUBJECT_CLAIM=oid.
func ctxWithSubjectIdentity(tenant, oid, email string) context.Context {
	return auth.WithIdentity(context.Background(), &auth.Identity{
		TenantID: tenant,
		Subject:  oid,
		Email:    email,
	})
}

// TestCallerIdentity_EntraOidUsedAsPrincipal: with an Entra-shaped identity
// (Subject=oid, Email≠oid), callerIdentity must return the oid as the user
// principal — not the email. This is the zero-trust authz path.
func TestCallerIdentity_EntraOidUsedAsPrincipal(t *testing.T) {
	svc := NewBrokerService(testConnectorDeps(t))
	oid := "cccccccc-0000-4000-8000-000000000001"
	email := "adam.dekan@gmail.com"
	ctx := ctxWithSubjectIdentity(testTenantUUID, oid, email)

	// ListConnectors calls callerIdentity; pass the oid as user_id (matching the principal).
	_, err := svc.ListConnectors(ctx, &brokerv1.ListConnectorsRequest{
		TenantId: testTenantUUID,
		UserId:   oid,
	})
	// Connectors backend returns ErrNotFound or similar — what we care about is NOT
	// PermissionDenied, which would mean the principal was the email (mismatch).
	if status.Code(err) == codes.PermissionDenied {
		t.Fatalf("callerIdentity should accept oid as user_id when Subject=oid; got PermissionDenied: %v", err)
	}
}

// TestCallerIdentity_EntraEmailMismatchRejected: body user_id=email while
// Subject=oid → the principal is oid, email disagrees → PermissionDenied.
// This proves callerIdentity is Subject-first: email alone can't impersonate.
func TestCallerIdentity_EntraEmailMismatchRejected(t *testing.T) {
	svc := NewBrokerService(testConnectorDeps(t))
	oid := "cccccccc-0000-4000-8000-000000000001"
	email := "adam.dekan@gmail.com"
	ctx := ctxWithSubjectIdentity(testTenantUUID, oid, email)

	_, err := svc.ListConnectors(ctx, &brokerv1.ListConnectorsRequest{
		TenantId: testTenantUUID,
		UserId:   email, // disagrees with Subject=oid
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("email user_id with Subject=oid should be PermissionDenied, got %v", err)
	}
}

// TestCallerIdentity_KeycloakSubjectEqualsEmail: Keycloak sets Subject=preferred_username=email.
// callerIdentity returns the email (via Subject) — same result as before, K2 preserved.
func TestCallerIdentity_KeycloakSubjectEqualsEmail(t *testing.T) {
	svc := NewBrokerService(testConnectorDeps(t))
	email := "alice@example.com"
	// Keycloak: Subject == preferred_username == email
	ctx := ctxWithSubjectIdentity(testTenantUUID, email, email)

	_, err := svc.ListConnectors(ctx, &brokerv1.ListConnectorsRequest{
		TenantId: testTenantUUID,
		UserId:   email,
	})
	if status.Code(err) == codes.PermissionDenied {
		t.Fatalf("Keycloak email-as-subject should pass; got PermissionDenied: %v", err)
	}
}

// TestCallerIdentity_SvcPrincipalRejectedEvenWithSubject: svc- in the resolved
// principal (here via Subject) must still be rejected on the north-bound surface.
func TestCallerIdentity_SvcPrincipalRejectedEvenWithSubject(t *testing.T) {
	svc := NewBrokerService(testConnectorDeps(t))
	ctx := ctxWithSubjectIdentity(testTenantUUID, "svc-some-agent", "")

	_, err := svc.ListConnectors(ctx, &brokerv1.ListConnectorsRequest{
		TenantId: testTenantUUID,
		UserId:   "svc-some-agent",
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("svc- principal via Subject must be PermissionDenied, got %v", err)
	}
}

// TestUpsertThrottle_InstanceIsolation verifies two service instances have
// independent throttle state (the old package-level singleton broke this).
func TestUpsertThrottle_InstanceIsolation(t *testing.T) {
	a := newUpsertThrottle()
	b := newUpsertThrottle()

	if !a.allow("tenant", "user1") {
		t.Fatal("first allow on a should pass")
	}
	// b has no knowledge of a's entry — must allow independently.
	if !b.allow("tenant", "user1") {
		t.Fatal("b is a separate instance; must allow independently of a")
	}
	// Second call on a within TTL must be suppressed.
	if a.allow("tenant", "user1") {
		t.Fatal("second allow on a within TTL should be suppressed")
	}
}

// TestUpsertThrottle_CapBounded verifies that filling past dirThrottleMaxEntries
// does not grow the map without bound: after the cap is reached the map size
// stays at or below dirThrottleMaxEntries+1 (the just-inserted entry).
func TestUpsertThrottle_CapBounded(t *testing.T) {
	u := newUpsertThrottle()

	// Pre-fill with entries that are already expired so eviction clears them.
	expired := time.Now().Add(-2 * dirUpsertTTL)
	u.mu.Lock()
	for i := 0; i < dirThrottleMaxEntries; i++ {
		u.seen[fmt.Sprintf("t/%d", i)] = expired
	}
	u.mu.Unlock()

	// One more allow() triggers the cap path; expired entries are evicted → map stays small.
	u.allow("t", "overflow")

	u.mu.Lock()
	size := len(u.seen)
	u.mu.Unlock()

	if size > dirThrottleMaxEntries {
		t.Fatalf("map size %d exceeds cap %d after cap enforcement", size, dirThrottleMaxEntries)
	}
}

// TestUpsertThrottle_CapBounded_ForcesClear verifies the fallback clear path:
// when all entries are still live (none expired) and the map is at cap, allow()
// clears the map and re-inserts just the new entry, bounding size to 1.
func TestUpsertThrottle_CapBounded_ForcesClear(t *testing.T) {
	u := newUpsertThrottle()

	// Pre-fill with non-expired entries.
	recent := time.Now()
	u.mu.Lock()
	for i := 0; i < dirThrottleMaxEntries; i++ {
		u.seen[fmt.Sprintf("t/%d", i)] = recent
	}
	u.mu.Unlock()

	u.allow("t", "overflow")

	u.mu.Lock()
	size := len(u.seen)
	u.mu.Unlock()

	// After forced clear + single insert, map has exactly 1 entry.
	if size != 1 {
		t.Fatalf("expected map size 1 after forced clear, got %d", size)
	}
}

func TestProviderMappingRoundTrip(t *testing.T) {
	for _, p := range []connector.Provider{connector.ProviderGoogleDrive, connector.ProviderOneDrive} {
		pb := providerToProto(p)
		back, ok := providerFromProto(pb)
		if !ok || back != p {
			t.Fatalf("round trip failed for %v: %v %v", p, back, ok)
		}
		if fromID, ok := providerFromConnectorID(p.ConnectorID()); !ok || fromID != p {
			t.Fatalf("connector id mapping failed for %v", p)
		}
	}
}
