package workspacefs

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// BackendKind identifies which storage backend a (tenant, user) pair's
// working folder currently points at.
type BackendKind string

const (
	KindLocal    BackendKind = "local"
	KindOneDrive BackendKind = "onedrive"
)

// PrefResolver resolves a user's active workspace-backend preference (CP5:
// db.WorkspacePrefsRepo behind a TTL cache). nil is a valid, fully-supported
// value — see Router.activeBackend — meaning "no preference lookups wired
// yet", the CP3/CP4 posture.
type PrefResolver interface {
	// WorkspaceBackend returns the active backend kind for (tenant, user).
	WorkspaceBackend(ctx context.Context, tenant, user string) (BackendKind, error)
}

// reservedFirstSegment is the case-insensitive set of first path segments
// that always route to the local Store, regardless of the active backend
// preference and BEFORE any Prefs lookup: session records (+ HMAC sidecars),
// connector/platform metadata, and vision reference images must never reach
// a remote backend (see 's Invariants).
// "skills" joins this set for personal-skill authoring — unlike the other three, its
// subtree stays fully visible/writable through Files, uploads, and
// doc.write; it is reserved only from the backend-preference decision, not
// from the explorer/config hiding rules in store.go's reservedTop.
var reservedFirstSegment = map[string]bool{
	".agent":     true,
	"config":     true,
	"references": true,
	"skills":     true,
}

// Router is the workspacefs.Backend implementation that dispatches each call
// between Local and Remote based on the caller's active workspace-backend
// preference, with reserved first-path-segments always pinned to Local. Its
// zero-Remote/zero-Prefs form (main.go's current wiring: &Router{Local:
// store}) is behavior-neutral — every call routes to Local exactly as if the
// Router weren't there at all. This is the whole point of CP3: the seam is
// inert until CP5 wires a remote Backend + PrefResolver.
//
// Local must be non-nil: every method call it unconditionally (at minimum for
// reserved-path routing), so a zero-value Router panics on first use. Remote
// and Prefs are the only fields with a supported nil/zero-value form —
// main.go always constructs Router with Local set.
type Router struct {
	Local  *Store
	Remote Backend
	Prefs  PrefResolver
}

func (r *Router) Enabled() bool { return r.Local.Enabled() }

// isReserved reports whether an already-CleanRel'd relative path's first
// segment is one of reservedFirstSegment's entries (case-insensitive) — the
// single lookup shared by route() and both of Move()'s from/to checks.
func isReserved(cleanRel string) bool {
	return reservedFirstSegment[strings.ToLower(firstSegment(cleanRel))]
}

// IsReservedRelPath is isReserved exported for broker/internal/broker's
// SetWorkspaceBackend (CP5), which must reject a working-folder path landing
// inside a reserved subtree (.agent/config/references) before ever calling
// ensureOneDriveOBO/EnsureFolder — rel must already be CleanRel'd.
func IsReservedRelPath(cleanRel string) bool {
	return isReserved(cleanRel)
}

// activeBackend decides which backend a NON-reserved path should use.
// Prefs == nil means preference lookups aren't wired at all (CP3/CP4's
// behavior-neutral posture) — Local, no Prefs call, matching Store's
// existing behavior exactly. Otherwise Prefs is consulted: an error fails
// loud as ErrUnavailable (never a silent fallback to Local), and a reported
// "onedrive" preference with no Remote wired is also ErrUnavailable rather
// than silently downgrading to Local.
func (r *Router) activeBackend(ctx context.Context, tenant, user string) (BackendKind, error) {
	if r.Prefs == nil {
		return KindLocal, nil
	}
	kind, err := r.Prefs.WorkspaceBackend(ctx, tenant, user)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if kind == KindOneDrive && r.Remote == nil {
		return "", ErrUnavailable
	}
	return kind, nil
}

// route cleans rel and decides Local vs Remote for a single-path operation.
// Reserved first segments always win, decided on the CLEANED path (so
// traversal spellings like "a/../.agent/x" still pin local) — checked before
// activeBackend, so Prefs is never consulted for a reserved path. A cleaning
// failure surfaces as ErrInvalidPath.
func (r *Router) route(ctx context.Context, tenant, user, rel string) (BackendKind, error) {
	clean, err := CleanRel(rel)
	if err != nil {
		return "", err
	}
	if isReserved(clean) {
		return KindLocal, nil
	}
	return r.activeBackend(ctx, tenant, user)
}

