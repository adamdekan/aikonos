package broker

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/connector"
	"github.com/adamdekan/aikonos/broker/internal/connectorstore"
	"github.com/adamdekan/aikonos/broker/internal/secrets"
)

// ctxWithBearer builds an incoming-gRPC-metadata context carrying an
// "authorization: Bearer <token>" header — what auth.BearerFromContext reads.
func ctxWithBearer(token string) context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+token))
}

// fakeOBOBroker is the oboBroker seam test double — ensureOneDriveOBO tests
// drive it directly instead of standing up a live Entra-shaped HTTP server
// (that coverage lives in connector/obo_test.go against the real OBOBroker).
type fakeOBOBroker struct {
	configured     bool
	bootstrapErr   error
	bootstrapCalls []string // "tenant/user"
}

func (f *fakeOBOBroker) Configured(context.Context, string) bool { return f.configured }
func (f *fakeOBOBroker) Bootstrap(_ context.Context, tenant, user, assertion string) error {
	if assertion == "" {
		panic("ensureOneDriveOBO must never call Bootstrap with an empty assertion")
	}
	f.bootstrapCalls = append(f.bootstrapCalls, tenant+"/"+user)
	return f.bootstrapErr
}

// fakeOboSecrets is a minimal secrets.Provider double for ensureOneDriveOBO's
// oboAlreadyHealthy check — only ReadConnector's presence/absence matters.
type fakeOboSecrets struct {
	hasToken bool
}

func (f *fakeOboSecrets) ReadConnector(context.Context, string, string, string) (*secrets.ConnectorToken, error) {
	if !f.hasToken {
		return nil, secrets.ErrNotFound
	}
	return &secrets.ConnectorToken{AccessToken: "at"}, nil
}
func (f *fakeOboSecrets) WriteConnector(context.Context, string, string, string, *secrets.ConnectorToken) error {
	return nil
}
func (f *fakeOboSecrets) DeleteConnector(context.Context, string, string, string) error { return nil }
func (f *fakeOboSecrets) ProviderKeyPath(tenant, providerID string) (string, error) {
	return secrets.ProviderKeyPath(tenant, providerID)
}
func (f *fakeOboSecrets) ReadProviderKey(context.Context, string, string) (string, bool, error) {
	return "", false, nil
}
func (f *fakeOboSecrets) WriteProviderKey(context.Context, string, string, string) error { return nil }
func (f *fakeOboSecrets) DeleteProviderKey(context.Context, string, string) error        { return nil }
func (f *fakeOboSecrets) ReadM365App(context.Context, string) (string, bool, error) {
	return "", false, nil
}
func (f *fakeOboSecrets) WriteM365App(context.Context, string, string) error { return nil }
func (f *fakeOboSecrets) DeleteM365App(context.Context, string) error        { return nil }

func testOboDeps(t *testing.T, obo oboBroker, cs *connectorstore.Store, sec secrets.Provider) Deps {
	t.Helper()
	em, err := audit.NewEmitter(context.Background(), audit.Config{})
	if err != nil {
		t.Fatal(err)
	}
	return Deps{OBOBroker: obo, Connectors: cs, Secrets: sec, Audit: em}
}

func TestEnsureOneDriveOBO_NoOpWhenOBOBrokerNil(t *testing.T) {
	svc := NewBrokerService(Deps{})
	svc.ensureOneDriveOBO(ctxWithBearer("t"), "t1", "u1") // must not panic
}

func TestEnsureOneDriveOBO_NoOpWhenUnconfigured(t *testing.T) {
	fake := &fakeOBOBroker{configured: false}
	svc := NewBrokerService(testOboDeps(t, fake, connectorstore.New(t.TempDir()), &fakeOboSecrets{}))
	svc.ensureOneDriveOBO(ctxWithBearer("t"), "t1", "u1")
	if len(fake.bootstrapCalls) != 0 {
		t.Fatalf("want no bootstrap attempt when the tenant is unconfigured, got %v", fake.bootstrapCalls)
	}
}

func TestEnsureOneDriveOBO_NoOpWhenAlreadyHealthy(t *testing.T) {
	cs := connectorstore.New(t.TempDir())
	connectorID := connector.ProviderOneDrive.ConnectorID()
	if err := cs.Upsert("t1", "u1", connectorstore.Entry{ConnectorID: connectorID, Status: connector.StatusConnected}); err != nil {
		t.Fatal(err)
	}
	fake := &fakeOBOBroker{configured: true}
	svc := NewBrokerService(testOboDeps(t, fake, cs, &fakeOboSecrets{hasToken: true}))

	svc.ensureOneDriveOBO(ctxWithBearer("t"), "t1", "u1")
	if len(fake.bootstrapCalls) != 0 {
		t.Fatalf("want no bootstrap attempt when already connected in Vault + connectorstore, got %v", fake.bootstrapCalls)
	}
}

