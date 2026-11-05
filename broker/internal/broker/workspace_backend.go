package broker

// workspace_backend.go — CP5: the PrefResolver + remote Backend adapters that
// wire onedrivefs.Store into workspacefs.Router.
// Three pieces:
//   - WorkspacePrefResolver: workspacefs.PrefResolver over db.WorkspacePrefsRepo,
//     applying the default rule (explicit row wins; no row + M365 configured ->
//     onedrive/Apps/Aikonos; else local), TTL-cached.
//   - oneDriveBackend: workspacefs.Backend over onedrivefs.Store, resolving
//     each (tenant,user)'s effective folder and auto-creating it on first use.
//   - oboTokenSource: adapts *connector.OBOBroker into onedrivefs.TokenSource,
//     mapping connector.ErrOBOReconnectRequired into the workspacefs sentinel
//     (onedrivefs itself never imports connector — see its package doc).

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/adamdekan/aikonos/broker/internal/connector"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/onedrivefs"
	"github.com/adamdekan/aikonos/broker/internal/workspacefs"
	"github.com/adamdekan/aikonos/broker/internal/workspacepath"
)

// defaultOneDriveFolderPath is the per-user default working folder applied by
// the resolver's default rule and by SetWorkspaceBackend when the caller
// omits a path.
const defaultOneDriveFolderPath = "Apps/Aikonos"

// effectivePrefCacheTTL bounds how stale a resolved pref can be before the
// next lookup re-reads Postgres. Sound only because the broker is a
// singleton (cmd/broker/singleton.go's advisory lock) — there is never a
// second in-memory cache to fall out of sync with.
const effectivePrefCacheTTL = 30 * time.Second

// workspacePrefsRepo is the seam WorkspacePrefResolver depends on — satisfied
// by *db.WorkspacePrefsRepo in production; narrowed to an interface
// (rpc-twins-tails precedent) so tests inject a fake instead of a live
// Postgres pool.
type workspacePrefsRepo interface {
	Get(ctx context.Context, tenant, user string) (db.WorkspacePrefs, bool, error)
	Set(ctx context.Context, tenant, user string, p db.WorkspacePrefs) error
}

// m365ConfiguredChecker reports whether a tenant has a usable M365 (OneDrive
// OBO) connection. Satisfied by *connector.OBOBroker — the same instance
// wired as Deps.OBOBroker for ensureOneDriveOBO.
type m365ConfiguredChecker interface {
	Configured(ctx context.Context, tenant string) bool
}

// EffectivePref is the fully-resolved workspace-backend preference for a
// (tenant, user) pair — the default rule already applied when no row exists.
type EffectivePref struct {
	Backend            workspacefs.BackendKind
	OneDriveFolderPath string
	DriveID            string
	RootItemID         string
	// Found reports whether an explicit row exists (SetWorkspaceBackend was
	// called at least once) as opposed to the default rule having been
	// applied for an absent row.
	Found bool
}

type prefCacheEntry struct {
	pref     EffectivePref
	cachedAt time.Time
}

// WorkspacePrefResolver implements workspacefs.PrefResolver over
// db.WorkspacePrefsRepo. Default rule (docs/spec's Storage section):
// explicit row wins; no row + M365 configured -> onedrive at
// defaultOneDriveFolderPath (ids resolved lazily by oneDriveBackend's
// first-use auto-create); no row + unconfigured -> local. A short TTL cache
// avoids a DB round trip on every file RPC; Set/Invalidate make a change
// visible on the very next call rather than waiting out the TTL.
type WorkspacePrefResolver struct {
	Repo       workspacePrefsRepo
	Configured m365ConfiguredChecker

	mu    sync.Mutex
	cache map[string]prefCacheEntry
}

// NewWorkspacePrefResolver builds the resolver wired in main.go.
func NewWorkspacePrefResolver(repo workspacePrefsRepo, configured m365ConfiguredChecker) *WorkspacePrefResolver {
	return &WorkspacePrefResolver{Repo: repo, Configured: configured, cache: make(map[string]prefCacheEntry)}
}

func prefCacheKey(tenant, user string) string { return tenant + "/" + user }

// WorkspaceBackend satisfies workspacefs.PrefResolver.
func (r *WorkspacePrefResolver) WorkspaceBackend(ctx context.Context, tenant, user string) (workspacefs.BackendKind, error) {
	pref, err := r.Effective(ctx, tenant, user)
	if err != nil {
		return "", err
	}
	return pref.Backend, nil
}

// Effective resolves the full effective pref (default rule applied), serving
// from the TTL cache when fresh.
func (r *WorkspacePrefResolver) Effective(ctx context.Context, tenant, user string) (EffectivePref, error) {
	if cached, hit := r.cached(tenant, user); hit {
		return cached, nil
	}
	pref, err := r.resolve(ctx, tenant, user)
	if err != nil {
		return EffectivePref{}, err
	}
	r.storeCache(tenant, user, pref)
	return pref, nil
}

