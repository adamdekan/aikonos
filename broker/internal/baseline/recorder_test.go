package baseline

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/adamdekan/aikonos/broker/internal/db"
)

func TestWindowStart(t *testing.T) {
	size := time.Minute
	t1 := time.Date(2026, 7, 4, 10, 30, 45, 0, time.UTC)
	want := time.Date(2026, 7, 4, 10, 30, 0, 0, time.UTC)
	got := windowStart(t1, size)
	if !got.Equal(want) {
		t.Fatalf("windowStart(%v, %v) = %v, want %v", t1, size, got, want)
	}

	// Already aligned — unchanged.
	aligned := time.Date(2026, 7, 4, 10, 30, 0, 0, time.UTC)
	if got := windowStart(aligned, size); !got.Equal(aligned) {
		t.Fatalf("windowStart(aligned) = %v, want %v", got, aligned)
	}

	// Zero/negative size is a pass-through, not a divide-by-zero.
	if got := windowStart(t1, 0); !got.Equal(t1) {
		t.Fatalf("windowStart with zero size = %v, want %v", got, t1)
	}
}

// fakeWindowStore is a narrow, in-memory windowStore for testing DBRecorder
// without a live DB.
type fakeWindowStore struct {
	upserts [][]db.WindowDelta
	tenants []string
	err     error
}

func (f *fakeWindowStore) UpsertWindows(_ context.Context, tenant string, deltas []db.WindowDelta) error {
	if f.err != nil {
		return f.err
	}
	f.tenants = append(f.tenants, tenant)
	f.upserts = append(f.upserts, deltas)
	return nil
}

