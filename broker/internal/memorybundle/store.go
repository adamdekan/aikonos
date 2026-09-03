// broker/internal/memorybundle/store.go
//
// Bundle I/O over workspacefs.Backend: concept listing/loading, index
// regeneration, log appending, and the per-bundle lock they run under. The
// format half of this package (memorybundle.go) stays pure; this file is where
// the bundle's on-disk shape is known.
//
// It lives here rather than in toolproxy because two surfaces mutate a bundle:
// the memory.write tool and the Phase 5 management RPCs in package broker
//. A second lock map in a
// second package would not serialize against the first.
package memorybundle

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/adamdekan/aikonos/broker/internal/workspacefs"
)

// Dir is the bundle root, relative to the scope's workspace segment. Under
// .agent/ it is pinned local by the workspace Router and hidden from the file
// explorer.
const Dir = ".agent/Memory"

// ConceptPath is one concept's path relative to its scope's workspace segment.
func ConceptPath(id string) string { return Dir + "/" + id + ".md" }

// bundleLocks holds one mutex per (tenant, segment) bundle, serializing the
// read-modify-write of index.md/log.md. Process-local locking is sound because
// deployment enforces exactly one broker process (broker/cmd/broker/singleton.go's
// advisory lock) — the same reasoning workspacefs.Store.lockUser relies on.
var (
	bundleLocksMu sync.Mutex
	bundleLocks   = map[string]*sync.Mutex{}
)

// LockBundle acquires the (tenant, seg) bundle's mutex and returns its unlock.
// Every writer of a bundle — the memory.write tool and the management RPCs —
// must hold it for the whole concept+index+log sequence.
func LockBundle(tenant, seg string) func() {
	key := tenant + "/" + seg
	bundleLocksMu.Lock()
	l, ok := bundleLocks[key]
	if !ok {
		l = &sync.Mutex{}
		bundleLocks[key] = l
	}
	bundleLocksMu.Unlock()
	l.Lock()
	return l.Unlock
}

// ListConceptIDs lists the bundle's concept ids (sorted, reserved
// index.md/log.md excluded). A bundle that doesn't exist yet lists empty.
func ListConceptIDs(ctx context.Context, be workspacefs.Backend, tenant, seg string) ([]string, error) {
	infos, err := be.ListDir(ctx, tenant, seg, Dir, true)
	if err != nil {
		return nil, fmt.Errorf("memorybundle: list bundle: %w", err)
	}
	out := make([]string, 0, len(infos))
	for _, fi := range infos {
		if fi.IsDir || !strings.HasSuffix(fi.Path, ".md") {
			continue
		}
		rel := strings.TrimPrefix(fi.Path, Dir+"/")
		if rel == IndexFile || rel == LogFile {
			continue
		}
		out = append(out, strings.TrimSuffix(rel, ".md"))
	}
	sort.Strings(out)
	return out, nil
}

// ReadIndex returns index.md's content, empty for an absent bundle.
func ReadIndex(ctx context.Context, be workspacefs.Backend, tenant, seg string) (string, error) {
	data, _, err := be.Read(ctx, tenant, seg, Dir+"/"+IndexFile)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("memorybundle: read index: %w", err)
	}
	return string(data), nil
}

// LoadConcept validates id, reads its file and parses it.
//
// Its errors are short, path-free markers because they are surfaced per-id to
// the model: workspacefs wraps absolute server paths, which are not the model's
// business.
func LoadConcept(ctx context.Context, be workspacefs.Backend, tenant, seg, id string) (Concept, error) {
	if err := ValidateID(id); err != nil {
		return Concept{}, errors.New("invalid concept id")
	}
	data, _, err := be.Read(ctx, tenant, seg, ConceptPath(id))
	if err != nil {
		return Concept{}, errors.New("not found")
	}
	c, err := ParseConcept(data)
	if err != nil {
		return Concept{}, errors.New("unparseable concept file")
	}
	return c, nil
}

// RegenerateIndex rebuilds index.md from the bundle's concept frontmatter.
// Callers hold LockBundle, so the index is always derived state — never
// patched. Returns how many concept files it had to drop because they no longer
// parse.
//
// ponytail: re-reads every concept per write (≤512 small local files, one
// writer per bundle at a time). A cached per-bundle entry list is the upgrade
// path if write latency ever shows up.
func RegenerateIndex(ctx context.Context, be workspacefs.Backend, tenant, seg string) (int64, error) {
	ids, err := ListConceptIDs(ctx, be, tenant, seg)
	if err != nil {
		return 0, err
	}
	var skipped int64
	entries := make([]IndexEntry, 0, len(ids))
	for _, id := range ids {
		c, err := LoadConcept(ctx, be, tenant, seg, id)
		if err != nil {
			// One unreadable file (a hand-edited bundle) must not brick every
			// later write; it is left out of the index rather than fatal, and
			// counted so the caller can see it happened.
			skipped++
			continue
		}
		entries = append(entries, IndexEntry{
			ID:          id,
			Type:        frontmatterString(c.Frontmatter, "type"),
			Title:       frontmatterString(c.Frontmatter, "title"),
			Description: frontmatterString(c.Frontmatter, "description"),
			Status:      frontmatterString(c.Frontmatter, "status"),
		})
	}
	if _, err := be.Write(ctx, tenant, seg, Dir+"/"+IndexFile, RenderIndex(entries)); err != nil {
		return 0, fmt.Errorf("memorybundle: write index: %w", err)
	}
	return skipped, nil
}

// AppendLogEntry prepends a dated entry to the bundle's log.md. Callers hold
// LockBundle — the read-modify-write is what needs serializing.
func AppendLogEntry(ctx context.Context, be workspacefs.Backend, tenant, seg, entry string) error {
	rel := Dir + "/" + LogFile
	existing, _, err := be.Read(ctx, tenant, seg, rel)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("memorybundle: read log: %w", err)
	}
	if _, err := be.Write(ctx, tenant, seg, rel, AppendLog(existing, entry, time.Now())); err != nil {
		return fmt.Errorf("memorybundle: write log: %w", err)
	}
	return nil
}

func frontmatterString(fm map[string]any, key string) string {
	s, _ := fm[key].(string)
	return s
}