func (r *WorkspacePrefResolver) resolve(ctx context.Context, tenant, user string) (EffectivePref, error) {
	if r.Repo == nil {
		return EffectivePref{Backend: workspacefs.KindLocal, OneDriveFolderPath: defaultOneDriveFolderPath}, nil
	}
	row, found, err := r.Repo.Get(ctx, tenant, user)
	if err != nil {
		return EffectivePref{}, err
	}
	if found {
		return EffectivePref{
			Backend:            workspacefs.BackendKind(row.Backend),
			OneDriveFolderPath: row.OneDriveFolderPath,
			DriveID:            row.DriveID,
			RootItemID:         row.RootItemID,
			Found:              true,
		}, nil
	}
	if r.Configured != nil && r.Configured.Configured(ctx, tenant) {
		return EffectivePref{Backend: workspacefs.KindOneDrive, OneDriveFolderPath: defaultOneDriveFolderPath}, nil
	}
	return EffectivePref{Backend: workspacefs.KindLocal, OneDriveFolderPath: defaultOneDriveFolderPath}, nil
}

func (r *WorkspacePrefResolver) cached(tenant, user string) (EffectivePref, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.cache[prefCacheKey(tenant, user)]
	if !ok || time.Since(e.cachedAt) > effectivePrefCacheTTL {
		return EffectivePref{}, false
	}
	return e.pref, true
}

func (r *WorkspacePrefResolver) storeCache(tenant, user string, pref EffectivePref) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cache[prefCacheKey(tenant, user)] = prefCacheEntry{pref: pref, cachedAt: time.Now()}
}

// Invalidate clears the cached entry for (tenant, user) so the very next
// resolution re-reads the repo instead of serving a stale TTL-cached value.
func (r *WorkspacePrefResolver) Invalidate(tenant, user string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.cache, prefCacheKey(tenant, user))
}

// InvalidateTenant clears every cached entry belonging to tenant (all
// users) — used when a tenant-wide config change (M365 connection
// upsert/delete) must take effect on the very next resolution for every user,
// not just whichever single (tenant,user) pair Invalidate would otherwise
// target.
func (r *WorkspacePrefResolver) InvalidateTenant(tenant string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	prefix := tenant + "/"
	for k := range r.cache {
		if strings.HasPrefix(k, prefix) {
			delete(r.cache, k)
		}
	}
}

// Set persists an explicit pref row and invalidates the cache — used by
// SetWorkspaceBackend and by oneDriveBackend's first-use auto-create to
// persist the ids EnsureFolder resolved.
func (r *WorkspacePrefResolver) Set(ctx context.Context, tenant, user string, p db.WorkspacePrefs) error {
	if r.Repo == nil {
		return fmt.Errorf("workspace prefs repo not configured")
	}
	if err := r.Repo.Set(ctx, tenant, user, p); err != nil {
		return err
	}
	r.Invalidate(tenant, user)
	return nil
}

// ── oneDriveBackend: workspacefs.Backend over onedrivefs.Store ────────────

// oneDriveRoot is the per-(driveID,itemID)-scoped operation surface
// *onedrivefs.Rooted exposes — narrowed to an interface so tests can fake a
// Graph-backed root for the adapter logic (auto-create-on-first-use, id
// persistence) without standing up an httptest server; onedrivefs' own test
// suite covers the real Graph wire contract. Go requires an exact return-type
// match for interface satisfaction, so OneDriveGraphStore.WithRoot can't
// return *onedrivefs.Rooted directly — graphStoreAdapter bridges that.
type oneDriveRoot interface {
	ListDir(ctx context.Context, tenant, user, dir string, recursive bool) ([]workspacefs.FileInfo, error)
	Read(ctx context.Context, tenant, user, rel string) ([]byte, time.Time, error)
	Write(ctx context.Context, tenant, user, rel string, data []byte) (workspacefs.FileInfo, error)
	Delete(ctx context.Context, tenant, user, rel string) error
	Move(ctx context.Context, tenant, user, from, to string) (workspacefs.FileInfo, error)
	Mkdir(ctx context.Context, tenant, user, rel string) error
}

// OneDriveGraphStore is the seam oneDriveBackend (and workspace_prefs.go's
// SetWorkspaceBackend/ListOneDriveFolders handlers) depend on for Graph
// access — satisfied by graphStoreAdapter wrapping *onedrivefs.Store.
type OneDriveGraphStore interface {
	WithRoot(driveID, itemID string) oneDriveRoot
	EnsureFolder(ctx context.Context, tenant, user, path string) (driveID, itemID string, err error)
	ListFolders(ctx context.Context, tenant, user, path string) ([]onedrivefs.FolderInfo, error)
}

