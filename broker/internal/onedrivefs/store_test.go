package onedrivefs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/adamdekan/aikonos/broker/internal/workspacefs"
)

// --- test scaffolding -------------------------------------------------

// fakeTokenSource returns a fixed token, or a fixed error when set.
type fakeTokenSource struct {
	tok string
	err error
}

func (f fakeTokenSource) FreshAccessToken(ctx context.Context, tenant, user string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.tok, nil
}

const testToken = "test-at"

func testStore(srv *httptest.Server) *Store {
	return &Store{Tokens: fakeTokenSource{tok: testToken}, HTTP: srv.Client()}
}

func withTestGraphBase(t *testing.T, srv *httptest.Server) {
	t.Helper()
	orig := graphBase
	graphBase = srv.URL + "/v1.0"
	t.Cleanup(func() { graphBase = orig })
}

func withUploadLimits(t *testing.T, simpleMax, chunk int64) {
	t.Helper()
	origMax, origChunk := simpleUploadMax, uploadChunkSize
	simpleUploadMax, uploadChunkSize = simpleMax, chunk
	t.Cleanup(func() { simpleUploadMax, uploadChunkSize = origMax, origChunk })
}

// callRecorder tallies "METHOD path" hits so tests can assert a request
// never happened (name validation, oversize-read short-circuit) or happened
// an exact number of times (429 retry-once, pagination).
type callRecorder struct {
	mu    sync.Mutex
	calls map[string]int
}

func (c *callRecorder) record(r *http.Request) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.calls == nil {
		c.calls = map[string]int{}
	}
	key := r.Method + " " + r.URL.Path
	c.calls[key]++
	return c.calls[key]
}

func (c *callRecorder) count(method, path string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[method+" "+path]
}

