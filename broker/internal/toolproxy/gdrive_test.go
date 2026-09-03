package toolproxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/adamdekan/aikonos/broker/internal/connector"
	"github.com/adamdekan/aikonos/broker/internal/secrets"
)

type fakeStore struct {
	missing bool
	at      string
}

func (f fakeStore) ReadConnector(_ context.Context, _, _, _ string) (*secrets.ConnectorToken, error) {
	if f.missing {
		return nil, secrets.ErrNotFound
	}
	return &secrets.ConnectorToken{AccessToken: f.at, RefreshToken: "rt", ExpiresAt: time.Now().Add(time.Hour)}, nil
}
func (fakeStore) WriteConnector(context.Context, string, string, string, *secrets.ConnectorToken) error {
	return nil
}
func (fakeStore) DeleteConnector(context.Context, string, string, string) error { return nil }
func (fakeStore) ProviderKeyPath(tenant, providerID string) (string, error) {
	return secrets.ProviderKeyPath(tenant, providerID)
}
func (fakeStore) ReadProviderKey(context.Context, string, string) (string, bool, error) {
	return "", false, nil
}
func (fakeStore) WriteProviderKey(context.Context, string, string, string) error { return nil }
func (fakeStore) DeleteProviderKey(context.Context, string, string) error        { return nil }
func (fakeStore) ReadM365App(context.Context, string) (string, bool, error)      { return "", false, nil }
func (fakeStore) WriteM365App(context.Context, string, string) error             { return nil }
func (fakeStore) DeleteM365App(context.Context, string) error                    { return nil }

func gdriveDeps(store secrets.Provider) connectorDeps {
	return connectorDeps{
		secrets: store,
		oauth: &connector.Registry{
			Credentials: map[connector.Provider]connector.AppCredentials{
				connector.ProviderGoogleDrive: {ClientID: "g-id", ClientSecret: "g-secret"},
				connector.ProviderOneDrive:    {ClientID: "m-id", ClientSecret: "m-secret"},
			},
		},
	}
}

// fakeDrive emulates the Drive v3 routes the handlers use.
func fakeDrive(t *testing.T) *httptest.Server {
	mux := http.NewServeMux()
	auth := func(r *http.Request) bool { return r.Header.Get("Authorization") == "Bearer test-at" }

	mux.HandleFunc("/drive/v3/files/", func(w http.ResponseWriter, r *http.Request) {
		if !auth(r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Query().Get("alt") == "media":
			_, _ = w.Write([]byte("hello from drive"))
		case strings.HasSuffix(r.URL.Path, "/export"):
			_, _ = w.Write([]byte("exported text"))
		case strings.HasSuffix(r.URL.Path, "/FLDR"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "FLDR", "name": "reports", "mimeType": "application/vnd.google-apps.folder",
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "FID", "name": "report.txt", "mimeType": "text/plain", "modifiedTime": "2026-01-01T00:00:00Z",
			})
		}
	})
	mux.HandleFunc("/drive/v3/files", func(w http.ResponseWriter, r *http.Request) {
		if !auth(r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if strings.Contains(r.URL.Query().Get("q"), "FLDR") {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"files": []map[string]any{{"id": "R1", "name": "q1.txt", "mimeType": "text/plain"}},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"files": []map[string]any{{"id": "F1", "name": "a.txt", "mimeType": "text/plain"}},
		})
	})
	mux.HandleFunc("/upload/drive/v3/files", func(w http.ResponseWriter, r *http.Request) {
		if !auth(r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "multipart/related") {
			t.Fatalf("expected multipart/related, got %q", ct)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "NEW", "name": "out.txt", "webViewLink": "https://drive.google.com/file/NEW",
		})
	})
	return httptest.NewServer(mux)
}

func withDriveBase(t *testing.T, srv *httptest.Server) {
	t.Helper()
	oa, ou := googleDriveAPIBase, googleDriveUploadBase
	googleDriveAPIBase = srv.URL + "/drive/v3"
	googleDriveUploadBase = srv.URL + "/upload/drive/v3"
	t.Cleanup(func() { googleDriveAPIBase, googleDriveUploadBase = oa, ou })
}

func req(args map[string]any) Request {
	return Request{TenantID: "t", UserID: "u", Args: args}
}

