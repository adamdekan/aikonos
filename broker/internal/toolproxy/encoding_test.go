package toolproxy

import (
	"strings"
	"testing"
	"unicode/utf8"

	"golang.org/x/text/encoding/unicode"
)

func TestDecodeToUTF8_Windows1252(t *testing.T) {
	// 0xE1 is á in Windows-1252; the raw bytes are invalid UTF-8.
	got := decodeToUTF8([]byte("M\xe1jer"))
	if got != "Májer" {
		t.Errorf("Windows-1252 decode = %q, want %q", got, "Májer")
	}
	if !utf8.ValidString(got) {
		t.Errorf("output not valid UTF-8: %q", got)
	}
}

func TestDecodeToUTF8_UTF16LEWithBOM(t *testing.T) {
	const want = "héllo — world"
	enc := unicode.UTF16(unicode.LittleEndian, unicode.UseBOM).NewEncoder()
	b, err := enc.Bytes([]byte(want))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if len(b) < 2 || b[0] != 0xFF || b[1] != 0xFE {
		t.Fatalf("expected a UTF-16LE BOM, got % x", b[:min(2, len(b))])
	}
	got := decodeToUTF8(b)
	if got != want {
		t.Errorf("UTF-16LE round-trip = %q, want %q", got, want)
	}
	if !utf8.ValidString(got) {
		t.Errorf("output not valid UTF-8: %q", got)
	}
}

func TestDecodeToUTF8_UTF8BOMStripped(t *testing.T) {
	got := decodeToUTF8([]byte("\xef\xbb\xbfhello"))
	if got != "hello" {
		t.Errorf("UTF-8 BOM strip = %q, want %q", got, "hello")
	}
	if !utf8.ValidString(got) {
		t.Errorf("output not valid UTF-8: %q", got)
	}
}

func TestDecodeToUTF8_ValidUTF8PassesThrough(t *testing.T) {
	const in = "already valid — ünïcödé 日本語"
	got := decodeToUTF8([]byte(in))
	if got != in {
		t.Errorf("valid UTF-8 not passed through byte-identical: got %q, want %q", got, in)
	}
}

func TestDecodeToUTF8_AlwaysValidUTF8(t *testing.T) {
	cases := [][]byte{
		[]byte("M\xe1jer"),
		{0xFF, 0xFE, 0x48, 0x00}, // UTF-16LE BOM + "H"
		{0xFE, 0xFF, 0x00, 0x48}, // UTF-16BE BOM + "H"
		[]byte("\xef\xbb\xbfhi"),
		{0x80, 0x81, 0x82, 0xFF}, // arbitrary high bytes
		{},
		[]byte("plain"),
	}
	for i, c := range cases {
		if got := decodeToUTF8(c); !utf8.ValidString(got) {
			t.Errorf("case %d: output not valid UTF-8: %q", i, got)
		}
	}
}

func TestTruncateUTF8_CutMidRune(t *testing.T) {
	// "héllo": h(1) + é(2 bytes) + llo(3). Cutting at 2 lands in the middle of é.
	got := truncateUTF8("héllo", 2)
	if !utf8.ValidString(got) {
		t.Errorf("truncated to invalid UTF-8: %q", got)
	}
	if len(got) > 2 {
		t.Errorf("truncated length %d exceeds max 2", len(got))
	}
	if got != "h" {
		t.Errorf("truncateUTF8(%q, 2) = %q, want %q", "héllo", got, "h")
	}
}

func TestTruncateUTF8_NonPositiveMax(t *testing.T) {
	for _, max := range []int{0, -1, -100} {
		if got := truncateUTF8("héllo", max); got != "" {
			t.Errorf("truncateUTF8(_, %d) = %q, want empty (must not panic)", max, got)
		}
	}
}

func TestTruncateUTF8_NoTruncationNeeded(t *testing.T) {
	s := "short"
	if got := truncateUTF8(s, 100); got != s {
		t.Errorf("truncateUTF8 shortened a fitting string: %q", got)
	}
}

func TestTruncateUTF8_ExactBoundary(t *testing.T) {
	// Multi-byte string cut exactly on a rune boundary stays intact up to max.
	s := "ab€cd" // a,b (2) + € (3 bytes) + c,d (2) = 7 bytes
	got := truncateUTF8(s, 5)
	if !utf8.ValidString(got) {
		t.Errorf("not valid UTF-8: %q", got)
	}
	if got != "ab€" {
		t.Errorf("truncateUTF8(%q, 5) = %q, want %q", s, got, "ab€")
	}
	if !strings.HasPrefix(s, got) {
		t.Errorf("truncation is not a prefix of the input: %q", got)
	}
}
