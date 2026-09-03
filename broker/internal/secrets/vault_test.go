package secrets

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeVault is an httptest server emulating the bits of Vault the client uses:
// kubernetes login and KV-v2 read/write/delete backed by an in-memory map.
// Each logical path keeps every written version (index 0 = KV-v2 version 1),
// so tests can exercise ?version=N historic reads alongside the latest-value
// convenience most callers use.
type fakeVault struct {
	leaseSeconds int
	logins       int32
	store        map[string][]json.RawMessage // logical path -> versions, oldest first
}

func newFakeVault(leaseSeconds int) *fakeVault {
	return &fakeVault{leaseSeconds: leaseSeconds, store: map[string][]json.RawMessage{}}
}

func (f *fakeVault) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/auth/kubernetes/login", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.logins, 1)
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["role"] == "" || body["jwt"] == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"auth": map[string]any{"client_token": "s.faketoken", "lease_duration": f.leaseSeconds},
		})
	})
	mux.HandleFunc("/v1/auth/approle/login", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.logins, 1)
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["role_id"] == "" || body["secret_id"] == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"auth": map[string]any{"client_token": "s.approletoken", "lease_duration": f.leaseSeconds},
		})
	})
	mux.HandleFunc("/v1/secret/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Vault-Token") == "" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		// /v1/secret/data/<logical> or /v1/secret/metadata/<logical>
		rest := strings.TrimPrefix(r.URL.Path, "/v1/secret/")
		kind, logical, _ := strings.Cut(rest, "/")
		switch {
		case kind == "data" && r.Method == http.MethodGet:
			versions, ok := f.store[logical]
			if !ok || len(versions) == 0 {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			version := len(versions) // latest, 1-indexed
			if raw := r.URL.Query().Get("version"); raw != "" {
				n, err := strconv.Atoi(raw)
				if err != nil || n < 1 || n > len(versions) {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				version = n
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"data":     json.RawMessage(versions[version-1]),
					"metadata": map[string]any{"version": version},
				},
			})
		case kind == "data" && r.Method == http.MethodPost:
			var payload struct {
				Data    json.RawMessage `json:"data"`
				Options *struct {
					CAS *int `json:"cas"`
				} `json:"options"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode write: %v", err)
			}
			// Honor check-and-set: cas=0 means "create only if absent".
			if payload.Options != nil && payload.Options.CAS != nil && *payload.Options.CAS == 0 {
				if len(f.store[logical]) > 0 {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
			}
			f.store[logical] = append(f.store[logical], payload.Data)
			w.WriteHeader(http.StatusNoContent)
		case kind == "metadata" && r.Method == http.MethodDelete:
			delete(f.store, logical)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	return mux
}

func newTestClient(t *testing.T, srv *httptest.Server) *VaultClient {
	t.Helper()
	jwtFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(jwtFile, []byte("fake-sa-jwt"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := NewVaultClient(Config{Addr: srv.URL, Role: "aikonos-broker", JWTPath: jwtFile})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestNewVaultClient_DisabledWhenAddrEmpty(t *testing.T) {
	c, err := NewVaultClient(Config{})
	if err != nil || c != nil {
		t.Fatalf("want (nil,nil) for empty addr, got (%v,%v)", c, err)
	}
}

func TestNewVaultClient_AppRoleRequiresCredentials(t *testing.T) {
	// approle with no role_id/secret_id must fail closed at construction.
	if _, err := NewVaultClient(Config{Addr: "http://vault:8200", AuthMethod: AuthAppRole}); err == nil {
		t.Fatal("expected error for approle auth without role_id/secret_id")
	}
	if _, err := NewVaultClient(Config{Addr: "http://vault:8200", AuthMethod: AuthAppRole, RoleID: "r"}); err == nil {
		t.Fatal("expected error for approle auth without secret_id")
	}
}

func TestNewVaultClient_UnknownAuthMethodRejected(t *testing.T) {
	if _, err := NewVaultClient(Config{Addr: "http://vault:8200", AuthMethod: "token"}); err == nil {
		t.Fatal("expected error for unknown auth method")
	}
}

// TestVault_AppRoleLogin proves the approle path authenticates and reads back a
// stored secret — no SA-JWT file involved (the compose deployment).
func TestVault_AppRoleLogin(t *testing.T) {
	fake := newFakeVault(3600)
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()
	c, err := NewVaultClient(Config{
		Addr:       srv.URL,
		AuthMethod: AuthAppRole,
		RoleID:     "role-abc",
		SecretID:   "secret-xyz",
	})
	if err != nil {
		t.Fatalf("new approle client: %v", err)
	}
	ctx := context.Background()
	if err := c.WriteConnector(ctx, "tenant-a", "alice", "gdrive", &ConnectorToken{AccessToken: "at"}); err != nil {
		t.Fatalf("write via approle: %v", err)
	}
	got, err := c.ReadConnector(ctx, "tenant-a", "alice", "gdrive")
	if err != nil {
		t.Fatalf("read via approle: %v", err)
	}
	if got.AccessToken != "at" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if n := atomic.LoadInt32(&fake.logins); n != 1 {
		t.Fatalf("expected exactly 1 approle login, got %d", n)
	}
}

func TestVault_WriteReadDeleteRoundTrip(t *testing.T) {
	fake := newFakeVault(3600)
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()
	c := newTestClient(t, srv)
	ctx := context.Background()

	tok := &ConnectorToken{
		AccessToken:  "at-1",
		RefreshToken: "rt-1",
		ExpiresAt:    time.Unix(1_900_000_000, 0).UTC(),
		Scopes:       []string{"gdrive:read"},
	}
	if err := c.WriteConnector(ctx, "tenant-a", "alice", "gdrive", tok); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := c.ReadConnector(ctx, "tenant-a", "alice", "gdrive")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.AccessToken != "at-1" || got.RefreshToken != "rt-1" || len(got.Scopes) != 1 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if err := c.DeleteConnector(ctx, "tenant-a", "alice", "gdrive"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := c.ReadConnector(ctx, "tenant-a", "alice", "gdrive"); err != ErrNotFound {
		t.Fatalf("want ErrNotFound after delete, got %v", err)
	}
}

func TestVault_ReadMissingIsNotFound(t *testing.T) {
	fake := newFakeVault(3600)
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()
	c := newTestClient(t, srv)
	if _, err := c.ReadConnector(context.Background(), "t", "u", "gdrive"); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestVault_LoginCachedThenRenewedNearExpiry(t *testing.T) {
	fake := newFakeVault(120) // 2-minute lease
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()
	c := newTestClient(t, srv)

	clock := time.Unix(1_000_000, 0)
	c.now = func() time.Time { return clock }
	ctx := context.Background()

	if err := c.WriteConnector(ctx, "t", "u", "gdrive", &ConnectorToken{AccessToken: "a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ReadConnector(ctx, "t", "u", "gdrive"); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt32(&fake.logins); n != 1 {
		t.Fatalf("expected token reuse (1 login), got %d", n)
	}

	// Advance to within the renew skew of the 120s lease → forces re-login.
	clock = clock.Add(115 * time.Second)
	if _, err := c.ReadConnector(ctx, "t", "u", "gdrive"); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt32(&fake.logins); n != 2 {
		t.Fatalf("expected re-login near expiry (2 logins), got %d", n)
	}
}

func TestVault_InvalidPathSegmentRejected(t *testing.T) {
	fake := newFakeVault(3600)
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()
	c := newTestClient(t, srv)
	if err := c.WriteConnector(context.Background(), "t", "../escape", "gdrive", &ConnectorToken{}); err == nil {
		t.Fatal("expected rejection of traversal segment")
	}
}

func TestVault_LoginPropagatesServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "boom")
	}))
	defer srv.Close()
	c := newTestClient(t, srv)
	if _, err := c.ReadConnector(context.Background(), "t", "u", "gdrive"); err == nil {
		t.Fatal("expected login error to propagate")
	}
}

func TestVault_McpBearerWriteReadRoundTrip(t *testing.T) {
	fake := newFakeVault(3600)
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()
	c := newTestClient(t, srv)
	ctx := context.Background()

	// Write a bearer token.
	path, err := McpBearerPath("tenant-a", "srv-1")
	if err != nil {
		t.Fatalf("McpBearerPath: %v", err)
	}
	if err := c.WriteMcpBearer(ctx, path, "tok-secret"); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Read it back via ReadMcpBearer.
	got, err := c.ReadMcpBearer(ctx, "tenant-a", "srv-1")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got != "tok-secret" {
		t.Fatalf("round-trip mismatch: %q", got)
	}
}

func TestVault_McpBearerReadMissingIsNotFound(t *testing.T) {
	fake := newFakeVault(3600)
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()
	c := newTestClient(t, srv)

	_, err := c.ReadMcpBearer(context.Background(), "tenant-a", "no-such-server")
	if err != ErrMcpBearerNotFound {
		t.Fatalf("want ErrMcpBearerNotFound, got %v", err)
	}
}

func TestVault_McpBearerDeleteThenReadIsNotFound(t *testing.T) {
	fake := newFakeVault(3600)
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()
	c := newTestClient(t, srv)
	ctx := context.Background()

	path, err := McpBearerPath("tenant-a", "srv-del")
	if err != nil {
		t.Fatalf("McpBearerPath: %v", err)
	}
	if err := c.WriteMcpBearer(ctx, path, "tok-to-delete"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := c.ReadMcpBearer(ctx, "tenant-a", "srv-del"); err != nil {
		t.Fatalf("read before delete: %v", err)
	}
	if err := c.DeleteMcpBearer(ctx, path); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := c.ReadMcpBearer(ctx, "tenant-a", "srv-del"); err != ErrMcpBearerNotFound {
		t.Fatalf("want ErrMcpBearerNotFound after delete, got %v", err)
	}
}

// ── Provider key tests ────────────────────────────────────────────────────────

func TestProviderKeyPath_Valid(t *testing.T) {
	path, err := ProviderKeyPath("tenant-a", "openrouter")
	if err != nil {
		t.Fatalf("ProviderKeyPath: %v", err)
	}
	want := "providers/tenant-a/openrouter"
	if path != want {
		t.Fatalf("got %q, want %q", path, want)
	}
}

func TestProviderKeyPath_RejectsTraversal(t *testing.T) {
	cases := []struct{ tenant, id string }{
		{"../escape", "openrouter"},
		{"tenant-a", "../escape"},
		{"", "openrouter"},
		{"tenant-a", ""},
		{"ten ant", "openrouter"},
		{"tenant-a", "has/slash"},
	}
	for _, c := range cases {
		if _, err := ProviderKeyPath(c.tenant, c.id); err == nil {
			t.Errorf("expected rejection for tenant=%q id=%q", c.tenant, c.id)
		}
	}
}

func TestVault_ProviderKeyWriteReadRoundTrip(t *testing.T) {
	fake := newFakeVault(3600)
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()
	c := newTestClient(t, srv)
	ctx := context.Background()

	if err := c.WriteProviderKey(ctx, "tenant-a", "openrouter", "sk-test-key"); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, ok, err := c.ReadProviderKey(ctx, "tenant-a", "openrouter")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !ok {
		t.Fatal("want ok=true, got false")
	}
	if got != "sk-test-key" {
		t.Fatalf("round-trip mismatch: %q", got)
	}
}

func TestVault_ProviderKeyReadAbsentReturnsFalse(t *testing.T) {
	fake := newFakeVault(3600)
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()
	c := newTestClient(t, srv)

	_, ok, err := c.ReadProviderKey(context.Background(), "tenant-a", "no-such-provider")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatal("want ok=false for absent key")
	}
}

func TestVault_ProviderKeyDeleteThenReadAbsent(t *testing.T) {
	fake := newFakeVault(3600)
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()
	c := newTestClient(t, srv)
	ctx := context.Background()

	if err := c.WriteProviderKey(ctx, "tenant-a", "openrouter", "sk-to-delete"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := c.DeleteProviderKey(ctx, "tenant-a", "openrouter"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	_, ok, err := c.ReadProviderKey(ctx, "tenant-a", "openrouter")
	if err != nil {
		t.Fatalf("read after delete: %v", err)
	}
	if ok {
		t.Fatal("want ok=false after delete")
	}
}

func TestVault_ProviderKeyPathTraversalRejected(t *testing.T) {
	fake := newFakeVault(3600)
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()
	c := newTestClient(t, srv)
	if err := c.WriteProviderKey(context.Background(), "../escape", "openrouter", "key"); err == nil {
		t.Fatal("expected rejection of traversal segment")
	}
}

func TestVault_CapabilityKeyRoundTrip(t *testing.T) {
	fake := newFakeVault(3600)
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()
	c := newTestClient(t, srv)
	ctx := context.Background()

	if _, err := c.ReadCapabilityKey(ctx); err != ErrNotFound {
		t.Fatalf("want ErrNotFound before provisioning, got %v", err)
	}
	if err := c.WriteCapabilityKey(ctx, "a2V5LWJhc2U2NA=="); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := c.ReadCapabilityKey(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got != "a2V5LWJhc2U2NA==" {
		t.Fatalf("round-trip mismatch: %q", got)
	}
	// A second create with cas=0 must conflict (key already exists).
	if err := c.WriteCapabilityKey(ctx, "b3RoZXI="); err != ErrCASConflict {
		t.Fatalf("want ErrCASConflict on second create, got %v", err)
	}
}

// TestVault_AuditSigningKeyVersioned proves the read-or-create + versioned
// read contract CP1.2 relies on: first write lands as KV-v2 version 1, a
// second (rotation) write as version 2, and ReadAuditSigningKeyVersion can
// still fetch version 1 after the rotation — the basis for VerifyChain
// spanning a key rotation without invalidating older events.
func TestVault_AuditSigningKeyVersioned(t *testing.T) {
	fake := newFakeVault(3600)
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()
	c := newTestClient(t, srv)
	ctx := context.Background()

	if _, _, err := c.ReadAuditSigningKey(ctx); err != ErrNotFound {
		t.Fatalf("want ErrNotFound before provisioning, got %v", err)
	}
	if err := c.WriteAuditSigningKey(ctx, "a2V5LXYx"); err != nil {
		t.Fatalf("write v1: %v", err)
	}
	gotKey, gotVersion, err := c.ReadAuditSigningKey(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if gotKey != "a2V5LXYx" || gotVersion != 1 {
		t.Fatalf("want (key=a2V5LXYx, version=1), got (key=%q, version=%d)", gotKey, gotVersion)
	}
	// A second create with cas=0 must conflict — rotation is a manual
	// operator write directly to Vault (docs/OPS-RUNBOOK.md), not via this
	// cas=0 path. Simulate it here by writing straight into the fake's store.
	if err := c.WriteAuditSigningKey(ctx, "b3RoZXI="); err != ErrCASConflict {
		t.Fatalf("want ErrCASConflict on second create, got %v", err)
	}
	fake.store[auditSigningKeyLogical] = append(fake.store[auditSigningKeyLogical], mustRawMessage(t, map[string]string{"key": "a2V5LXYy"}))

	// Latest read now returns version 2...
	gotKey, gotVersion, err = c.ReadAuditSigningKey(ctx)
	if err != nil {
		t.Fatalf("read after rotation: %v", err)
	}
	if gotKey != "a2V5LXYy" || gotVersion != 2 {
		t.Fatalf("want (key=a2V5LXYy, version=2) after rotation, got (key=%q, version=%d)", gotKey, gotVersion)
	}
	// ...but version 1 is still resolvable by explicit historic read.
	oldKey, err := c.ReadAuditSigningKeyVersion(ctx, 1)
	if err != nil {
		t.Fatalf("read version 1 after rotation: %v", err)
	}
	if oldKey != "a2V5LXYx" {
		t.Fatalf("want old version 1 key a2V5LXYx, got %q", oldKey)
	}
	if _, err := c.ReadAuditSigningKeyVersion(ctx, 99); err != ErrNotFound {
		t.Fatalf("want ErrNotFound for a nonexistent version, got %v", err)
	}
}

// ── M365 app secret tests ─────────────────────────────────────────────────────

func TestM365AppPath_Valid(t *testing.T) {
	path, err := M365AppPath("tenant-a")
	if err != nil {
		t.Fatalf("M365AppPath: %v", err)
	}
	want := "m365/tenant-a/app"
	if path != want {
		t.Fatalf("got %q, want %q", path, want)
	}
}

func TestM365AppPath_RejectsBadSegments(t *testing.T) {
	for _, tenant := range []string{"../escape", "", "ten ant", "has/slash", ".", ".."} {
		if _, err := M365AppPath(tenant); err == nil {
			t.Errorf("expected rejection for tenant=%q", tenant)
		}
	}
}

func TestVault_M365AppWriteReadRoundTrip(t *testing.T) {
	fake := newFakeVault(3600)
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()
	c := newTestClient(t, srv)
	ctx := context.Background()

	if err := c.WriteM365App(ctx, "tenant-a", "m365-secret"); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, ok, err := c.ReadM365App(ctx, "tenant-a")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !ok {
		t.Fatal("want ok=true, got false")
	}
	if got != "m365-secret" {
		t.Fatalf("round-trip mismatch: %q", got)
	}
}

func TestVault_M365AppReadAbsentReturnsFalse(t *testing.T) {
	fake := newFakeVault(3600)
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()
	c := newTestClient(t, srv)

	_, ok, err := c.ReadM365App(context.Background(), "tenant-a")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatal("want ok=false for absent secret")
	}
}

func TestVault_M365AppDeleteThenReadAbsent(t *testing.T) {
	fake := newFakeVault(3600)
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()
	c := newTestClient(t, srv)
	ctx := context.Background()

	if err := c.WriteM365App(ctx, "tenant-a", "to-delete"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := c.DeleteM365App(ctx, "tenant-a"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	_, ok, err := c.ReadM365App(ctx, "tenant-a")
	if err != nil {
		t.Fatalf("read after delete: %v", err)
	}
	if ok {
		t.Fatal("want ok=false after delete")
	}
}

func TestVault_M365AppPathTraversalRejected(t *testing.T) {
	fake := newFakeVault(3600)
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()
	c := newTestClient(t, srv)
	if err := c.WriteM365App(context.Background(), "../escape", "secret"); err == nil {
		t.Fatal("expected rejection of traversal segment")
	}
}

// F-1 (reviewer feedback, iteration 1): CP1 shipped WebSearchAppPath/
// ReadWebSearchApp untested; CP2 closes the gap with the full read/write/
// delete suite, mirroring TestM365App* above 1:1.

func TestWebSearchAppPath_Valid(t *testing.T) {
	path, err := WebSearchAppPath("tenant-a")
	if err != nil {
		t.Fatalf("WebSearchAppPath: %v", err)
	}
	want := "websearch/tenant-a/app"
	if path != want {
		t.Fatalf("got %q, want %q", path, want)
	}
}

func TestWebSearchAppPath_RejectsBadSegments(t *testing.T) {
	for _, tenant := range []string{"../escape", "", "ten ant", "has/slash", ".", ".."} {
		if _, err := WebSearchAppPath(tenant); err == nil {
			t.Errorf("expected rejection for tenant=%q", tenant)
		}
	}
}

func TestVault_WebSearchAppWriteReadRoundTrip(t *testing.T) {
	fake := newFakeVault(3600)
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()
	c := newTestClient(t, srv)
	ctx := context.Background()

	if err := c.WriteWebSearchApp(ctx, "tenant-a", "brave-key"); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, ok, err := c.ReadWebSearchApp(ctx, "tenant-a")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !ok {
		t.Fatal("want ok=true, got false")
	}
	if got != "brave-key" {
		t.Fatalf("round-trip mismatch: %q", got)
	}
}

func TestVault_WebSearchAppReadAbsentReturnsFalse(t *testing.T) {
	fake := newFakeVault(3600)
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()
	c := newTestClient(t, srv)

	_, ok, err := c.ReadWebSearchApp(context.Background(), "tenant-a")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatal("want ok=false for absent secret")
	}
}

func TestVault_WebSearchAppDeleteThenReadAbsent(t *testing.T) {
	fake := newFakeVault(3600)
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()
	c := newTestClient(t, srv)
	ctx := context.Background()

	if err := c.WriteWebSearchApp(ctx, "tenant-a", "to-delete"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := c.DeleteWebSearchApp(ctx, "tenant-a"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	_, ok, err := c.ReadWebSearchApp(ctx, "tenant-a")
	if err != nil {
		t.Fatalf("read after delete: %v", err)
	}
	if ok {
		t.Fatal("want ok=false after delete")
	}
}

func TestVault_WebSearchAppPathTraversalRejected(t *testing.T) {
	fake := newFakeVault(3600)
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()
	c := newTestClient(t, srv)
	if err := c.WriteWebSearchApp(context.Background(), "../escape", "key"); err == nil {
		t.Fatal("expected rejection of traversal segment")
	}
}

func mustRawMessage(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
