package broker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/onedrivefs"
	"github.com/adamdekan/aikonos/broker/internal/workspacefs"
)

// ── fakes ──────────────────────────────────────────────────────────────────

// fakeWorkspacePrefsRepo's mu guards rows/getCalls/setCalls — a real
// db.WorkspacePrefsRepo is backed by a Postgres connection pool and safe for
// concurrent use across requests; this in-memory fake's plain map+counters
// are not, and F-5's concurrency test (TestOneDriveBackend_
// ConcurrentFirstUse_EnsureFolderFiresOnce) can legitimately drive a
// concurrent cache-miss Get from two different (tenant,user) resolutions
// racing the same bootstrap window.
type fakeWorkspacePrefsRepo struct {
	mu       sync.Mutex
	rows     map[string]db.WorkspacePrefs
	getCalls int
	setCalls int
	getErr   error
	setErr   error
}

func newFakeWorkspacePrefsRepo() *fakeWorkspacePrefsRepo {
	return &fakeWorkspacePrefsRepo{rows: map[string]db.WorkspacePrefs{}}
}

func (f *fakeWorkspacePrefsRepo) key(tenant, user string) string { return tenant + "/" + user }

func (f *fakeWorkspacePrefsRepo) Get(ctx context.Context, tenant, user string) (db.WorkspacePrefs, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getCalls++
	if f.getErr != nil {
		return db.WorkspacePrefs{}, false, f.getErr
	}
	row, ok := f.rows[f.key(tenant, user)]
	return row, ok, nil
}

func (f *fakeWorkspacePrefsRepo) Set(ctx context.Context, tenant, user string, p db.WorkspacePrefs) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setCalls++
	if f.setErr != nil {
		return f.setErr
	}
	f.rows[f.key(tenant, user)] = p
	return nil
}

type fakeConfiguredChecker struct{ configured bool }

func (f fakeConfiguredChecker) Configured(ctx context.Context, tenant string) bool {
	return f.configured
}

// ── WorkspacePrefResolver: default-rule matrix ─────────────────────────────

func TestWorkspacePrefResolver_ExplicitRowWins_Local(t *testing.T) {
	repo := newFakeWorkspacePrefsRepo()
	repo.rows[repo.key("t1", "u1")] = db.WorkspacePrefs{Backend: "local", OneDriveFolderPath: "Custom"}
	r := NewWorkspacePrefResolver(repo, fakeConfiguredChecker{configured: true}) // configured is irrelevant — row wins

	pref, err := r.Effective(context.Background(), "t1", "u1")
	if err != nil {
		t.Fatalf("Effective: %v", err)
	}
	if pref.Backend != workspacefs.KindLocal || !pref.Found {
		t.Fatalf("expected explicit local row to win, got %+v", pref)
	}
}

func TestWorkspacePrefResolver_ExplicitRowWins_OneDrive(t *testing.T) {
	repo := newFakeWorkspacePrefsRepo()
	repo.rows[repo.key("t1", "u1")] = db.WorkspacePrefs{Backend: "onedrive", OneDriveFolderPath: "Custom", DriveID: "D1", RootItemID: "I1"}
	r := NewWorkspacePrefResolver(repo, fakeConfiguredChecker{configured: false}) // configured is irrelevant — row wins

	pref, err := r.Effective(context.Background(), "t1", "u1")
	if err != nil {
		t.Fatalf("Effective: %v", err)
	}
	if pref.Backend != workspacefs.KindOneDrive || pref.DriveID != "D1" || pref.RootItemID != "I1" || !pref.Found {
		t.Fatalf("expected explicit onedrive row to win, got %+v", pref)
	}
}

func TestWorkspacePrefResolver_NoRow_Configured_DefaultsOneDrive(t *testing.T) {
	repo := newFakeWorkspacePrefsRepo()
	r := NewWorkspacePrefResolver(repo, fakeConfiguredChecker{configured: true})

	pref, err := r.Effective(context.Background(), "t1", "u1")
	if err != nil {
		t.Fatalf("Effective: %v", err)
	}
	if pref.Backend != workspacefs.KindOneDrive || pref.OneDriveFolderPath != defaultOneDriveFolderPath || pref.Found {
		t.Fatalf("expected default-rule onedrive at %q, got %+v", defaultOneDriveFolderPath, pref)
	}
}

