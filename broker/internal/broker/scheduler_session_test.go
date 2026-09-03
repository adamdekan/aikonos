package broker

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/auth"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/workspacefs"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// fakeReportStore is a minimal fake for the scheduler-report path.
// Satisfies scheduledRunStore (ReportResult/Get only; the rest via the
// embedded stub).
type fakeReportStore struct {
	stubScheduledStore
	runs map[string]*db.ScheduledRun // keyed by run-id string
}

func (f *fakeReportStore) ReportResult(_ context.Context, _, _ string, _ bool, _ string) error {
	return nil
}

func (f *fakeReportStore) Get(_ context.Context, _, id string) (*db.ScheduledRun, error) {
	if r, ok := f.runs[id]; ok {
		return r, nil
	}
	return nil, errFakeRunNotFound
}

var errFakeRunNotFound = &fakeRunNotFoundErr{}

type fakeRunNotFoundErr struct{}

func (e *fakeRunNotFoundErr) Error() string { return "run not found" }

// sandboxSvcForSessionTest builds a SandboxService with an in-memory scheduled
// repo and a real workspacefs.Store under wsRoot.
func sandboxSvcForSessionTest(t *testing.T, repo *fakeReportStore, wsRoot string) *SandboxService {
	t.Helper()
	em, err := audit.NewEmitter(context.Background(), audit.Config{})
	if err != nil {
		t.Fatal(err)
	}
	return NewSandboxService(Deps{
		Logger:          zap.NewNop(),
		Audit:           em,
		GatewaySpiffeID: testGWSpiffeID,
		Workspace:       WorkspaceDeps{Local: workspacefs.New(wsRoot)},
		Scheduled:       repo,
	})
}

func gwReportCtx() context.Context {
	return auth.WithIdentity(context.Background(), &auth.Identity{SpiffeID: testGWSpiffeID})
}

// TestReportScheduledRunResult_WritesSessionFile asserts that a valid
// session_record + session_id causes the file to be written under the owner's
// workspace at .agent/Sessions/<session_id>.json with bytes == session_record.
func TestReportScheduledRunResult_WritesSessionFile(t *testing.T) {
	runID := uuid.New()
	sessionID := uuid.New()
	owner := "alice@example.com"
	record := []byte(`{"id":"` + sessionID.String() + `","title":"test session"}`)

	repo := &fakeReportStore{
		runs: map[string]*db.ScheduledRun{
			runID.String(): {ScheduledRunID: runID, OwnerUserID: owner},
		},
	}

	wsRoot := t.TempDir()
	svc := sandboxSvcForSessionTest(t, repo, wsRoot)

	_, err := svc.ReportScheduledRunResult(gwReportCtx(), &brokerv1.ReportScheduledRunResultRequest{
		TenantId:      testTenantUUID,
		Id:            runID.String(),
		Success:       true,
		Summary:       "ok",
		SessionId:     sessionID.String(),
		SessionRecord: record,
	})
	if err != nil {
		t.Fatalf("ReportScheduledRunResult: %v", err)
	}

	// Verify via the same Store that the file is readable and the content matches.
	ws := workspacefs.New(wsRoot)
	got, _, readErr := ws.Read(context.Background(), testTenantUUID, owner, ".agent/Sessions/"+sessionID.String()+".json")
	if readErr != nil {
		t.Fatalf("session file unreadable: %v", readErr)
	}
	if string(got) != string(record) {
		t.Fatalf("session file mismatch:\n  got  %q\n  want %q", got, record)
	}
}

// TestReportScheduledRunResult_NoRecordNoWrite verifies that absent
// session_record leaves the RPC successful and writes nothing.
func TestReportScheduledRunResult_NoRecordNoWrite(t *testing.T) {
	runID := uuid.New()
	repo := &fakeReportStore{
		runs: map[string]*db.ScheduledRun{
			runID.String(): {ScheduledRunID: runID, OwnerUserID: "alice@example.com"},
		},
	}

	wsRoot := t.TempDir()
	svc := sandboxSvcForSessionTest(t, repo, wsRoot)

	_, err := svc.ReportScheduledRunResult(gwReportCtx(), &brokerv1.ReportScheduledRunResultRequest{
		TenantId: testTenantUUID,
		Id:       runID.String(),
		Success:  true,
		Summary:  "ok",
		// no SessionId / SessionRecord
	})
	if err != nil {
		t.Fatalf("should succeed without session_record: %v", err)
	}

	// No file should be listed under the workspace.
	ws := workspacefs.New(wsRoot)
	files, listErr := ws.List(context.Background(), testTenantUUID, "alice@example.com")
	if listErr != nil {
		t.Fatalf("list: %v", listErr)
	}
	if len(files) != 0 {
		t.Fatalf("expected 0 files, got %d: %v", len(files), files)
	}
}

// TestReportScheduledRunResult_BadSessionIDSkipsWrite verifies that a non-UUID
// session_id causes the write to be silently skipped; the RPC returns success.
func TestReportScheduledRunResult_BadSessionIDSkipsWrite(t *testing.T) {
	runID := uuid.New()
	repo := &fakeReportStore{
		runs: map[string]*db.ScheduledRun{
			runID.String(): {ScheduledRunID: runID, OwnerUserID: "alice@example.com"},
		},
	}

	wsRoot := t.TempDir()
	svc := sandboxSvcForSessionTest(t, repo, wsRoot)

	_, err := svc.ReportScheduledRunResult(gwReportCtx(), &brokerv1.ReportScheduledRunResultRequest{
		TenantId:      testTenantUUID,
		Id:            runID.String(),
		Success:       true,
		SessionId:     "not-a-uuid",
		SessionRecord: []byte(`{"id":"x"}`),
	})
	if err != nil {
		t.Fatalf("bad session_id must not fail the RPC: %v", err)
	}

	ws := workspacefs.New(wsRoot)
	files, listErr := ws.List(context.Background(), testTenantUUID, "alice@example.com")
	if listErr != nil {
		t.Fatalf("list: %v", listErr)
	}
	if len(files) != 0 {
		t.Fatalf("no file should be written for bad session_id, got %v", files)
	}
}