func TestEnsureOneDriveOBO_BootstrapsWhenConnectorstoreSaysConnectedButVaultHasNoToken(t *testing.T) {
	// A stale "connected" entry with no backing Vault token must still trigger
	// a bootstrap attempt — the connectorstore alone is not proof of health.
	cs := connectorstore.New(t.TempDir())
	connectorID := connector.ProviderOneDrive.ConnectorID()
	if err := cs.Upsert("t1", "u1", connectorstore.Entry{ConnectorID: connectorID, Status: connector.StatusConnected}); err != nil {
		t.Fatal(err)
	}
	fake := &fakeOBOBroker{configured: true}
	svc := NewBrokerService(testOboDeps(t, fake, cs, &fakeOboSecrets{hasToken: false}))

	svc.ensureOneDriveOBO(ctxWithBearer("t"), "t1", "u1")
	if len(fake.bootstrapCalls) != 1 {
		t.Fatalf("want one bootstrap attempt when Vault has no token despite a stale connected entry, got %v", fake.bootstrapCalls)
	}
}

func TestEnsureOneDriveOBO_NoOpWhenNoBearerOnContext(t *testing.T) {
	fake := &fakeOBOBroker{configured: true}
	svc := NewBrokerService(testOboDeps(t, fake, connectorstore.New(t.TempDir()), &fakeOboSecrets{}))
	svc.ensureOneDriveOBO(context.Background(), "t1", "u1") // no incoming metadata at all
	if len(fake.bootstrapCalls) != 0 {
		t.Fatalf("want no bootstrap attempt with no bearer on the context, got %v", fake.bootstrapCalls)
	}
}

func TestEnsureOneDriveOBO_SuccessUpsertsConnectedEntry(t *testing.T) {
	cs := connectorstore.New(t.TempDir())
	fake := &fakeOBOBroker{configured: true}
	svc := NewBrokerService(testOboDeps(t, fake, cs, &fakeOboSecrets{}))

	svc.ensureOneDriveOBO(ctxWithBearer("caller-bearer"), "t1", "u1")

	if len(fake.bootstrapCalls) != 1 || fake.bootstrapCalls[0] != "t1/u1" {
		t.Fatalf("want exactly one bootstrap attempt for t1/u1, got %v", fake.bootstrapCalls)
	}
	entry, ok, err := cs.Resolve("t1", "u1", connector.ProviderOneDrive.ConnectorID())
	if err != nil || !ok {
		t.Fatalf("expected a connectorstore entry, ok=%v err=%v", ok, err)
	}
	if entry.Status != connector.StatusConnected {
		t.Fatalf("want status=connected, got %q", entry.Status)
	}
	if entry.Provider != string(connector.ProviderOneDrive) || len(entry.Scopes) == 0 {
		t.Fatalf("unexpected entry: %+v", entry)
	}
}

func TestEnsureOneDriveOBO_FailureUpsertsReconnectNeededEntry(t *testing.T) {
	cs := connectorstore.New(t.TempDir())
	fake := &fakeOBOBroker{configured: true, bootstrapErr: errors.New("exchange failed")}
	svc := NewBrokerService(testOboDeps(t, fake, cs, &fakeOboSecrets{}))

	svc.ensureOneDriveOBO(ctxWithBearer("caller-bearer"), "t1", "u1")

	entry, ok, err := cs.Resolve("t1", "u1", connector.ProviderOneDrive.ConnectorID())
	if err != nil || !ok {
		t.Fatalf("expected a connectorstore entry, ok=%v err=%v", ok, err)
	}
	if entry.Status != connector.StatusReconnectNeeded {
		t.Fatalf("want status=reconnect_needed, got %q", entry.Status)
	}
}

func TestEnsureOneDriveOBO_ThrottleSuppressesImmediateRetry(t *testing.T) {
	cs := connectorstore.New(t.TempDir())
	fake := &fakeOBOBroker{configured: true, bootstrapErr: errors.New("exchange failed")}
	svc := NewBrokerService(testOboDeps(t, fake, cs, &fakeOboSecrets{}))
	ctx := ctxWithBearer("caller-bearer")

	svc.ensureOneDriveOBO(ctx, "t1", "u1")
	svc.ensureOneDriveOBO(ctx, "t1", "u1")

	if len(fake.bootstrapCalls) != 1 {
		t.Fatalf("want exactly one bootstrap attempt within the throttle window, got %d: %v", len(fake.bootstrapCalls), fake.bootstrapCalls)
	}
}

