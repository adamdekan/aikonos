package workspacefs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

const (
	tenant = "11111111-1111-1111-1111-111111111111"
	user   = "alice@example.com"
)

func TestRoundTrip(t *testing.T) {
	s := New(t.TempDir())

	if _, err := s.Write(context.Background(), tenant, user, "reports/q1.csv", []byte("a,b,c\n1,2,3\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := s.Write(context.Background(), tenant, user, "notes.txt", []byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}

	files, err := s.List(context.Background(), tenant, user)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	// 2 files + the "reports" directory entry that List now also emits.
	if len(files) != 3 {
		t.Fatalf("want 3 entries, got %d: %+v", len(files), files)
	}
	got := map[string]int64{}
	isDir := map[string]bool{}
	for _, f := range files {
		got[f.Path] = f.Size
		isDir[f.Path] = f.IsDir
	}
	if got["notes.txt"] != 5 || got["reports/q1.csv"] == 0 {
		t.Fatalf("unexpected listing: %+v", got)
	}
	if isDir["notes.txt"] || isDir["reports/q1.csv"] {
		t.Fatalf("files must not be marked IsDir: %+v", isDir)
	}
	if !isDir["reports"] {
		t.Fatalf("want reports dir entry with IsDir=true: %+v", isDir)
	}

	data, _, err := s.Read(context.Background(), tenant, user, "notes.txt")
	if err != nil || string(data) != "hello" {
		t.Fatalf("read: %q err=%v", data, err)
	}

	if err := s.Delete(context.Background(), tenant, user, "notes.txt"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, _, err := s.Read(context.Background(), tenant, user, "notes.txt"); err == nil {
		t.Fatal("expected read of deleted file to fail")
	}
}

func TestPathTraversalRejected(t *testing.T) {
	s := New(t.TempDir())
	bad := []string{"../escape", "../../etc/passwd", "/abs/path", "reports/../../x", "config/mcp_connections.json", "config"}
	for _, p := range bad {
		if _, err := s.Write(context.Background(), tenant, user, p, []byte("x")); err == nil {
			t.Errorf("write(%q) should have been rejected", p)
		}
		if _, _, err := s.Read(context.Background(), tenant, user, p); err == nil {
			t.Errorf("read(%q) should have been rejected", p)
		}
		if err := s.Delete(context.Background(), tenant, user, p); err == nil {
			t.Errorf("delete(%q) should have been rejected", p)
		}
	}
}

func TestUserIsolation(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	if _, err := s.Write(context.Background(), tenant, "bob@example.com", "secret.txt", []byte("bob's")); err != nil {
		t.Fatal(err)
	}
	// alice's listing must not see bob's file; her dir is separate.
	files, err := s.List(context.Background(), tenant, user)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatalf("alice should see no files, got %+v", files)
	}
	// And the on-disk layout is per-user (segments sanitised like the Tool Proxy,
	// so uploads land where doc.read/workspace.read look).
	if _, err := os.Stat(filepath.Join(root, tenant, sanitizeSeg("bob@example.com"), "secret.txt")); err != nil {
		t.Fatalf("bob's file should exist on disk: %v", err)
	}
}

func TestReservedConfigHidden(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	// Simulate the connector reference file the platform writes.
	cfgDir := filepath.Join(root, tenant, sanitizeSeg(user), "config")
	if err := os.MkdirAll(cfgDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "mcp_connections.json"), []byte("{}"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(context.Background(), tenant, user, "doc.txt", []byte("x")); err != nil {
		t.Fatal(err)
	}
	files, err := s.List(context.Background(), tenant, user)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasPrefix(f.Path, "config/") {
			t.Errorf("config/ should be hidden from the explorer, saw %q", f.Path)
		}
	}
	if len(files) != 1 || files[0].Path != "doc.txt" {
		t.Fatalf("want only doc.txt, got %+v", files)
	}
}

func TestListIncludesDirs(t *testing.T) {
	s := New(t.TempDir())
	if _, err := s.Write(context.Background(), tenant, user, "nested/deep/file.txt", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := s.Mkdir(context.Background(), tenant, user, "empty"); err != nil {
		t.Fatal(err)
	}

	files, err := s.List(context.Background(), tenant, user)
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]FileInfo{}
	for _, f := range files {
		byPath[f.Path] = f
	}

	// The user-root entry itself must never appear.
	if _, ok := byPath["."]; ok {
		t.Fatalf("root entry \".\" must be skipped, got %+v", files)
	}

	for _, tc := range []struct {
		path  string
		isDir bool
	}{
		{"nested", true},
		{"nested/deep", true},
		{"nested/deep/file.txt", false},
		{"empty", true},
	} {
		f, ok := byPath[tc.path]
		if !ok {
			t.Fatalf("want entry %q in listing, got %+v", tc.path, files)
		}
		if f.IsDir != tc.isDir {
			t.Errorf("%q: IsDir=%v, want %v", tc.path, f.IsDir, tc.isDir)
		}
	}
	if byPath["empty"].Size != 0 {
		t.Errorf("empty dir Size should be 0, got %d", byPath["empty"].Size)
	}
}

func TestMove(t *testing.T) {
	t.Run("renames a file", func(t *testing.T) {
		s := New(t.TempDir())
		if _, err := s.Write(context.Background(), tenant, user, "a.txt", []byte("hello")); err != nil {
			t.Fatal(err)
		}
		got, err := s.Move(context.Background(), tenant, user, "a.txt", "b.txt")
		if err != nil {
			t.Fatalf("move: %v", err)
		}
		if got.Path != "b.txt" || got.IsDir {
			t.Fatalf("unexpected move result: %+v", got)
		}
		if _, _, err := s.Read(context.Background(), tenant, user, "a.txt"); err == nil {
			t.Fatal("old path should be gone")
		}
		data, _, err := s.Read(context.Background(), tenant, user, "b.txt")
		if err != nil || string(data) != "hello" {
			t.Fatalf("new path unreadable: data=%q err=%v", data, err)
		}
	})

	t.Run("renames a directory", func(t *testing.T) {
		s := New(t.TempDir())
		if _, err := s.Write(context.Background(), tenant, user, "dir/file.txt", []byte("x")); err != nil {
			t.Fatal(err)
		}
		got, err := s.Move(context.Background(), tenant, user, "dir", "dir2")
		if err != nil {
			t.Fatalf("move: %v", err)
		}
		if got.Path != "dir2" || !got.IsDir {
			t.Fatalf("unexpected move result: %+v", got)
		}
		if _, _, err := s.Read(context.Background(), tenant, user, "dir2/file.txt"); err != nil {
			t.Fatalf("moved dir's contents should be readable: %v", err)
		}
	})

	t.Run("refuses when dest exists", func(t *testing.T) {
		s := New(t.TempDir())
		if _, err := s.Write(context.Background(), tenant, user, "a.txt", []byte("1")); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Write(context.Background(), tenant, user, "b.txt", []byte("2")); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Move(context.Background(), tenant, user, "a.txt", "b.txt"); !errors.Is(err, ErrExists) {
			t.Fatalf("want ErrExists, got %v", err)
		}
	})

	t.Run("rejects a traversal target", func(t *testing.T) {
		s := New(t.TempDir())
		if _, err := s.Write(context.Background(), tenant, user, "a.txt", []byte("1")); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Move(context.Background(), tenant, user, "a.txt", "../escape.txt"); !errors.Is(err, ErrInvalidPath) {
			t.Fatalf("want ErrInvalidPath, got %v", err)
		}
		if _, err := s.Move(context.Background(), tenant, user, "../escape.txt", "a.txt"); !errors.Is(err, ErrInvalidPath) {
			t.Fatalf("want ErrInvalidPath for source traversal, got %v", err)
		}
	})

	t.Run("rejects a destination under the reserved config tree", func(t *testing.T) {
		s := New(t.TempDir())
		if _, err := s.Write(context.Background(), tenant, user, "a.txt", []byte("1")); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Move(context.Background(), tenant, user, "a.txt", "config/x.txt"); !errors.Is(err, ErrInvalidPath) {
			t.Fatalf("want ErrInvalidPath, got %v", err)
		}
	})
}

// TestMove_ConcurrentMoveVsMoveOntoSameDestination races two Move calls that
// both target the same destination from different sources. F14: the
// check-then-act (stat dest, then rename) must be serialized per-user so
// exactly one call wins and the loser observes ErrExists — never both
// succeeding (which would silently clobber whichever won the OS-level race).
func TestMove_ConcurrentMoveVsMoveOntoSameDestination(t *testing.T) {
	s := New(t.TempDir())
	if _, err := s.Write(context.Background(), tenant, user, "a.txt", []byte("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(context.Background(), tenant, user, "b.txt", []byte("b")); err != nil {
		t.Fatal(err)
	}

	var start sync.WaitGroup
	start.Add(1)
	var wg sync.WaitGroup
	var successes int64

	race := func(from string) {
		defer wg.Done()
		start.Wait()
		if _, err := s.Move(context.Background(), tenant, user, from, "dest.txt"); err == nil {
			atomic.AddInt64(&successes, 1)
		} else if !errors.Is(err, ErrExists) {
			t.Errorf("move(%s): want nil or ErrExists, got %v", from, err)
		}
	}
	wg.Add(2)
	go race("a.txt")
	go race("b.txt")
	start.Done()
	wg.Wait()

	if successes != 1 {
		t.Fatalf("want exactly one winner, got %d successes", successes)
	}
}

// TestMove_ConcurrentMoveVsDeleteOverSameTree races a Move of a directory
// against a Delete of a file inside that directory. F14: without the lock,
// Delete's stat-then-remove can interleave with Move's stat-then-rename,
// producing a spurious success/failure pairing inconsistent with either
// pure-before or pure-after ordering. Serialized, exactly one consistent
// outcome results: either the move relocates the whole subtree (delete then
// fails NotExist) or the delete removes the file first (move still succeeds
// on the now-lighter directory).
func TestMove_ConcurrentMoveVsDeleteOverSameTree(t *testing.T) {
	s := New(t.TempDir())
	if _, err := s.Write(context.Background(), tenant, user, "dir/file.txt", []byte("x")); err != nil {
		t.Fatal(err)
	}

	var start sync.WaitGroup
	start.Add(1)
	var wg sync.WaitGroup
	wg.Add(2)

	var moveErr, delErr error
	go func() {
		defer wg.Done()
		start.Wait()
		_, moveErr = s.Move(context.Background(), tenant, user, "dir", "dir2")
	}()
	go func() {
		defer wg.Done()
		start.Wait()
		delErr = s.Delete(context.Background(), tenant, user, "dir/file.txt")
	}()
	start.Done()
	wg.Wait()

	// Exactly one of the two must have observed the tree in the state left by
	// the other's completed effect — never a torn intermediate: e.g. Delete
	// succeeding on a file that Move already relocated (which would mean the
	// delete silently vanished, or corrupted the moved tree), or both
	// reporting success in a way that a serialized run could never produce.
	if moveErr == nil && delErr == nil {
		// Move went first, deleted the file from within dir before dir was
		// renamed — consistent order (rename happens-after nothing racy).
		// This is a valid serialized outcome only if dir2/file.txt is now
		// gone (delete raced correctly against the already-moved path is not
		// possible here since delete targeted "dir/file.txt", not
		// "dir2/file.txt" — so if delete "succeeded" concurrently with a
		// completed move, it must have run before the rename).
		if _, _, err := s.Read(context.Background(), tenant, user, "dir2/file.txt"); err == nil {
			t.Fatal("both move and delete reported success but file.txt still exists post-move — not a valid serialized outcome")
		}
	}
}

func TestMkdir(t *testing.T) {
	s := New(t.TempDir())
	if err := s.Mkdir(context.Background(), tenant, user, "newdir"); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	files, err := s.List(context.Background(), tenant, user)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range files {
		if f.Path == "newdir" && f.IsDir {
			found = true
		}
	}
	if !found {
		t.Fatalf("newdir should appear in listing: %+v", files)
	}

	if err := s.Mkdir(context.Background(), tenant, user, "newdir"); !errors.Is(err, ErrExists) {
		t.Fatalf("want ErrExists on repeat mkdir, got %v", err)
	}
}

func TestDeleteDir(t *testing.T) {
	t.Run("removes an empty dir", func(t *testing.T) {
		s := New(t.TempDir())
		if err := s.Mkdir(context.Background(), tenant, user, "empty"); err != nil {
			t.Fatal(err)
		}
		if err := s.Delete(context.Background(), tenant, user, "empty"); err != nil {
			t.Fatalf("delete: %v", err)
		}
		files, err := s.List(context.Background(), tenant, user)
		if err != nil {
			t.Fatal(err)
		}
		if len(files) != 0 {
			t.Fatalf("want empty listing after delete, got %+v", files)
		}
	})

	t.Run("refuses a non-empty dir", func(t *testing.T) {
		s := New(t.TempDir())
		if _, err := s.Write(context.Background(), tenant, user, "full/file.txt", []byte("x")); err != nil {
			t.Fatal(err)
		}
		if err := s.Delete(context.Background(), tenant, user, "full"); !errors.Is(err, ErrNotEmpty) {
			t.Fatalf("want ErrNotEmpty, got %v", err)
		}
	})

	t.Run("existing file delete still works", func(t *testing.T) {
		s := New(t.TempDir())
		if _, err := s.Write(context.Background(), tenant, user, "f.txt", []byte("x")); err != nil {
			t.Fatal(err)
		}
		if err := s.Delete(context.Background(), tenant, user, "f.txt"); err != nil {
			t.Fatalf("delete: %v", err)
		}
	})
}

func TestSizeCap(t *testing.T) {
	s := New(t.TempDir())
	big := make([]byte, MaxFileBytes+1)
	if _, err := s.Write(context.Background(), tenant, user, "big.bin", big); err == nil {
		t.Fatal("oversized write should be rejected")
	}
}

// seedTree lays out a workspace used by the ListDir semantics tests:
//
//	notes.txt
//	reports/q1.csv
//	reports/archive/old.csv
//	.agent/Sessions/s1.json
func seedListDirTree(t *testing.T, s *Store) {
	t.Helper()
	for path, data := range map[string]string{
		"notes.txt":               "hello",
		"reports/q1.csv":          "a,b,c",
		"reports/archive/old.csv": "x,y,z",
		".agent/Sessions/s1.json": "{}",
	} {
		if _, err := s.Write(context.Background(), tenant, user, path, []byte(data)); err != nil {
			t.Fatalf("seed write %s: %v", path, err)
		}
	}
}

func pathSet(files []FileInfo) map[string]bool {
	out := make(map[string]bool, len(files))
	for _, f := range files {
		out[f.Path] = true
	}
	return out
}

func TestListDir_EmptyDirIsRoot(t *testing.T) {
	s := New(t.TempDir())
	seedListDirTree(t, s)

	got, err := s.ListDir(context.Background(), tenant, user, "", true)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	want, err := s.List(context.Background(), tenant, user)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("ListDir(\"\", true) should match List: got %d want %d", len(got), len(want))
	}
}

func TestListDir_DotIsRootShallow(t *testing.T) {
	s := New(t.TempDir())
	seedListDirTree(t, s)

	got, err := s.ListDir(context.Background(), tenant, user, ".", false)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	set := pathSet(got)
	// Immediate children of root only: notes.txt, reports (dir), .agent (dir).
	if len(set) != 3 {
		t.Fatalf("want 3 immediate root entries, got %+v", set)
	}
	if !set["notes.txt"] || !set["reports"] || !set[".agent"] {
		t.Fatalf("unexpected root entries: %+v", set)
	}
	if set["reports/q1.csv"] {
		t.Fatalf("shallow root listing must not descend into reports: %+v", set)
	}
}

func TestListDir_ShallowListsImmediateChildrenOnly(t *testing.T) {
	s := New(t.TempDir())
	seedListDirTree(t, s)

	got, err := s.ListDir(context.Background(), tenant, user, "reports", false)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	set := pathSet(got)
	if len(set) != 2 {
		t.Fatalf("want 2 immediate children of reports, got %+v", set)
	}
	if !set["reports/q1.csv"] || !set["reports/archive"] {
		t.Fatalf("unexpected entries: %+v", set)
	}
	if set["reports/archive/old.csv"] {
		t.Fatalf("shallow listing must not descend into subdirectories: %+v", set)
	}
	for _, f := range got {
		if f.Path == "reports/archive" && !f.IsDir {
			t.Fatalf("reports/archive should be marked IsDir")
		}
	}
}

func TestListDir_RecursiveUnderDirListsSubtree(t *testing.T) {
	s := New(t.TempDir())
	seedListDirTree(t, s)

	got, err := s.ListDir(context.Background(), tenant, user, "reports", true)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	set := pathSet(got)
	if !set["reports/q1.csv"] || !set["reports/archive"] || !set["reports/archive/old.csv"] {
		t.Fatalf("recursive listing should include the whole subtree: %+v", set)
	}
	if set["notes.txt"] {
		t.Fatalf("scoped recursive listing must not include entries outside the scope: %+v", set)
	}
}

func TestListDir_NonExistentDirIsEmptyNotError(t *testing.T) {
	s := New(t.TempDir())
	seedListDirTree(t, s)

	got, err := s.ListDir(context.Background(), tenant, user, "does/not/exist", true)
	if err != nil {
		t.Fatalf("non-existent dir should not error, got %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want empty slice, got %+v", got)
	}
}

func TestListDir_ConfigExcludedAsScopeTarget(t *testing.T) {
	s := New(t.TempDir())
	seedListDirTree(t, s)

	_, err := s.ListDir(context.Background(), tenant, user, "config", true)
	if err == nil || !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("listing the reserved config/ dir as scope target should be ErrInvalidPath, got %v", err)
	}
}

func TestListDir_ConfigExcludedAsDescendant(t *testing.T) {
	s := New(t.TempDir())
	seedListDirTree(t, s)
	// A top-level config/ entry, written directly on disk (bypassing the
	// resolve() guard that Write() itself would apply), mirrors how it gets
	// there in production (the connector reference file).
	base := s.userDir(tenant, user)
	if err := os.MkdirAll(filepath.Join(base, "config"), 0o750); err != nil {
		t.Fatalf("seed config dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(base, "config", "secrets.json"), []byte("{}"), 0o640); err != nil {
		t.Fatalf("seed config file: %v", err)
	}

	got, err := s.ListDir(context.Background(), tenant, user, "", true)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	set := pathSet(got)
	if set["config"] || set["config/secrets.json"] {
		t.Fatalf("config/ must stay excluded as a descendant: %+v", set)
	}
}

func TestListDir_DotDirIsValidScope(t *testing.T) {
	s := New(t.TempDir())
	seedListDirTree(t, s)

	got, err := s.ListDir(context.Background(), tenant, user, ".agent/Sessions", false)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	set := pathSet(got)
	if !set[".agent/Sessions/s1.json"] {
		t.Fatalf("dot-dir scope should list its contents: %+v", set)
	}
}

func TestListDir_TraversalRejectedSameAsResolve(t *testing.T) {
	s := New(t.TempDir())
	seedListDirTree(t, s)

	_, err := s.ListDir(context.Background(), tenant, user, "../../etc", true)
	if err == nil || !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("traversal scope should be ErrInvalidPath, got %v", err)
	}
}

// ── CP4.2: session file integrity sidecars ──────────────────────────────────

const testSessionKey = "a-32-byte-test-key-aaaaaaaaaaaa!"

func TestWrite_SessionPathCreatesSidecar(t *testing.T) {
	s := New(t.TempDir())
	s.SetSessionSigningKey([]byte(testSessionKey))

	rel := ".agent/Sessions/abc.json"
	if _, err := s.Write(context.Background(), tenant, user, rel, []byte(`{"id":"abc"}`)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, _, err := s.Read(context.Background(), tenant, user, rel+".sig"); err != nil {
		t.Fatalf("expected sidecar file to exist: %v", err)
	}
}

func TestWrite_NoKeyConfigured_NoSidecar(t *testing.T) {
	s := New(t.TempDir()) // no signing key set — degraded/disabled

	rel := ".agent/Sessions/abc.json"
	if _, err := s.Write(context.Background(), tenant, user, rel, []byte(`{"id":"abc"}`)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, _, err := s.Read(context.Background(), tenant, user, rel+".sig"); err == nil {
		t.Fatalf("expected no sidecar to be written when signing is disabled")
	}
}

func TestWrite_OrdinaryPathNeverSigned(t *testing.T) {
	s := New(t.TempDir())
	s.SetSessionSigningKey([]byte(testSessionKey))

	if _, err := s.Write(context.Background(), tenant, user, "notes.txt", []byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, _, err := s.Read(context.Background(), tenant, user, "notes.txt.sig"); err == nil {
		t.Fatalf("an ordinary workspace write must never get a sidecar")
	}
}

func TestReadVerified_RoundTripClean(t *testing.T) {
	s := New(t.TempDir())
	s.SetSessionSigningKey([]byte(testSessionKey))

	rel := ".agent/Sessions/abc.json"
	want := []byte(`{"id":"abc","messages":[]}`)
	if _, err := s.Write(context.Background(), tenant, user, rel, want); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, _, legacy, err := s.ReadVerified(context.Background(), tenant, user, rel)
	if err != nil {
		t.Fatalf("ReadVerified: %v", err)
	}
	if legacy {
		t.Fatalf("freshly-signed file should not be reported legacy")
	}
	if string(got) != string(want) {
		t.Fatalf("content mismatch: got %q want %q", got, want)
	}
}

func TestReadVerified_TamperedContentRefused(t *testing.T) {
	s := New(t.TempDir())
	s.SetSessionSigningKey([]byte(testSessionKey))

	rel := ".agent/Sessions/abc.json"
	if _, err := s.Write(context.Background(), tenant, user, rel, []byte(`{"id":"abc"}`)); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Tamper with the content on disk directly, bypassing the Store API —
	// simulates an out-of-band modification of the session file.
	full := filepath.Join(s.userDir(tenant, user), rel)
	if err := os.WriteFile(full, []byte(`{"id":"abc","injected":true}`), 0o640); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	_, _, _, err := s.ReadVerified(context.Background(), tenant, user, rel)
	if !errors.Is(err, ErrIntegrityMismatch) {
		t.Fatalf("expected ErrIntegrityMismatch, got %v", err)
	}
	if !strings.Contains(err.Error(), "session integrity check failed") {
		t.Fatalf("error message must be grep-stable, got %q", err.Error())
	}
}

func TestReadVerified_LegacyFileNoSidecarAllowed(t *testing.T) {
	s := New(t.TempDir())
	s.SetSessionSigningKey([]byte(testSessionKey))

	rel := ".agent/Sessions/abc.json"
	// Written with signing disabled — simulates a file persisted before
	// CP4.2 shipped.
	unsigned := New(s.root)
	want := []byte(`{"id":"abc"}`)
	if _, err := unsigned.Write(context.Background(), tenant, user, rel, want); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, _, legacy, err := s.ReadVerified(context.Background(), tenant, user, rel)
	if err != nil {
		t.Fatalf("ReadVerified: %v", err)
	}
	if !legacy {
		t.Fatalf("file with no sidecar should be reported legacy")
	}
	if string(got) != string(want) {
		t.Fatalf("content mismatch: got %q want %q", got, want)
	}
}

func TestListDir_ExcludesSigFiles(t *testing.T) {
	s := New(t.TempDir())
	s.SetSessionSigningKey([]byte(testSessionKey))

	rel := ".agent/Sessions/abc.json"
	if _, err := s.Write(context.Background(), tenant, user, rel, []byte(`{"id":"abc"}`)); err != nil {
		t.Fatalf("write: %v", err)
	}

	for _, recursive := range []bool{true, false} {
		got, err := s.ListDir(context.Background(), tenant, user, ".agent/Sessions", recursive)
		if err != nil {
			t.Fatalf("list(recursive=%v): %v", recursive, err)
		}
		set := pathSet(got)
		if set[rel+".sig"] {
			t.Fatalf("sig file leaked into listing (recursive=%v): %+v", recursive, set)
		}
		if !set[rel] {
			t.Fatalf("session record itself should still be listed (recursive=%v): %+v", recursive, set)
		}
	}

	// Root listing must also exclude it.
	got, err := s.List(context.Background(), tenant, user)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if pathSet(got)[rel+".sig"] {
		t.Fatalf("sig file leaked into root listing: %+v", pathSet(got))
	}
}

// ── CP4.2 review fix #1: session-ness must be decided from the RESOLVED path,
// not the caller's raw string. "./.agent/Sessions/x" and
// "a/../.agent/Sessions/x" both resolve into the real Sessions directory
// (filepath.Join collapses "./" and ".." identically to the canonical form)
// and must be signed/verified exactly like the canonical spelling.

func TestWrite_DotPrefixedSessionPathStillSigned(t *testing.T) {
	s := New(t.TempDir())
	s.SetSessionSigningKey([]byte(testSessionKey))

	raw := "./.agent/Sessions/abc.json"
	if _, err := s.Write(context.Background(), tenant, user, raw, []byte(`{"id":"abc"}`)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, _, err := s.Read(context.Background(), tenant, user, ".agent/Sessions/abc.json.sig"); err != nil {
		t.Fatalf("a \"./\"-prefixed session write must still produce a sidecar: %v", err)
	}
}

func TestWrite_TraversalSessionPathStillSigned(t *testing.T) {
	s := New(t.TempDir())
	s.SetSessionSigningKey([]byte(testSessionKey))

	raw := "x/../.agent/Sessions/evil.json"
	if _, err := s.Write(context.Background(), tenant, user, raw, []byte(`{"id":"evil"}`)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, _, err := s.Read(context.Background(), tenant, user, ".agent/Sessions/evil.json.sig"); err != nil {
		t.Fatalf("a traversal-spelled session write must still produce a sidecar: %v", err)
	}
}

func TestReadVerified_DotPrefixedPathTamperRefused(t *testing.T) {
	s := New(t.TempDir())
	s.SetSessionSigningKey([]byte(testSessionKey))

	if _, err := s.Write(context.Background(), tenant, user, "./.agent/Sessions/abc.json", []byte(`{"id":"abc"}`)); err != nil {
		t.Fatalf("write: %v", err)
	}
	full := filepath.Join(s.userDir(tenant, user), ".agent/Sessions/abc.json")
	if err := os.WriteFile(full, []byte(`{"id":"abc","injected":true}`), 0o640); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	// Read back through the "./"-prefixed spelling too — must still refuse.
	_, _, _, err := s.ReadVerified(context.Background(), tenant, user, "./.agent/Sessions/abc.json")
	if !errors.Is(err, ErrIntegrityMismatch) {
		t.Fatalf("expected ErrIntegrityMismatch for a dot-prefixed tampered read, got %v", err)
	}
}

// ── CP4.2 review fix #2: Move is a writer too — moving/renaming a file onto a
// path that resolves under SessionsDirPrefix must (re)sign the destination,
// and never leave a stale sidecar at the source.

func TestMove_IntoSessionsDirGetsSignedAndVerifies(t *testing.T) {
	s := New(t.TempDir())
	s.SetSessionSigningKey([]byte(testSessionKey))

	content := []byte(`{"id":"moved"}`)
	if _, err := s.Write(context.Background(), tenant, user, "draft.json", content); err != nil {
		t.Fatalf("write: %v", err)
	}
	dest := ".agent/Sessions/moved.json"
	if _, err := s.Move(context.Background(), tenant, user, "draft.json", dest); err != nil {
		t.Fatalf("move: %v", err)
	}

	got, _, legacy, err := s.ReadVerified(context.Background(), tenant, user, dest)
	if err != nil {
		t.Fatalf("ReadVerified after move: %v", err)
	}
	if legacy {
		t.Fatalf("a file moved into Sessions should have a fresh sidecar, not be legacy")
	}
	if string(got) != string(content) {
		t.Fatalf("content mismatch after move: got %q want %q", got, content)
	}

	// Tamper after the move — must still be refused.
	full := filepath.Join(s.userDir(tenant, user), dest)
	if err := os.WriteFile(full, []byte(`{"id":"moved","injected":true}`), 0o640); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	if _, _, _, err := s.ReadVerified(context.Background(), tenant, user, dest); !errors.Is(err, ErrIntegrityMismatch) {
		t.Fatalf("tamper-after-move should be refused, got %v", err)
	}
}

func TestMove_OutOfSessionsDirDropsStaleSidecar(t *testing.T) {
	s := New(t.TempDir())
	s.SetSessionSigningKey([]byte(testSessionKey))

	src := ".agent/Sessions/abc.json"
	if _, err := s.Write(context.Background(), tenant, user, src, []byte(`{"id":"abc"}`)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := s.Move(context.Background(), tenant, user, src, "archive/abc.json"); err != nil {
		t.Fatalf("move: %v", err)
	}
	if _, _, err := s.Read(context.Background(), tenant, user, src+".sig"); err == nil {
		t.Fatalf("sidecar must not linger at the old Sessions-dir location after moving out")
	}
	if _, _, err := s.Read(context.Background(), tenant, user, "archive/abc.json.sig"); err == nil {
		t.Fatalf("a file moved out of Sessions must not carry a sidecar at the new location")
	}
}

// TestUnderAgentDir pins the two exported predicates the user-facing file RPCs
// and the toolproxy seam both gate on. Traversal spellings must be caught (they
// resolve into the real .agent/ tree) and a name that merely shares the prefix
// must not be.
func TestUnderAgentDir(t *testing.T) {
	cases := []struct {
		rel         string
		agent, sess bool
	}{
		{".agent", true, false},
		{".agent/x", true, false},
		{".agent/Memory/facts/x.md", true, false},
		{"./.agent/Memory/x.md", true, false},
		{"a/../.agent/Memory/x.md", true, false},
		{".agent/Sessions", true, true},
		{".agent/Sessions/x.json", true, true},
		{"./.agent/Sessions/x.json", true, true},
		{"a/../.agent/Sessions/x.json", true, true},
		// The carve-out escape: cleaning must happen BEFORE the Sessions test,
		// or a path spelled through Sessions/ walks straight into Memory/.
		{".agent/Sessions/../Memory/x.md", true, false},
		// CleanRel trims leading whitespace before it resolves, so a predicate
		// that cleans without trimming reports "not under .agent/" for a path
		// the Store still writes into the real tree.
		{" .agent/Memory/x.md", true, false},
		{"\t.agent/Sessions/x.json", true, true},
		{"\n.agent/Sessions/../Memory/x.md", true, false},
		// Case: the block side fails closed (see UnderAgentDir's doc comment).
		{".AGENT/Memory/x.md", true, false},
		{".agentfoo", false, false},
		{".agentfoo/x.md", false, false},
		{".agentic/x", false, false},
		{"a/.agent/x", false, false},
		{"notes.txt", false, false},
		{"config/x", false, false},
	}
	for _, tc := range cases {
		if got := UnderAgentDir(tc.rel); got != tc.agent {
			t.Errorf("UnderAgentDir(%q) = %v, want %v", tc.rel, got, tc.agent)
		}
		if got := UnderSessionsDir(tc.rel); got != tc.sess {
			t.Errorf("UnderSessionsDir(%q) = %v, want %v", tc.rel, got, tc.sess)
		}
	}
}
