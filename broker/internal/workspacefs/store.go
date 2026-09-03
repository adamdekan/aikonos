// Package workspacefs is the user-facing file manager over the per-user
// workspace (Root/<tenant>/<user>/) — the same dir the Tool Proxy writes docs to
// and the workspace.read/doc.read tools read from. It backs the browser file
// explorer: an authenticated user listing/reading/uploading/deleting files in
// their OWN workspace. Files uploaded here are visible to that user's agents with
// no further change.
//
// Path safety mirrors broker/internal/toolproxy (safeJoin + sanitizeSeg): tenant
// and user segments are sanitised; the relative path may not be absolute, may not
// escape the user dir via "..", and may not touch the reserved "config/" subtree
// (the connector reference file lives there).
package workspacefs

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/adamdekan/aikonos/broker/internal/workspacepath"
)

// MaxFileBytes caps a single uploaded/read file. Unary gRPC bytes fields make
// large files impractical (see the broker's raised message-size limit); bigger
// files want streaming/object-store upload (a follow-up).
const MaxFileBytes = 10 << 20 // 10 MiB

// Sentinel errors let the RPC layer map to gRPC codes: ErrInvalidPath/ErrTooLarge
// → InvalidArgument, a missing file (fs.ErrNotExist) → NotFound, ErrExists →
// AlreadyExists, ErrNotEmpty → FailedPrecondition, ErrUnavailable →
// Unavailable, ErrReconnectRequired → FailedPrecondition, ErrForbidden →
// PermissionDenied, else Internal.
var (
	ErrInvalidPath = errors.New("workspacefs: invalid path")
	ErrTooLarge    = errors.New("workspacefs: file too large")
	ErrExists      = errors.New("workspacefs: path already exists")
	ErrNotEmpty    = errors.New("workspacefs: directory is not empty")

	// ErrIntegrityMismatch is returned by ReadVerified when a session record's
	// on-disk content no longer matches its HMAC sidecar (CP4.2). The message
	// text is grep-stable — the RPC layer maps it verbatim into a
	// FailedPrecondition status, and ops tooling may grep logs for it.
	ErrIntegrityMismatch = errors.New("workspacefs: session integrity check failed")

	// ErrUnavailable is returned by a Backend (Router, CP5's onedrivefs.Store)
	// when the remote backend or a preference lookup can't be reached right
	// now — fails loud rather than silently falling back to the local store.
	ErrUnavailable = errors.New("workspacefs: backend temporarily unavailable")

	// ErrReconnectRequired signals a dead/rotten remote-backend credential
	// (CP5's OBO refresh token). The "reconnect_needed" substring is
	// grep-stable — ops tooling and (later) the webui's reconnect banner key
	// off it, so any wrapping must preserve the message verbatim.
	ErrReconnectRequired = errors.New("workspacefs: reconnect_needed, remote backend credential is stale")

	// ErrForbidden is returned by a Backend (CP5's onedrivefs.Store) when the
	// remote backend rejects a call as a permissions error (e.g. a 403 from
	// Graph) — wrapped and never conflated with ErrUnavailable, since a
	// permissions denial is not a transient/retryable condition.
	ErrForbidden = errors.New("workspacefs: forbidden")
)

// reservedTop is the set of top-level path segments the explorer hides and
// refuses to mutate — internal platform state, not user documents.
var reservedTop = map[string]bool{"config": true}

// sigSuffix is appended to a session record's path to name its HMAC sidecar
// (CP4.2): ".agent/Sessions/<id>.json" -> ".agent/Sessions/<id>.json.sig".
const sigSuffix = ".sig"