// ActiveKind reports which backend a call for rel would route to right now
// for (tenant, user) — the single source of truth for the routing decision.
// This is what broker/internal/broker's fileRPCRoutesOneDrive delegates to
// instead of re-implementing CleanRel+isReserved+Prefs itself (see that
// function's doc comment); List/ListDir below are its other caller. rel == ""
// or "." (the List/ListDir root-scope convention) is treated as a
// non-reserved path: CleanRel rejects the empty string, so those two
// spellings skip straight to activeBackend — which is what surfaces the
// Remote == nil-with-an-onedrive-Prefs case as ErrUnavailable rather than a
// silent Local fallback (see activeBackend's doc comment). Any other rel goes
// through the ordinary reserved-first-segment-then-Prefs decision (route()).
func (r *Router) ActiveKind(ctx context.Context, tenant, user, rel string) (BackendKind, error) {
	if rel == "" || rel == "." {
		return r.activeBackend(ctx, tenant, user)
	}
	return r.route(ctx, tenant, user, rel)
}

func (r *Router) List(ctx context.Context, tenant, user string) ([]FileInfo, error) {
	return r.ListDir(ctx, tenant, user, "", true)
}

func (r *Router) ListDir(ctx context.Context, tenant, user, dir string, recursive bool) ([]FileInfo, error) {
	kind, err := r.ActiveKind(ctx, tenant, user, dir)
	if err != nil {
		return nil, err
	}
	if kind != KindOneDrive {
		return r.Local.ListDir(ctx, tenant, user, dir, recursive)
	}
	remote, err := r.Remote.ListDir(ctx, tenant, user, dir, recursive)
	if err != nil {
		return nil, err
	}
	if dir != "" && dir != "." {
		// Only the root scope has reserved subtrees to merge — a single-path
		// op under a non-root OneDrive directory already routes to Local via
		// route()'s isReserved check (see ActiveKind), so it never reaches
		// here with kind==KindOneDrive in the first place.
		return remote, nil
	}
	// Drop any Remote top-level entry whose first segment is reserved: a
	// user-created OneDrive folder literally named e.g. "references" would
	// otherwise duplicate (and shadow the unreachability of) the Local
	// reserved entry appended below — isReserved always pins ops on that
	// name to Local (route()), so a Remote entry there can never be read/
	// written/moved and must not be listed either.
	filtered := remote[:0]
	for _, fi := range remote {
		if isReserved(fi.Path) {
			slog.Default().Warn("workspacefs: dropping remote top-level entry shadowed by a reserved local subtree",
				"tenant", tenant, "user", user, "path", fi.Path)
			continue
		}
		filtered = append(filtered, fi)
	}
	return append(filtered, reservedEntries(ctx, r.Local, tenant, user, recursive)...), nil
}

// reservedEntries lists Local's full tree and keeps only the entries whose
// first path segment is reserved (.agent/config/references) — exactly the
// subtrees a single-path op on those prefixes would route to Local for (see
// isReserved). Anything else in Local (e.g. content left over from before the
// backend switched to OneDrive) is deliberately excluded: it has no live
// routing path once the active backend is remote, so surfacing it in a
// listing would resurrect orphaned files. A Local read error is logged (never
// silently swallowed to an invisible empty slice) but still yields an empty
// slice rather than failing the whole listing — Remote already answered
// successfully, and reserved entries are supplementary to it.
func reservedEntries(ctx context.Context, local *Store, tenant, user string, recursive bool) []FileInfo {
	entries, err := local.ListDir(ctx, tenant, user, "", recursive)
	if err != nil {
		slog.Default().Error("workspacefs: local read failed while merging reserved entries into a remote root listing",
			"tenant", tenant, "user", user, "error", err)
		return nil
	}
	out := entries[:0]
	for _, fi := range entries {
		if isReserved(fi.Path) {
			out = append(out, fi)
		}
	}
	return out
}

