// broker/internal/personalskill/store.go
//
// I/O over workspacefs.Backend: shallow discovery, transfer snapshot/install,
// and bounded delete. The format half (personalskill.go) stays pure; this
// file is where Skills/<name>/'s on-disk shape is known.
package personalskill

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"sort"
	"strings"

	"github.com/adamdekan/aikonos/broker/internal/workspacefs"
)

// ErrExists is returned by Install when the target skill already exists and
// replace was not requested.
var ErrExists = errors.New("personalskill: skill already exists")

// Scan performs the shallow discovery of Skills/*/SKILL.md for one user's
// workspace: only the immediate subdirectories of Skills/ are considered,
// each contributing at most one entry from its own SKILL.md. Never fails on
// one bad skill — see buildEntry. Returns at most MaxSkills entries, sorted
// by directory name for a stable list.
func Scan(ctx context.Context, be workspacefs.Backend, tenant, user string) ([]SkillEntry, error) {
	infos, err := be.ListDir(ctx, tenant, user, Dir, false)
	if err != nil {
		return nil, fmt.Errorf("personalskill: list %s: %w", Dir, err)
	}
	var names []string
	for _, fi := range infos {
		if !fi.IsDir {
			continue
		}
		name := strings.TrimPrefix(fi.Path, Dir+"/")
		if name == "" || strings.Contains(name, "/") {
			continue // defensive: a shallow ListDir should never emit a nested path
		}
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]SkillEntry, 0, len(names))
	for _, name := range names {
		if len(out) >= MaxSkills {
			break
		}
		out = append(out, scanOne(ctx, be, tenant, user, name))
	}
	return out, nil
}

// scanOne reads and validates one skill's SKILL.md, never returning an error
// — a missing/oversize/unparseable file becomes an invalid entry.
func scanOne(ctx context.Context, be workspacefs.Backend, tenant, user, name string) SkillEntry {
	data, _, err := be.Read(ctx, tenant, user, SkillMdPath(name))
	if err != nil {
		return SkillEntry{Name: name, Warning: "missing SKILL.md"}
	}
	if len(data) > MaxSkillMdBytes {
		return SkillEntry{Name: name, Warning: fmt.Sprintf("SKILL.md is %d bytes, exceeds the %d byte cap", len(data), MaxSkillMdBytes)}
	}
	fm, err := ParseFrontmatter(data)
	return buildEntry(name, fm, err)
}

// ListFiles returns the sorted relative paths of every file under
// Skills/<name>/ except SKILL.md itself — the runtime file-tree manifest
// surfaced at activation. A missing skill
// directory yields an empty slice, not an error, mirroring Scan's ListDir
// semantics.
func ListFiles(ctx context.Context, be workspacefs.Backend, tenant, user, name string) ([]string, error) {
	if !ValidSkillName(name) {
		return nil, fmt.Errorf("personalskill: invalid skill name %q", name)
	}
	dir := SkillDir(name)
	infos, err := be.ListDir(ctx, tenant, user, dir, true)
	if err != nil {
		return nil, fmt.Errorf("personalskill: list %s: %w", dir, err)
	}
	prefix := dir + "/"
	out := make([]string, 0, len(infos))
	for _, fi := range infos {
		if fi.IsDir {
			continue
		}
		rel := strings.TrimPrefix(fi.Path, prefix)
		if rel == SkillMdName {
			continue
		}
		out = append(out, rel)
	}
	sort.Strings(out)
	return out, nil
}

// ManifestEntry is one file in a SnapshotResult's manifest.
type ManifestEntry struct {
	Path string
	Size int64
}

// SnapshotResult is Snapshot's output: the copied folder's content hash,
// per-file manifest, and any injection-scan flags.
type SnapshotResult struct {
	// ContentHash is sha256 (hex) over the sorted (path, bytes) pairs of every
	// copied file — deterministic regardless of filesystem walk order.
	ContentHash string
	Manifest    []ManifestEntry
	// Flags is the deduplicated, first-seen-order union of scan(text) over
	// SKILL.md and every .md/.txt file's content.
	Flags []string
}

// isScannable reports whether rel's content should be passed to Snapshot's
// scan hook: SKILL.md itself and any other markdown/text file. SKILL.md
// already ends in ".md", so no separate check is needed for it.
func isScannable(rel string) bool {
	return strings.HasSuffix(rel, ".md") || strings.HasSuffix(rel, ".txt")
}

