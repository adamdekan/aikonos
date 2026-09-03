package broker

import (
	"context"
	"sync"
	"time"

	"github.com/adamdekan/aikonos/broker/internal/db"
)

// userDirectoryLister is the minimal user-directory surface the email resolver
// needs. *db.UserDirectoryRepo satisfies it; tests inject a fake.
type userDirectoryLister interface {
	ListByTenant(ctx context.Context, tenantID string) (map[string]db.UserDirectoryEntry, error)
}

// CachedEmailResolver resolves a (tenant, subject) pair to a human-readable
// email from the user directory, with a per-tenant in-memory cache so the audit
// hot path does not hit Postgres on every emitted event. It satisfies
// audit.EmailResolver. A miss or a backing-store error returns "" (the audit
// view falls back to actor_user_id) — it never fails the audited operation.
type CachedEmailResolver struct {
	repo userDirectoryLister
	ttl  time.Duration

	mu    sync.Mutex
	cache map[string]tenantEmails // tenantID → snapshot
}

type tenantEmails struct {
	emails  map[string]string // subject → email
	fetched time.Time
}

// NewCachedEmailResolver builds the resolver. ttl <= 0 defaults to 60s.
func NewCachedEmailResolver(repo userDirectoryLister, ttl time.Duration) *CachedEmailResolver {
	if ttl <= 0 {
		ttl = 60 * time.Second
	}
	return &CachedEmailResolver{repo: repo, ttl: ttl, cache: make(map[string]tenantEmails)}
}

// Email returns the email for subject under tenantID, "" on any miss/error.
func (r *CachedEmailResolver) Email(ctx context.Context, tenantID, subject string) string {
	if r == nil || r.repo == nil || tenantID == "" || subject == "" {
		return ""
	}

	r.mu.Lock()
	entry, ok := r.cache[tenantID]
	fresh := ok && time.Since(entry.fetched) < r.ttl
	r.mu.Unlock()

	if fresh {
		return entry.emails[subject]
	}

	entries, err := r.repo.ListByTenant(ctx, tenantID)
	if err != nil {
		// Fall back to a stale snapshot if we have one; else empty.
		if ok {
			return entry.emails[subject]
		}
		return ""
	}

	emails := make(map[string]string, len(entries))
	for sub, e := range entries {
		emails[sub] = e.Email
	}
	snapshot := tenantEmails{emails: emails, fetched: time.Now()}

	r.mu.Lock()
	r.cache[tenantID] = snapshot
	r.mu.Unlock()

	return emails[subject]
}