func (r *Router) Read(ctx context.Context, tenant, user, rel string) ([]byte, time.Time, error) {
	kind, err := r.route(ctx, tenant, user, rel)
	if err != nil {
		return nil, time.Time{}, err
	}
	if kind == KindOneDrive {
		return r.Remote.Read(ctx, tenant, user, rel)
	}
	return r.Local.Read(ctx, tenant, user, rel)
}

// ReadVerified is not part of Backend (see backend.go) but is required by the
// broker package's workspaceFS consumer interface. Reserved paths and
// Local-routed non-reserved paths delegate to Local.ReadVerified (HMAC
// sidecar verification applies exactly as it does calling Local directly);
// a remote-routed path has no sidecar concept, so it delegates to
// Remote.Read and reports legacy=false unconditionally.
func (r *Router) ReadVerified(ctx context.Context, tenant, user, rel string) (data []byte, modTime time.Time, legacy bool, err error) {
	kind, err := r.route(ctx, tenant, user, rel)
	if err != nil {
		return nil, time.Time{}, false, err
	}
	if kind == KindOneDrive {
		data, modTime, err = r.Remote.Read(ctx, tenant, user, rel)
		return data, modTime, false, err
	}
	return r.Local.ReadVerified(ctx, tenant, user, rel)
}

func (r *Router) Write(ctx context.Context, tenant, user, rel string, data []byte) (FileInfo, error) {
	kind, err := r.route(ctx, tenant, user, rel)
	if err != nil {
		return FileInfo{}, err
	}
	if kind == KindOneDrive {
		return r.Remote.Write(ctx, tenant, user, rel, data)
	}
	return r.Local.Write(ctx, tenant, user, rel, data)
}

func (r *Router) Delete(ctx context.Context, tenant, user, rel string) error {
	kind, err := r.route(ctx, tenant, user, rel)
	if err != nil {
		return err
	}
	if kind == KindOneDrive {
		return r.Remote.Delete(ctx, tenant, user, rel)
	}
	return r.Local.Delete(ctx, tenant, user, rel)
}

func (r *Router) Mkdir(ctx context.Context, tenant, user, rel string) error {
	kind, err := r.route(ctx, tenant, user, rel)
	if err != nil {
		return err
	}
	if kind == KindOneDrive {
		return r.Remote.Mkdir(ctx, tenant, user, rel)
	}
	return r.Local.Mkdir(ctx, tenant, user, rel)
}

// Move renames/relocates from -> to. Both endpoints are cleaned and checked
// for the reserved-first-segment rule independently: both reserved always
// stays Local; both non-reserved routes on the active backend (Remote when
// preference is onedrive); a MIXED pair (one reserved, one not) is rejected
// as ErrInvalidPath when the active backend is remote (a working-folder file
// and internal storage live on different backends — there is no cross-backend
// move), but is allowed straight through to Local when the active backend is
// local, since today everything really is one disk (behavior-neutral: this
// is the only path an existing caller could hit in CP3, where Remote is nil).
func (r *Router) Move(ctx context.Context, tenant, user, from, to string) (FileInfo, error) {
	fromClean, err := CleanRel(from)
	if err != nil {
		return FileInfo{}, err
	}
	toClean, err := CleanRel(to)
	if err != nil {
		return FileInfo{}, err
	}
	fromReserved := isReserved(fromClean)
	toReserved := isReserved(toClean)

	if fromReserved && toReserved {
		return r.Local.Move(ctx, tenant, user, from, to)
	}

	kind, err := r.activeBackend(ctx, tenant, user)
	if err != nil {
		return FileInfo{}, err
	}
	if fromReserved != toReserved {
		if kind == KindOneDrive {
			return FileInfo{}, fmt.Errorf("%w: cannot move between working folder and internal storage", ErrInvalidPath)
		}
		return r.Local.Move(ctx, tenant, user, from, to)
	}
	// Neither is reserved: route on the active backend like any other write.
	if kind == KindOneDrive {
		return r.Remote.Move(ctx, tenant, user, from, to)
	}
	return r.Local.Move(ctx, tenant, user, from, to)
}