// graphStoreAdapter adapts *onedrivefs.Store into OneDriveGraphStore: its
// WithRoot returns the concrete *onedrivefs.Rooted, which Go's interface
// satisfaction rules won't accept in place of the oneDriveRoot interface
// return type without this thin wrapper.
type graphStoreAdapter struct {
	store *onedrivefs.Store
}

func (a graphStoreAdapter) WithRoot(driveID, itemID string) oneDriveRoot {
	return a.store.WithRoot(driveID, itemID)
}

func (a graphStoreAdapter) EnsureFolder(ctx context.Context, tenant, user, path string) (string, string, error) {
	return a.store.EnsureFolder(ctx, tenant, user, path)
}

func (a graphStoreAdapter) ListFolders(ctx context.Context, tenant, user, path string) ([]onedrivefs.FolderInfo, error) {
	return a.store.ListFolders(ctx, tenant, user, path)
}

var (
	_ oneDriveRoot       = (*onedrivefs.Rooted)(nil)
	_ OneDriveGraphStore = graphStoreAdapter{}
)

// NewOneDriveGraphAdapter wraps store to satisfy the package-private
// OneDriveGraphStore seam both NewOneDriveRemoteBackend and
// SetWorkspaceBackend/ListOneDriveFolders (Deps.Workspace.Graph) depend on.
// Exported so main.go — which constructs *onedrivefs.Store itself and can't
// name graphStoreAdapter directly — has a way to build a compatible value;
// callers hold the result via type inference (`:=`) rather than naming its
// (unexported) type.
func NewOneDriveGraphAdapter(store *onedrivefs.Store) OneDriveGraphStore {
	return graphStoreAdapter{store: store}
}

