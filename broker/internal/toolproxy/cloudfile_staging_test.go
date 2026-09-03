// broker/internal/toolproxy/cloudfile_staging_test.go
//
// Checkpoint 5: save_to on *.read stages
// bytes to the workspace instead of echoing content; from_path on *.write
// uploads workspace bytes instead of taking content inline. Covers the
// staging contract's loud-overflow, native-file-rejection, and
// content/from_path-exclusivity semantics for both providers.
package toolproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/adamdekan/aikonos/broker/internal/workspacefs"
)

// stageFile writes data into the workspace at Root/tenant/user/rel, creating
// parent dirs — simulates a prior write the *.write from_path test then reads.
func stageFile(t *testing.T, ws WorkspaceConfig, tenant, user, rel string, data []byte) {
	t.Helper()
	full := filepath.Join(ws.Root, tenant, user, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, data, 0o640); err != nil {
		t.Fatal(err)
	}
}

func TestGDriveRead_SaveTo_WritesWorkspaceMetadataOnly(t *testing.T) {
	srv := fakeDrive(t)
	defer srv.Close()
	withDriveBase(t, srv)

	ws := WorkspaceConfig{Root: t.TempDir()}
	h := newGDriveReadHandler(gdriveDeps(fakeStore{at: "test-at"}), ws)
	out, _, err := h(context.Background(), req(map[string]any{"file_id": "FID", "save_to": "in/report.txt"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, has := out["content"]; has {
		t.Fatalf("staged read result must not carry content: %+v", out)
	}
	if _, has := out["content_length"]; has {
		t.Fatalf("staged read result must not carry content_length: %+v", out)
	}
	if out["path"] != "in/report.txt" {
		t.Fatalf("unexpected staged read output: %+v", out)
	}
	got, err := os.ReadFile(filepath.Join(ws.Root, "t", "u", "in", "report.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello from drive" {
		t.Fatalf("workspace file content = %q, want %q", got, "hello from drive")
	}
}

func TestOneDriveRead_SaveTo_WritesWorkspaceMetadataOnly(t *testing.T) {
	srv := fakeGraph(t)
	defer srv.Close()
	withGraphBase(t, srv)

	ws := WorkspaceConfig{Root: t.TempDir()}
	h := newOneDriveReadHandler(gdriveDeps(fakeStore{at: "test-at"}), ws)
	out, _, err := h(context.Background(), req(map[string]any{"item_id": "I1", "save_to": "in/report.txt"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, has := out["content"]; has {
		t.Fatalf("staged read result must not carry content: %+v", out)
	}
	got, err := os.ReadFile(filepath.Join(ws.Root, "t", "u", "in", "report.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "onedrive content" {
		t.Fatalf("workspace file content = %q, want %q", got, "onedrive content")
	}
}

// fakeDriveOversize serves a single BIG file whose content exceeds the
// workspace cap, standing in for a real Drive response too large to stage.
func fakeDriveOversize(big []byte) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/drive/v3/files/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("alt") == "media" {
			_, _ = w.Write(big)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "BIG", "name": "big.bin", "mimeType": "application/octet-stream",
		})
	})
	return httptest.NewServer(mux)
}

func TestGDriveRead_SaveTo_Oversize_LoudErrorNoPartialWrite(t *testing.T) {
	big := bytes.Repeat([]byte("a"), workspacefs.MaxFileBytes+1024)
	srv := fakeDriveOversize(big)
	defer srv.Close()
	withDriveBase(t, srv)

	ws := WorkspaceConfig{Root: t.TempDir()}
	h := newGDriveReadHandler(gdriveDeps(fakeStore{at: "test-at"}), ws)
	_, _, err := h(context.Background(), req(map[string]any{"file_id": "BIG", "save_to": "in/big.bin"}))
	if err == nil {
		t.Fatal("expected a loud overflow error for an oversize staged download")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected error naming size and cap, got %v", err)
	}
	target := filepath.Join(ws.Root, "t", "u", "in", "big.bin")
	if _, statErr := os.Stat(target); !os.IsNotExist(statErr) {
		t.Fatalf("expected no partial workspace write at %s, stat err = %v", target, statErr)
	}
}

// fakeDriveNative serves a single native-Google-Docs file (DOC1) whose
// content route is the lossy text/plain export, standing in for the case
// save_to must refuse to stage.
func fakeDriveNative() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/drive/v3/files/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/export") {
			_, _ = w.Write([]byte("exported text"))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "DOC1", "name": "plan.doc", "mimeType": "application/vnd.google-apps.document",
		})
	})
	return httptest.NewServer(mux)
}

