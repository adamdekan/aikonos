// broker/internal/policy/bundle.go
// Policy bundles: the OPA policy the broker serves, as a content-addressed unit.
//
// The broker reads the Rego policy directory, names the result by a digest of
// its files (the revision), archives a snapshot of exactly those files in the
// audit store, and only then serves it to OPA as a bundle. OPA reports the
// revision with every decision (provenance), so each recorded decision names
// the policy that made it, and that policy can be fetched from the archive and
// re-run. See docs/15-decision-replay.md.
package policy

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// BundleSnapshotKind identifies the archived snapshot format.
const BundleSnapshotKind = "aikonos.policy-bundle/v1"

// BundleFile is one policy file in a bundle, addressed by its path relative to
// the policy directory (always forward slashes).
type BundleFile struct {
	Path    string `json:"path"`
	SHA256  string `json:"sha256"`
	Content string `json:"content"`
}

// Bundle is an immutable, content-addressed policy set.
type Bundle struct {
	Revision string       // "sha256:<hex>", see bundleRevision
	Roots    []string     // OPA bundle roots: the packages the bundle owns
	Files    []BundleFile // sorted by Path
	tarball  []byte
}

// BundleSnapshot is the archived form of a bundle: every file's full content,
// so a decision can be re-run from the archive alone.
type BundleSnapshot struct {
	Kind     string       `json:"kind"`
	Revision string       `json:"revision"`
	Roots    []string     `json:"roots"`
	Files    []BundleFile `json:"files"`
}

// LoadBundle reads the policy files under dir: every .rego file except
// *_test.rego, and OPA data files (data.json, data.yaml, data.yml). Hidden
// files and directories are skipped, as OPA's --ignore=.* did. Tests and
// fixtures therefore stay out of the policy that decides.
func LoadBundle(dir string, roots []string) (*Bundle, error) {
	if len(roots) == 0 {
		return nil, fmt.Errorf("policy bundle: at least one root is required")
	}
	var files []BundleFile
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if path != dir && strings.HasPrefix(name, ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() || !bundleFileIncluded(name) {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(content)
		files = append(files, BundleFile{
			Path:    filepath.ToSlash(rel),
			SHA256:  hex.EncodeToString(sum[:]),
			Content: string(content),
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("policy bundle: read %s: %w", dir, err)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("policy bundle: no policy files under %s", dir)
	}
	return newBundle(roots, files)
}

func bundleFileIncluded(name string) bool {
	switch {
	case strings.HasSuffix(name, "_test.rego"):
		return false
	case strings.HasSuffix(name, ".rego"):
		return true
	default:
		return name == "data.json" || name == "data.yaml" || name == "data.yml"
	}
}

func newBundle(roots []string, files []BundleFile) (*Bundle, error) {
	roots = append([]string(nil), roots...)
	sort.Strings(roots)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	rev, err := bundleRevision(roots, files)
	if err != nil {
		return nil, err
	}
	b := &Bundle{Revision: rev, Roots: roots, Files: files}
	if b.tarball, err = b.buildTarball(); err != nil {
		return nil, err
	}
	return b, nil
}

// bundleRevision is the SHA-256 of the canonical JSON
//
//	{"files":[{"path":"<path>","sha256":"<hex>"},...],"roots":["<root>",...]}
//
// with files sorted by path, roots sorted, no whitespace and no HTML escaping:
// exactly what `jq -cS '{files: [.files[] | {path, sha256}], roots}'` prints for
// a snapshot, so anyone can recompute it (scripts/replay-decision.sh does).
func bundleRevision(roots []string, files []BundleFile) (string, error) {
	type entry struct {
		Path   string `json:"path"`
		SHA256 string `json:"sha256"`
	}
	canon := struct {
		Files []entry  `json:"files"`
		Roots []string `json:"roots"`
	}{Files: make([]entry, 0, len(files)), Roots: roots}
	for _, f := range files {
		canon.Files = append(canon.Files, entry{Path: f.Path, SHA256: f.SHA256})
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(canon); err != nil {
		return "", fmt.Errorf("policy bundle: canonical form: %w", err)
	}
	sum := sha256.Sum256(bytes.TrimSuffix(buf.Bytes(), []byte("\n")))
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// Snapshot returns the archived JSON form of the bundle.
func (b *Bundle) Snapshot() ([]byte, error) {
	return json.Marshal(BundleSnapshot{Kind: BundleSnapshotKind, Revision: b.Revision, Roots: b.Roots, Files: b.Files})
}

// Tarball returns the gzip'd tar OPA's bundle plugin downloads: a .manifest
// carrying the revision and roots, plus each policy file at its relative path.
func (b *Bundle) Tarball() []byte { return b.tarball }

// ETag is the HTTP entity tag for the bundle download.
func (b *Bundle) ETag() string { return `"` + strings.Replace(b.Revision, ":", "-", 1) + `"` }

func (b *Bundle) buildTarball() ([]byte, error) {
	manifest, err := json.Marshal(map[string]any{"revision": b.Revision, "roots": b.Roots})
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	tw := tar.NewWriter(gz)
	// Fixed modification time keeps the tarball a pure function of the files.
	mtime := time.Unix(0, 0).UTC()
	add := func(name string, data []byte) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), ModTime: mtime, Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}
	if err := add("/.manifest", manifest); err != nil {
		return nil, fmt.Errorf("policy bundle: tar manifest: %w", err)
	}
	for _, f := range b.Files {
		if err := add("/"+f.Path, []byte(f.Content)); err != nil {
			return nil, fmt.Errorf("policy bundle: tar %s: %w", f.Path, err)
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// VerifySnapshot checks that an archived snapshot is internally consistent:
// each file's content matches its sha256, and the file list hashes to the
// recorded revision. It returns the recomputed revision.
func VerifySnapshot(data []byte) (string, error) {
	var snap BundleSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return "", fmt.Errorf("policy snapshot: decode: %w", err)
	}
	if snap.Kind != BundleSnapshotKind {
		return "", fmt.Errorf("policy snapshot: unexpected kind %q", snap.Kind)
	}
	for _, f := range snap.Files {
		sum := sha256.Sum256([]byte(f.Content))
		if hex.EncodeToString(sum[:]) != f.SHA256 {
			return "", fmt.Errorf("policy snapshot: %s does not match its sha256", f.Path)
		}
	}
	files := append([]BundleFile(nil), snap.Files...)
	roots := append([]string(nil), snap.Roots...)
	sort.Strings(roots)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	rev, err := bundleRevision(roots, files)
	if err != nil {
		return "", err
	}
	if rev != snap.Revision {
		return rev, fmt.Errorf("policy snapshot: files hash to %s, snapshot claims %s", rev, snap.Revision)
	}
	return rev, nil
}
