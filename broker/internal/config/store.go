// broker/internal/config/store.go
// Cached read-through store for tenant-scoped runtime config. The pure
// schema-validation + guardrail logic is unit-tested against a fake
// configRepo; the DB repo is wired in CP2.
package config

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"go.uber.org/zap"
)

// ErrUnknownKey is returned by Set when the key is not in Schema.
var ErrUnknownKey = errors.New("config: unknown key")

// ValidationError wraps a schema Validate rejection so callers can distinguish
// user-input errors (unknown key, out-of-range value) from repo/DB failures.
type ValidationError struct{ Cause error }

func (e *ValidationError) Error() string { return e.Cause.Error() }
func (e *ValidationError) Unwrap() error { return e.Cause }

// cacheTTL is the maximum staleness of a cached value. Set invalidates the
// local entry, so a change applies at once on this broker; deployment is
// single-broker (singleton.go), so no other replica serves an older value.
const cacheTTL = 30 * time.Second

// Repo is the storage back-end. Satisfied by db.PlatformConfigRepo;
// exported so callers in other packages (e.g. broker tests) can supply a fake
// and get a real Store without depending on the DB.
type Repo interface {
	Get(ctx context.Context, tenant, key string) (string, bool, error)
	Set(ctx context.Context, tenant, key, value, actor string) error
	List(ctx context.Context, tenant string) (map[string]string, error)
}

type cacheEntry struct {
	value   string
	setAt   time.Time
	missing bool // true when the row was absent in the DB
}

// Store provides typed, cached, schema-validated access to tenant config.
type Store struct {
	repo   Repo
	logger *zap.Logger
	mu     sync.Mutex
	cache  map[cacheKey]cacheEntry
}

type cacheKey struct {
	tenant, key string
}

// New builds a Store backed by repo. Logger defaults to nop; use WithLogger
// to attach a live logger in production.
func New(repo Repo) *Store {
	return &Store{
		repo:   repo,
		logger: zap.NewNop(),
		cache:  map[cacheKey]cacheEntry{},
	}
}

// WithLogger attaches a logger to the Store and returns it for chaining.
func (s *Store) WithLogger(l *zap.Logger) *Store {
	s.logger = l
	return s
}

// Entry is one row in the List response — the effective value (stored or
// default) plus the schema metadata for the Settings UI.
type Entry struct {
	Key     string
	Value   string // effective: stored value if present, else Default
	Default string
	Kind    Kind
	Doc     string
}

// GetInt returns the int value for key in tenant's config: the stored value,
// or the schema default when the tenant has not set one. No key may loosen a
// gate (schema.go), so an enforced key's default is its most permissive
// value: a value that cannot be read is an error, never silently the
// default. Callers that enforce a key fail closed on it.
func (s *Store) GetInt(ctx context.Context, tenant, key string) (int, error) {
	raw, err := s.value(ctx, tenant, key)
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("config: %s for tenant %s is not an integer: %q", key, tenant, raw)
	}
	return n, nil
}

// GetString returns the string value for key in tenant's config: the stored
// value, or the schema default when the tenant has not set one. As with
// GetInt, a value that cannot be read is an error, never the default.
func (s *Store) GetString(ctx context.Context, tenant, key string) (string, error) {
	return s.value(ctx, tenant, key)
}

// value returns the effective raw value for key: stored, else the schema
// default. Unknown keys and repo errors are errors.
func (s *Store) value(ctx context.Context, tenant, key string) (string, error) {
	k, ok := Schema[key]
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrUnknownKey, key)
	}
	raw, found, err := s.get(ctx, tenant, key)
	if err != nil {
		return "", err
	}
	if !found {
		return k.Default, nil
	}
	return raw, nil
}

// get returns the raw stored value and whether a row exists. Errors are not
// cached, so the next call retries.
func (s *Store) get(ctx context.Context, tenant, key string) (string, bool, error) {
	ck := cacheKey{tenant, key}

	s.mu.Lock()
	if e, ok := s.cache[ck]; ok && time.Since(e.setAt) < cacheTTL {
		s.mu.Unlock()
		if e.missing {
			return "", false, nil
		}
		return e.value, true, nil
	}
	s.mu.Unlock()

	val, found, err := s.repo.Get(ctx, tenant, key)
	if err != nil {
		s.logger.Warn("config: repo.Get failed",
			zap.String("tenant", tenant), zap.String("key", key), zap.Error(err))
		return "", false, fmt.Errorf("config: read %s for tenant %s: %w", key, tenant, err)
	}

	s.mu.Lock()
	if found {
		s.cache[ck] = cacheEntry{value: val, setAt: time.Now()}
	} else {
		s.cache[ck] = cacheEntry{missing: true, setAt: time.Now()}
	}
	s.mu.Unlock()

	return val, found, nil
}

// Set validates and writes a config value. Returns ErrUnknownKey if the key
// is not in Schema, or a validation error if the value is out of range.
// On success it invalidates the local cache entry so the next GetInt sees the
// new value immediately (other replicas pick it up within the TTL).
func (s *Store) Set(ctx context.Context, tenant, key, value, actor string) error {
	k, ok := Schema[key]
	if !ok {
		return ErrUnknownKey
	}
	if k.Validate != nil {
		if err := k.Validate(value); err != nil {
			return &ValidationError{Cause: err}
		}
	}
	if err := s.repo.Set(ctx, tenant, key, value, actor); err != nil {
		return err
	}
	// invalidate so the next read reflects the written value
	ck := cacheKey{tenant, key}
	s.mu.Lock()
	delete(s.cache, ck)
	s.mu.Unlock()
	return nil
}

// List returns an Entry for every schema key, with the effective value
// (stored or default) for the given tenant. A repo error is returned, not
// replaced by the defaults, so the Settings page never shows a tenant's
// restrictions as lifted when they could not be read.
func (s *Store) List(ctx context.Context, tenant string) ([]Entry, error) {
	stored, err := s.repo.List(ctx, tenant)
	if err != nil {
		return nil, fmt.Errorf("config: list settings for tenant %s: %w", tenant, err)
	}
	out := make([]Entry, 0, len(Schema))
	for name, k := range Schema {
		val, ok := stored[name]
		if !ok {
			val = k.Default
		}
		out = append(out, Entry{
			Key:     name,
			Value:   val,
			Default: k.Default,
			Kind:    k.Kind,
			Doc:     k.Doc,
		})
	}
	return out, nil
}