func TestGDriveRead_SaveTo_NativeGoogleFile_ExplicitError(t *testing.T) {
	srv := fakeDriveNative()
	defer srv.Close()
	withDriveBase(t, srv)

	ws := WorkspaceConfig{Root: t.TempDir()}
	h := newGDriveReadHandler(gdriveDeps(fakeStore{at: "test-at"}), ws)
	_, _, err := h(context.Background(), req(map[string]any{"file_id": "DOC1", "save_to": "in/plan.docx"}))
	if err == nil {
		t.Fatal("expected an explicit error staging a native Google file")
	}
	if !strings.Contains(err.Error(), "staged") {
		t.Fatalf("expected error naming the staging deferral, got %v", err)
	}
	target := filepath.Join(ws.Root, "t", "u", "in", "plan.docx")
	if _, statErr := os.Stat(target); !os.IsNotExist(statErr) {
		t.Fatalf("expected no lossy-export write at %s, stat err = %v", target, statErr)
	}
}

// Without save_to, the existing lossy-export behavior must be unchanged.
func TestGDriveRead_NativeGoogleFile_NoSaveTo_StillExports(t *testing.T) {
	srv := fakeDriveNative()
	defer srv.Close()
	withDriveBase(t, srv)

	ws := WorkspaceConfig{Root: t.TempDir()}
	h := newGDriveReadHandler(gdriveDeps(fakeStore{at: "test-at"}), ws)
	out, _, err := h(context.Background(), req(map[string]any{"file_id": "DOC1"}))
	if err != nil {
		t.Fatal(err)
	}
	if out["content"] != "exported text" {
		t.Fatalf("unexpected non-staged native read output: %+v", out)
	}
}

func TestGDriveWrite_FromPath_UploadsWorkspaceBytes(t *testing.T) {
	srv := fakeDrive(t)
	defer srv.Close()
	withDriveBase(t, srv)

	ws := WorkspaceConfig{Root: t.TempDir()}
	stageFile(t, ws, "t", "u", "out/report.bin", []byte("binary payload"))

	h := newGDriveWriteHandler(gdriveDeps(fakeStore{at: "test-at"}), ws)
	out, _, err := h(context.Background(), req(map[string]any{"name": "report.bin", "from_path": "out/report.bin"}))
	if err != nil {
		t.Fatal(err)
	}
	if out["file_id"] != "NEW" {
		t.Fatalf("unexpected write output: %+v", out)
	}
}

func TestGDriveWrite_ContentAndFromPath_MutuallyExclusive(t *testing.T) {
	ws := WorkspaceConfig{Root: t.TempDir()}
	h := newGDriveWriteHandler(gdriveDeps(fakeStore{at: "test-at"}), ws)
	_, _, err := h(context.Background(), req(map[string]any{
		"name": "out.txt", "content": "data", "from_path": "in/data.bin",
	}))
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected mutually-exclusive error, got %v", err)
	}
}

func TestOneDriveWrite_ContentAndFromPath_MutuallyExclusive(t *testing.T) {
	ws := WorkspaceConfig{Root: t.TempDir()}
	h := newOneDriveWriteHandler(gdriveDeps(fakeStore{at: "test-at"}), ws)
	_, _, err := h(context.Background(), req(map[string]any{
		"path": "out.txt", "content": "data", "from_path": "in/data.bin",
	}))
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected mutually-exclusive error, got %v", err)
	}
}

func TestOneDriveWrite_FromPath_BinarySafeContentType(t *testing.T) {
	var gotCT string
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.0/me/drive/root:/out.bin:/content", func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "PUT1", "name": "out.bin", "webUrl": "https://1drv.ms/out"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	withGraphBase(t, srv)

	ws := WorkspaceConfig{Root: t.TempDir()}
	stageFile(t, ws, "t", "u", "stage/out.bin", []byte{0x00, 0x01, 0x02, 0xFF})

	h := newOneDriveWriteHandler(gdriveDeps(fakeStore{at: "test-at"}), ws)
	if _, _, err := h(context.Background(), req(map[string]any{"path": "out.bin", "from_path": "stage/out.bin"})); err != nil {
		t.Fatal(err)
	}
	if gotCT != "application/octet-stream" {
		t.Fatalf("expected binary-safe content type for an unrecognized extension, got %q", gotCT)
	}
}

func TestContentTypeForPath(t *testing.T) {
	if got := contentTypeForPath("report.bin"); got != "application/octet-stream" {
		t.Fatalf("contentTypeForPath(report.bin) = %q, want application/octet-stream", got)
	}
	if got := contentTypeForPath("photo.png"); got != "image/png" {
		t.Fatalf("contentTypeForPath(photo.png) = %q, want image/png", got)
	}
}
