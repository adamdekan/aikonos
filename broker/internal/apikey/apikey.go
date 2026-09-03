// broker/internal/apikey/apikey.go
// Per-agent API key generation and peppered hashing.
//
// Key format: "tk_" + base62(16 random bytes) — 16 bytes = 128 bits entropy.
// At-rest format: HMAC-SHA256(pepper, sha256(raw_key)) — the pepper is a broker
// secret (env/Vault) so neither DB compromise nor code access alone recovers keys.
// Resolve mechanism: the gateway sends sha256(raw_key); the broker peppers it and
// looks up by full peppered hash (DB equality, no in-process compare needed).
package apikey

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"
)

const base62Chars = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// Generate creates a new random API key of the form "tk_<22+ base62 chars>".
// 16 random bytes encoded in base62 yields ~21.5 chars; we use 16 bytes to
// guarantee ≥128 bits of entropy with a small surplus.
func Generate() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("apikey.Generate: %w", err)
	}
	n := new(big.Int).SetBytes(buf)
	base := big.NewInt(62)
	rem := new(big.Int)
	var sb strings.Builder
	sb.WriteString("tk_")
	for n.Sign() > 0 {
		n.DivMod(n, base, rem)
		sb.WriteByte(base62Chars[rem.Int64()])
	}
	// Pad to ensure at least 22 suffix chars (guarantees ≥128-bit display width).
	for sb.Len() < 3+22 {
		sb.WriteByte(base62Chars[0])
	}
	return sb.String(), nil
}

// Prefix returns the first 8 characters of a raw key for display purposes.
func Prefix(rawKey string) string {
	if len(rawKey) >= 8 {
		return rawKey[:8]
	}
	return rawKey
}

// PepperedHash computes HMAC-SHA256(pepper, sha256(rawKey)) and returns the
// hex-encoded digest. This is the value stored in the DB. pepper must not be
// empty in production; callers that need to reject an empty pepper should check
// before calling.
func PepperedHash(pepper, rawKey string) (string, error) {
	// inner: sha256(rawKey)
	inner := sha256.Sum256([]byte(rawKey))
	// outer: HMAC-SHA256(pepper, inner)
	mac := hmac.New(sha256.New, []byte(pepper))
	mac.Write(inner[:])
	return hex.EncodeToString(mac.Sum(nil)), nil
}

// HashForResolve computes sha256(rawKey) as a hex string — the value the
// gateway sends to the broker's ResolveAgentApiKey RPC. The broker then applies
// the pepper server-side.
func HashForResolve(rawKey string) string {
	h := sha256.Sum256([]byte(rawKey))
	return hex.EncodeToString(h[:])
}

// PepperedHashFromResolveHash computes HMAC-SHA256(pepper, resolveHashBytes)
// where resolveHashBytes is the decoded sha256(rawKey). This is what the broker
// calls when processing a ResolveAgentApiKey request.
func PepperedHashFromResolveHash(pepper, resolveHashHex string) (string, error) {
	inner, err := hex.DecodeString(resolveHashHex)
	if err != nil {
		return "", fmt.Errorf("apikey.PepperedHashFromResolveHash: decode hex: %w", err)
	}
	mac := hmac.New(sha256.New, []byte(pepper))
	mac.Write(inner)
	return hex.EncodeToString(mac.Sum(nil)), nil
}