func requireAuth(t *testing.T, r *http.Request) bool {
	if r.Header.Get("Authorization") != "Bearer "+testToken {
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// --- 409 -> ErrExists (Mkdir leaf conflict) ----------------------------

func TestMkdir_LeafConflict_ErrExists(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.0/drives/DRV1/items/ROOT/children", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(t, r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusConflict)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	withTestGraphBase(t, srv)

	root := testStore(srv).WithRoot("DRV1", "ROOT")
	err := root.Mkdir(context.Background(), "t", "u", "newdir")
	if !errors.Is(err, workspacefs.ErrExists) {
		t.Fatalf("Mkdir leaf conflict: got %v, want ErrExists", err)
	}
}

// --- 404 -> fs.ErrNotExist (Read of missing file) ----------------------

func TestRead_Missing_ErrNotExist(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.0/drives/DRV1/items/ROOT:/missing.txt", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(t, r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	withTestGraphBase(t, srv)

	root := testStore(srv).WithRoot("DRV1", "ROOT")
	_, _, err := root.Read(context.Background(), "t", "u", "missing.txt")
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Read missing: got %v, want fs.ErrNotExist", err)
	}
}

// --- missing dir lists empty --------------------------------------------

func TestListDir_MissingDir_ListsEmpty(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.0/drives/DRV1/items/ROOT:/noexist:/children", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(t, r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	withTestGraphBase(t, srv)

	root := testStore(srv).WithRoot("DRV1", "ROOT")
	out, err := root.ListDir(context.Background(), "t", "u", "noexist", false)
	if err != nil {
		t.Fatalf("ListDir missing dir: unexpected error %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("ListDir missing dir: got %d entries, want 0", len(out))
	}
}

// --- childCount>0 delete -> ErrNotEmpty (no DELETE issued) --------------

func TestDelete_NonEmptyDir_ErrNotEmpty(t *testing.T) {
	rec := &callRecorder{}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.0/drives/DRV1/items/ROOT:/full", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(t, r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		rec.record(r)
		if r.Method == http.MethodDelete {
			t.Fatalf("DELETE must not be issued when childCount > 0")
		}
		writeJSON(w, map[string]any{"id": "F1", "name": "full", "folder": map[string]any{"childCount": 2}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	withTestGraphBase(t, srv)

	root := testStore(srv).WithRoot("DRV1", "ROOT")
	err := root.Delete(context.Background(), "t", "u", "full")
	if !errors.Is(err, workspacefs.ErrNotEmpty) {
		t.Fatalf("Delete non-empty dir: got %v, want ErrNotEmpty", err)
	}
	if got := rec.count("GET", "/v1.0/drives/DRV1/items/ROOT:/full"); got != 1 {
		t.Fatalf("expected exactly 1 metadata GET, got %d", got)
	}
}

// --- oversize read: ErrTooLarge before any content fetch ----------------

func TestRead_Oversize_ErrTooLarge_NoContentFetch(t *testing.T) {
	contentHits := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.0/drives/DRV1/items/ROOT:/big.bin", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(t, r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		writeJSON(w, map[string]any{"id": "F1", "name": "big.bin", "size": 11 * 1024 * 1024})
	})
	mux.HandleFunc("/v1.0/drives/DRV1/items/ROOT:/big.bin:/content", func(w http.ResponseWriter, r *http.Request) {
		contentHits++
		w.Write([]byte("should never be fetched"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	withTestGraphBase(t, srv)

	root := testStore(srv).WithRoot("DRV1", "ROOT")
	_, _, err := root.Read(context.Background(), "t", "u", "big.bin")
	if !errors.Is(err, workspacefs.ErrTooLarge) {
		t.Fatalf("Read oversize: got %v, want ErrTooLarge", err)
	}
	if contentHits != 0 {
		t.Fatalf("Read oversize: content endpoint hit %d times, want 0", contentHits)
	}
}

// --- Read of a folder path -> ErrInvalidPath ----------------------------

func TestRead_FolderFacet_ErrInvalidPath(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.0/drives/DRV1/items/ROOT:/reports", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(t, r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		writeJSON(w, map[string]any{"id": "F1", "name": "reports", "folder": map[string]any{"childCount": 0}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	withTestGraphBase(t, srv)

	root := testStore(srv).WithRoot("DRV1", "ROOT")
	_, _, err := root.Read(context.Background(), "t", "u", "reports")
	if !errors.Is(err, workspacefs.ErrInvalidPath) {
		t.Fatalf("Read folder: got %v, want ErrInvalidPath", err)
	}
}

// --- oversize write pre-check: ErrTooLarge, zero requests ---------------

func TestWrite_Oversize_ErrTooLarge_NoRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("no HTTP request expected, got %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()
	withTestGraphBase(t, srv)

	root := testStore(srv).WithRoot("DRV1", "ROOT")
	data := make([]byte, workspacefs.MaxFileBytes+1)
	_, err := root.Write(context.Background(), "t", "u", "big.bin", data)
	if !errors.Is(err, workspacefs.ErrTooLarge) {
		t.Fatalf("Write oversize: got %v, want ErrTooLarge", err)
	}
}

// --- simple PUT for <= simpleUploadMax -----------------------------------

func TestWrite_Simple_UsesContentPUT(t *testing.T) {
	putHits := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.0/drives/DRV1/items/ROOT:/out.txt:/content", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(t, r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodPut {
			t.Fatalf("expected PUT, got %s", r.Method)
		}
		putHits++
		body, _ := io.ReadAll(r.Body)
		writeJSON(w, map[string]any{"id": "F1", "name": "out.txt", "size": len(body), "lastModifiedDateTime": "2026-01-01T00:00:00Z"})
	})
	mux.HandleFunc("/v1.0/drives/DRV1/items/ROOT:/out.txt:/createUploadSession", func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("createUploadSession must not be hit for a small write")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	withTestGraphBase(t, srv)

	root := testStore(srv).WithRoot("DRV1", "ROOT")
	info, err := root.Write(context.Background(), "t", "u", "out.txt", []byte("hello"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if putHits != 1 {
		t.Fatalf("expected exactly 1 PUT, got %d", putHits)
	}
	if info.Size != 5 {
		t.Fatalf("unexpected size: %d", info.Size)
	}
}

// --- chunked upload session for > simpleUploadMax ------------------------

func TestWrite_Session_ChunksContentRange(t *testing.T) {
	withUploadLimits(t, 5, 3) // simpleUploadMax=5 bytes, uploadChunkSize=3 bytes

	var mu sync.Mutex
	var ranges []string
	var chunkBodies [][]byte

	// srv is forward-declared so the createUploadSession handler (registered
	// before the server exists) can close over its URL — the closure runs at
	// request time, after srv has been assigned below.
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.0/drives/DRV1/items/ROOT:/big.bin:/createUploadSession", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(t, r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		writeJSON(w, map[string]any{"uploadUrl": srv.URL + "/upload/session1"})
	})
	mux.HandleFunc("/upload/session1", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Fatalf("expected PUT to upload session, got %s", r.Method)
		}
		mu.Lock()
		ranges = append(ranges, r.Header.Get("Content-Range"))
		body, _ := io.ReadAll(r.Body)
		chunkBodies = append(chunkBodies, body)
		mu.Unlock()
		cr := r.Header.Get("Content-Range")
		var start, end, total int
		fmt.Sscanf(cr, "bytes %d-%d/%d", &start, &end, &total)
		if end+1 == total {
			writeJSON(w, map[string]any{"id": "F1", "name": "big.bin", "size": total, "lastModifiedDateTime": "2026-01-01T00:00:00Z"})
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()
	withTestGraphBase(t, srv)

	root := testStore(srv).WithRoot("DRV1", "ROOT")
	data := []byte("0123456789") // 10 bytes, chunk=3 -> 4 chunks: 0-2,3-5,6-8,9-9
	info, err := root.Write(context.Background(), "t", "u", "big.bin", data)
	if err != nil {
		t.Fatalf("Write (session): %v", err)
	}
	wantRanges := []string{"bytes 0-2/10", "bytes 3-5/10", "bytes 6-8/10", "bytes 9-9/10"}
	mu.Lock()
	gotRanges := append([]string(nil), ranges...)
	gotBodies := append([][]byte(nil), chunkBodies...)
	mu.Unlock()
	if len(gotRanges) != len(wantRanges) {
		t.Fatalf("Content-Range sequence: got %v, want %v", gotRanges, wantRanges)
	}
	for i, want := range wantRanges {
		if gotRanges[i] != want {
			t.Fatalf("Content-Range[%d]: got %q, want %q", i, gotRanges[i], want)
		}
	}
	if string(gotBodies[0]) != "012" || string(gotBodies[3]) != "9" {
		t.Fatalf("unexpected chunk bodies: %v", gotBodies)
	}
	if info.Size != 10 {
		t.Fatalf("unexpected final size: %d", info.Size)
	}
}

// TestSessionUpload_Chunk429_ErrUnavailable pins F-3 (CP4 review finding,
// closed in CP5): a mid-upload 429 on a chunk PUT must map to
// workspacefs.ErrUnavailable, not a generic non-sentinel error — chunk PUTs
// bypass doWithRetryFor429 (the session's uploadUrl is pre-signed, no
// Authorization header, and a chunk write is not the safe-to-retry
// idempotent GET that helper covers), so classifyStatus must classify 429
// exactly like the 5xx case it already handles.
func TestSessionUpload_Chunk429_ErrUnavailable(t *testing.T) {
	withUploadLimits(t, 5, 3) // simpleUploadMax=5 bytes, uploadChunkSize=3 bytes -> forces the session path

	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.0/drives/DRV1/items/ROOT:/big.bin:/createUploadSession", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(t, r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		writeJSON(w, map[string]any{"uploadUrl": srv.URL + "/upload/session1"})
	})
	hits := 0
	mux.HandleFunc("/upload/session1", func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusTooManyRequests)
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()
	withTestGraphBase(t, srv)

	root := testStore(srv).WithRoot("DRV1", "ROOT")
	_, err := root.Write(context.Background(), "t", "u", "big.bin", []byte("0123456789"))
	if !errors.Is(err, workspacefs.ErrUnavailable) {
		t.Fatalf("chunk PUT 429: got %v, want ErrUnavailable", err)
	}
	if hits != 1 {
		t.Fatalf("expected exactly 1 chunk attempt (no retry — uploads aren't idempotent), got %d", hits)
	}
}

func TestUploadChunkSize_DefaultIsMultipleOf320KiB(t *testing.T) {
	const unit = 320 * 1024
	if uploadChunkSize%unit != 0 || uploadChunkSize == 0 {
		t.Fatalf("default uploadChunkSize %d is not a positive multiple of 320 KiB", uploadChunkSize)
	}
	if simpleUploadMax != 4<<20 {
		t.Fatalf("default simpleUploadMax = %d, want 4 MiB", simpleUploadMax)
	}
}

// --- pagination: two pages concatenated ---------------------------------

func TestListDir_Pagination_ConcatenatesPages(t *testing.T) {
	// srv is forward-declared so the first-page handler (registered before
	// the server exists) can close over its URL for the nextLink — the
	// closure runs at request time, after srv has been assigned below.
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.0/drives/DRV1/items/ROOT/children", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(t, r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		writeJSON(w, map[string]any{
			"value":           []map[string]any{{"id": "I1", "name": "a.txt"}},
			"@odata.nextLink": srv.URL + "/v1.0/page2",
		})
	})
	mux.HandleFunc("/v1.0/page2", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(t, r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		writeJSON(w, map[string]any{
			"value": []map[string]any{{"id": "I2", "name": "b.txt"}},
		})
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()
	withTestGraphBase(t, srv)

	root := testStore(srv).WithRoot("DRV1", "ROOT")
	out, err := root.ListDir(context.Background(), "t", "u", "", false)
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 entries across 2 pages, got %d: %+v", len(out), out)
	}
	names := map[string]bool{out[0].Path: true, out[1].Path: true}
	if !names["a.txt"] || !names["b.txt"] {
		t.Fatalf("unexpected entries: %+v", out)
	}
}