func TestGDriveRead_FileContent(t *testing.T) {
	srv := fakeDrive(t)
	defer srv.Close()
	withDriveBase(t, srv)

	h := newGDriveReadHandler(gdriveDeps(fakeStore{at: "test-at"}), WorkspaceConfig{Root: t.TempDir()})
	out, _, err := h(context.Background(), req(map[string]any{"file_id": "FID"}))
	if err != nil {
		t.Fatal(err)
	}
	if out["content"] != "hello from drive" || out["name"] != "report.txt" {
		t.Fatalf("unexpected read output: %+v", out)
	}
}

func TestGDriveRead_List(t *testing.T) {
	srv := fakeDrive(t)
	defer srv.Close()
	withDriveBase(t, srv)

	h := newGDriveReadHandler(gdriveDeps(fakeStore{at: "test-at"}), WorkspaceConfig{Root: t.TempDir()})
	out, _, err := h(context.Background(), req(map[string]any{"query": "name contains 'a'"}))
	if err != nil {
		t.Fatal(err)
	}
	if out["count"].(int) != 1 {
		t.Fatalf("expected 1 file, got %+v", out)
	}
}

// TestGDriveList_WithFolderID mirrors TestOneDriveList_WithPath: a read
// selector that resolves to a folder falls through to a listing scoped to
// that folder's children (dispatch rule documented in cloudfile.go).
func TestGDriveList_WithFolderID(t *testing.T) {
	srv := fakeDrive(t)
	defer srv.Close()
	withDriveBase(t, srv)

	h := newGDriveReadHandler(gdriveDeps(fakeStore{at: "test-at"}), WorkspaceConfig{Root: t.TempDir()})
	out, _, err := h(context.Background(), req(map[string]any{"file_id": "FLDR"}))
	if err != nil {
		t.Fatal(err)
	}
	if out["count"].(int) != 1 {
		t.Fatalf("expected 1 item under folder FLDR, got %+v", out)
	}
}

func TestGDriveWrite_Upload(t *testing.T) {
	srv := fakeDrive(t)
	defer srv.Close()
	withDriveBase(t, srv)

	h := newGDriveWriteHandler(gdriveDeps(fakeStore{at: "test-at"}), WorkspaceConfig{Root: t.TempDir()})
	out, _, err := h(context.Background(), req(map[string]any{"name": "out.txt", "content": "data"}))
	if err != nil {
		t.Fatal(err)
	}
	if out["file_id"] != "NEW" || out["web_url"] == "" {
		t.Fatalf("unexpected write output: %+v", out)
	}
}

func TestGDriveWrite_MissingName(t *testing.T) {
	h := newGDriveWriteHandler(gdriveDeps(fakeStore{at: "test-at"}), WorkspaceConfig{Root: t.TempDir()})
	if _, _, err := h(context.Background(), req(map[string]any{"content": "x"})); err == nil {
		t.Fatal("expected missing-name error")
	}
}

func TestGDrive_NotConnected(t *testing.T) {
	h := newGDriveReadHandler(gdriveDeps(fakeStore{missing: true}), WorkspaceConfig{Root: t.TempDir()})
	_, _, err := h(context.Background(), req(map[string]any{"file_id": "FID"}))
	if err == nil || !strings.Contains(err.Error(), "not connected") {
		t.Fatalf("expected not-connected error, got %v", err)
	}
}

func TestGDrive_RequiresUserContext(t *testing.T) {
	h := newGDriveReadHandler(gdriveDeps(fakeStore{at: "test-at"}), WorkspaceConfig{Root: t.TempDir()})
	_, _, err := h(context.Background(), Request{TenantID: "t", Args: map[string]any{"file_id": "x"}})
	if err == nil || !strings.Contains(err.Error(), "user context") {
		t.Fatalf("expected user-context error, got %v", err)
	}
}