// SessionsDirPrefix is the workspace-relative directory whose writes get an
// HMAC-SHA256 integrity sidecar (CP4.2) — session records persisted by the
// broker scheduler (scheduler.go) and by the webui via the generic
// UploadWorkspaceFile/ReadWorkspaceFile/MoveWorkspaceFile RPCs
// (broker/internal/broker/files.go).
//
// Session-ness is decided EXCLUSIVELY from the resolved (Join-cleaned)
// absolute path — see resolveSession — never from the caller's raw rel
// string. A first cut of this feature exported an IsSessionRecordPath(rel
// string) helper that raw-prefix-tested the caller's string before it was
// resolved; "./.agent/Sessions/x.json" or "a/../.agent/Sessions/x.json"
// resolve into the real Sessions directory (filepath.Join collapses both
// forms identically) but failed that raw check, so they were written
// unsigned and later read back as "legacy" with no sidecar to verify against
// — a live bypass of the integrity guarantee. Centralizing the check inside
// Write/Read/Move at the resolved-path layer (this file) closes that class of
// bug structurally: any current or future Store operation that lands inside
// SessionsDirPrefix is signed/verified, regardless of how the caller spelled
// the path.
const SessionsDirPrefix = ".agent/Sessions/"

// sessionsDir is SessionsDirPrefix without its trailing slash — the directory
// itself, as opposed to something below it.
const sessionsDir = ".agent/Sessions"

// agentDir is the top-level server-maintained subtree SessionsDirPrefix lives
// under.
const agentDir = ".agent"

// guardClean normalizes rel for the two exported predicates below, by calling
// the Store's OWN resolver rather than re-deriving normalization. This is the
// point, not an implementation detail: an earlier cut cleaned with
// path.Clean(filepath.ToSlash(rel)) while CleanRel trims leading whitespace
// FIRST, so " .agent/Memory/x.md" read as "not under .agent/" to the guard and
// as ".agent/Memory/x.md" to the resolver that then wrote it — two independent
// normalizations of one string is the bug, and one source is the fix.
//
// A CleanRel error reports "not under": the Store refuses those paths outright,
// so the guard has nothing left to protect and need not double-report.
func guardClean(rel string) (string, bool) {
	clean, err := CleanRel(rel)
	if err != nil {
		return "", false
	}
	return filepath.ToSlash(clean), true
}

// UnderAgentDir reports whether rel targets .agent/ or anything below it — the
// server-maintained tree (memory bundles, Sessions). Exported because two
// independent boundaries gate on it with different policies: the toolproxy seam
// refuses all of it, the user-facing file RPCs refuse it except Sessions.
//
// Matched case-insensitively, like router.go's isReserved: this is the BLOCK
// side of the gate, so over-matching only ever refuses more. On a
// case-insensitive mount ".AGENT/Memory/x.md" resolves into the real bundle,
// and a case-sensitive predicate would wave it through. UnderSessionsDir is
// deliberately NOT casefolded — see there.
func UnderAgentDir(rel string) bool {
	clean, ok := guardClean(rel)
	if !ok {
		return false
	}
	clean = strings.ToLower(clean)
	return clean == agentDir || strings.HasPrefix(clean, agentDir+"/")
}

// UnderSessionsDir reports whether rel targets .agent/Sessions/ or anything
// below it. Normalized via guardClean, for the same reason as UnderAgentDir.
//
// Case-SENSITIVE, unlike UnderAgentDir: this is the ALLOW side (it carves the
// signed Sessions tree out of the .agent/ block), and it must not admit more
// than resolveSession's own case-sensitive prefix test signs. Casefolding here
// would let ".agent/sessions/x.json" past the block and then leave it unsigned.
func UnderSessionsDir(rel string) bool {
	clean, ok := guardClean(rel)
	if !ok {
		return false
	}
	return clean == sessionsDir || strings.HasPrefix(clean, SessionsDirPrefix)
}

// resolveSession resolves rel exactly as every other Store operation does
// (resolve(): Join-clean, traversal/reserved-segment rejection) and reports
// whether the RESOLVED path falls under SessionsDirPrefix. See the
// SessionsDirPrefix doc comment for why this must run on the resolved path,
// not the raw rel string.
func (s *Store) resolveSession(tenant, user, rel string) (clean string, isSession bool, err error) {
	clean, err = s.resolve(tenant, user, rel)
	if err != nil {
		return "", false, err
	}
	base := s.userDir(tenant, user)
	relClean, rerr := filepath.Rel(base, clean)
	if rerr != nil {
		return clean, false, nil
	}
	return clean, strings.HasPrefix(filepath.ToSlash(relClean), SessionsDirPrefix), nil
}

