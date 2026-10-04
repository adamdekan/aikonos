// broker/internal/policy/bundle_server.go
// Serves the current policy bundle to OPA's bundle plugin.
package policy

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"go.uber.org/zap"
)

// BundleArchive stores a policy snapshot durably (the audit store). Archiving
// the same revision twice must succeed.
type BundleArchive interface {
	ArchivePolicyBundle(ctx context.Context, revision string, snapshot []byte) error
}

// BundleServer re-reads the policy directory, archives every new revision, and
// serves the newest archived one over HTTP. A revision that cannot be archived
// is never served: OPA keeps deciding with the previous one, or, at startup,
// with no policy at all, so every decision fails closed until the archive is
// reachable.
type BundleServer struct {
	dir     string
	roots   []string
	archive BundleArchive // nil: serve without archiving (logged once at startup)
	logger  *zap.Logger

	// OnChange is called after a new revision has been archived and is being
	// served; prev is nil for the first one. Set before Run.
	OnChange func(prev, next *Bundle)

	mu      sync.RWMutex
	current *Bundle
}

// NewBundleServer returns a server for the policy directory dir. archive may
// be nil only where there is no audit store at all (logging-only mode).
func NewBundleServer(dir string, roots []string, archive BundleArchive, logger *zap.Logger) *BundleServer {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &BundleServer{dir: dir, roots: roots, archive: archive, logger: logger}
}

// Current returns the bundle being served, or nil before the first reload
// succeeded.
func (s *BundleServer) Current() *Bundle {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.current
}

// Reload reads the policy directory and, when its revision differs from the
// one being served, archives the new snapshot and switches to it. It reports
// whether the served revision changed.
func (s *BundleServer) Reload(ctx context.Context) (bool, error) {
	b, err := LoadBundle(s.dir, s.roots)
	if err != nil {
		return false, err
	}
	prev := s.Current()
	if prev != nil && prev.Revision == b.Revision {
		return false, nil
	}
	if s.archive != nil {
		snap, err := b.Snapshot()
		if err != nil {
			return false, err
		}
		if err := s.archive.ArchivePolicyBundle(ctx, b.Revision, snap); err != nil {
			return false, fmt.Errorf("archive policy %s: %w", b.Revision, err)
		}
	}
	s.mu.Lock()
	s.current = b
	s.mu.Unlock()
	s.logger.Info("policy bundle: serving revision",
		zap.String("revision", b.Revision), zap.Int("files", len(b.Files)), zap.Bool("archived", s.archive != nil))
	if s.OnChange != nil {
		s.OnChange(prev, b)
	}
	return true, nil
}

// Run reloads every interval until ctx is done. Errors are logged, not fatal:
// the previous revision keeps being served.
func (s *BundleServer) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := s.Reload(ctx); err != nil {
				s.logger.Error("policy bundle: reload failed; still serving the previous revision", zap.Error(err))
			}
		}
	}
}

// ServeHTTP answers OPA's bundle download. The ETag is the revision, so OPA's
// conditional polls get 304 until the policy changes.
func (s *BundleServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	b := s.Current()
	if b == nil {
		http.Error(w, "no policy bundle archived yet", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("ETag", b.ETag())
	w.Header().Set("Cache-Control", "no-cache")
	if r.Header.Get("If-None-Match") == b.ETag() {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Length", strconv.Itoa(len(b.Tarball())))
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(b.Tarball())
}
