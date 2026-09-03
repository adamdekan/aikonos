package workflowsvc

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/db"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// fakeProposeStore serves the current version (for owner verification +
// metadata copy) and records the proposed row so binding inheritance is
// assertable. GetCurrent and ProposeVersion are backed by the same current row.
type fakeProposeStore struct {
	stubWorkflowStore
	current  *db.WorkflowRow
	proposed []db.WorkflowRow
}

func (f *fakeProposeStore) GetCurrent(_ context.Context, _ string, _ uuid.UUID) (*db.WorkflowRow, error) {
	return f.current, nil
}

func (f *fakeProposeStore) ProposeVersion(_ context.Context, _ string, row db.WorkflowRow) (db.WorkflowRow, error) {
	row.Version = f.current.Version + 1
	f.proposed = append(f.proposed, row)
	return row, nil
}

// TestPropose_InheritsBinding: a proposed version inherits the current
// version's agent binding (lineage-immutable, F9).
func TestPropose_InheritsBinding(t *testing.T) {
	lineage := uuid.New()
	agentID := uuid.New()
	store := &fakeProposeStore{current: &db.WorkflowRow{
		ID:               uuid.New(),
		LineageID:        lineage,
		Version:          1,
		TenantID:         uuid.MustParse(testWFTenant),
		OwnerUserID:      testWFOwner,
		Name:             "bound-wf",
		Status:           "private",
		VisibilityKind:   "private",
		VisibilityGroups: json.RawMessage(`[]`),
		Definition:       json.RawMessage(minimalValidWorkflowJSON()),
		ApprovalState:    "approved",
		BoundAgentID:     &agentID,
	}}

	em, _ := audit.NewEmitter(context.Background(), audit.Config{})
	_, err := Propose(
		context.Background(), testWFTenant, testWFOwner,
		&brokerv1.ProposeWorkflowVersionRequest{
			TenantId:       testWFTenant,
			LineageId:      lineage.String(),
			DefinitionJson: minimalValidWorkflowJSON(),
		},
		store, store, nil, nil, em, zap.NewNop(),
	)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if len(store.proposed) != 1 {
		t.Fatalf("want 1 proposed row, got %d", len(store.proposed))
	}
	got := store.proposed[0].BoundAgentID
	if got == nil || *got != agentID {
		t.Errorf("bound_agent_id: want inherited %s, got %v", agentID, got)
	}
}
