package policy

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

type recordingArchive struct {
	revisions []string
	err       error
}

func (r *recordingArchive) ArchivePolicyBundle(_ context.Context, revision string, snapshot []byte) error {
	if r.err != nil {
		return r.err
	}
	if _, err := VerifySnapshot(snapshot); err != nil {
		return err
	}
	r.revisions = append(r.revisions, revision)
	return nil
}

func get(t *testing.T, h http.Handler, etag string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/opa/bundles/aikonos.tar.gz", nil)
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestBundleServer_ServesOnlyArchivedRevisions(t *testing.T) {
	dir := writeTree(t, map[string]string{"a.rego": "package aikonos.a\n"})
	archive := &recordingArchive{}
	srv := NewBundleServer(dir, []string{"aikonos"}, archive, nil)

	if rec := get(t, srv, ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("before any reload: status %d, want 503", rec.Code)
	}

	var changes []string
	srv.OnChange = func(prev, next *Bundle) { changes = append(changes, next.Revision) }
	if changed, err := srv.Reload(context.Background()); err != nil || !changed {
		t.Fatalf("first reload = %v, %v", changed, err)
	}
	b := srv.Current()
	if len(archive.revisions) != 1 || archive.revisions[0] != b.Revision {
		t.Fatalf("archived %v, serving %s", archive.revisions, b.Revision)
	}

	rec := get(t, srv, "")
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), b.Tarball()) || rec.Header().Get("ETag") != b.ETag() {
		t.Fatalf("GET: status %d, etag %q", rec.Code, rec.Header().Get("ETag"))
	}
	if rec := get(t, srv, b.ETag()); rec.Code != http.StatusNotModified {
		t.Fatalf("conditional GET: status %d, want 304", rec.Code)
	}
	post := httptest.NewRecorder()
	srv.ServeHTTP(post, httptest.NewRequest(http.MethodPost, "/opa/bundles/aikonos.tar.gz", nil))
	if post.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST: status %d, want 405", post.Code)
	}

	// Unchanged files: no new archive write, no change event.
	if changed, err := srv.Reload(context.Background()); err != nil || changed {
		t.Fatalf("unchanged reload = %v, %v", changed, err)
	}
	if len(archive.revisions) != 1 || len(changes) != 1 {
		t.Fatalf("unchanged reload archived %v, changes %v", archive.revisions, changes)
	}
}

func TestBundleServer_UnarchivedRevisionIsNotServed(t *testing.T) {
	dir := writeTree(t, map[string]string{"a.rego": "package aikonos.a\n"})
	archive := &recordingArchive{}
	srv := NewBundleServer(dir, []string{"aikonos"}, archive, nil)
	if _, err := srv.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := srv.Current().Revision

	if err := os.WriteFile(filepath.Join(dir, "a.rego"), []byte("package aikonos.a\nallow := true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	archive.err = errors.New("object store down")
	if changed, err := srv.Reload(context.Background()); err == nil || changed {
		t.Fatalf("reload with a failing archive = %v, %v; want an error", changed, err)
	}
	if srv.Current().Revision != first {
		t.Fatal("a revision that could not be archived must not be served")
	}

	archive.err = nil
	if changed, err := srv.Reload(context.Background()); err != nil || !changed {
		t.Fatalf("reload after recovery = %v, %v", changed, err)
	}
	if srv.Current().Revision == first {
		t.Fatal("the new revision should be served once archived")
	}
}

func TestBundleServer_WithoutArchiveStillServes(t *testing.T) {
	srv := NewBundleServer(writeTree(t, map[string]string{"a.rego": "package aikonos.a\n"}), []string{"aikonos"}, nil, nil)
	if _, err := srv.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rec := get(t, srv, ""); rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
}
