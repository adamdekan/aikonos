package workspacepath

import "testing"

func TestSanitizeSegReplacesDisallowedBytes(t *testing.T) {
	got := SanitizeSeg("alice@corp.com")
	want := "alice_corp_com"
	if got != want {
		t.Fatalf("SanitizeSeg(%q) = %q, want %q", "alice@corp.com", got, want)
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
	want := "____h_llo"
	if got != want {
		t.Fatalf("SanitizeSeg(%q) = %q, want %q", "../\\héllo", got, want)
	}
}

func TestSanitizeSegNeverErrors(t *testing.T) {
	// SanitizeSeg has no error return; this test documents that contract so a
	// future signature change is deliberate, not accidental.
	var _ func(string) string = SanitizeSeg
}