// --- BFS caps: entry count truncates without error ----------------------

func TestListDir_Recursive_EntryCapTruncates(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(t, r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		// Every children request returns 600 plain files (no subdirs), well
		// over bfsMaxEntries — proves the cap truncates without erroring.
		value := make([]map[string]any, 600)
		for i := range value {
			value[i] = map[string]any{"id": fmt.Sprintf("F%d", i), "name": fmt.Sprintf("f%d.txt", i)}
		}
		writeJSON(w, map[string]any{"value": value})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	withTestGraphBase(t, srv)

	root := testStore(srv).WithRoot("DRV1", "ROOT")
	out, err := root.ListDir(context.Background(), "t", "u", "", true)
	if err != nil {
		t.Fatalf("ListDir recursive: unexpected error %v", err)
	}
	if len(out) != bfsMaxEntries {
		t.Fatalf("expected exactly %d entries (cap), got %d", bfsMaxEntries, len(out))
	}
}

// --- BFS caps: depth truncates without error -----------------------------

func TestListDir_Recursive_DepthCapTruncates(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(t, r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		// Every children request returns exactly one subdirectory named "d",
		// so the tree nests infinitely unless bfsMaxDepth stops recursion.
		writeJSON(w, map[string]any{
			"value": []map[string]any{{"id": "D", "name": "d", "folder": map[string]any{"childCount": 1}}},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	withTestGraphBase(t, srv)

	root := testStore(srv).WithRoot("DRV1", "ROOT")
	out, err := root.ListDir(context.Background(), "t", "u", "", true)
	if err != nil {
		t.Fatalf("ListDir recursive depth: unexpected error %v", err)
	}
	if len(out) == 0 {
		t.Fatalf("expected at least one entry")
	}
	if len(out) > bfsMaxDepth+1 {
		t.Fatalf("BFS depth cap did not bound the walk: got %d entries", len(out))
	}
}

// --- name validation rejects before any HTTP call ------------------------

func TestNameValidation_RejectsBeforeHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("no HTTP request expected for an invalid name, got %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()
	withTestGraphBase(t, srv)

	root := testStore(srv).WithRoot("DRV1", "ROOT")
	cases := []string{
		"bad:name.txt",
		"bad*name.txt",
		"bad?name.txt",
		"bad<name>.txt",
		"bad|name.txt",
		"bad\"name.txt",
		"trailing.",
		// workspacefs.CleanRel trims OUTER whitespace on the whole path
		// before we ever see it (same as the local Store), so a
		// leading/trailing-whitespace *segment* is only observable when it
		// isn't also the outermost boundary of the whole path.
		"reports /file.txt",
		"notes/ q1.txt",
		"CON",
		"con.txt",
		"LPT1",
	}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := root.Read(context.Background(), "t", "u", name); !errors.Is(err, workspacefs.ErrInvalidPath) {
				t.Fatalf("Read(%q): got %v, want ErrInvalidPath", name, err)
			}
			if err := root.Mkdir(context.Background(), "t", "u", name); !errors.Is(err, workspacefs.ErrInvalidPath) {
				t.Fatalf("Mkdir(%q): got %v, want ErrInvalidPath", name, err)
			}
			if _, err := root.Write(context.Background(), "t", "u", name, []byte("x")); !errors.Is(err, workspacefs.ErrInvalidPath) {
				t.Fatalf("Write(%q): got %v, want ErrInvalidPath", name, err)
			}
		})
	}
}

// --- 429 handling ---------------------------------------------------------

func TestGet_429_RetryAfterOnce_ThenSucceeds(t *testing.T) {
	hits := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.0/drives/DRV1/items/ROOT:/f.txt", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(t, r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		hits++
		if hits == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		writeJSON(w, map[string]any{"id": "F1", "name": "f.txt", "size": 1})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	withTestGraphBase(t, srv)

	root := testStore(srv).WithRoot("DRV1", "ROOT")
	_, found, err := root.metadata(context.Background(), testToken, "f.txt")
	if err != nil {
		t.Fatalf("metadata after 429 retry: unexpected error %v", err)
	}
	if !found {
		t.Fatalf("expected found=true after successful retry")
	}
	if hits != 2 {
		t.Fatalf("expected exactly 2 requests (1 x 429 + 1 retry), got %d", hits)
	}
}

func TestGet_429_Twice_ErrUnavailable(t *testing.T) {
	hits := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.0/drives/DRV1/items/ROOT:/f.txt", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(t, r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		hits++
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	withTestGraphBase(t, srv)

	root := testStore(srv).WithRoot("DRV1", "ROOT")
	_, _, err := root.metadata(context.Background(), testToken, "f.txt")
	if !errors.Is(err, workspacefs.ErrUnavailable) {
		t.Fatalf("second 429: got %v, want ErrUnavailable", err)
	}
	if hits != 2 {
		t.Fatalf("expected exactly 2 requests (initial + 1 retry), got %d", hits)
	}
}

func TestNonGET_429_NoRetry_ErrUnavailable(t *testing.T) {
	hits := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.0/drives/DRV1/items/ROOT:/f.txt:/content", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(t, r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		hits++
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	withTestGraphBase(t, srv)

	root := testStore(srv).WithRoot("DRV1", "ROOT")
	_, err := root.simplePut(context.Background(), testToken, "f.txt", []byte("x"))
	if !errors.Is(err, workspacefs.ErrUnavailable) {
		t.Fatalf("non-GET 429: got %v, want ErrUnavailable", err)
	}
	if hits != 1 {
		t.Fatalf("expected exactly 1 request (no retry for non-GET), got %d", hits)
	}
}

func TestGet_429_RetryAfterTooLong_NoRetry_ErrUnavailable(t *testing.T) {
	hits := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.0/drives/DRV1/items/ROOT:/f.txt", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(t, r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		hits++
		w.Header().Set("Retry-After", "10")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	withTestGraphBase(t, srv)

	root := testStore(srv).WithRoot("DRV1", "ROOT")
	start := time.Now()
	_, _, err := root.metadata(context.Background(), testToken, "f.txt")
	elapsed := time.Since(start)
	if !errors.Is(err, workspacefs.ErrUnavailable) {
		t.Fatalf("Retry-After > 5s: got %v, want ErrUnavailable", err)
	}
	if hits != 1 {
		t.Fatalf("expected exactly 1 request (no retry), got %d", hits)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("must not have waited for the long Retry-After, elapsed=%v", elapsed)
	}
}

// --- token error propagation ----------------------------------------------

func TestToken_WrappedSentinel_Propagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("no HTTP request expected when the token fetch itself fails")
	}))
	defer srv.Close()
	withTestGraphBase(t, srv)

	store := &Store{Tokens: fakeTokenSource{err: fmt.Errorf("obo: %w", workspacefs.ErrReconnectRequired)}, HTTP: srv.Client()}
	root := store.WithRoot("DRV1", "ROOT")
	_, _, err := root.Read(context.Background(), "t", "u", "f.txt")
	if !errors.Is(err, workspacefs.ErrReconnectRequired) {
		t.Fatalf("token wrapped-sentinel: got %v, want ErrReconnectRequired", err)
	}
}