func TestWorkspacePrefResolver_NoRow_Unconfigured_DefaultsLocal(t *testing.T) {
	repo := newFakeWorkspacePrefsRepo()
	r := NewWorkspacePrefResolver(repo, fakeConfiguredChecker{configured: false})

	pref, err := r.Effective(context.Background(), "t1", "u1")
	if err != nil {
		t.Fatalf("Effective: %v", err)
	}
	if pref.Backend != workspacefs.KindLocal || pref.Found {
		t.Fatalf("expected default-rule local, got %+v", pref)
	}
}

func TestWorkspacePrefResolver_TTLCache_HitsRepoOnce(t *testing.T) {
	repo := newFakeWorkspacePrefsRepo()
	r := NewWorkspacePrefResolver(repo, fakeConfiguredChecker{configured: false})

	if _, err := r.Effective(context.Background(), "t1", "u1"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Effective(context.Background(), "t1", "u1"); err != nil {
		t.Fatal(err)
	}
	if repo.getCalls != 1 {
		t.Fatalf("expected the repo to be hit once (second lookup served from cache), got %d", repo.getCalls)
	}
}

func TestWorkspacePrefResolver_Invalidate_ForcesReread(t *testing.T) {
	repo := newFakeWorkspacePrefsRepo()
	r := NewWorkspacePrefResolver(repo, fakeConfiguredChecker{configured: false})

	if _, err := r.Effective(context.Background(), "t1", "u1"); err != nil {
		t.Fatal(err)
	}
	r.Invalidate("t1", "u1")
	if _, err := r.Effective(context.Background(), "t1", "u1"); err != nil {
		t.Fatal(err)
	}
	if repo.getCalls != 2 {
		t.Fatalf("expected Invalidate to force a re-read, got %d repo calls", repo.getCalls)
	}
}

// TestWorkspacePrefResolver_InvalidateTenant_ClearsAllUsersOnlyForThatTenant
// pins CP3 fix #2: a tenant-wide config change (M365 upsert/delete) must drop
// every cached user's pref for that tenant on the next Effective() call,
// without touching another tenant's cache.
func TestWorkspacePrefResolver_InvalidateTenant_ClearsAllUsersOnlyForThatTenant(t *testing.T) {
	repo := newFakeWorkspacePrefsRepo()
	r := NewWorkspacePrefResolver(repo, fakeConfiguredChecker{configured: false})
	ctx := context.Background()

	if _, err := r.Effective(ctx, "t1", "u1"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Effective(ctx, "t1", "u2"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Effective(ctx, "t2", "u1"); err != nil {
		t.Fatal(err)
	}
	if repo.getCalls != 3 {
		t.Fatalf("expected 3 initial repo reads, got %d", repo.getCalls)
	}

	r.InvalidateTenant("t1")

	if _, err := r.Effective(ctx, "t1", "u1"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Effective(ctx, "t1", "u2"); err != nil {
		t.Fatal(err)
	}
	if repo.getCalls != 5 {
		t.Fatalf("expected InvalidateTenant(t1) to force a re-read for both t1 users, got %d repo calls", repo.getCalls)
	}

	if _, err := r.Effective(ctx, "t2", "u1"); err != nil {
		t.Fatal(err)
	}
	if repo.getCalls != 5 {
		t.Fatalf("expected t2's cache entry to survive InvalidateTenant(t1) (still TTL-cached), got %d repo calls", repo.getCalls)
	}
}

func TestWorkspacePrefResolver_RepoError_Propagates(t *testing.T) {
	repo := newFakeWorkspacePrefsRepo()
	repo.getErr = errors.New("db unreachable")
	r := NewWorkspacePrefResolver(repo, fakeConfiguredChecker{configured: false})

	if _, err := r.Effective(context.Background(), "t1", "u1"); err == nil {
		t.Fatal("expected error to propagate")
	}
}

// ── oneDriveBackend: first-use auto-create + id persistence ────────────────

// recordingRoot is a fake oneDriveRoot — calls are appended for assertion.
type recordingRoot struct {
	calls []string
}

func (r *recordingRoot) ListDir(ctx context.Context, tenant, user, dir string, recursive bool) ([]workspacefs.FileInfo, error) {
	r.calls = append(r.calls, "ListDir:"+dir)
	return nil, nil
}

func (r *recordingRoot) Read(ctx context.Context, tenant, user, rel string) ([]byte, time.Time, error) {
	r.calls = append(r.calls, "Read:"+rel)
	return []byte("data"), time.Time{}, nil
}

func (r *recordingRoot) Write(ctx context.Context, tenant, user, rel string, data []byte) (workspacefs.FileInfo, error) {
	r.calls = append(r.calls, "Write:"+rel)
	return workspacefs.FileInfo{Path: rel, Size: int64(len(data))}, nil
}

func (r *recordingRoot) Delete(ctx context.Context, tenant, user, rel string) error {
	r.calls = append(r.calls, "Delete:"+rel)
	return nil
}

func (r *recordingRoot) Move(ctx context.Context, tenant, user, from, to string) (workspacefs.FileInfo, error) {
	r.calls = append(r.calls, "Move:"+from+"->"+to)
	return workspacefs.FileInfo{Path: to}, nil
}

func (r *recordingRoot) Mkdir(ctx context.Context, tenant, user, rel string) error {
	r.calls = append(r.calls, "Mkdir:"+rel)
	return nil
}

// fakeGraphStore is a fake OneDriveGraphStore recording EnsureFolder calls
// and the (driveID, itemID) pair WithRoot was most recently scoped to. mu
// guards ensureCalls/rootDrive/rootItem — F-5's fix deliberately leaves
// rootFor's steady-state fast path (ids already resolved) lock-free, so
// WithRoot can legitimately be called concurrently once the auto-create race
// settles; the real onedrivefs.Store.WithRoot has no shared mutable state to
// race on, but this fake's plain fields do.
type fakeGraphStore struct {
	mu                  sync.Mutex
	ensureCalls         int
	root                *recordingRoot
	rootDrive, rootItem string
}

func (f *fakeGraphStore) WithRoot(driveID, itemID string) oneDriveRoot {
	f.mu.Lock()
	f.rootDrive, f.rootItem = driveID, itemID
	f.mu.Unlock()
	return f.root
}

func (f *fakeGraphStore) EnsureFolder(ctx context.Context, tenant, user, path string) (string, string, error) {
	f.mu.Lock()
	f.ensureCalls++
	f.mu.Unlock()
	return "DRV1", "ITEM1", nil
}

func (f *fakeGraphStore) ListFolders(ctx context.Context, tenant, user, path string) ([]onedrivefs.FolderInfo, error) {
	return nil, nil
}

func TestOneDriveBackend_FirstUse_AutoCreatesAndPersistsIds(t *testing.T) {
	repo := newFakeWorkspacePrefsRepo()
	resolver := NewWorkspacePrefResolver(repo, fakeConfiguredChecker{configured: true})
	store := &fakeGraphStore{root: &recordingRoot{}}
	backend := &oneDriveBackend{Store: store, Resolver: resolver}

	if _, err := backend.Write(context.Background(), "t1", "u1", "notes.txt", []byte("hi")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if store.ensureCalls != 1 {
		t.Fatalf("expected EnsureFolder to be called once on first use, got %d", store.ensureCalls)
	}
	if store.rootDrive != "DRV1" || store.rootItem != "ITEM1" {
		t.Fatalf("expected WithRoot(DRV1,ITEM1), got (%q,%q)", store.rootDrive, store.rootItem)
	}
	row, found, err := repo.Get(context.Background(), "t1", "u1")
	if err != nil || !found {
		t.Fatalf("expected the resolved ids to be persisted, found=%v err=%v", found, err)
	}
	if row.DriveID != "DRV1" || row.RootItemID != "ITEM1" {
		t.Fatalf("expected persisted ids (DRV1,ITEM1), got %+v", row)
	}

	// A second op for the same user must reuse the persisted ids — EnsureFolder
	// (an idempotent but non-free Graph call) is not called again.
	if _, err := backend.Write(context.Background(), "t1", "u1", "notes2.txt", []byte("hi2")); err != nil {
		t.Fatalf("second write: %v", err)
	}
	if store.ensureCalls != 1 {
		t.Fatalf("expected EnsureFolder to be skipped once ids are persisted, got %d calls", store.ensureCalls)
	}
}

func TestOneDriveBackend_ExistingIds_SkipsEnsureFolder(t *testing.T) {
	repo := newFakeWorkspacePrefsRepo()
	repo.rows[repo.key("t1", "u1")] = db.WorkspacePrefs{
		Backend: "onedrive", OneDriveFolderPath: "Apps/Aikonos", DriveID: "DRV9", RootItemID: "ITEM9",
	}
	resolver := NewWorkspacePrefResolver(repo, fakeConfiguredChecker{configured: true})
	store := &fakeGraphStore{root: &recordingRoot{}}
	backend := &oneDriveBackend{Store: store, Resolver: resolver}

	if _, _, err := backend.Read(context.Background(), "t1", "u1", "notes.txt"); err != nil {
		t.Fatalf("read: %v", err)
	}
	if store.ensureCalls != 0 {
		t.Fatalf("expected EnsureFolder never called when ids are already persisted, got %d", store.ensureCalls)
	}
	if store.rootDrive != "DRV9" || store.rootItem != "ITEM9" {
		t.Fatalf("expected WithRoot(DRV9,ITEM9), got (%q,%q)", store.rootDrive, store.rootItem)
	}
}

// TestOneDriveBackend_ConcurrentFirstUse_EnsureFolderFiresOnce (F-5) proves
// rootFor's per-(tenant,user) keyed mutex actually serializes the auto-create
// bootstrap: two concurrent first-use ops for the SAME (tenant, user) must
// not both call EnsureFolder/Resolver.Set — one wins the race, the other's
// re-check (taken under the same lock) sees the already-persisted ids and
// skips straight to WithRoot. Calls rootFor directly (not Write) so the test
// is scoped to exactly the surface F-5 serializes; going through Write would
// additionally race on fakeGraphStore's single shared recordingRoot handle
// once every goroutine holds the resolved root — a fake-only artifact (the
// real onedrivefs.Rooted has no such shared mutable state), not something
// rootFor's lock is meant to cover. The resolver's cache is pre-warmed with
// one synchronous Effective() call before the goroutines start, so their
// OUTER (lock-free, steady-state-fast-path) ids-check is a pure cache hit
// rather than a concurrent fake-repo race of its own — isolating the test to
// exactly the section the new lock protects (mirrors workspacefs.Store's F14
// lockUser precedent, which likewise leaves lock-free reads alone). Run with
// -race.
func TestOneDriveBackend_ConcurrentFirstUse_EnsureFolderFiresOnce(t *testing.T) {
	repo := newFakeWorkspacePrefsRepo()
	resolver := NewWorkspacePrefResolver(repo, fakeConfiguredChecker{configured: true})
	store := &fakeGraphStore{root: &recordingRoot{}}
	backend := &oneDriveBackend{Store: store, Resolver: resolver}
	ctx := context.Background()

	if _, err := resolver.Effective(ctx, "t1", "u1"); err != nil {
		t.Fatalf("prewarm: %v", err)
	}

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := backend.rootFor(ctx, "t1", "u1")
			errs[i] = err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("op %d: %v", i, err)
		}
	}
	if store.ensureCalls != 1 {
		t.Fatalf("expected EnsureFolder to fire exactly once under concurrent first-use, got %d", store.ensureCalls)
	}
	if repo.setCalls != 1 {
		t.Fatalf("expected Resolver.Set to persist ids exactly once, got %d", repo.setCalls)
	}
	row, found, err := repo.Get(ctx, "t1", "u1")
	if err != nil || !found || row.DriveID != "DRV1" || row.RootItemID != "ITEM1" {
		t.Fatalf("expected persisted ids (DRV1,ITEM1), got found=%v row=%+v err=%v", found, row, err)
	}
}
