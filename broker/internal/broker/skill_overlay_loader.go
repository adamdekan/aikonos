package broker

// skill_overlay_loader.go — service-level lazy loader for the tenant skill overlay.
// Invariant I6: InvokeTool and SubmitPlan never do a per-call DB read.
// One DB read per tenant per process lifetime; reloads on explicit invalidate.

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"go.uber.org/zap"

	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/toolregistry"
)

// skillOverlayStore is the minimal read interface the broker layer needs from
// the skill_overlay DB repo (lazy loader). Satisfied by *db.SkillOverlayRepo;
// extracted so unit tests can inject a fake without Postgres.
type skillOverlayStore interface {
	ListByTenant(ctx context.Context, tenant string) ([]db.SkillOverlay, error)
}

// skillOverlayMutStore extends skillOverlayStore with mutation methods needed
// by UpsertSkill and DeleteSkill. Production wires *db.SkillOverlayRepo which
// satisfies both interfaces.
type skillOverlayMutStore interface {
	skillOverlayStore
	Upsert(ctx context.Context, tenant string, row db.SkillOverlay) error
	Delete(ctx context.Context, tenant, toolID string) error
}

// skillOverlayLoader lazy-loads each tenant's overlay exactly once, caching the
// result in the CP2 registry via SetTenantOverlay. Subsequent calls for the
// same tenant are no-ops. invalidate(tenant) bumps the tenant's generation so
// the next ensureLoaded re-reads from the store (called after every mutation
// in CP4); a load that read before the bump is never cached (F25).
type skillOverlayLoader struct {
	reg   *toolregistry.Registry
	store skillOverlayStore

	mu     sync.Mutex
	gen    map[string]uint64 // tenant → generation, bumped by invalidate
	loaded map[string]uint64 // tenant → generation the cached overlay was read at
}

// newSkillOverlayLoader constructs a loader bound to reg and store.
func newSkillOverlayLoader(reg *toolregistry.Registry, store skillOverlayStore) *skillOverlayLoader {
	return &skillOverlayLoader{reg: reg, store: store, gen: map[string]uint64{}, loaded: map[string]uint64{}}
}

// overlayLoadAttempts bounds re-reads when invalidations keep racing a load.
const overlayLoadAttempts = 3

// ensureLoaded loads the tenant overlay from the store unless the cached copy
// is current. It fails closed (F25): a store error, or invalidations racing
// every attempt, returns an error and leaves the tenant unloaded — the
// overlay may hold an admin's disable, so callers on the enforcement path
// deny rather than serve a stale or empty overlay. Never panics.
// ponytail: invalidate is per process; a multi-replica broker needs a TTL or
// cross-replica invalidation (version column / LISTEN) to see another
// replica's change.
func (l *skillOverlayLoader) ensureLoaded(ctx context.Context, tenant string, log *zap.Logger) error {
	l.mu.Lock()
	g := l.gen[tenant]
	lg, ok := l.loaded[tenant]
	l.mu.Unlock()
	if ok && lg == g {
		return nil
	}
	for range overlayLoadAttempts {
		rows, err := l.store.ListByTenant(ctx, tenant)
		if err != nil {
			if log != nil {
				log.Warn("skillOverlayLoader: load failed — failing closed",
					zap.String("tenant", tenant), zap.Error(err))
			}
			return fmt.Errorf("load skill overlay: %w", err)
		}
		overlayRows := overlayRowsFrom(tenant, rows, log)
		l.mu.Lock()
		if l.gen[tenant] == g {
			l.reg.SetTenantOverlay(tenant, overlayRows)
			l.loaded[tenant] = g
			l.mu.Unlock()
			return nil
		}
		g = l.gen[tenant] // invalidated mid-read: what we read may be stale
		l.mu.Unlock()
	}
	return errors.New("load skill overlay: changed during every attempt")
}

// overlayRowsFrom converts stored rows to registry overlay rows.
func overlayRowsFrom(tenant string, rows []db.SkillOverlay, log *zap.Logger) []toolregistry.OverlayRow {

	overlayRows := make([]toolregistry.OverlayRow, 0, len(rows))
	for _, r := range rows {
		ec, ok := toolregistry.EffectClassFromRego(r.EffectClass)
		if !ok {
			// Rows are written through validateSkillOverlay which validates the
			// class, so an unrecognised class implies DB tampering or migration
			// drift. Skip the row entirely — caching UNSPECIFIED would make the
			// overlay authoritative for this tool with an unknown class, which
			// is a routing downgrade / fail-open. Log loudly and let the tool
			// fall back to its un-curated global default (safe, I7).
			if log != nil {
				log.Warn("skillOverlayLoader: skipping row with unrecognised effect_class",
					zap.String("tenant", tenant),
					zap.String("tool_id", r.ToolID),
					zap.String("effect_class", r.EffectClass))
			}
			continue
		}
		overlayRows = append(overlayRows, toolregistry.OverlayRow{
			ToolID:       r.ToolID,
			Enabled:      r.Enabled,
			EffectClass:  ec,
			Scope:        r.CapabilityScope,
			ExecutorKind: r.ExecutorKind,
		})
	}
	return overlayRows
}

// invalidate bumps the tenant's generation so the next ensureLoaded re-reads
// from the store and an in-flight load is not cached. Called after every
// UpsertSkill / DeleteSkill.
func (l *skillOverlayLoader) invalidate(tenant string) {
	l.mu.Lock()
	l.gen[tenant]++
	l.mu.Unlock()
}
