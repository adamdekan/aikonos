package alerting

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/adamdekan/aikonos/broker/internal/notify"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	"go.uber.org/zap"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// fakeAlertRepo captures persisted alerts.
type fakeAlertRepo struct {
	mu     sync.Mutex
	stored []StoredAlert
}

func (f *fakeAlertRepo) Insert(ctx context.Context, a StoredAlert) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stored = append(f.stored, a)
	return nil
}

func (f *fakeAlertRepo) List(ctx context.Context, tenantID string, limit int) ([]StoredAlert, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []StoredAlert
	for _, a := range f.stored {
		if a.TenantID == tenantID {
			out = append(out, a)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}

func (f *fakeAlertRepo) all() []StoredAlert {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make([]StoredAlert, len(f.stored))
	copy(cp, f.stored)
	return cp
}

func TestEngine_PersistsAlertViaRepo(t *testing.T) {
	bus := notify.NewMemoryBus()
	subject := "aikonos.audit.t1"

	ev := NewEvaluator(Config{DenyRateThreshold: 0})
	fn := &fakeNotifier{}
	repo := &fakeAlertRepo{}
	log := zap.NewNop()

	eng, err := NewEngineWithRepo(bus, subject, ev, fn, repo, log)
	if err != nil {
		t.Fatalf("NewEngineWithRepo: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	eng.Start(ctx)
	defer eng.Close()

	denyEv := &auditv1.AuditEvent{
		EventId:     "evt-persist",
		TenantId:    "t1",
		EventType:   "tool.invoke",
		ResourceRef: "task/1",
		Decision:    auditv1.PolicyDecision_DENY,
		OccurredAt:  timestamppb.Now(),
	}
	data, _ := json.Marshal(denyEv)

	if err := bus.Publish(context.Background(), subject, data); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	eventually(t, time.Second, func() bool {
		return len(repo.all()) >= 1
	})

	stored := repo.all()
	if len(stored) < 1 {
		t.Fatalf("want ≥1 stored alert, got %d", len(stored))
	}
	if stored[0].TenantID != "t1" {
		t.Errorf("stored tenant: want t1, got %s", stored[0].TenantID)
	}
	if stored[0].Rule != "denial" {
		t.Errorf("stored rule: want denial, got %s", stored[0].Rule)
	}
	if stored[0].ID == "" {
		t.Error("stored alert ID must not be empty")
	}
}

func TestEngine_NilRepoNoPanic(t *testing.T) {
	bus := notify.NewMemoryBus()
	subject := "aikonos.audit.t1"

	ev := NewEvaluator(Config{DenyRateThreshold: 0})
	fn := &fakeNotifier{}
	log := zap.NewNop()

	// nil repo — must not panic, must still notify
	eng, err := NewEngineWithRepo(bus, subject, ev, fn, nil, log)
	if err != nil {
		t.Fatalf("NewEngineWithRepo: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	eng.Start(ctx)
	defer eng.Close()

	denyEv := &auditv1.AuditEvent{
		EventId:     "evt-nilrepo",
		TenantId:    "t1",
		EventType:   "tool.invoke",
		ResourceRef: "task/1",
		Decision:    auditv1.PolicyDecision_DENY,
		OccurredAt:  timestamppb.Now(),
	}
	data, _ := json.Marshal(denyEv)
	if err := bus.Publish(context.Background(), subject, data); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	eventually(t, time.Second, func() bool {
		return len(fn.received()) >= 1
	})
}
