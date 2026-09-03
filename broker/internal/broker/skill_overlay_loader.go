package broker

// skill_overlay_loader.go — service-level lazy loader for the tenant skill overlay.
// Invariant I6: InvokeTool and SubmitPlan never do a per-call DB read.
// One DB read per tenant per process lifetime; reloads on explicit invalidate.

import (
	"context"
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
// same tenant are no-ops. invalidate(tenant) clears the flag so the next
// ensureLoaded re-reads from the store (called after every mutation in CP4).
type skillOverlayLoader struct {
	reg     *toolregistry.Registry
	store   skillOverlayStore
	loaded  sync.Map // tenant → struct{} sentinel
}

// newSkillOverlayLoader constructs a loader bound to reg and store.
func newSkillOverlayLoader(reg *toolregistry.Registry, store skillOverlayStore) *skillOverlayLoader {
	return &skillOverlayLoader{reg: reg, store: store}
}

// ensureLoaded loads the tenant overlay from the store if not already cached.
// On store error it logs and leaves the cache as-is (last-good / empty is safe
// per I7 — un-curated tools keep their existing behaviour). Never panics.
func (l *skillOverlayLoader) ensureLoaded(ctx context.Context, tenant string, log *zap.Logger) {
	if _, ok := l.loaded.Load(tenant); ok {
		return
	}
	rows, err := l.store.ListByTenant(ctx, tenant)
	if err != nil {
		if log != nil {
			log.Warn("skillOverlayLoader: load failed — retaining last-good cache",
				zap.String("tenant", tenant), zap.Error(err))
		}
		// Do NOT mark as loaded so the next call retries.
		return
	}

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
	l.reg.SetTenantOverlay(tenant, overlayRows)
	l.loaded.Store(tenant, struct{}{})
}

// invalidate clears the loaded flag for tenant so the next ensureLoaded
// re-reads from the store. Called after every UpsertSkill / DeleteSkill.
func (l *skillOverlayLoader) invalidate(tenant string) {
	l.loaded.Delete(tenant)
}
