package broker

import (
	"context"
	"time"

	"github.com/adamdekan/aikonos/broker/internal/db"
)

// stubScheduledStore implements the full scheduledRunStore interface with
// zero-value returns (no error) for every method. Test-specific fakes embed
// this by value and override only the methods they exercise — the CP3
// collapse (rpc-twins-tails) replaced the two per-op Deps test-override
// fields (claimDue, scheduledReport) with a single Deps.Scheduled field, so a
// fake now has to satisfy the whole interface even when a given test only
// cares about one or two operations. Mirrors stubTaskStore
// (task_store_stub_test.go).
type stubScheduledStore struct{}

func (stubScheduledStore) Create(_ context.Context, _ *db.ScheduledRun) error {
	return nil
}

func (stubScheduledStore) Get(_ context.Context, _, _ string) (*db.ScheduledRun, error) {
	return nil, nil
}

func (stubScheduledStore) List(_ context.Context, _, _ string) ([]*db.ScheduledRun, error) {
	return nil, nil
}

func (stubScheduledStore) Update(_ context.Context, _, _ string, _ *db.ScheduledRun) error {
	return nil
}

func (stubScheduledStore) SetState(_ context.Context, _, _ string, _ db.ScheduledRunState, _ *time.Time) error {
	return nil
}

func (stubScheduledStore) Delete(_ context.Context, _, _ string) error {
	return nil
}

func (stubScheduledStore) ClaimDue(_ context.Context, _ string, _ time.Time, _ int) ([]*db.ScheduledRun, error) {
	return nil, nil
}

func (stubScheduledStore) ReportResult(_ context.Context, _, _ string, _ bool, _ string) error {
	return nil
}

var _ scheduledRunStore = stubScheduledStore{}