// oneDriveBackend implements workspacefs.Backend over onedrivefs.Store,
// resolving each (tenant,user)'s effective OneDrive folder via Resolver. When
// the effective pref has no resolved ids yet (the default-rule "onedrive"
// posture — never explicitly Set), the folder is auto-created on first use
// and the resolved ids are persisted so subsequent calls reuse them without
// re-hitting Graph's create-folder path (spec: "auto-created on first use").
type oneDriveBackend struct {
	Store    OneDriveGraphStore
	Resolver *WorkspacePrefResolver

	// mu guards locks (the map itself); locks holds one *sync.Mutex per
	// (tenant,user) pair, serializing ONLY the first-use auto-create path in
	// rootFor below (F-5, same keyed-mutex precedent as workspacefs.Store's
	// F14 lockUser) — so two concurrent first ops for the same user can't
	// both call EnsureFolder/Resolver.Set. Steady-state ops (ids already
	// resolved) stay lock-free.
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func (b *oneDriveBackend) Enabled() bool { return true }

// lockUser acquires the per-(tenant,user) mutex and returns the unlock func —
// mirrors workspacefs.Store.lockUser (same map-of-mutexes pattern, same
// workspacepath.SanitizeSeg key-collision guard so a "/" in an unsanitized
// tenant/user value can't collide two different pairs onto one lock),
// lazily initializing locks on first use since oneDriveBackend is often
// constructed via a bare struct literal (see NewOneDriveRemoteBackend and the
// test fakes) rather than a constructor.
func (b *oneDriveBackend) lockUser(tenant, user string) func() {
	key := workspacepath.SanitizeSeg(tenant) + "/" + workspacepath.SanitizeSeg(user)
	b.mu.Lock()
	l, ok := b.locks[key]
	if !ok {
		l = &sync.Mutex{}
		if b.locks == nil {
			b.locks = make(map[string]*sync.Mutex)
		}
		b.locks[key] = l
	}
	b.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// rootFor resolves the (driveID, itemID) to operate against for (tenant,
// user), auto-creating + persisting on first use when needed. The steady-
// state fast path (ids already resolved) is lock-free; only the auto-create
// branch takes the per-(tenant,user) lock, re-checking under it in case
// another goroutine already won the bootstrap race while this one waited.
func (b *oneDriveBackend) rootFor(ctx context.Context, tenant, user string) (oneDriveRoot, error) {
	pref, err := b.Resolver.Effective(ctx, tenant, user)
	if err != nil {
		return nil, err
	}
	if pref.DriveID != "" && pref.RootItemID != "" {
		return b.Store.WithRoot(pref.DriveID, pref.RootItemID), nil
	}

	unlock := b.lockUser(tenant, user)
	defer unlock()

	pref, err = b.Resolver.Effective(ctx, tenant, user)
	if err != nil {
		return nil, err
	}
	if pref.DriveID != "" && pref.RootItemID != "" {
		return b.Store.WithRoot(pref.DriveID, pref.RootItemID), nil
	}
	folderPath := pref.OneDriveFolderPath
	if folderPath == "" {
		folderPath = defaultOneDriveFolderPath
	}
	driveID, itemID, err := b.Store.EnsureFolder(ctx, tenant, user, folderPath)
	if err != nil {
		return nil, err
	}
	if err := b.Resolver.Set(ctx, tenant, user, db.WorkspacePrefs{
		Backend:            string(workspacefs.KindOneDrive),
		OneDriveFolderPath: folderPath,
		DriveID:            driveID,
		RootItemID:         itemID,
	}); err != nil {
		return nil, err
	}
	return b.Store.WithRoot(driveID, itemID), nil
}

func (b *oneDriveBackend) List(ctx context.Context, tenant, user string) ([]workspacefs.FileInfo, error) {
	return b.ListDir(ctx, tenant, user, "", true)
}

func (b *oneDriveBackend) ListDir(ctx context.Context, tenant, user, dir string, recursive bool) ([]workspacefs.FileInfo, error) {
	root, err := b.rootFor(ctx, tenant, user)
	if err != nil {
		return nil, err
	}
	return root.ListDir(ctx, tenant, user, dir, recursive)
}

func (b *oneDriveBackend) Read(ctx context.Context, tenant, user, rel string) ([]byte, time.Time, error) {
	root, err := b.rootFor(ctx, tenant, user)
	if err != nil {
		return nil, time.Time{}, err
	}
	return root.Read(ctx, tenant, user, rel)
}

func (b *oneDriveBackend) Write(ctx context.Context, tenant, user, rel string, data []byte) (workspacefs.FileInfo, error) {
	root, err := b.rootFor(ctx, tenant, user)
	if err != nil {
		return workspacefs.FileInfo{}, err
	}
	return root.Write(ctx, tenant, user, rel, data)
}

func (b *oneDriveBackend) Delete(ctx context.Context, tenant, user, rel string) error {
	root, err := b.rootFor(ctx, tenant, user)
	if err != nil {
		return err
	}
	return root.Delete(ctx, tenant, user, rel)
}

func (b *oneDriveBackend) Move(ctx context.Context, tenant, user, from, to string) (workspacefs.FileInfo, error) {
	root, err := b.rootFor(ctx, tenant, user)
	if err != nil {
		return workspacefs.FileInfo{}, err
	}
	return root.Move(ctx, tenant, user, from, to)
}

func (b *oneDriveBackend) Mkdir(ctx context.Context, tenant, user, rel string) error {
	root, err := b.rootFor(ctx, tenant, user)
	if err != nil {
		return err
	}
	return root.Mkdir(ctx, tenant, user, rel)
}

var _ workspacefs.Backend = (*oneDriveBackend)(nil)

// NewOneDriveRemoteBackend builds the workspacefs.Backend CP5 wires into both
// workspace Routers' Remote field. graph is normally
// NewOneDriveGraphAdapter's result — sharing that same instance also as
// Deps.Workspace.Graph is what lets SetWorkspaceBackend's EnsureFolder call and
// this backend's own auto-create-on-first-use hit the identical underlying
// *onedrivefs.Store.
func NewOneDriveRemoteBackend(graph OneDriveGraphStore, resolver *WorkspacePrefResolver) workspacefs.Backend {
	return &oneDriveBackend{Store: graph, Resolver: resolver}
}

// ── oboTokenSource: connector.OBOBroker -> onedrivefs.TokenSource ─────────

// oboTokenSource adapts *connector.OBOBroker.FreshAccessToken into
// onedrivefs.TokenSource, mapping connector.ErrOBOReconnectRequired into
// workspacefs.ErrReconnectRequired (preserving the grep-stable
// "reconnect_needed" substring) — the mapping onedrivefs' own package doc
// says deliberately does NOT happen inside onedrivefs, so it never imports
// broker/internal/connector.
type oboTokenSource struct {
	Broker *connector.OBOBroker
}

func (a oboTokenSource) FreshAccessToken(ctx context.Context, tenant, user string) (string, error) {
	tok, err := a.Broker.FreshAccessToken(ctx, tenant, user)
	if err != nil {
		if errors.Is(err, connector.ErrOBOReconnectRequired) {
			return "", fmt.Errorf("%w: %v", workspacefs.ErrReconnectRequired, err)
		}
		return "", err
	}
	return tok, nil
}

var _ onedrivefs.TokenSource = oboTokenSource{}

// NewOBOTokenSource adapts b into onedrivefs.TokenSource — main.go's sole
// construction entrypoint for oboTokenSource (unexported, so it can't be
// named directly outside this package).
func NewOBOTokenSource(b *connector.OBOBroker) onedrivefs.TokenSource {
	return oboTokenSource{Broker: b}
}
