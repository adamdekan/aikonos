package toolproxy

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/adamdekan/aikonos/broker/internal/workspacefs"
)

// TestSafeJoin_RejectsTraversalViaCleanRel pins CP3 fix #3: safeJoin's escape
// check now delegates to workspacefs.CleanRel instead of its own HasPrefix
// re-implementation. Behavior must stay identical — a traversal attempt is
// still rejected, and it must reject via the same ErrInvalidPath sentinel
// CleanRel returns (proving the delegation actually happened, not just a
// parallel check that happens to agree).
func TestSafeJoin_RejectsTraversalViaCleanRel(t *testing.T) {
	base := t.TempDir()
	for _, bad := range []string{"../../../etc/passwd", "/etc/passwd", "a/../../escape"} {
		_, err := safeJoin(base, "ten1", "task1", bad)
		if err == nil {
			t.Fatalf("safeJoin(%q): expected rejection, got nil error", bad)
		}
		if !errors.Is(err, workspacefs.ErrInvalidPath) {
			t.Fatalf("safeJoin(%q): expected ErrInvalidPath (via CleanRel), got %v", bad, err)
		}
	}
}

// TestSafeJoin_AcceptsNormalPath proves the refactor didn't change the happy
// path: a normal relative path still resolves under base/tenant/task.
func TestSafeJoin_AcceptsNormalPath(t *testing.T) {
	base := t.TempDir()
	got, err := safeJoin(base, "ten1", "task1", "notes/out.md")
	if err != nil {
		t.Fatalf("safeJoin: unexpected error: %v", err)
	}
	want := filepath.Join(base, "ten1", "task1", "notes", "out.md")
	if got != want {
		t.Fatalf("safeJoin = %q, want %q", got, want)
	}
}
