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
		if Hashed(seg) != (seg != id) || Hashed(id) {
			t.Fatalf("Hashed misclassifies %q / %q", id, seg)
		}
	}
}

const testTenant = "11111111-1111-1111-1111-111111111111"

// seedLegacy makes root/testTenant/name holding notes.md.
func seedLegacy(t *testing.T, root, name string) string {
	t.Helper()
	dir := filepath.Join(root, testTenant, name)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.md"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func hasNotes(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "notes.md"))
	return err == nil
}

// TestDirNeverClaimsLegacyDirectory pins G32/F175: an identity colliding with
// jane.doe@corp.com's pre-upgrade name that reaches Dir first gets an empty
// directory, not her workspace.
func TestDirNeverClaimsLegacyDirectory(t *testing.T) {
	root := t.TempDir()
	old := seedLegacy(t, root, "jane_doe_corp_com")
	if hasNotes(Dir(root, testTenant, "jane_doe@corp.com")) {
		t.Fatal("a colliding identity received the legacy workspace")
	}
	if !hasNotes(old) {
		t.Fatal("Dir moved the legacy workspace")
	}
}

// TestMigrateLegacy: a legacy directory with one known owner moves to that
// owner; one several known subjects map to goes to an unreachable quarantine
// and is reported; one nobody known maps to stays, as does a group memory
// bundle a group-* subject maps to; later runs are no-ops.
func TestMigrateLegacy(t *testing.T) {
	root := t.TempDir()
	const uid = "22222222-2222-2222-2222-222222222222"
	seedLegacy(t, root, "alice_corp_com")
	seedLegacy(t, root, "jane_doe_corp_com")
	bob := seedLegacy(t, root, "bob_corp_com")
	seedLegacy(t, root, uid)
	bundle := seedLegacy(t, root, "group-fin_ops")

	amb, err := MigrateLegacy(root, testTenant, []string{"alice@corp.com", "jane.doe@corp.com", "jane_doe@corp.com", uid, "group-fin.ops", ""})
	if err != nil {
		t.Fatal(err)
	}
	quarantine := filepath.Join(root, testTenant, ".legacy-jane_doe_corp_com")
	if len(amb) != 1 || amb[0] != quarantine {
		t.Fatalf("ambiguous = %v, want [%s]", amb, quarantine)
	}
	if !hasNotes(Dir(root, testTenant, "alice@corp.com")) {
		t.Fatal("sole owner's workspace not moved")
	}
	if hasNotes(Dir(root, testTenant, "jane.doe@corp.com")) || hasNotes(Dir(root, testTenant, "jane_doe@corp.com")) {
		t.Fatal("a shared legacy workspace went to one of its claimants")
	}
	if !hasNotes(quarantine) {
		t.Fatal("shared legacy workspace not quarantined")
	}
	if !hasNotes(bob) || !hasNotes(Dir(root, testTenant, uid)) || !hasNotes(bundle) {
		t.Fatal("an unowned, passthrough or group bundle directory was moved")
	}

	// A subject first known after the upgrade takes nothing on a restart.
	if amb, err = MigrateLegacy(root, testTenant, []string{"bob@corp.com"}); err != nil || len(amb) != 0 {
		t.Fatalf("second run: %v %v", amb, err)
	}
	if !hasNotes(bob) || hasNotes(Dir(root, testTenant, "bob@corp.com")) {
		t.Fatal("a second run moved a legacy directory")
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