// TestEnsureOneDriveOBO_HealsOnNextCallAfterThrottleWindow proves the full
// sequence: a failure records reconnect_needed and throttles retries; once
// the throttle window has passed, the next call with a fresh bearer
// succeeds and flips the entry back to connected.
func TestEnsureOneDriveOBO_HealsOnNextCallAfterThrottleWindow(t *testing.T) {
	cs := connectorstore.New(t.TempDir())
	fake := &fakeOBOBroker{configured: true, bootstrapErr: errors.New("exchange failed")}
	svc := NewBrokerService(testOboDeps(t, fake, cs, &fakeOboSecrets{}))
	ctx := ctxWithBearer("caller-bearer")

	svc.ensureOneDriveOBO(ctx, "t1", "u1")
	entry, _, _ := cs.Resolve("t1", "u1", connector.ProviderOneDrive.ConnectorID())
	if entry.Status != connector.StatusReconnectNeeded {
		t.Fatalf("want reconnect_needed after the failed attempt, got %q", entry.Status)
	}

	// Simulate the throttle window having elapsed rather than sleeping 2 minutes.
	svc.oboThrottle.mu.Lock()
	svc.oboThrottle.seen = map[string]time.Time{}
	svc.oboThrottle.mu.Unlock()

	fake.bootstrapErr = nil
	svc.ensureOneDriveOBO(ctx, "t1", "u1")

	if len(fake.bootstrapCalls) != 2 {
		t.Fatalf("want a second bootstrap attempt after the throttle window, got %v", fake.bootstrapCalls)
	}
	entry, _, _ = cs.Resolve("t1", "u1", connector.ProviderOneDrive.ConnectorID())
	if entry.Status != connector.StatusConnected {
		t.Fatalf("want connected after the healed attempt, got %q", entry.Status)
	}
}

// TestOboThrottle_TTLPinnedToTwoMinutes pins oboThrottle's TTL to the
// ~2-minute value  CP2 requires, distinct
// from dirThrottle's unrelated 10-minute dirUpsertTTL — a regression that
// collapsed both onto one shared TTL constant must fail this test.
func TestOboThrottle_TTLPinnedToTwoMinutes(t *testing.T) {
	svc := NewBrokerService(Deps{})
	now := time.Now()

	// Just inside the OBO TTL window (2min - epsilon ago): still suppressed.
	svc.oboThrottle.mu.Lock()
	svc.oboThrottle.seen = map[string]time.Time{"t1/u1": now.Add(-(oboThrottleTTL - time.Second))}
	svc.oboThrottle.mu.Unlock()
	if svc.oboThrottle.allow("t1", "u1") {
		t.Fatal("want suppressed just inside the OBO TTL window (T+2min-ε)")
	}

	// Just outside the OBO TTL window (2min + epsilon ago): allowed again.
	svc.oboThrottle.mu.Lock()
	svc.oboThrottle.seen = map[string]time.Time{"t1/u1": now.Add(-(oboThrottleTTL + time.Second))}
	svc.oboThrottle.mu.Unlock()
	if !svc.oboThrottle.allow("t1", "u1") {
		t.Fatal("want allowed just outside the OBO TTL window (T+2min+ε)")
	}

	// dirThrottle must stay on its own 10-minute TTL: an entry aged past the
	// (much shorter) OBO TTL but still inside dirUpsertTTL must still be
	// suppressed there — proves the two throttles' TTLs are independent, not
	// both silently reading oboThrottleTTL.
	svc.dirThrottle.mu.Lock()
	svc.dirThrottle.seen = map[string]time.Time{"t1/u1": now.Add(-(oboThrottleTTL + time.Second))}
	svc.dirThrottle.mu.Unlock()
	if svc.dirThrottle.allow("t1", "u1") {
		t.Fatal("want dirThrottle unaffected by oboThrottleTTL, still on its own 10-minute dirUpsertTTL")
	}
}

func TestOneDriveManaged(t *testing.T) {
	if (&BrokerService{}).oneDriveManaged(context.Background(), "t1") {
		t.Fatal("want false when OBOBroker is unwired")
	}
	svc := NewBrokerService(Deps{OBOBroker: &fakeOBOBroker{configured: true}})
	if !svc.oneDriveManaged(context.Background(), "t1") {
		t.Fatal("want true when OBOBroker reports configured")
	}
	if svc.oneDriveManaged(context.Background(), "") {
		t.Fatal("want false for an empty tenant regardless of Configured")
	}
}