func TestDBRecorder_ObserveFlush_Additivity(t *testing.T) {
	store := &fakeWindowStore{}
	rec := NewDBRecorder(store, time.Minute, nil)
	fixed := time.Date(2026, 7, 4, 10, 0, 0, 0, time.UTC)
	rec.now = func() time.Time { return fixed }

	ctx := context.Background()
	rec.Observe(ctx, "tenant-a", "agent-1", "web.fetch", 5)
	rec.Observe(ctx, "tenant-a", "agent-1", "web.fetch", 7)
	rec.Observe(ctx, "tenant-a", "agent-1", "doc.write", 1)

	closed := rec.Flush(ctx, fixed.Add(time.Minute)) // window fully elapsed

	if len(store.upserts) != 1 {
		t.Fatalf("expected exactly one UpsertWindows call, got %d", len(store.upserts))
	}
	deltas := store.upserts[0]
	if len(deltas) != 2 {
		t.Fatalf("expected 2 distinct (agent,tool) deltas, got %d", len(deltas))
	}
	var fetch, write *db.WindowDelta
	for i := range deltas {
		switch deltas[i].ToolID {
		case "web.fetch":
			fetch = &deltas[i]
		case "doc.write":
			write = &deltas[i]
		}
	}
	if fetch == nil || fetch.Invocations != 2 || fetch.CostUnits != 12 {
		t.Fatalf("web.fetch delta wrong: %+v", fetch)
	}
	if write == nil || write.Invocations != 1 || write.CostUnits != 1 {
		t.Fatalf("doc.write delta wrong: %+v", write)
	}

	if len(closed) != 1 || closed[0].Tenant != "tenant-a" || closed[0].Agent != "agent-1" {
		t.Fatalf("expected one closed window for tenant-a/agent-1, got %+v", closed)
	}
	if !closed[0].WindowStart.Equal(fixed) {
		t.Fatalf("closed window start = %v, want %v", closed[0].WindowStart, fixed)
	}
	if want := []string{"doc.write", "web.fetch"}; !equalStrings(closed[0].Tools, want) {
		t.Fatalf("closed window tools = %v, want %v", closed[0].Tools, want)
	}
	if closed[0].Invocations != 3 {
		t.Fatalf("closed window invocations = %d, want 3 (sum across tools)", closed[0].Invocations)
	}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestDBRecorder_Flush_OnlyReturnsClosedWindows(t *testing.T) {
	store := &fakeWindowStore{}
	rec := NewDBRecorder(store, time.Minute, nil)
	fixed := time.Date(2026, 7, 4, 10, 0, 0, 0, time.UTC)
	rec.now = func() time.Time { return fixed }

	ctx := context.Background()
	rec.Observe(ctx, "tenant-a", "agent-1", "web.fetch", 1)

	// now is still inside the window (window ends at fixed+1m) — not closed.
	closed := rec.Flush(ctx, fixed.Add(30*time.Second))

	if len(store.upserts) != 1 {
		t.Fatalf("expected the flush to still upsert, got %d calls", len(store.upserts))
	}
	if len(closed) != 0 {
		t.Fatalf("expected no closed windows while window is still open, got %+v", closed)
	}
}

// fakeWindowStoreWithHook lets a test inject a side effect (e.g. a concurrent
// Observe) exactly when UpsertWindows is called — the gap between Flush's
// snapshot and its re-lock to reconcile the buffer.
type fakeWindowStoreWithHook struct {
	fakeWindowStore
	onUpsert func()
}

func (f *fakeWindowStoreWithHook) UpsertWindows(ctx context.Context, tenant string, deltas []db.WindowDelta) error {
	if f.onUpsert != nil {
		f.onUpsert()
	}
	return f.fakeWindowStore.UpsertWindows(ctx, tenant, deltas)
}

func TestDBRecorder_Flush_SurvivesConcurrentObserveInGap(t *testing.T) {
	store := &fakeWindowStoreWithHook{}
	rec := NewDBRecorder(store, time.Minute, nil)
	fixed := time.Date(2026, 7, 4, 10, 0, 0, 0, time.UTC)
	rec.now = func() time.Time { return fixed }

	ctx := context.Background()
	rec.Observe(ctx, "tenant-a", "agent-1", "web.fetch", 1)

	// Simulate a concurrent Observe landing in the snapshot→re-lock gap.
	store.onUpsert = func() {
		rec.Observe(ctx, "tenant-a", "agent-1", "web.fetch", 100)
	}

	closed := rec.Flush(ctx, fixed.Add(time.Minute))
	if len(store.upserts) != 1 || store.upserts[0][0].Invocations != 1 {
		t.Fatalf("first flush should have upserted only the pre-gap observation: %+v", store.upserts)
	}
	if len(closed) != 1 {
		// windowStart+size <= now, so the window is genuinely over even
		// though a residual (the gap Observe) still sits in the buffer.
		t.Fatalf("expected the elapsed window to be reported closed, got %+v", closed)
	}

	// The gap-Observe's increment must not have been lost: a second flush
	// should pick it up rather than have it silently deleted.
	store.onUpsert = nil
	closed = rec.Flush(ctx, fixed.Add(time.Minute))
	if len(store.upserts) != 2 {
		t.Fatalf("expected a second upsert call, got %d", len(store.upserts))
	}
	second := store.upserts[1]
	if len(second) != 1 || second[0].Invocations != 1 || second[0].CostUnits != 100 {
		t.Fatalf("gap observation was lost instead of carried into the next flush: %+v", second)
	}
	if len(closed) != 1 {
		t.Fatalf("expected the window to close once its residual is flushed: %+v", closed)
	}
}

func TestDBRecorder_Flush_RetainsBufferOnError(t *testing.T) {
	store := &fakeWindowStore{err: errors.New("db unavailable")}
	rec := NewDBRecorder(store, time.Minute, nil)
	fixed := time.Date(2026, 7, 4, 10, 0, 0, 0, time.UTC)
	rec.now = func() time.Time { return fixed }

	ctx := context.Background()
	rec.Observe(ctx, "tenant-a", "agent-1", "web.fetch", 1)

	closed := rec.Flush(ctx, fixed.Add(time.Minute))
	if len(closed) != 0 {
		t.Fatalf("expected no closed windows on flush error, got %+v", closed)
	}

	// Retry with a working store — the buffered observation must still be there.
	store.err = nil
	closed = rec.Flush(ctx, fixed.Add(time.Minute))
	if len(store.upserts) != 1 {
		t.Fatalf("expected exactly one successful upsert after retry, got %d", len(store.upserts))
	}
	if len(store.upserts[0]) != 1 || store.upserts[0][0].Invocations != 1 {
		t.Fatalf("retained buffer should still hold the original observation: %+v", store.upserts[0])
	}
	if len(closed) != 1 {
		t.Fatalf("expected the retained window to close on successful retry, got %+v", closed)
	}
}

// raceWindowStore is a thread-safe windowStore fake that accumulates every
// upserted delta, additive per tool — the shape UpsertWindows itself
// guarantees on the real DB, so this fake's job is only to prove DBRecorder
// never hands it a lost or duplicated delta.
type raceWindowStore struct {
	mu          sync.Mutex
	invocations map[string]int64
	costUnits   map[string]int64
}

func newRaceWindowStore() *raceWindowStore {
	return &raceWindowStore{invocations: map[string]int64{}, costUnits: map[string]int64{}}
}

func (s *raceWindowStore) UpsertWindows(_ context.Context, _ string, deltas []db.WindowDelta) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range deltas {
		s.invocations[d.ToolID] += d.Invocations
		s.costUnits[d.ToolID] += d.CostUnits
	}
	return nil
}

