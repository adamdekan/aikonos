package broker

// Tests for Checkpoint 3 (F22): client_request_id idempotency on task
// creation. fakeIdempotentCreateStore mimics migration 031's partial unique
// index on (tenant_id, client_request_id) in-memory, exercising the same
// sentinel contract db.TaskRepo.Create uses: on a duplicate non-empty
// client_request_id, mutate t.TaskID/t.TraceID to the existing row and return
// db.ErrClientRequestIDExists rather than inserting a second row.

import (
	"context"
	"testing"

	"go.uber.org/zap"

	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/policy"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// fakeIdempotentCreateStore satisfies taskStore (Create only; rest via the
// embedded stub) and reproduces the (tenant_id, client_request_id) uniqueness
// contract of migration 031.
type fakeIdempotentCreateStore struct {
	stubTaskStore
	created []*db.Task
	byKey   map[string]*db.Task
}

func (f *fakeIdempotentCreateStore) Create(_ context.Context, t *db.Task) error {
	if t.ClientRequestID != "" {
		key := t.TenantID.String() + "|" + t.ClientRequestID
		if existing, ok := f.byKey[key]; ok {
			t.TaskID = existing.TaskID
			t.TraceID = existing.TraceID
			return db.ErrClientRequestIDExists
		}
		if f.byKey == nil {
			f.byKey = map[string]*db.Task{}
		}
		f.byKey[key] = t
	}
	f.created = append(f.created, t)
	return nil
}

// TestCreateTask_DuplicateClientRequestID_ResolvesToExisting proves that two
// sequential CreateTask calls carrying the same tenant + client_request_id
// resolve to a single stored task, and both responses carry the identical
// task_id — the duplicate call is a success, not an error, and does not
// insert a second row.
func TestCreateTask_DuplicateClientRequestID_ResolvesToExisting(t *testing.T) {
	store := &fakeIdempotentCreateStore{}
	svc := newBrokerSvcForCreateTaskTest(t, store)
	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")

	req := &brokerv1.CreateTaskRequest{
		TenantId:        testTenantUUID,
		UserId:          "alice@example.com",
		Prompt:          "do something idempotently",
		ClientRequestId: "req-123",
	}

	first, err := svc.CreateTask(ctx, req)
	if err != nil {
		t.Fatalf("first CreateTask: unexpected error %v", err)
	}

	second, err := svc.CreateTask(ctx, req)
	if err != nil {
		t.Fatalf("second CreateTask (duplicate client_request_id): unexpected error %v", err)
	}

	if second.TaskId != first.TaskId {
		t.Fatalf("expected identical task_id on replay, got first=%s second=%s", first.TaskId, second.TaskId)
	}
	if len(store.created) != 1 {
		t.Fatalf("expected a single stored task, got %d: %v", len(store.created), store.created)
	}
}

// TestCreateTask_EmptyClientRequestID_CreatesDistinctTasks proves the empty-id
// path is byte-identical to pre-F22 behavior: two calls with no
// client_request_id create two distinct tasks, never deduped.
func TestCreateTask_EmptyClientRequestID_CreatesDistinctTasks(t *testing.T) {
	store := &fakeIdempotentCreateStore{}
	svc := newBrokerSvcForCreateTaskTest(t, store)
	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")

	req := &brokerv1.CreateTaskRequest{
		TenantId: testTenantUUID,
		UserId:   "alice@example.com",
		Prompt:   "do something",
	}

	first, err := svc.CreateTask(ctx, req)
	if err != nil {
		t.Fatalf("first CreateTask: unexpected error %v", err)
	}
	second, err := svc.CreateTask(ctx, req)
	if err != nil {
		t.Fatalf("second CreateTask: unexpected error %v", err)
	}

	if first.TaskId == second.TaskId {
		t.Fatalf("empty client_request_id must never dedupe: got identical task_id %s", first.TaskId)
	}
	if len(store.created) != 2 {
		t.Fatalf("expected two distinct stored tasks, got %d", len(store.created))
	}
}

// TestCreateTask_DuplicateClientRequestID_RepairsRelationsNotAudit proves the
// reviewer-flagged crash-window fix: on the dedup (resolve-to-existing) path,
// persistManagedTask re-runs the FGA owner/approver relation write (a
// check-and-repair for the case where the original call committed the INSERT
// but died before writeTaskRelationsFromDeps ran) but does NOT re-emit the
// audit event or re-publish the CREATED event — those must not be duplicated.
func TestCreateTask_DuplicateClientRequestID_RepairsRelationsNotAudit(t *testing.T) {
	fga := &fakeFGA{}
	srv := fga.server(t)
	t.Cleanup(srv.Close)

	eng, err := policy.NewEngine(context.Background(), policy.Config{
		OPAEndpoint:     "http://unused",
		OpenFGAEndpoint: srv.URL,
		OpenFGAStoreID:  "store-1",
	})
	if err != nil {
		t.Fatalf("policy.NewEngine: %v", err)
	}
	em := &recordingEmitter{}

	store := &fakeIdempotentCreateStore{}
	svc := NewBrokerService(Deps{
		Logger: zap.NewNop(),
		Policy: eng,
		Audit:  em,
		Tasks:  store,
	})
	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")

	req := &brokerv1.CreateTaskRequest{
		TenantId:        testTenantUUID,
		UserId:          "alice@example.com",
		Prompt:          "do something idempotently",
		ClientRequestId: "req-repair-456",
	}

	if _, err := svc.CreateTask(ctx, req); err != nil {
		t.Fatalf("first CreateTask: unexpected error %v", err)
	}
	writesAfterFirst := len(fga.writes)
	auditAfterFirst := em.count()

	if _, err := svc.CreateTask(ctx, req); err != nil {
		t.Fatalf("second CreateTask (duplicate client_request_id): unexpected error %v", err)
	}

	if len(fga.writes) <= writesAfterFirst {
		t.Errorf("expected the dedup path to re-run the FGA relation write (repair), got %d writes before and %d after", writesAfterFirst, len(fga.writes))
	}
	if em.count() != auditAfterFirst {
		t.Errorf("expected the dedup path to NOT re-emit an audit event, got %d events before and %d after", auditAfterFirst, em.count())
	}
}
