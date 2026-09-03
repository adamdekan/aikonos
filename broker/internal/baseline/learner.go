// broker/internal/baseline/learner.go
// Periodic envelope computation for automated agent behavioral baseline
// learning. Recompute reads
// each agent's recent windows, derives tool_set/rpm_p95/cost_p95, and upserts
// the learned baseline; then prunes windows past retention. now is always
// passed in — this file never calls time.Now() in computation logic, so
// tests are deterministic.
package baseline

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/adamdekan/aikonos/broker/internal/db"
)

// Config holds every tunable for the baseline pipeline. CP-B3's main.go
// populates it from AIKONOS_BASELINE_* env vars; this package never reads env.
type Config struct {
	WindowSize       time.Duration
	LearnInterval    time.Duration
	MinSampleWindows int
	DriftMultiplier  float64
	RetentionWindows int
}

// percentile returns the pth (0..100) nearest-rank percentile of sorted
// ascending values. Pure. Empty input returns 0.
func percentile(sorted []float64, p float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n == 1 {
		return sorted[0]
	}
	rank := int(math.Ceil(p / 100 * float64(n)))
	if rank < 1 {
		rank = 1
	}
	if rank > n {
		rank = n
	}
	return sorted[rank-1]
}

// learnerStore is the narrow persistence seam Learner needs, satisfied by
// *db.AgentBaselineRepo — declared here so this package tests with a fake.
type learnerStore interface {
	ListRecentWindows(ctx context.Context, tenant, agent string, sinceWindows int) ([]db.WindowRow, error)
	DistinctAgentsWithWindows(ctx context.Context, since time.Time) ([]db.AgentRef, error)
	UpsertBaseline(ctx context.Context, tenant string, b db.Baseline) error
	PruneWindowsBefore(ctx context.Context, cutoff time.Time) (int64, error)
}

// Learner periodically recomputes each agent's learned behavior envelope.
type Learner struct {
	store learnerStore
	cfg   Config
}

// NewLearner constructs a Learner.
func NewLearner(store learnerStore, cfg Config) *Learner {
	return &Learner{store: store, cfg: cfg}
}

// Recompute recomputes and upserts the baseline for every agent with windows
// in the retention horizon as of now, then prunes windows older than that
// horizon. Failures for individual agents are collected and returned
// together (via errors.Join) rather than aborting the whole sweep — one bad
// agent must not block the rest from getting a fresh envelope.
func (l *Learner) Recompute(ctx context.Context, now time.Time) error {
	since := now.Add(-time.Duration(l.cfg.RetentionWindows) * l.cfg.WindowSize)

	agents, err := l.store.DistinctAgentsWithWindows(ctx, since)
	if err != nil {
		return fmt.Errorf("list distinct agents: %w", err)
	}

	var errs []error
	for _, ref := range agents {
		rows, err := l.store.ListRecentWindows(ctx, ref.TenantID, ref.AgentID, l.cfg.RetentionWindows)
		if err != nil {
			errs = append(errs, fmt.Errorf("list recent windows for %s/%s: %w", ref.TenantID, ref.AgentID, err))
			continue
		}
		b := computeBaseline(ref.TenantID, ref.AgentID, rows, now)
		if err := l.store.UpsertBaseline(ctx, ref.TenantID, b); err != nil {
			errs = append(errs, fmt.Errorf("upsert baseline for %s/%s: %w", ref.TenantID, ref.AgentID, err))
		}
	}

	if _, err := l.store.PruneWindowsBefore(ctx, since); err != nil {
		errs = append(errs, fmt.Errorf("prune windows: %w", err))
	}

	return errors.Join(errs...)
}

// computeBaseline aggregates one agent's window rows into a learned envelope.
// Pure — no I/O, no time.Now().
func computeBaseline(tenant, agent string, rows []db.WindowRow, now time.Time) db.Baseline {
	type perWindow struct {
		invocations int64
		cost        int64
	}
	byWindow := make(map[time.Time]*perWindow)
	toolSeen := make(map[string]bool)
	var firstSeen time.Time

	for _, row := range rows {
		toolSeen[row.ToolID] = true
		if firstSeen.IsZero() || row.WindowStart.Before(firstSeen) {
			firstSeen = row.WindowStart
		}
		pw, ok := byWindow[row.WindowStart]
		if !ok {
			pw = &perWindow{}
			byWindow[row.WindowStart] = pw
		}
		pw.invocations += row.Invocations
		pw.cost += row.CostUnits
	}

	invocations := make([]float64, 0, len(byWindow))
	costs := make([]float64, 0, len(byWindow))
	for _, pw := range byWindow {
		invocations = append(invocations, float64(pw.invocations))
		costs = append(costs, float64(pw.cost))
	}
	sort.Float64s(invocations)
	sort.Float64s(costs)

	toolSet := make([]string, 0, len(toolSeen))
	for t := range toolSeen {
		toolSet = append(toolSet, t)
	}
	sort.Strings(toolSet)

	return db.Baseline{
		TenantID:      tenant,
		AgentID:       agent,
		ToolSet:       toolSet,
		RpmP95:        percentile(invocations, 95),
		CostP95:       percentile(costs, 95),
		SampleWindows: len(byWindow),
		FirstSeen:     firstSeen,
		ComputedAt:    now,
	}
}
