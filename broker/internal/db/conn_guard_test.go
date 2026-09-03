package db

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// acquireRe matches a call to Acquire on a pool/connection-pool-shaped
// receiver — the exact convention every repo method used before withConn.
var acquireRe = regexp.MustCompile(`\.Acquire\(`)

// TestNoRawPoolAcquireOutsideSeam proves every repo query goes through the
// withConn/withConnErr/withConn2/withConnUnscoped seam instead of the old
// Acquire→withTenant→Release convention. conn.go (the seam's own
// implementation) and pool_test.go (low-level pool/GUC tests that must hold a
// raw connection to prove reset-on-release and GUC visibility) are the only
// sanctioned exceptions.
//
// To prove this guard is real, not vacuous: reverting any one migrated call
// site in this package back to a raw `conn, err := r.pool.Acquire(ctx)` turns
// this test red (verified by hand during CP3 — see 
// CP3 evidence).
func TestNoRawPoolAcquireOutsideSeam(t *testing.T) {
	const dir = "."
	allowed := map[string]bool{
		"conn.go":            true,
		"pool_test.go":       true,
		"conn_guard_test.go": true, // this file's own pattern literal contains ".Acquire("
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || allowed[name] {
			continue
		}

		path := filepath.Join(dir, name)
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}

		if loc := acquireRe.FindIndex(src); loc != nil {
			line := 1 + strings.Count(string(src[:loc[0]]), "\n")
			t.Errorf("%s:%d: raw .Acquire( call outside the withConn seam — route through withConn/withConnErr/withConn2/withConnUnscoped instead", name, line)
		}
	}
}
