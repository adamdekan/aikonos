package broker

import (
	"context"

	"github.com/google/uuid"

	"github.com/adamdekan/aikonos/broker/internal/db"
)

// stubTaskStore implements the full taskStore interface with zero-value
// returns (no error) for every method. Test-specific fakes embed this by
// value and override only the methods they exercise — the CP2 collapse
// (rpc-twins-tails) replaced the eight per-op Deps test-override fields
// (tasks, approveTasks, createTasks, getTasks, cancelTasks, emitStatusTasks,
// invokeToolTasks, gatewayApproveTasks) with a single Deps.Tasks field, so a
// fake now has to satisfy the whole interface even when a given test only
// cares about one or two operations. Mirrors stubWorkflowStore
// (workflow_store_stub_test.go), the equivalent pattern from the CP1
// workflowsvc.Store collapse.
type stubTaskStore struct{}

func (stubTaskStore) Get(_ context.Context, _, _ string) (*db.Task, error) {
	return nil, nil
}

func (stubTaskStore) Transition(_ context.Context, _, _ string, _, _ db.TaskState) error {
	return nil
}

func (stubTaskStore) Create(_ context.Context, _ *db.Task) error {
	return nil
}

func (stubTaskStore) IncrementCost(_ context.Context, _, _ string, _ int64) error {
	return nil
}

func (stubTaskStore) InsertPlanSteps(_ context.Context, _ string, _ []db.PlanStepRecord) error {
	return nil
}

func (stubTaskStore) CreateApprovalRequest(_ context.Context, _ *db.ApprovalRequest) (uuid.UUID, error) {
	return uuid.UUID{}, nil
}

func (stubTaskStore) ListMintableSteps(_ context.Context, _, _ string) ([]db.ExecutableStep, error) {
	return nil, nil
}

func (stubTaskStore) GetPlanStepTraces(_ context.Context, _, _ string) ([]db.PlanStepTrace, error) {
	return nil, nil
}

func (stubTaskStore) GetPendingApprovalByTask(_ context.Context, _, _ string) (*db.ApprovalRequest, error) {
	return nil, nil
}

func (stubTaskStore) ResolveApproval(_ context.Context, _, _, _ string, _ bool, _ string) (db.ApprovalState, bool, error) {
	return "", false, nil
}

func (stubTaskStore) ListInbox(_ context.Context, _, _ string, _ bool) ([]*db.Envelope, error) {
	return nil, nil
}

func (stubTaskStore) GetEnvelope(_ context.Context, _, _ string) (*db.Envelope, error) {
	return nil, nil
}

func (stubTaskStore) RespondEnvelope(_ context.Context, _, _, _ string, _ bool) (db.EnvelopeState, error) {
	return "", nil
}

func (stubTaskStore) CreateEnvelope(_ context.Context, _ *db.Envelope, _ db.EnvelopeState) (uuid.UUID, error) {
	return uuid.UUID{}, nil
}

func (stubTaskStore) DismissEnvelope(_ context.Context, _, _, _ string) (db.EnvelopeState, error) {
	return "", nil
}

func (stubTaskStore) CountEnvelopesByPayloadRef(_ context.Context, _, _ string) (int, error) {
	return 0, nil
}

var _ taskStore = stubTaskStore{}