// FileInfo is one file in the user's workspace. Path is relative to the user dir
// (forward slashes), so subfolders appear as "reports/q1.csv".
type FileInfo struct {
	Path    string
	Size    int64
	ModTime time.Time
	IsDir   bool
}

// Store serves a user's workspace files under a root. An empty root disables it.
type Store struct {
	root string

	// mu guards locks (the map itself); locks holds one *sync.Mutex per
	// sanitized (tenant,user) pair, serializing that user's check-then-act
	// mutations (Move/Mkdir/Delete) so a stat and the act it gates can't be
	// interleaved by a concurrent call for the same user. Per-process locking
	// is sound here because deployment enforces exactly one broker process
	// (see cmd/broker/singleton.go's advisory-lock singleton) — there is
	// never a second process racing this one against the same workspace.
	mu    sync.Mutex
	locks map[string]*sync.Mutex

	// sessionKey is the HMAC-SHA256 key (CP4.2) used by Write/Read/Move to
	// sign/verify session-record sidecars for any resolved path under
	// SessionsDirPrefix. nil/empty disables signing entirely — Write skips the
	// sidecar and Read/ReadVerified have nothing to verify against, mirroring
	// the audit emitter's pre-existing "empty key == unsigned" degraded
	// posture (broker/internal/audit/signing_key.go).
	sessionKey []byte
}

func New(root string) *Store { return &Store{root: root, locks: make(map[string]*sync.Mutex)} }

func (s *Store) Enabled() bool { return s.root != "" }

// SetSessionSigningKey installs the HMAC key used by Write/Read/ReadVerified/
// Move for session-record sidecars. Called once at startup
// (broker/cmd/broker/main.go, resolveWorkspaceSessionKey) after both the
// north and south Store instances are constructed, so a session written by
// one gRPC surface verifies cleanly when read back through the other. Not
// safe to call concurrently with Write/Read/Move.
func (s *Store) SetSessionSigningKey(key []byte) { s.sessionKey = key }

// signingEnabled reports whether a non-empty key is installed.
func (s *Store) signingEnabled() bool { return len(s.sessionKey) > 0 }

func (s *Store) sign(data []byte) string {
	mac := hmac.New(sha256.New, s.sessionKey)
	mac.Write(data)
	return hex.EncodeToString(mac.Sum(nil))
}

// lockUser acquires the per-(tenant,user) mutex and returns the unlock func.
// Callers hold it around their full check-then-act sequence (stat, then
// create/rename/remove). Read paths (List/ListDir/Read) intentionally do not
// take this lock.
func (s *Store) lockUser(tenant, user string) func() {
	key := sanitizeSeg(tenant) + "/" + sanitizeSeg(user)
	s.mu.Lock()
	l, ok := s.locks[key]
	if !ok {
		l = &sync.Mutex{}
		s.locks[key] = l
	}
	s.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// sanitizeSeg delegates to the one shared (tenant, user)→path rule; kept as a
// thin unexported wrapper so the existing test suite's direct references
// don't need to change.
func sanitizeSeg(seg string) string {
	return workspacepath.SanitizeSeg(seg)
}

func (s *Store) userDir(tenant, user string) string {
	return filepath.Join(s.root, sanitizeSeg(tenant), sanitizeSeg(user))
}

// CleanRel validates and cleans a caller-supplied workspace-relative path in
// isolation — no tenant/user/root involved. It rejects an empty string, an
// absolute path, and any spelling that escapes its own root via ".."
// (including indirect ones like "reports/../../x" or "a/../.agent/x" — the
// escape/reserved-segment check always runs against this CLEANED form, never
// the caller's raw string; see SessionsDirPrefix's doc comment for the bug
// class that distinction closes). It does NOT reject the reserved "config/"
// subtree — resolve() layers that check on top for the Store, and
// router.go's reserved-first-segment routing does its own case-insensitive
// check on top too.
func CleanRel(rel string) (string, error) {
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return "", fmt.Errorf("%w: path is required", ErrInvalidPath)
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("%w: absolute paths are not permitted", ErrInvalidPath)
	}
	clean := filepath.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("%w: path escapes the workspace", ErrInvalidPath)
	}
	return clean, nil
}

