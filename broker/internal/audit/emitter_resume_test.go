package audit

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"google.golang.org/protobuf/types/known/timestamppb"

	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
)

// fakeStore satisfies chainStore so loadChainHeads runs against it.
func (f *fakeStore) listLevel(ctx context.Context, prefix string) ([]string, []objMeta, error) {
	if f.listErr != nil {
		return nil, nil, f.listErr
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err // as a MinIO listing does
	}
	seen := map[string]bool{}
	var dirs []string
	var objs []objMeta
	for k := range f.objects {
		rest, ok := strings.CutPrefix(k, prefix)
		if !ok {
			continue
		}
		if i := strings.Index(rest, "/"); i >= 0 {
			if d := prefix + rest[:i+1]; !seen[d] {
				seen[d] = true
				dirs = append(dirs, d)
			}
			continue
		}
		objs = append(objs, objMeta{key: k, lastModified: f.modTime[k]})
	}
	return dirs, objs, nil
}

// put stores data unless putErr is set (the chain hash is recomputed on read).
func (f *fakeStore) put(_ context.Context, key string, data []byte, _ string) error {
	if f.putErr != nil {
		return f.putErr
	}
	if f.objects == nil {
		f.objects = map[string][]byte{}
	}
	f.objects[key] = data
	return f.putLandsErr
}

// chainHash recomputes what put stores as chain-hash metadata.
func (f *fakeStore) chainHash(ctx context.Context, key string) (string, error) {
	data, err := f.get(ctx, key)
	if err != nil {
		return "", err
	}
	ev, err := unmarshalAuditEvent(data)
	if err != nil {
		return "", err
	}
	return eventHash(canonicalBytes(ev)), nil
}

// TestLoadChainHeads_SameMillisecondBurstThenRestart: a concurrent burst
// stamped with one occurred_at lands in the store in arbitrary order with an
// identical LastModified, so "newest object" is a coin toss. A restarted
// emitter must still resume from the true chain tail, and the whole trail,
// including the first post-restart event, must verify as one unforked chain.
func TestLoadChainHeads_SameMillisecondBurstThenRestart(t *testing.T) {
	ctx := context.Background()
	const tenant, n = "t1", 64
	ks := &fakeKeySource{currentKey: "k", currentVersion: 2, byVersion: map[int32]string{2: "k"}}
	at := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	before, _ := NewEmitter(ctx, Config{SigningKeySource: ks})
	evs := make([]*auditv1.AuditEvent, n)
	var wg sync.WaitGroup
	for i := range evs {
		evs[i] = &auditv1.AuditEvent{EventId: fmt.Sprintf("e%03d", i), TenantId: tenant, EventType: "test.event", OccurredAt: timestamppb.New(at)}
		wg.Add(1)
		go func(x *auditv1.AuditEvent) { defer wg.Done(); _ = before.Emit(ctx, x) }(evs[i])
	}
	wg.Wait()
	tail := before.lastHash[tenant]

	store := &fakeStore{objects: map[string][]byte{}, modTime: map[string]time.Time{}}
	for _, x := range evs {
		k := objectKey(x)
		store.objects[k] = mustMarshalEvent(x)
		store.modTime[k] = at.Add(time.Millisecond) // the same millisecond for all
	}
	// Not a tenant: loadChainHeads must skip it.
	store.objects["_policy/2026/03/01/p.json"] = []byte(`{}`)

	after, _ := NewEmitter(ctx, Config{SigningKeySource: ks})
	after.store = store
	after.loadChainHeads(ctx, zap.NewNop())
	if got := after.lastHash[tenant]; got != tail {
		t.Fatalf("resumed head = %q, want the true tail %q", got, tail)
	}
	if _, ok := after.lastHash["_policy"]; ok {
		t.Error("_policy/ must not be resumed as a tenant")
	}

	next := &auditv1.AuditEvent{EventId: "post-restart", TenantId: tenant, EventType: "test.event", OccurredAt: timestamppb.New(at)}
	if err := after.Emit(ctx, next); err != nil {
		t.Fatal(err)
	}
	if rep := VerifyChainVersioned(ctx, append(evs, next), ks); !rep.OK || rep.Total != n+1 {
		t.Fatalf("chain after restart: %+v", rep)
	}
}
