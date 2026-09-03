// broker/internal/baseline/recorder.go
// In-memory observation buffer for automated agent behavioral baseline
// learning. Observe is the
// hot-path call from InvokeTool (wired in CP-B3): it only touches an
// in-memory map under a mutex, never the DB, so it can never fail or delay a
// tool call. Flush drains the buffer to Postgres on a ticker and reports
// which windows have just closed, for the Detector to check.
package baseline

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/adamdekan/aikonos/broker/internal/db"
	"go.uber.org/zap"
)

// Recorder observes one successful tool invocation. Implementations must be
// non-blocking and fail-open — a nil Recorder, buffer pressure, or any
// downstream error must never affect the caller.
type Recorder interface {
	Observe(ctx context.Context, tenant, agent, tool string, costUnits int64)
}

// windowStore is the narrow persistence seam DBRecorder needs. Satisfied by
// *db.AgentBaselineRepo; declared here (not imported from db) so this
// package's tests use a fake instead of a live pool.
type windowStore interface {
	UpsertWindows(ctx context.Context, tenant string, deltas []db.WindowDelta) error
}

// windowStart truncates t to the start of its window of the given size. Pure.
func windowStart(t time.Time, size time.Duration) time.Time {
	if size <= 0 {
		return t
	}
	return t.Truncate(size)
}

type bufKey struct {
	tenant      string
	agent       string
	tool        string
	windowStart time.Time
}

type bufVal struct {
	invocations int64
	costUnits   int64
}

// ClosedWindow identifies one (tenant, agent, windowStart) window whose
// invocations were just flushed to Postgres and whose window has fully
// elapsed as of the Flush call's now — the unit the Detector checks next.
// Tools and Invocations are the union/sum of that window's flushed deltas, so
// CP-B3's caller can build a detector.WindowSummary directly, without a DB
// round-trip.
type ClosedWindow struct {
	Tenant      string
	Agent       string
	WindowStart time.Time
	Tools       []string
	Invocations int64
}

// DBRecorder buffers Observe calls in memory, keyed by (tenant, agent, tool,
// window), and flushes them to a windowStore in one batched upsert per
// tenant. A Flush failure for a tenant logs and leaves that tenant's buffered
// deltas in place for the next tick (at-least-once, additive on the DB side).
type DBRecorder struct {
	mu         sync.Mutex
	buf        map[bufKey]*bufVal
	windowSize time.Duration
	store      windowStore
	logger     *zap.Logger

	// now is overridable in tests (same package) for deterministic window
	// bucketing; production always uses time.Now via the constructor.
	now func() time.Time
}

// NewDBRecorder constructs a DBRecorder. logger may be nil.
func NewDBRecorder(store windowStore, windowSize time.Duration, logger *zap.Logger) *DBRecorder {
	return &DBRecorder{
		buf:        make(map[bufKey]*bufVal),
		windowSize: windowSize,
		store:      store,
		logger:     logger,
		now:        time.Now,
	}
}

// Observe records one invocation against the current window's in-memory
// counters. Never touches the DB.
func (r *DBRecorder) Observe(_ context.Context, tenant, agent, tool string, costUnits int64) {
	key := bufKey{
		tenant:      tenant,
		agent:       agent,
		tool:        tool,
		windowStart: windowStart(r.now(), r.windowSize),
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.buf[key]
	if !ok {
		v = &bufVal{}
		r.buf[key] = v
	}
	v.invocations++
	v.costUnits += costUnits
}

// Flush drains the buffer into one UpsertWindows call per tenant and returns
// every window that both flushed successfully and has fully elapsed as of
// now (windowStart+windowSize <= now). A tenant whose upsert fails keeps its
// buffered deltas (logged, not returned as closed) so the next Flush retries
// them.
func (r *DBRecorder) Flush(ctx context.Context, now time.Time) []ClosedWindow {
	r.mu.Lock()
	deltasByTenant := make(map[string][]db.WindowDelta)
	keysByTenant := make(map[string][]bufKey)
	for k, v := range r.buf {
		deltasByTenant[k.tenant] = append(deltasByTenant[k.tenant], db.WindowDelta{
			AgentID:     k.agent,
			ToolID:      k.tool,
			WindowStart: k.windowStart,
			Invocations: v.invocations,
			CostUnits:   v.costUnits,
		})
		keysByTenant[k.tenant] = append(keysByTenant[k.tenant], k)
	}
	r.mu.Unlock()

	type closedKey struct {
		tenant      string
		agent       string
		windowStart time.Time
	}
	type closedAgg struct {
		tools       map[string]bool
		invocations int64
	}
	aggs := make(map[closedKey]*closedAgg)
	var order []closedKey

	for tenant, deltas := range deltasByTenant {
		if err := r.store.UpsertWindows(ctx, tenant, deltas); err != nil {
			if r.logger != nil {
				r.logger.Error("baseline flush failed, retaining buffer",
					zap.String("tenant", tenant), zap.Error(err))
			}
			continue
		}

		// Subtract exactly what was flushed rather than deleting outright: an
		// Observe for the same key can land in the gap between the snapshot
		// above and this re-lock, incrementing the still-referenced bufVal.
		// Deleting unconditionally would silently discard that increment
		// (lost-update). Only remove the key once its residual is back to
		// zero; otherwise keep it so the next Flush picks up the remainder.
		r.mu.Lock()
		for i, k := range keysByTenant[tenant] {
			v, ok := r.buf[k]
			if !ok {
				continue
			}
			flushed := deltasByTenant[tenant][i]
			v.invocations -= flushed.Invocations
			v.costUnits -= flushed.CostUnits
			if v.invocations == 0 && v.costUnits == 0 {
				delete(r.buf, k)
			}
		}
		r.mu.Unlock()

		for i, k := range keysByTenant[tenant] {
			if k.windowStart.Add(r.windowSize).After(now) {
				continue // window not yet closed
			}
			ck := closedKey{tenant: k.tenant, agent: k.agent, windowStart: k.windowStart}
			agg, ok := aggs[ck]
			if !ok {
				agg = &closedAgg{tools: make(map[string]bool)}
				aggs[ck] = agg
				order = append(order, ck)
			}
			flushed := deltasByTenant[tenant][i]
			agg.tools[flushed.ToolID] = true
			agg.invocations += flushed.Invocations
		}
	}

	closed := make([]ClosedWindow, 0, len(order))
	for _, ck := range order {
		agg := aggs[ck]
		tools := make([]string, 0, len(agg.tools))
		for t := range agg.tools {
			tools = append(tools, t)
		}
		sort.Strings(tools)
		closed = append(closed, ClosedWindow{
			Tenant:      ck.tenant,
			Agent:       ck.agent,
			WindowStart: ck.windowStart,
			Tools:       tools,
			Invocations: agg.invocations,
		})
	}
	return closed
}
