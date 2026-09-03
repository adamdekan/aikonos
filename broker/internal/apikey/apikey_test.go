package apikey_test

import (
	"strings"
	"testing"

	"github.com/adamdekan/aikonos/broker/internal/apikey"
)

func TestGenerate_Format(t *testing.T) {
	key, err := apikey.Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !strings.HasPrefix(key, "tk_") {
		t.Errorf("key must start with tk_, got %q", key[:min(len(key), 10)])
	}
	// base62 alphabet after prefix; ≥128 bits entropy ≈ ≥22 base62 chars
	suffix := strings.TrimPrefix(key, "tk_")
	if len(suffix) < 22 {
		t.Errorf("suffix too short for ≥128 bits entropy: got %d chars", len(suffix))
	}
	for _, c := range suffix {
		if !isBase62(c) {
			t.Errorf("non-base62 char %q in key suffix", c)
		}
	}
}

func TestPepperedHash_Deterministic(t *testing.T) {
	pepper := "test-pepper-value"
	key := "tk_AAABBBCCC"
	h1, err := apikey.PepperedHash(pepper, key)
	if err != nil {
		t.Fatalf("PepperedHash: %v", err)
	}
	h2, err := apikey.PepperedHash(pepper, key)
	if err != nil {
		t.Fatalf("PepperedHash: %v", err)
	}
	if h1 != h2 {
		t.Error("hash must be deterministic for same pepper+key")
	}
}

func TestPepperedHash_DifferentPepperDifferentHash(t *testing.T) {
	key := "tk_AAABBBCCC"
	h1, _ := apikey.PepperedHash("pepper-a", key)
	h2, _ := apikey.PepperedHash("pepper-b", key)
	if h1 == h2 {
		t.Error("different peppers must yield different hashes")
	}
}

func TestPepperedHash_RawKeyNotRecoverable(t *testing.T) {
	pepper := "pepper"
	key := "tk_SOMESECRETKEY"
	hash, err := apikey.PepperedHash(pepper, key)
	if err != nil {
		t.Fatalf("PepperedHash: %v", err)
	}
	// The stored hash must not equal the key.
	if hash == key {
		t.Error("hash must not equal the raw key")
	}
	// The stored hash without the pepper must differ (no unpeppered fallback).
	hashNoPepper, _ := apikey.PepperedHash("", key)
	if hash == hashNoPepper {
		t.Error("peppered hash must differ from hash produced with empty pepper")
	}
}

func isBase62(r rune) bool {
	return (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