func (s *raceWindowStore) totals() (invocations, cost int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range s.invocations {
		invocations += v
	}
	for _, v := range s.costUnits {
		cost += v
	}
	return invocations, cost
}

// TestDBRecorder_ConcurrentObserveAndFlush_Conservation proves the
// snapshot→unlock→UpsertWindows→relock sequence in Flush loses no updates and
// double-counts none under genuine concurrency — many goroutines calling
// Observe while a separate goroutine repeatedly calls Flush with a
// far-future now (so every window it sees is already "closed"), the real
// production shape (ticker flushing while InvokeTool request-goroutines
// observe). Run with -race.
func TestDBRecorder_ConcurrentObserveAndFlush_Conservation(t *testing.T) {
	store := newRaceWindowStore()
	rec := NewDBRecorder(store, time.Minute, nil)
	fixed := time.Date(2026, 7, 4, 10, 0, 0, 0, time.UTC)
	rec.now = func() time.Time { return fixed }
	farFuture := fixed.Add(24 * time.Hour)

	const numGoroutines = 50
	const opsPerGoroutine = 100
	tools := []string{"web.fetch", "doc.write", "email.draft"}

	var observers sync.WaitGroup
	observers.Add(numGoroutines)
	flushDone := make(chan struct{})
	var flusher sync.WaitGroup
	flusher.Add(1)
	go func() {
		defer flusher.Done()
		ctx := context.Background()
		for {
			select {
			case <-flushDone:
				return
			default:
				rec.Flush(ctx, farFuture)
			}
		}
	}()

	for i := 0; i < numGoroutines; i++ {
		go func() {
			defer observers.Done()
			ctx := context.Background()
			for j := 0; j < opsPerGoroutine; j++ {
				rec.Observe(ctx, "tenant-a", "agent-1", tools[j%len(tools)], 1)
			}
		}()
	}

	observers.Wait()
	close(flushDone)
	flusher.Wait()

	// Final flush catches whatever the flusher's last iteration missed.
	rec.Flush(context.Background(), farFuture)

	wantInvocations := int64(numGoroutines * opsPerGoroutine)
	gotInvocations, gotCost := store.totals()
	if gotInvocations != wantInvocations {
		t.Fatalf("conservation violated: total invocations captured = %d, want %d (N*ops, no lost/duplicate updates)",
			gotInvocations, wantInvocations)
	}
	if gotCost != wantInvocations {
		t.Fatalf("conservation violated: total cost captured = %d, want %d (cost=1 per Observe)",
			gotCost, wantInvocations)
	}
}