// Snapshot copies fromUser's Skills/<name>/ subtree into stagingSeg,
// preserving the folder's relative layout (SKILL.md, references/foo.txt, …
// land at the same relative paths under stagingSeg — not re-nested under
// another "Skills/<name>/" level). stagingSeg is used exactly like a
// workspacefs "user" segment; the caller is responsible for handing in an
// already-clean value (e.g. "xfer-<uuid>").
//
// Enforces MaxFolderFiles/MaxFolderBytes before writing anything, so a cap
// violation never leaves a partial copy in staging. scan is called once per
// scannable file's text content; callers inject toolproxy.ScanForInjection so
// this package never imports toolproxy. A nil scan is accepted (no flags).
func Snapshot(ctx context.Context, be workspacefs.Backend, tenant, fromUser, name, stagingSeg string, scan func(string) []string) (SnapshotResult, error) {
	if !ValidSkillName(name) {
		return SnapshotResult{}, fmt.Errorf("personalskill: invalid skill name %q", name)
	}
	srcDir := SkillDir(name)
	infos, err := be.ListDir(ctx, tenant, fromUser, srcDir, true)
	if err != nil {
		return SnapshotResult{}, fmt.Errorf("personalskill: list %s: %w", srcDir, err)
	}

	prefix := srcDir + "/"
	var fileInfos []workspacefs.FileInfo
	var preSizeTotal int64
	for _, fi := range infos {
		if fi.IsDir {
			continue
		}
		fileInfos = append(fileInfos, fi)
		preSizeTotal += fi.Size
	}
	if len(fileInfos) == 0 {
		return SnapshotResult{}, fmt.Errorf("personalskill: skill %q not found or empty", name)
	}
	// Both caps are checked before any Read, off ListDir's own metadata, so an
	// adversarial folder is rejected without ever buffering its content. The
	// byte cap is re-checked against actual bytes read in the loop below —
	// ListDir's reported Size is metadata from the backend, not proof.
	if len(fileInfos) > MaxFolderFiles {
		return SnapshotResult{}, fmt.Errorf("personalskill: skill %q has %d files, exceeds the %d file cap", name, len(fileInfos), MaxFolderFiles)
	}
	if preSizeTotal > MaxFolderBytes {
		return SnapshotResult{}, fmt.Errorf("personalskill: skill %q is %d bytes, exceeds the %d byte cap", name, preSizeTotal, MaxFolderBytes)
	}

	type file struct {
		rel  string
		data []byte
	}
	var files []file
	var totalBytes int64
	for _, fi := range fileInfos {
		data, _, rerr := be.Read(ctx, tenant, fromUser, fi.Path)
		if rerr != nil {
			return SnapshotResult{}, fmt.Errorf("personalskill: read %s: %w", fi.Path, rerr)
		}
		totalBytes += int64(len(data))
		if totalBytes > MaxFolderBytes {
			return SnapshotResult{}, fmt.Errorf("personalskill: skill %q is %d bytes, exceeds the %d byte cap", name, totalBytes, MaxFolderBytes)
		}
		files = append(files, file{rel: strings.TrimPrefix(fi.Path, prefix), data: data})
	}

	sort.Slice(files, func(i, j int) bool { return files[i].rel < files[j].rel })

	h := sha256.New()
	seenFlag := map[string]bool{}
	var flags []string
	manifest := make([]ManifestEntry, 0, len(files))
	for _, f := range files {
		io.WriteString(h, f.rel)
		h.Write([]byte{0})
		h.Write(f.data)
		h.Write([]byte{0})
		manifest = append(manifest, ManifestEntry{Path: f.rel, Size: int64(len(f.data))})
		if scan != nil && isScannable(f.rel) {
			for _, flag := range scan(string(f.data)) {
				if !seenFlag[flag] {
					seenFlag[flag] = true
					flags = append(flags, flag)
				}
			}
		}
	}

	for _, f := range files {
		if _, werr := be.Write(ctx, tenant, stagingSeg, f.rel, f.data); werr != nil {
			return SnapshotResult{}, fmt.Errorf("personalskill: write staging %s: %w", f.rel, werr)
		}
	}

	return SnapshotResult{
		ContentHash: hex.EncodeToString(h.Sum(nil)),
		Manifest:    manifest,
		Flags:       flags,
	}, nil
}

