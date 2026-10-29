package workspacepath

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The lossy part is still readable; the hash suffix keeps it one-to-one.
func TestSanitizeSegReplacesDisallowedBytes(t *testing.T) {
	got := SanitizeSeg("alice@corp.com")
	if !strings.HasPrefix(got, "alice_corp_com.") || len(got) != len("alice_corp_com.")+16 {
		t.Fatalf("SanitizeSeg(%q) = %q, want alice_corp_com.<16 hex>", "alice@corp.com", got)
	}
}

// TestSanitizeSegCollisionFree pins F175/G32 (L-10): identifiers that the old
// rule folded together get distinct segments, and a lossy one never meets a
// passthrough one.
func TestSanitizeSegCollisionFree(t *testing.T) {
	ids := []string{"john.doe@corp.com", "john_doe@corp.com", "john-doe@corp.com", "john+doe@corp.com",
		"john_doe_corp_com", "a/b", "a_b", "a.b", "group-x", "group_x", "11111111-1111-1111-1111-111111111111"}
	seen := map[string]string{}
	for _, id := range ids {
		seg := SanitizeSeg(id)
		if prev, ok := seen[seg]; ok {
			t.Fatalf("%q and %q share segment %q", prev, id, seg)
		}
		seen[seg] = id
		if strings.ContainsAny(seg, "/\\") || seg == "." || seg == ".." {
			t.Fatalf("%q -> unsafe segment %q", id, seg)
		}
	}
}

// Dir moves a workspace still under the pre-F175 name on first use, and
// leaves the new directory alone once it exists.
func TestDirMovesLegacyDirectory(t *testing.T) {
	root := t.TempDir()
	const tenant, user = "11111111-1111-1111-1111-111111111111", "alice@corp.com"
	old := filepath.Join(root, tenant, "alice_corp_com")
	if err := os.MkdirAll(old, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "notes.md"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := Dir(root, tenant, user)
	if dir == old {
		t.Fatal("Dir returned the lossy legacy path")
	}
	if _, err := os.Stat(filepath.Join(dir, "notes.md")); err != nil {
		t.Fatalf("legacy file not moved: %v", err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("legacy dir still present: %v", err)
	}
	// A colliding identifier no longer reaches alice's files.
	if other := Dir(root, tenant, "alice_corp.com"); other == dir {
		t.Fatal("a colliding id resolved to alice's directory")
	}
}

func TestSanitizeSegEmptyBecomesUnderscore(t *testing.T) {
	if got := SanitizeSeg(""); got != "_" {
		t.Fatalf("SanitizeSeg(\"\") = %q, want \"_\"", got)
	}
}

func TestSanitizeSegAllowsAlnumDashUnderscore(t *testing.T) {
	if got := SanitizeSeg("tenant-1_A9"); got != "tenant-1_A9" {
		t.Fatalf("SanitizeSeg passthrough changed: %q", got)
	}
}

func TestSanitizeSegUnicodeAndSeparators(t *testing.T) {
	got := SanitizeSeg("../\\héllo")
	want := "____h_llo."
	if !strings.HasPrefix(got, want) {
		t.Fatalf("SanitizeSeg(%q) = %q, want %q", "../\\héllo", got, want)
	}
}

func TestSanitizeSegNeverErrors(t *testing.T) {
	// SanitizeSeg has no error return; this test documents that contract so a
	// future signature change is deliberate, not accidental.
	var _ func(string) string = SanitizeSeg
}
