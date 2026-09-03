package broker

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/adamdekan/aikonos/broker/internal/db"
)

type fakeDirLister struct {
	calls   atomic.Int32
	entries map[string]map[string]db.UserDirectoryEntry // tenantID → subject → entry
	err     error
}

func (f *fakeDirLister) ListByTenant(_ context.Context, tenantID string) (map[string]db.UserDirectoryEntry, error) {
	f.calls.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	return f.entries[tenantID], nil
}

func TestCachedEmailResolver_HitFromDirectory(t *testing.T) {
	repo := &fakeDirLister{entries: map[string]map[string]db.UserDirectoryEntry{
		"t1": {"oid-1": {Subject: "oid-1", Email: "alice@example.com"}},
	}}
	r := NewCachedEmailResolver(repo, time.Minute)
	if got := r.Email(context.Background(), "t1", "oid-1"); got != "alice@example.com" {
		t.Fatalf("want alice@example.com, got %q", got)
	}
}

func TestCachedEmailResolver_MissReturnsEmpty(t *testing.T) {
	repo := &fakeDirLister{entries: map[string]map[string]db.UserDirectoryEntry{
		"t1": {"oid-1": {Email: "alice@example.com"}},
	}}
	r := NewCachedEmailResolver(repo, time.Minute)
	if got := r.Email(context.Background(), "t1", "unknown"); got != "" {
		t.Fatalf("miss must be empty, got %q", got)
	}
}

func TestCachedEmailResolver_CachesWithinTTL(t *testing.T) {
	repo := &fakeDirLister{entries: map[string]map[string]db.UserDirectoryEntry{
		"t1": {"oid-1": {Email: "alice@example.com"}},
	}}
	r := NewCachedEmailResolver(repo, time.Minute)
	for i := 0; i < 5; i++ {
		r.Email(context.Background(), "t1", "oid-1")
	}
	if c := repo.calls.Load(); c != 1 {
		t.Fatalf("TTL cache must collapse to 1 query, got %d", c)
	}
}

func TestCachedEmailResolver_RefetchesAfterTTL(t *testing.T) {
	repo := &fakeDirLister{entries: map[string]map[string]db.UserDirectoryEntry{
		"t1": {"oid-1": {Email: "alice@example.com"}},
	}}
	r := NewCachedEmailResolver(repo, time.Nanosecond)
	r.Email(context.Background(), "t1", "oid-1")
	time.Sleep(time.Millisecond)
	r.Email(context.Background(), "t1", "oid-1")
	if c := repo.calls.Load(); c < 2 {
		t.Fatalf("expired TTL must refetch, got %d calls", c)
	}
}

func TestCachedEmailResolver_ErrorReturnsEmpty(t *testing.T) {
	repo := &fakeDirLister{err: errors.New("db down")}
	r := NewCachedEmailResolver(repo, time.Minute)
	if got := r.Email(context.Background(), "t1", "oid-1"); got != "" {
		t.Fatalf("error must yield empty, got %q", got)
	}
}

func TestCachedEmailResolver_ErrorFallsBackToStaleCache(t *testing.T) {
	repo := &fakeDirLister{entries: map[string]map[string]db.UserDirectoryEntry{
		"t1": {"oid-1": {Email: "alice@example.com"}},
	}}
	r := NewCachedEmailResolver(repo, time.Nanosecond)
	if got := r.Email(context.Background(), "t1", "oid-1"); got != "alice@example.com" {
		t.Fatalf("first fetch want alice, got %q", got)
	}
	time.Sleep(time.Millisecond)
	repo.err = errors.New("db down") // next refetch fails
	if got := r.Email(context.Background(), "t1", "oid-1"); got != "alice@example.com" {
		t.Fatalf("error after a good fetch must serve stale cache, got %q", got)
	}
}

func TestCachedEmailResolver_NilSafe(t *testing.T) {
	var r *CachedEmailResolver
	if got := r.Email(context.Background(), "t1", "oid"); got != "" {
		t.Fatalf("nil resolver must be empty, got %q", got)
	}
	r2 := NewCachedEmailResolver(nil, time.Minute)
	if got := r2.Email(context.Background(), "t1", "oid"); got != "" {
		t.Fatalf("nil repo must be empty, got %q", got)
	}
}