// firstSegment returns the first path segment of an already-CleanRel'd
// relative path (e.g. "reports/q1.csv" -> "reports"; "notes.txt" ->
// "notes.txt"; "." -> "", since "." denotes the root itself, not a segment).
func firstSegment(clean string) string {
	if clean == "." {
		return ""
	}
	if i := strings.IndexRune(clean, os.PathSeparator); i >= 0 {
		return clean[:i]
	}
	return clean
}

// resolve validates rel and returns its absolute path under the user dir. It
// rejects absolute paths, ".." escapes (both via CleanRel), and the reserved
// "config/" subtree.
func (s *Store) resolve(tenant, user, rel string) (string, error) {
	clean, err := CleanRel(rel)
	if err != nil {
		return "", err
	}
	if top := firstSegment(clean); reservedTop[top] {
		return "", fmt.Errorf("%w: %q is reserved", ErrInvalidPath, top)
	}
	return filepath.Join(s.userDir(tenant, user), clean), nil
}

// List walks the user dir and returns its files and directories (relative
// paths + sizes + mtimes), skipping the user-root entry itself and the
// reserved subtrees. A missing dir → empty.
func (s *Store) List(ctx context.Context, tenant, user string) ([]FileInfo, error) {
	return s.ListDir(ctx, tenant, user, "", true)
}

// ListDir lists the contents of dir within the user's workspace. dir=""
// (or ".") means the workspace root. When recursive is false, only dir's
// immediate children are returned (files + subdirectory entries); when
// true, the full subtree beneath dir is walked — same as List. dir is
// validated via resolve() (traversal/absolute rejection, "config/"
// exclusion, both as the scope target itself and as a descendant); a
// non-existent dir returns an empty slice with a nil error rather than an
// error, so callers can treat "not there yet" and "empty" identically.
func (s *Store) ListDir(ctx context.Context, tenant, user, dir string, recursive bool) ([]FileInfo, error) {
	base := s.userDir(tenant, user)
	walkRoot := base
	if dir != "" && dir != "." {
		clean, err := s.resolve(tenant, user, dir)
		if err != nil {
			return nil, err
		}
		walkRoot = clean
	}
	if info, err := os.Stat(walkRoot); err != nil || !info.IsDir() {
		if os.IsNotExist(err) {
			return nil, nil
		}
		if err == nil && !info.IsDir() {
			return nil, nil
		}
		return nil, fmt.Errorf("workspacefs: list: %w", err)
	}

	var out []FileInfo
	err := filepath.WalkDir(walkRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		rel, rerr := filepath.Rel(base, path)
		if rerr != nil {
			return nil
		}
		relToRoot, rerr2 := filepath.Rel(walkRoot, path)
		if rerr2 != nil {
			return nil
		}
		if relToRoot == "." {
			// The scope directory itself (walkRoot, which is base when
			// dir=="" or "."): never emitted as an entry of its own listing.
			return nil
		}
		top := rel
		if i := strings.IndexRune(rel, os.PathSeparator); i >= 0 {
			top = rel[:i]
		}
		rootDepth := strings.Count(relToRoot, string(os.PathSeparator)) + 1
		if d.IsDir() {
			if reservedTop[top] {
				return filepath.SkipDir
			}
			if !recursive && rootDepth > 1 {
				return filepath.SkipDir
			}
			var mod time.Time
			if info, ierr := d.Info(); ierr == nil {
				mod = info.ModTime()
			}
			out = append(out, FileInfo{Path: filepath.ToSlash(rel), Size: 0, ModTime: mod, IsDir: true})
			return nil
		}
		if reservedTop[top] {
			return nil
		}
		if strings.HasSuffix(rel, sigSuffix) {
			// CP4.2 integrity sidecar: internal bookkeeping, never a user-facing
			// file — excluded from every listing mode exactly like config/.
			return nil
		}
		if !recursive && rootDepth > 1 {
			return nil
		}
		info, ierr := d.Info()
		var size int64
		var mod time.Time
		if ierr == nil {
			size = info.Size()
			mod = info.ModTime()
		}
		out = append(out, FileInfo{Path: filepath.ToSlash(rel), Size: size, ModTime: mod})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("workspacefs: list: %w", err)
	}
	return out, nil
}