// TestCloudFile_SchemaConformance pins F45: gdrive and onedrive must emit the
// exact same output key set per operation (read/write/list) so an LLM
// consumer never needs provider-specific prompting. Key sets are asserted
// for exact equality, not mere presence — a reintroduced legacy key (e.g.
// item_id) must fail this test.
func TestCloudFile_SchemaConformance(t *testing.T) {
	gsrv := fakeDrive(t)
	defer gsrv.Close()
	withDriveBase(t, gsrv)
	osrv := fakeGraph(t)
	defer osrv.Close()
	withGraphBase(t, osrv)

	store := fakeStore{at: "test-at"}
	deps := gdriveDeps(store)
	ws := WorkspaceConfig{Root: t.TempDir()}

	cases := []struct {
		name     string
		gdrive   map[string]any
		onedrive map[string]any
		want     []string
	}{
		{
			name:     "read",
			gdrive:   mustHandle(t, newGDriveReadHandler(deps, ws), req(map[string]any{"file_id": "FID"})),
			onedrive: mustHandle(t, newOneDriveReadHandler(deps, ws), req(map[string]any{"item_id": "I1"})),
			want:     []string{"file_id", "name", "mime_type", "modified_time", "content", "content_length"},
		},
		{
			name:     "write",
			gdrive:   mustHandle(t, newGDriveWriteHandler(deps, ws), req(map[string]any{"name": "out.txt", "content": "data"})),
			onedrive: mustHandle(t, newOneDriveWriteHandler(deps, ws), req(map[string]any{"path": "docs/out.txt", "content": "data"})),
			want:     []string{"file_id", "name", "web_url", "bytes_written"},
		},
		{
			name:     "list",
			gdrive:   firstListItem(t, mustHandle(t, newGDriveReadHandler(deps, ws), req(map[string]any{"query": "name contains 'a'"}))),
			onedrive: firstListItem(t, mustHandle(t, newOneDriveReadHandler(deps, ws), req(map[string]any{}))),
			want:     []string{"file_id", "name", "mime_type", "modified_time"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertExactKeys(t, "gdrive."+tc.name, tc.gdrive, tc.want)
			assertExactKeys(t, "onedrive."+tc.name, tc.onedrive, tc.want)
		})
	}
}

// firstListItem unwraps a list-op handler's {"files": [...], "count": N}
// envelope to the first per-item map, which is what actually carries the
// provider-normalized key set (listMap's output).
func firstListItem(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	files, _ := out["files"].([]any)
	if len(files) == 0 {
		t.Fatalf("expected at least one file in list output: %+v", out)
	}
	item, ok := files[0].(map[string]any)
	if !ok {
		t.Fatalf("list item is not a map[string]any: %+v", files[0])
	}
	return item
}

// assertExactKeys fails unless got's key set equals want exactly — presence
// alone would let a reintroduced legacy key (e.g. item_id) pass silently.
func assertExactKeys(t *testing.T, label string, got map[string]any, want []string) {
	t.Helper()
	gotKeys := make([]string, 0, len(got))
	for k := range got {
		gotKeys = append(gotKeys, k)
	}
	sort.Strings(gotKeys)
	wantSorted := append([]string(nil), want...)
	sort.Strings(wantSorted)
	if !reflect.DeepEqual(gotKeys, wantSorted) {
		t.Errorf("%s key set = %v, want %v (value: %+v)", label, gotKeys, wantSorted, got)
	}
}

func mustHandle(t *testing.T, h Handler, r Request) map[string]any {
	t.Helper()
	out, _, err := h(context.Background(), r)
	if err != nil {
		t.Fatalf("handler failed: %v", err)
	}
	return out
}

// TestHandlerConnectorID_ResolvesViaPlugin verifies that the provider constants
// used by the gdrive and onedrive handlers resolve their connector ids through
// the plugin registry (connector.ProviderGoogleDrive.ConnectorID() and
// connector.ProviderOneDrive.ConnectorID()), not through hard-coded literals.
// If the plugin registry is not seeded or is broken, these return "".
func TestHandlerConnectorID_ResolvesViaPlugin(t *testing.T) {
	cases := []struct {
		provider connector.Provider
		wantID   string
	}{
		{connector.ProviderGoogleDrive, "gdrive"},
		{connector.ProviderOneDrive, "onedrive"},
	}
	for _, tc := range cases {
		got := tc.provider.ConnectorID()
		if got != tc.wantID {
			t.Errorf("provider %q: ConnectorID() = %q, want %q (plugin registry not seeded or wrong)", tc.provider, got, tc.wantID)
		}
	}
}
