package workflowsvc

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"

	"github.com/adamdekan/aikonos/broker/internal/db"
)

// errStubNotOverridden is returned by the stub's row-fetching methods
// (GetCurrent / GetVersion) when a test calls one without overriding it. Those
// two methods return a pointer that production callers deref unconditionally
// on a nil error, so — unlike the other methods here — a bare zero value
// ((*db.WorkflowRow)(nil), nil) would segfault the caller instead of failing
// safely. Errors are the honest "zero value" for this pair.
//
// Duplicated from broker/internal/broker/workflow_store_stub_test.go
// (workflowsvc-extraction CP2): broker's wrapper-level tests still need their
// own copy satisfying workflowStore, so this copy satisfies workflowsvc.Store
// for the core-level tests that moved here.
var errStubNotOverridden = errors.New("stubWorkflowStore: method not overridden by this test")

// stubWorkflowStore implements the full Store interface with zero-value
// returns (no error) for every method. Test-specific fakes embed this by
// value and override only the methods they exercise.
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

var _ Store = stubWorkflowStore{}

// ── shared test constants/helpers ────────────────────────────────────────────
//
// Duplicated from broker/internal/broker/service_workflows_test.go: the
// broker-package wrapper-level tests still reference these by the same names,
// so per  CP2 ("duplication is acceptable,
// weakening an assertion is not") this is a deliberate duplicate, not a move.

const (
	testWFTenant = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	testWFOwner  = "alice@example.com"
	// testWFTenantObject is the FGA tenant object List passes into
	// MayOperateAgent (F9); production builds it as "tenant:" + Deps.TenantID.
	testWFTenantObject = "tenant:aikonos-dev"
)

// minimalValidWorkflowJSON returns a schema-valid Workflow JSON string.
func minimalValidWorkflowJSON() string {
	m := map[string]any{
		"apiVersion": "aikonos.com/v1",
		"kind":       "Workflow",
		"metadata": map[string]any{
			"name": "test-workflow",
			"visibility": map[string]any{
				"kind": "private",
			},
		},
		"steps": []any{
			map[string]any{"skill": "doc.read"},
		},
	}
	b, _ := json.Marshal(m)
	return string(b)
}
