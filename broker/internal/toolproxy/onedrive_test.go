package toolproxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeGraph emulates the Microsoft Graph routes the OneDrive handlers use.
func fakeGraph(t *testing.T) *httptest.Server {
	mux := http.NewServeMux()
	auth := func(r *http.Request) bool { return r.Header.Get("Authorization") == "Bearer test-at" }

	// Item content (by id or by path) — registered first via suffix checks below.
	mux.HandleFunc("/v1.0/me/drive/root/children", func(w http.ResponseWriter, r *http.Request) {
		if !auth(r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"value": []map[string]any{{"id": "I1", "name": "a.txt"}, {"id": "I2", "name": "b.txt"}},
		})
	})
	mux.HandleFunc("/v1.0/me/drive/root:/reports:/children", func(w http.ResponseWriter, r *http.Request) {
		if !auth(r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"value": []map[string]any{{"id": "R1", "name": "q1.txt"}},
		})
	})
	mux.HandleFunc("/v1.0/me/drive/", func(w http.ResponseWriter, r *http.Request) {
		if !auth(r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/content") && r.Method == http.MethodGet:
			_, _ = w.Write([]byte("onedrive content"))
		case strings.HasSuffix(r.URL.Path, "/content") && r.Method == http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "PUT1", "name": "out.txt", "webUrl": "https://1drv.ms/out", "size": len(body),
			})
		case r.URL.Path == "/v1.0/me/drive/root:/reports":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "RDIR", "name": "reports", "folder": map[string]any{"childCount": 1},
			})
		default: // metadata
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "I1", "name": "report.txt", "lastModifiedDateTime": "2026-01-01T00:00:00Z",
				"file": map[string]any{"mimeType": "text/plain"},
			})
		}
	})
	return httptest.NewServer(mux)
}

func withGraphBase(t *testing.T, srv *httptest.Server) {
	t.Helper()
	orig := graphAPIBase
	graphAPIBase = srv.URL + "/v1.0"
	t.Cleanup(func() { graphAPIBase = orig })
}

func TestOneDriveRead_ItemContent(t *testing.T) {
	srv := fakeGraph(t)
	defer srv.Close()
	withGraphBase(t, srv)

	h := newOneDriveReadHandler(gdriveDeps(fakeStore{at: "test-at"}), WorkspaceConfig{Root: t.TempDir()})
	out, _, err := h(context.Background(), req(map[string]any{"item_id": "I1"}))
	if err != nil {
		t.Fatal(err)
	}
	if out["content"] != "onedrive content" || out["file_id"] != "I1" || out["mime_type"] != "text/plain" {
		t.Fatalf("unexpected read output: %+v", out)
	}
}

func TestOneDriveRead_List(t *testing.T) {
	srv := fakeGraph(t)
	defer srv.Close()
	withGraphBase(t, srv)

	h := newOneDriveReadHandler(gdriveDeps(fakeStore{at: "test-at"}), WorkspaceConfig{Root: t.TempDir()})
	out, _, err := h(context.Background(), req(map[string]any{}))
	if err != nil {
		t.Fatal(err)
	}
	if out["count"].(int) != 2 {
		t.Fatalf("expected 2 items, got %+v", out)
	}
}

func TestOneDriveList_WithPath(t *testing.T) {
	srv := fakeGraph(t)
	defer srv.Close()
	withGraphBase(t, srv)

	h := newOneDriveReadHandler(gdriveDeps(fakeStore{at: "test-at"}), WorkspaceConfig{Root: t.TempDir()})
	out, _, err := h(context.Background(), req(map[string]any{"path": "reports"}))
	if err != nil {
		t.Fatal(err)
	}
	if out["count"].(int) != 1 {
		t.Fatalf("expected 1 item under /reports, got %+v", out)
	}
}

func TestOneDriveWrite_Upload(t *testing.T) {
	srv := fakeGraph(t)
	defer srv.Close()
	withGraphBase(t, srv)

	h := newOneDriveWriteHandler(gdriveDeps(fakeStore{at: "test-at"}), WorkspaceConfig{Root: t.TempDir()})
	out, _, err := h(context.Background(), req(map[string]any{"path": "docs/out.txt", "content": "data"}))
	if err != nil {
		t.Fatal(err)
	}
	if out["file_id"] != "PUT1" || out["web_url"] == "" {
		t.Fatalf("unexpected write output: %+v", out)
	}
}

func TestOneDriveWrite_MissingPath(t *testing.T) {
	h := newOneDriveWriteHandler(gdriveDeps(fakeStore{at: "test-at"}), WorkspaceConfig{Root: t.TempDir()})
	if _, _, err := h(context.Background(), req(map[string]any{"content": "x"})); err == nil {
		t.Fatal("expected missing-path error")
	}
}

func TestEscapeGraphPath(t *testing.T) {
	if got := escapeGraphPath("/folder/my file.txt"); got != "folder/my%20file.txt" {
		t.Fatalf("escapeGraphPath = %q", got)
	}
}