// readAtClean is the raw read given an already-resolved absolute path — no
// resolve()/resolveSession() call, so it never recurses when Read's
// integrity check reads a "<clean>.sig" sidecar.
func (s *Store) readAtClean(clean string) ([]byte, time.Time, error) {
	info, err := os.Stat(clean)
	if err != nil {
		return nil, time.Time{}, err
	}
	if info.IsDir() {
		return nil, time.Time{}, fmt.Errorf("%w: %q is a directory", ErrInvalidPath, clean)
	}
	if info.Size() > MaxFileBytes {
		return nil, time.Time{}, fmt.Errorf("%w: read exceeds %d bytes", ErrTooLarge, int64(MaxFileBytes))
	}
	data, err := os.ReadFile(clean)
	if err != nil {
		return nil, time.Time{}, err
	}
	return data, info.ModTime(), nil
}

// readAndVerify is the single implementation behind both Read and
// ReadVerified. Session-ness (and therefore whether a sidecar is even
// consulted) is decided from the RESOLVED path via resolveSession — see its
// doc comment for why. CP4.2 investigation note: this is the sole
// server-side read that can seed downstream execution — the scheduler never
// resumes a run from a stored session file (each scheduled run builds a
// fresh prompt; see agent-gateway/src/scheduler/ticker.ts), so the only path
// by which a session record's bytes reach an LLM call is the webui
// downloading it here (ReadWorkspaceFile) and replaying it as /agui chat
// history into the gateway child (F62's PromptMessage.history).
//
//   - resolved path is not under SessionsDirPrefix: legacy=false, no sidecar
//     lookup at all — ordinary workspace files are never HMAC-checked.
//   - sidecar absent: legacy=true, err=nil (migration posture — CP4.2 spec;
//     flipping absent-sidecar to a hard failure is a documented one-line
//     follow-up once every session file has been re-signed).
//   - sidecar present and matches: legacy=false, err=nil.
//   - sidecar present and mismatched: err=ErrIntegrityMismatch (fail loud).
func (s *Store) readAndVerify(tenant, user, rel string) (data []byte, modTime time.Time, legacy bool, err error) {
	clean, isSession, err := s.resolveSession(tenant, user, rel)
	if err != nil {
		return nil, time.Time{}, false, err
	}
	data, modTime, err = s.readAtClean(clean)
	if err != nil {
		return nil, time.Time{}, false, err
	}
	if !isSession || strings.HasSuffix(clean, sigSuffix) {
		return data, modTime, false, nil
	}
	sig, _, sigErr := s.readAtClean(clean + sigSuffix)
	if sigErr != nil {
		// No sidecar (or unreadable for the same reasons the primary read
		// would be, e.g. permission) — treat as legacy/unsigned and allow.
		return data, modTime, true, nil
	}
	if !s.signingEnabled() {
		// A sidecar exists but we have no key to verify it with (e.g. Vault
		// and the env fallback are both unavailable at startup). Can't prove
		// integrity either way — degrade to allow rather than lock users out.
		return data, modTime, true, nil
	}
	want := s.sign(data)
	if !hmac.Equal([]byte(want), sig) {
		return nil, time.Time{}, false, ErrIntegrityMismatch
	}
	return data, modTime, false, nil
}