func TestToken_PlainError_WrapsUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("no HTTP request expected when the token fetch itself fails")
	}))
	defer srv.Close()
	withTestGraphBase(t, srv)

	store := &Store{Tokens: fakeTokenSource{err: errors.New("boom")}, HTTP: srv.Client()}
	root := store.WithRoot("DRV1", "ROOT")
	_, _, err := root.Read(context.Background(), "t", "u", "f.txt")
	if !errors.Is(err, workspacefs.ErrUnavailable) {
		t.Fatalf("token plain error: got %v, want ErrUnavailable", err)
	}
}

// --- EnsureFolder: nested create + idempotent rerun ----------------------

func TestEnsureFolder_NestedAndIdempotent(t *testing.T) {
	appsCalls := 0
	aikonosCalls := 0

	mux := http.NewServeMux()
	mux.HandleFunc("/v1.0/me/drive", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(t, r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		writeJSON(w, map[string]any{"id": "DRV1"})
	})
	mux.HandleFunc("/v1.0/drives/DRV1/items/root/children", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(t, r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		appsCalls++
		if appsCalls == 1 {
			w.WriteHeader(http.StatusCreated)
			writeJSON(w, map[string]any{"id": "id-Apps", "name": "Apps", "folder": map[string]any{}})
			return
		}
		w.WriteHeader(http.StatusConflict)
	})
	mux.HandleFunc("/v1.0/drives/DRV1/items/root:/Apps", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(t, r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		writeJSON(w, map[string]any{"id": "id-Apps", "name": "Apps", "folder": map[string]any{}})
	})
	mux.HandleFunc("/v1.0/drives/DRV1/items/id-Apps/children", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(t, r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		aikonosCalls++
		if aikonosCalls == 1 {
			w.WriteHeader(http.StatusCreated)
			writeJSON(w, map[string]any{"id": "id-Aikonos", "name": "Aikonos", "folder": map[string]any{}})
			return
		}
		w.WriteHeader(http.StatusConflict)
	})
	mux.HandleFunc("/v1.0/drives/DRV1/items/id-Apps:/Aikonos", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(t, r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		writeJSON(w, map[string]any{"id": "id-Aikonos", "name": "Aikonos", "folder": map[string]any{}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	withTestGraphBase(t, srv)

	store := testStore(srv)
	drive1, item1, err := store.EnsureFolder(context.Background(), "t", "u", "Apps/Aikonos")
	if err != nil {
		t.Fatalf("EnsureFolder (first): %v", err)
	}
	if drive1 != "DRV1" || item1 != "id-Aikonos" {
		t.Fatalf("EnsureFolder (first): got (%q,%q)", drive1, item1)
	}

	drive2, item2, err := store.EnsureFolder(context.Background(), "t", "u", "Apps/Aikonos")
	if err != nil {
		t.Fatalf("EnsureFolder (rerun): %v", err)
	}
	if drive2 != drive1 || item2 != item1 {
		t.Fatalf("EnsureFolder rerun not idempotent: got (%q,%q), want (%q,%q)", drive2, item2, drive1, item1)
	}
	if appsCalls != 2 || aikonosCalls != 2 {
		t.Fatalf("expected 2 create attempts per segment (create+conflict), got Apps=%d Aikonos=%d", appsCalls, aikonosCalls)
	}
}

// --- ListFolders: drive-root folder-only browse (CP5 picker) -------------

func TestListFolders_FoldersOnlyFromDriveRoot(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.0/me/drive", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(t, r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		writeJSON(w, map[string]any{"id": "DRV1"})
	})
	mux.HandleFunc("/v1.0/drives/DRV1/items/root/children", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(t, r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		writeJSON(w, map[string]any{
			"value": []map[string]any{
				{"id": "F1", "name": "Apps", "folder": map[string]any{}},
				{"id": "I1", "name": "notes.txt"},
				{"id": "F2", "name": "Reports", "folder": map[string]any{}},
			},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	withTestGraphBase(t, srv)

	folders, err := testStore(srv).ListFolders(context.Background(), "t", "u", "")
	if err != nil {
		t.Fatalf("ListFolders: %v", err)
	}
	if len(folders) != 2 {
		t.Fatalf("expected 2 folders (files excluded), got %d: %+v", len(folders), folders)
	}
	names := map[string]string{}
	for _, f := range folders {
		names[f.Name] = f.Path
	}
	if names["Apps"] != "Apps" || names["Reports"] != "Reports" {
		t.Fatalf("unexpected folder entries: %+v", folders)
	}
}

func TestListFolders_MissingDir_ListsEmpty(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.0/me/drive", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(t, r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		writeJSON(w, map[string]any{"id": "DRV1"})
	})
	mux.HandleFunc("/v1.0/drives/DRV1/items/root:/Gone/children", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	withTestGraphBase(t, srv)

	folders, err := testStore(srv).ListFolders(context.Background(), "t", "u", "Gone")
	if err != nil {
		t.Fatalf("ListFolders(missing dir): %v", err)
	}
	if len(folders) != 0 {
		t.Fatalf("expected empty slice for a missing dir, got %+v", folders)
	}
}

// --- Move: destination conflict / source missing -------------------------

func TestMove_DestinationConflict_ErrExists(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.0/drives/DRV1/items/ROOT:/a.txt", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(t, r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, map[string]any{"id": "F1", "name": "a.txt", "size": 3})
		case http.MethodPatch:
			w.WriteHeader(http.StatusConflict)
		default:
			t.Fatalf("unexpected method %s", r.Method)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	withTestGraphBase(t, srv)

	root := testStore(srv).WithRoot("DRV1", "ROOT")
	_, err := root.Move(context.Background(), "t", "u", "a.txt", "b.txt")
	if !errors.Is(err, workspacefs.ErrExists) {
		t.Fatalf("Move destination conflict: got %v, want ErrExists", err)
	}
}

func TestMove_SourceMissing_ErrNotExist(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.0/drives/DRV1/items/ROOT:/missing.txt", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(t, r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	withTestGraphBase(t, srv)

	root := testStore(srv).WithRoot("DRV1", "ROOT")
	_, err := root.Move(context.Background(), "t", "u", "missing.txt", "b.txt")
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Move source missing: got %v, want fs.ErrNotExist", err)
	}
}