// Install copies stagingSeg's contents into toUser's Skills/<targetName>/.
// When replace is true, an existing target subtree is deleted first; when
// false, an existing target is rejected with ErrExists and nothing is
// written.
func Install(ctx context.Context, be workspacefs.Backend, tenant, stagingSeg, toUser, targetName string, replace bool) error {
	if !ValidSkillName(targetName) {
		return fmt.Errorf("personalskill: invalid skill name %q", targetName)
	}
	targetDir := SkillDir(targetName)

	_, _, err := be.Read(ctx, tenant, toUser, targetDir+"/"+SkillMdName)
	switch {
	case err == nil:
		if !replace {
			return ErrExists
		}
		if derr := deleteSubtree(ctx, be, tenant, toUser, targetDir); derr != nil {
			return derr
		}
	case errors.Is(err, fs.ErrNotExist):
		// nothing to replace — proceed to install
	default:
		return fmt.Errorf("personalskill: check existing %s: %w", targetDir, err)
	}

	infos, err := be.ListDir(ctx, tenant, stagingSeg, "", true)
	if err != nil {
		return fmt.Errorf("personalskill: list staging: %w", err)
	}
	for _, fi := range infos {
		if fi.IsDir {
			continue
		}
		data, _, rerr := be.Read(ctx, tenant, stagingSeg, fi.Path)
		if rerr != nil {
			return fmt.Errorf("personalskill: read staging %s: %w", fi.Path, rerr)
		}
		dest := targetDir + "/" + fi.Path
		if _, werr := be.Write(ctx, tenant, toUser, dest, data); werr != nil {
			return fmt.Errorf("personalskill: write %s: %w", dest, werr)
		}
	}
	return nil
}

// DeleteSkill recursively deletes Skills/<name>/ for one user, bounded to
// that subtree. Rejects a name that fails ValidSkillName before touching the
// filesystem.
func DeleteSkill(ctx context.Context, be workspacefs.Backend, tenant, user, name string) error {
	if !ValidSkillName(name) {
		return fmt.Errorf("personalskill: invalid skill name %q", name)
	}
	return deleteSubtree(ctx, be, tenant, user, SkillDir(name))
}

// deleteSubtree recursively deletes every file under dir for (tenant, user),
// then its now-empty subdirectories deepest-first, then dir itself. A dir
// that does not exist is a no-op, not an error — mirrors workspacefs.ListDir's
// "not there yet" semantics. Bounded to dir's own subtree: every path deleted
// comes from dir's own ListDir walk, so a caller-validated dir (SkillDir(name)
// with a ValidSkillName-checked name) can never reach outside Skills/<name>/.
func deleteSubtree(ctx context.Context, be workspacefs.Backend, tenant, user, dir string) error {
	infos, err := be.ListDir(ctx, tenant, user, dir, true)
	if err != nil {
		return fmt.Errorf("personalskill: list %s: %w", dir, err)
	}
	var dirs []string
	for _, fi := range infos {
		if fi.IsDir {
			dirs = append(dirs, fi.Path)
			continue
		}
		if err := be.Delete(ctx, tenant, user, fi.Path); err != nil {
			return fmt.Errorf("personalskill: delete %s: %w", fi.Path, err)
		}
	}
	// Deepest (longest path) first so each directory is already empty by the
	// time workspacefs.Delete rejects a non-empty one.
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	for _, d := range dirs {
		if err := be.Delete(ctx, tenant, user, d); err != nil {
			return fmt.Errorf("personalskill: delete %s: %w", d, err)
		}
	}
	if err := be.Delete(ctx, tenant, user, dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("personalskill: delete %s: %w", dir, err)
	}
	return nil
}

// FreeName returns the first of base, base-2, base-3, … whose Skills/<name>/
// does not yet exist for (tenant, user) — probed via SKILL.md presence, the
// same existence check Install uses.
func FreeName(ctx context.Context, be workspacefs.Backend, tenant, user, base string) (string, error) {
	if !ValidSkillName(base) {
		return "", fmt.Errorf("personalskill: invalid skill name %q", base)
	}
	name := base
	for n := 2; ; n++ {
		_, _, err := be.Read(ctx, tenant, user, SkillMdPath(name))
		if errors.Is(err, fs.ErrNotExist) {
			return name, nil
		}
		if err != nil {
			return "", fmt.Errorf("personalskill: check %s: %w", name, err)
		}
		name = fmt.Sprintf("%s-%d", base, n)
	}
}
