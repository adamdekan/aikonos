package broker

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/workflowsvc"
)

// errStubNotOverridden is returned by the stub's row-fetching methods
// (GetCurrent / GetVersion) when a test calls one without overriding it. Those
// two methods return a pointer that production callers deref unconditionally
// on a nil error, so — unlike the other methods here — a bare zero value
// ((*db.WorkflowRow)(nil), nil) would segfault the caller instead of failing
// safely. Errors are the honest "zero value" for this pair.
var errStubNotOverridden = errors.New("stubWorkflowStore: method not overridden by this test")

// stubWorkflowStore implements the full workflowStore interface with
// zero-value returns (no error) for every method. Test-specific fakes embed
// this by value and override only the methods they exercise — the CP1
// collapse (fable-rpc-twins) replaced the 11 per-op Deps test-override fields
// with a single Deps.Workflows field, so a fake now has to satisfy the whole
// interface even when a given test only cares about one or two operations.
//
// Zero-value (not panic) is deliberate: because Deps.Workflows is now one
// field shared by every operation on a service instance, a handler path can
// legitimately reach a method the test didn't intend to exercise (e.g.
// RateWorkflowRun's non-fatal MarkSuccessRated stamp during a propose-focused
// test). Panicking there would fail tests for reasons unrelated to what they
// assert; a no-op zero-value return keeps them silently inert instead.
type stubWorkflowStore struct{}

func (stubWorkflowStore) GetCurrent(ctx context.Context, tenant string, lineageID uuid.UUID) (*db.WorkflowRow, error) {
	return nil, errStubNotOverridden
}

func (stubWorkflowStore) CreateVersion(ctx context.Context, tenant string, row db.WorkflowRow) (db.WorkflowRow, error) {
	return db.WorkflowRow{}, nil
}

func (stubWorkflowStore) ProposeVersion(ctx context.Context, tenant string, row db.WorkflowRow) (db.WorkflowRow, error) {
	return db.WorkflowRow{}, nil
}

func (stubWorkflowStore) ApproveVersion(ctx context.Context, tenant string, lineageID uuid.UUID, version int) error {
	return nil
}

func (stubWorkflowStore) RejectVersion(ctx context.Context, tenant string, lineageID uuid.UUID, version int, reason string) error {
	return nil
}

func (stubWorkflowStore) GetVersion(ctx context.Context, tenant string, lineageID uuid.UUID, version int) (*db.WorkflowRow, error) {
	return nil, errStubNotOverridden
}

func (stubWorkflowStore) MarkSuccessRated(ctx context.Context, tenant string, lineageID uuid.UUID, version int) error {
	return nil
}

func (stubWorkflowStore) PublishVersion(ctx context.Context, tenant string, lineageID uuid.UUID, version int, groups []string, definitionJSON []byte) error {
	return nil
}

func (stubWorkflowStore) SetVersionPin(ctx context.Context, tenant, userID string, lineageID uuid.UUID, version int) error {
	return nil
}

func (stubWorkflowStore) ClearVersionPin(ctx context.Context, tenant, userID string, lineageID uuid.UUID) error {
	return nil
}

func (stubWorkflowStore) ResolveVersionForUser(ctx context.Context, tenant, userID string, lineageID uuid.UUID) (int, error) {
	return 0, nil
}

func (stubWorkflowStore) DeleteLineage(ctx context.Context, tenant string, lineageID uuid.UUID) (int, error) {
	return 0, nil
}

func (stubWorkflowStore) ListByOwner(ctx context.Context, tenant, ownerUserID, afterLineage string, limit int) ([]*db.WorkflowRow, error) {
	return nil, nil
}

func (stubWorkflowStore) ListVisibleShared(ctx context.Context, tenant string, groups []string, afterLineage string, limit int) ([]*db.WorkflowRow, error) {
	return nil, nil
}

func (stubWorkflowStore) ListVersions(ctx context.Context, tenant string, lineageID uuid.UUID, beforeVersion, limit int) ([]*db.WorkflowRow, error) {
	return nil, nil
}

var _ workflowsvc.Store = stubWorkflowStore{}
