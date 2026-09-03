// broker/internal/toolproxy/encoding.go
//
// Byte→UTF-8 decoding for tool results. Tool output is marshalled into a
// protobuf Struct (ResultStruct → structpb.NewStruct), which rejects any string
// that isn't valid UTF-8. A single non-UTF-8 file read (a Windows-1252 CSV, a
// PowerShell-exported UTF-16 log) would otherwise fail the whole InvokeTool RPC
// with an opaque codes.Internal. decodeToUTF8 turns arbitrary bytes into a
// valid UTF-8 string; truncateUTF8 clips without splitting a multi-byte rune.
package toolproxy

import (
	"bytes"
	"unicode/utf8"

	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/unicode"
)

// decodeToUTF8 turns arbitrary bytes into a valid UTF-8 string. It is total: it
// never fails and its output always satisfies utf8.ValidString. Detection
// ladder: UTF-16 BOM sniff, strip a UTF-8 BOM, pass valid UTF-8 through
// unchanged, else decode as Windows-1252 (a total decode — undefined bytes map
// to U+FFFD — which also covers ISO-8859-1 in practice).
//
// ponytail: charset detection beyond BOM + Windows-1252 (HTML <meta charset>,
// Shift-JIS, EUC, …) is deliberately out of scope; add a real detector
// (golang.org/x/net/html/charset) only if a concrete need appears.
func decodeToUTF8(b []byte) string {
	// UTF-16 BOM: PowerShell's `Export-Csv -Encoding Unicode` writes UTF-16LE
	// with a BOM, so this is a real case. UseBOM consumes the BOM (not emitted).
	if len(b) >= 2 {
		switch {
		case b[0] == 0xFF && b[1] == 0xFE:
			return decodeUTF16(b, unicode.LittleEndian)
		case b[0] == 0xFE && b[1] == 0xFF:
			return decodeUTF16(b, unicode.BigEndian)
		}
	}
	b = bytes.TrimPrefix(b, []byte{0xEF, 0xBB, 0xBF}) // strip a leading UTF-8 BOM
	if utf8.Valid(b) {
		return string(b)
	}
	out, err := charmap.Windows1252.NewDecoder().Bytes(b)
	if err != nil {
		// Windows-1252 decoding is total, but never trust that blindly.
		return string(bytes.ToValidUTF8(b, []byte("�")))
	}
	return string(out)
}

func decodeUTF16(b []byte, endian unicode.Endianness) string {
	out, err := unicode.UTF16(endian, unicode.UseBOM).NewDecoder().Bytes(b)
	if err != nil {
		return string(bytes.ToValidUTF8(b, []byte("�")))
	}
	return string(out)
}

// truncateUTF8 clips s to at most max bytes without splitting a multi-byte
// rune, walking the cut point back to the nearest rune boundary.
func truncateUTF8(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	i := max
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i]
}