// Read returns a file's bytes (and mtime). Rejects directories and files over
// the cap. For a path resolving under SessionsDirPrefix, this also verifies
// the HMAC sidecar when one exists — see readAndVerify — and returns
// ErrIntegrityMismatch on a mismatch; it never silently returns tampered
// content.
func (s *Store) Read(ctx context.Context, tenant, user, rel string) ([]byte, time.Time, error) {
	data, modTime, _, err := s.readAndVerify(tenant, user, rel)
	return data, modTime, err
}

// ReadVerified is Read plus the legacy flag (true when the resolved path is a
// session record with no sidecar to check against) — callers that want to log
// the migration-era "read without a sidecar" case use this instead of Read.
func (s *Store) ReadVerified(ctx context.Context, tenant, user, rel string) (data []byte, modTime time.Time, legacy bool, err error) {
	return s.readAndVerify(tenant, user, rel)
}

// writeAtClean is the raw atomic write (temp + rename) given an
// already-resolved absolute path and the tenant/user needed to compute the
// returned FileInfo's workspace-relative Path. No resolve()/resolveSession()
// call, so it never recurses when Write's signing step writes a
// "<clean>.sig" sidecar.
func (s *Store) writeAtClean(tenant, user, clean string, data []byte) (FileInfo, error) {
	if err := os.MkdirAll(filepath.Dir(clean), 0o750); err != nil {
		return FileInfo{}, fmt.Errorf("workspacefs: mkdir: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(clean), ".upload-*")
	if err != nil {
		return FileInfo{}, err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return FileInfo{}, err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return FileInfo{}, err
	}
	if err := os.Chmod(tmpName, 0o640); err != nil {
		os.Remove(tmpName)
		return FileInfo{}, err
	}
	if err := os.Rename(tmpName, clean); err != nil {
		os.Remove(tmpName)
		return FileInfo{}, err
	}
	base := s.userDir(tenant, user)
	relSlash, _ := filepath.Rel(base, clean)
	return FileInfo{Path: filepath.ToSlash(relSlash), Size: int64(len(data)), ModTime: time.Now().UTC()}, nil
}

// Write creates or overwrites a file atomically (temp + rename), enforcing
// the size cap and creating parent dirs. When the RESOLVED path falls under
// SessionsDirPrefix and a signing key is configured, it also (re)writes the
// "<rel>.sig" HMAC-SHA256 sidecar from the same content — every writer that
// goes through Write (files.go's UploadWorkspaceFile, scheduler.go's
// ReportScheduledRunResult, and any future one) gets this automatically; see
// the SessionsDirPrefix doc comment for why session-ness is decided from the
// resolved path rather than the caller's raw rel string. General workspace
// writes (resolved path outside SessionsDirPrefix) are never signed.
func (s *Store) Write(ctx context.Context, tenant, user, rel string, data []byte) (FileInfo, error) {
	if int64(len(data)) > MaxFileBytes {
		return FileInfo{}, fmt.Errorf("%w: %d bytes", ErrTooLarge, int64(MaxFileBytes))
	}
	clean, isSession, err := s.resolveSession(tenant, user, rel)
	if err != nil {
		return FileInfo{}, err
	}
	info, err := s.writeAtClean(tenant, user, clean, data)
	if err != nil {
		return FileInfo{}, err
	}
	if strings.HasSuffix(clean, sigSuffix) {
		return info, nil // writing a sidecar itself never recurses into signing
	}
	if isSession && s.signingEnabled() {
		if _, err := s.writeAtClean(tenant, user, clean+sigSuffix, []byte(s.sign(data))); err != nil {
			return FileInfo{}, fmt.Errorf("workspacefs: session signature write failed: %w", err)
		}
	}
	return info, nil
}

// Delete removes a file, or an empty directory. A non-empty directory is
// rejected with ErrNotEmpty (no recursive delete — see spec out-of-scope).
func (s *Store) Delete(ctx context.Context, tenant, user, rel string) error {
	defer s.lockUser(tenant, user)()
	clean, err := s.resolve(tenant, user, rel)
	if err != nil {
		return err
	}
	info, err := os.Stat(clean)
	if err != nil {
		return err
	}
	if info.IsDir() {
		entries, rerr := os.ReadDir(clean)
		if rerr != nil {
			return rerr
		}
		if len(entries) > 0 {
			return fmt.Errorf("%w: %q is not empty", ErrNotEmpty, rel)
		}
		return os.Remove(clean)
	}
	return os.Remove(clean)
}

// Move renames/relocates a file or directory within the user's workspace.
// Both endpoints are resolved through the same path-traversal guard as every
// other mutation; the destination must not already exist (no clobber).
//
// CP4.2 (review fix #2): Move is a writer too — moving/renaming an ordinary
// file onto a path that resolves under SessionsDirPrefix must not silently
// produce an unsigned "session record" that later reads back as legacy. After
// a successful rename, any sidecar at the OLD location is dropped (never left
// stale behind), and a sidecar for the moved content is (re)computed at the
// NEW location whenever the destination resolves under SessionsDirPrefix and
// signing is enabled; otherwise any sidecar at the destination is removed so
// one can't linger for a path that is no longer (or never was) a session
// record. Directories are never signed (no single content blob to hash).
func (s *Store) Move(ctx context.Context, tenant, user, from, to string) (FileInfo, error) {
	defer s.lockUser(tenant, user)()
	fromClean, err := s.resolve(tenant, user, from)
	if err != nil {
		return FileInfo{}, err
	}
	toClean, toIsSession, err := s.resolveSession(tenant, user, to)
	if err != nil {
		return FileInfo{}, err
	}
	fromInfo, err := os.Stat(fromClean)
	if err != nil {
		return FileInfo{}, err
	}
	if _, err := os.Stat(toClean); err == nil {
		return FileInfo{}, fmt.Errorf("%w: %q", ErrExists, to)
	}
	if err := os.MkdirAll(filepath.Dir(toClean), 0o750); err != nil {
		return FileInfo{}, fmt.Errorf("workspacefs: mkdir: %w", err)
	}
	if err := os.Rename(fromClean, toClean); err != nil {
		return FileInfo{}, err
	}
	info, err := os.Stat(toClean)
	if err != nil {
		return FileInfo{}, err
	}

	if !fromInfo.IsDir() {
		_ = os.Remove(fromClean + sigSuffix) // no-op if it never existed; never leave a stale sidecar at the old path
		toSig := toClean + sigSuffix
		if toIsSession && s.signingEnabled() {
			data, rerr := os.ReadFile(toClean)
			if rerr != nil {
				return FileInfo{}, fmt.Errorf("workspacefs: read moved session file for signing: %w", rerr)
			}
			if werr := os.WriteFile(toSig, []byte(s.sign(data)), 0o640); werr != nil {
				return FileInfo{}, fmt.Errorf("workspacefs: session signature write failed: %w", werr)
			}
		} else {
			_ = os.Remove(toSig) // destination isn't a session path (or signing is disabled) — no sidecar belongs there
		}
	}

	base := s.userDir(tenant, user)
	relSlash, _ := filepath.Rel(base, toClean)
	return FileInfo{
		Path:    filepath.ToSlash(relSlash),
		Size:    info.Size(),
		ModTime: info.ModTime(),
		IsDir:   info.IsDir(),
	}, nil
}

// Mkdir creates a new (possibly nested) directory. Refuses to clobber an
// existing path.
func (s *Store) Mkdir(ctx context.Context, tenant, user, rel string) error {
	defer s.lockUser(tenant, user)()
	clean, err := s.resolve(tenant, user, rel)
	if err != nil {
		return err
	}
	if _, err := os.Stat(clean); err == nil {
		return fmt.Errorf("%w: %q", ErrExists, rel)
	}
	return os.MkdirAll(clean, 0o750)
}
