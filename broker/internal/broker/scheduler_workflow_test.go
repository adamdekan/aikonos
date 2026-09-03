package broker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/db"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// fakeWorkflowStoreForSchedule is a minimal workflowsvc.Store fake for the
// scheduler's workflow-mode validation path (ResolveVersionForUser +
// GetVersion only; the rest via the embedded stub).
type fakeWorkflowStoreForSchedule struct {
	stubWorkflowStore
	version    int
	resolveErr error
	row        *db.WorkflowRow
	getErr     error
}

func (f *fakeWorkflowStoreForSchedule) ResolveVersionForUser(_ context.Context, _, _ string, _ uuid.UUID) (int, error) {
	if f.resolveErr != nil {
		return 0, f.resolveErr
	}
	return f.version, nil
}

func (f *fakeWorkflowStoreForSchedule) GetVersion(_ context.Context, _ string, _ uuid.UUID, _ int) (*db.WorkflowRow, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.row, nil
}

// captureScheduledStore records the ScheduledRun passed to Create/Update and
// serves it back on Get, so RPC-level tests can assert on the persisted
// definition. Satisfies scheduledRunStore via the embedded stub.
type captureScheduledStore struct {
	stubScheduledStore
	created *db.ScheduledRun
	updated *db.ScheduledRun
	get     *db.ScheduledRun // seeds Get for update tests
}

func (f *captureScheduledStore) Create(_ context.Context, s *db.ScheduledRun) error {
	f.created = s
	return nil
}

func (f *captureScheduledStore) Update(_ context.Context, _, _ string, s *db.ScheduledRun) error {
	f.updated = s
	return nil
}

func (f *captureScheduledStore) Get(_ context.Context, _, id string) (*db.ScheduledRun, error) {
	if f.created != nil && f.created.ScheduledRunID.String() == id {
		return f.created, nil
	}
	if f.get != nil {
		return f.get, nil
	}
	return nil, errors.New("not found")
}

// ── validateWorkflowScheduleDef (pure validation) ────────────────────────────

func TestValidateWorkflowScheduleDef(t *testing.T) {
	now := time.Date(2026, 6, 3, 8, 0, 0, 0, time.UTC)
	lineageID := uuid.New()

	t.Run("rejects prompt", func(t *testing.T) {
		store := &fakeWorkflowStoreForSchedule{version: 1, row: &db.WorkflowRow{}}
		_, _, _, _, _, err := validateWorkflowScheduleDef(context.Background(), store, "t", "u",
			"do it", nil, lineageID.String(), nil,
			brokerv1.ScheduleKind_SCHEDULE_KIND_CRON, "0 9 * * *", nil, now, 0)
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("want InvalidArgument, got %v", err)
		}
	})

	t.Run("rejects approved_tools", func(t *testing.T) {
		store := &fakeWorkflowStoreForSchedule{version: 1, row: &db.WorkflowRow{}}
		_, _, _, _, _, err := validateWorkflowScheduleDef(context.Background(), store, "t", "u",
			"", []string{"web.fetch"}, lineageID.String(), nil,
			brokerv1.ScheduleKind_SCHEDULE_KIND_CRON, "0 9 * * *", nil, now, 0)
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("want InvalidArgument, got %v", err)
		}
	})

	t.Run("rejects an unparseable lineage id", func(t *testing.T) {
		store := &fakeWorkflowStoreForSchedule{}
		_, _, _, _, _, err := validateWorkflowScheduleDef(context.Background(), store, "t", "u",
			"", nil, "not-a-uuid", nil,
			brokerv1.ScheduleKind_SCHEDULE_KIND_CRON, "0 9 * * *", nil, now, 0)
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("want InvalidArgument, got %v", err)
		}
	})

	t.Run("rejects an unresolvable lineage", func(t *testing.T) {
		store := &fakeWorkflowStoreForSchedule{resolveErr: errors.New("not found")}
		_, _, _, _, _, err := validateWorkflowScheduleDef(context.Background(), store, "t", "u",
			"", nil, lineageID.String(), nil,
			brokerv1.ScheduleKind_SCHEDULE_KIND_CRON, "0 9 * * *", nil, now, 0)
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("want InvalidArgument, got %v", err)
		}
	})

	t.Run("rejects an agent-bound workflow", func(t *testing.T) {
		agentID := uuid.New()
		store := &fakeWorkflowStoreForSchedule{version: 1, row: &db.WorkflowRow{BoundAgentID: &agentID}}
		_, _, _, _, _, err := validateWorkflowScheduleDef(context.Background(), store, "t", "u",
			"", nil, lineageID.String(), nil,
			brokerv1.ScheduleKind_SCHEDULE_KIND_CRON, "0 9 * * *", nil, now, 0)
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("want InvalidArgument for an agent-bound lineage, got %v", err)
		}
	})

	t.Run("accepts a valid unbound workflow", func(t *testing.T) {
		store := &fakeWorkflowStoreForSchedule{version: 1, row: &db.WorkflowRow{}}
		kind, cronExpr, next, gotLineage, inputs, err := validateWorkflowScheduleDef(context.Background(), store, "t", "u",
			"", nil, lineageID.String(), map[string]string{"topic": "foo"},
			brokerv1.ScheduleKind_SCHEDULE_KIND_CRON, "0 9 * * *", nil, now, 0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if kind != db.ScheduleKindCron || cronExpr != "0 9 * * *" {
			t.Fatalf("got kind=%s cron=%s", kind, cronExpr)
		}
		if !next.Equal(time.Date(2026, 6, 3, 9, 0, 0, 0, time.UTC)) {
			t.Fatalf("got next=%v", next)
		}
		if gotLineage != lineageID {
			t.Fatalf("got lineage=%v want %v", gotLineage, lineageID)
		}
		if inputs["topic"] != "foo" {
			t.Fatalf("got inputs=%v", inputs)
		}
	})
}

// ── CreateScheduledRun (workflow mode, RPC level) ────────────────────────────

func TestCreateScheduledRun_WorkflowMode_Success(t *testing.T) {
	deps := testAdminDeps(t, "") // FGA disabled: skill:scheduler / skill:workflows both allow-all
	lineageID := uuid.New()
	deps.Workflows = &fakeWorkflowStoreForSchedule{version: 1, row: &db.WorkflowRow{}}
	store := &captureScheduledStore{}
	deps.Scheduled = store
	svc := NewBrokerService(deps)

	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")
	resp, err := svc.CreateScheduledRun(ctx, &brokerv1.CreateScheduledRunRequest{
		UserId:            "alice@example.com",
		WorkflowLineageId: lineageID.String(),
		WorkflowInputs:    map[string]string{"topic": "foo"},
		Kind:              brokerv1.ScheduleKind_SCHEDULE_KIND_CRON,
		CronExpr:          "0 9 * * *",
	})
	if err != nil {
		t.Fatalf("CreateScheduledRun: %v", err)
	}
	if resp.Run.WorkflowLineageId != lineageID.String() {
		t.Errorf("WorkflowLineageId = %q, want %q", resp.Run.WorkflowLineageId, lineageID.String())
	}
	if resp.Run.Prompt != "" {
		t.Errorf("Prompt should be empty on a workflow schedule, got %q", resp.Run.Prompt)
	}
	if len(resp.Run.ApprovedTools) != 0 {
		t.Errorf("ApprovedTools should be empty on a workflow schedule, got %v", resp.Run.ApprovedTools)
	}
	if store.created == nil || store.created.WorkflowLineageID == nil || *store.created.WorkflowLineageID != lineageID {
		t.Fatalf("stored run does not carry the workflow lineage id: %+v", store.created)
	}
	if store.created.WorkflowInputs["topic"] != "foo" {
		t.Errorf("stored run workflow_inputs = %v", store.created.WorkflowInputs)
	}
}

func TestCreateScheduledRun_WorkflowMode_RejectsApprovedTools(t *testing.T) {
	deps := testAdminDeps(t, "")
	lineageID := uuid.New()
	deps.Workflows = &fakeWorkflowStoreForSchedule{version: 1, row: &db.WorkflowRow{}}
	deps.Scheduled = &captureScheduledStore{}
	svc := NewBrokerService(deps)

	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")
	_, err := svc.CreateScheduledRun(ctx, &brokerv1.CreateScheduledRunRequest{
		UserId:            "alice@example.com",
		WorkflowLineageId: lineageID.String(),
		ApprovedTools:     []string{"web.fetch"},
		Kind:              brokerv1.ScheduleKind_SCHEDULE_KIND_CRON,
		CronExpr:          "0 9 * * *",
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument for approved_tools on a workflow schedule, got %v", err)
	}
}

func TestCreateScheduledRun_WorkflowMode_RejectsAgentBound(t *testing.T) {
	deps := testAdminDeps(t, "")
	lineageID := uuid.New()
	agentID := uuid.New()
	deps.Workflows = &fakeWorkflowStoreForSchedule{version: 1, row: &db.WorkflowRow{BoundAgentID: &agentID}}
	deps.Scheduled = &captureScheduledStore{}
	svc := NewBrokerService(deps)

	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")
	_, err := svc.CreateScheduledRun(ctx, &brokerv1.CreateScheduledRunRequest{
		UserId:            "alice@example.com",
		WorkflowLineageId: lineageID.String(),
		Kind:              brokerv1.ScheduleKind_SCHEDULE_KIND_CRON,
		CronExpr:          "0 9 * * *",
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument for an agent-bound lineage, got %v", err)
	}
}

// Workflow resolution re-checks skill:workflows in addition to skill:scheduler:
// a caller granted only the scheduler skill must still be denied.
func TestCreateScheduledRun_WorkflowMode_RequiresWorkflowsSkill(t *testing.T) {
	f := &fakeFGA{checks: map[string]bool{
		"user:alice@example.com|can_invoke|skill:scheduler": true,
		"user:alice@example.com|can_invoke|skill:workflows": false,
	}}
	srv := f.server(t)
	defer srv.Close()
	deps := testAdminDeps(t, srv.URL)
	lineageID := uuid.New()
	deps.Workflows = &fakeWorkflowStoreForSchedule{version: 1, row: &db.WorkflowRow{}}
	deps.Scheduled = &captureScheduledStore{}
	svc := NewBrokerService(deps)

	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")
	_, err := svc.CreateScheduledRun(ctx, &brokerv1.CreateScheduledRunRequest{
		UserId:            "alice@example.com",
		WorkflowLineageId: lineageID.String(),
		Kind:              brokerv1.ScheduleKind_SCHEDULE_KIND_CRON,
		CronExpr:          "0 9 * * *",
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("want PermissionDenied without skill:workflows, got %v", err)
	}
}

// ── UpdateScheduledRun (workflow mode full-edit branch) ──────────────────────

func TestUpdateScheduledRun_WorkflowMode_TimingOnlyAccepted(t *testing.T) {
	deps := testAdminDeps(t, "")
	lineageID := uuid.New()
	runID := uuid.New()
	existing := &db.ScheduledRun{
		ScheduledRunID:    runID,
		OwnerUserID:       "alice@example.com",
		Kind:              db.ScheduleKindCron,
		CronExpr:          "0 9 * * *",
		WorkflowLineageID: &lineageID,
		WorkflowInputs:    map[string]string{"topic": "foo"},
	}
	store := &captureScheduledStore{get: existing}
	deps.Scheduled = store
	svc := NewBrokerService(deps)

	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")
	future := timestamppb.New(time.Now().Add(48 * time.Hour))
	_, err := svc.UpdateScheduledRun(ctx, &brokerv1.UpdateScheduledRunRequest{
		UserId: "alice@example.com",
		Id:     runID.String(),
		Kind:   brokerv1.ScheduleKind_SCHEDULE_KIND_ONCE,
		RunAt:  future,
	})
	if err != nil {
		t.Fatalf("timing-only edit on a workflow schedule should succeed: %v", err)
	}
	if store.updated == nil {
		t.Fatal("expected Update to be called")
	}
	if store.updated.WorkflowLineageID == nil || *store.updated.WorkflowLineageID != lineageID {
		t.Errorf("workflow binding should be preserved, got %+v", store.updated.WorkflowLineageID)
	}
	if store.updated.WorkflowInputs["topic"] != "foo" {
		t.Errorf("workflow inputs should be preserved, got %v", store.updated.WorkflowInputs)
	}
}

func TestUpdateScheduledRun_WorkflowMode_RejectsPromptChange(t *testing.T) {
	deps := testAdminDeps(t, "")
	lineageID := uuid.New()
	runID := uuid.New()
	existing := &db.ScheduledRun{
		ScheduledRunID:    runID,
		OwnerUserID:       "alice@example.com",
		Kind:              db.ScheduleKindCron,
		CronExpr:          "0 9 * * *",
		WorkflowLineageID: &lineageID,
	}
	deps.Scheduled = &captureScheduledStore{get: existing}
	svc := NewBrokerService(deps)

	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")
	_, err := svc.UpdateScheduledRun(ctx, &brokerv1.UpdateScheduledRunRequest{
		UserId:   "alice@example.com",
		Id:       runID.String(),
		Prompt:   "sneak in a prompt",
		Kind:     brokerv1.ScheduleKind_SCHEDULE_KIND_CRON,
		CronExpr: "0 9 * * *",
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument for a prompt change on a workflow schedule, got %v", err)
	}
}

func TestUpdateScheduledRun_WorkflowMode_RejectsBindingChange(t *testing.T) {
	deps := testAdminDeps(t, "")
	lineageID := uuid.New()
	otherLineageID := uuid.New()
	runID := uuid.New()
	existing := &db.ScheduledRun{
		ScheduledRunID:    runID,
		OwnerUserID:       "alice@example.com",
		Kind:              db.ScheduleKindCron,
		CronExpr:          "0 9 * * *",
		WorkflowLineageID: &lineageID,
	}
	deps.Scheduled = &captureScheduledStore{get: existing}
	svc := NewBrokerService(deps)

	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")
	_, err := svc.UpdateScheduledRun(ctx, &brokerv1.UpdateScheduledRunRequest{
		UserId:            "alice@example.com",
		Id:                runID.String(),
		WorkflowLineageId: otherLineageID.String(),
		Kind:              brokerv1.ScheduleKind_SCHEDULE_KIND_CRON,
		CronExpr:          "0 9 * * *",
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument for a binding change, got %v", err)
	}
}

// A prompt-mode row cannot be switched into a workflow-mode row on edit either
// — the binding is create-time-only in both directions.
func TestUpdateScheduledRun_PromptMode_RejectsWorkflowBind(t *testing.T) {
	deps := testAdminDeps(t, "")
	runID := uuid.New()
	existing := &db.ScheduledRun{
		ScheduledRunID: runID,
		OwnerUserID:    "alice@example.com",
		Prompt:         "do the thing",
		Kind:           db.ScheduleKindCron,
		CronExpr:       "0 9 * * *",
		ApprovedTools:  []string{},
	}
	deps.Scheduled = &captureScheduledStore{get: existing}
	svc := NewBrokerService(deps)

	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")
	_, err := svc.UpdateScheduledRun(ctx, &brokerv1.UpdateScheduledRunRequest{
		UserId:            "alice@example.com",
		Id:                runID.String(),
		Prompt:            "do the thing",
		Kind:              brokerv1.ScheduleKind_SCHEDULE_KIND_CRON,
		CronExpr:          "0 9 * * *",
		WorkflowLineageId: uuid.New().String(),
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument binding a prompt schedule to a workflow on edit, got %v", err)
	}
}

// ── ClaimDueScheduledRuns carries workflow fields (SC4) ──────────────────────

func TestClaimDueScheduledRuns_CarriesWorkflowFields(t *testing.T) {
	runID := uuid.New()
	lineageID := uuid.New()
	claimStore := &fakeClaimDueStore{
		runs: []*db.ScheduledRun{
			{
				ScheduledRunID:    runID,
				OwnerUserID:       "alice@example.com",
				WorkflowLineageID: &lineageID,
				WorkflowInputs:    map[string]string{"topic": "foo"},
			},
		},
	}
	em, err := audit.NewEmitter(context.Background(), audit.Config{})
	if err != nil {
		t.Fatal(err)
	}
	deps := Deps{
		Logger:          zap.NewNop(),
		Audit:           em,
		GatewaySpiffeID: testGWSpiffeID,
		GatewayGrantKey: testGrantKey,
		GatewayGrantTTL: testGrantTTL,
		Scheduled:       claimStore,
	}
	svc := NewSandboxService(deps)

	resp, err := svc.ClaimDueScheduledRuns(
		gatewayCtx(testGWSpiffeID),
		&brokerv1.ClaimDueScheduledRunsRequest{TenantId: testTenantUUID},
	)
	if err != nil {
		t.Fatalf("ClaimDueScheduledRuns: %v", err)
	}
	if len(resp.Runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(resp.Runs))
	}
	got := resp.Runs[0]
	if got.WorkflowLineageId != lineageID.String() {
		t.Errorf("WorkflowLineageId = %q, want %q", got.WorkflowLineageId, lineageID.String())
	}
	if got.WorkflowInputs["topic"] != "foo" {
		t.Errorf("WorkflowInputs = %v", got.WorkflowInputs)
	}
}

// ── ListScheduledRuns surfaces the joined workflow display name (SC9) ───────

func TestListScheduledRuns_SurfacesWorkflowDisplayName(t *testing.T) {
	lineageID := uuid.New()
	listStore := &fakeListWithNameStore{
		runs: []*db.ScheduledRun{
			{
				ScheduledRunID:      uuid.New(),
				OwnerUserID:         "alice@example.com",
				WorkflowLineageID:   &lineageID,
				WorkflowDisplayName: "Weekly report",
			},
		},
	}
	deps := testAdminDeps(t, "")
	deps.Scheduled = listStore
	svc := NewBrokerService(deps)

	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")
	resp, err := svc.ListScheduledRuns(ctx, &brokerv1.ListScheduledRunsRequest{UserId: "alice@example.com"})
	if err != nil {
		t.Fatalf("ListScheduledRuns: %v", err)
	}
	if len(resp.Runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(resp.Runs))
	}
	if resp.Runs[0].WorkflowDisplayName != "Weekly report" {
		t.Errorf("WorkflowDisplayName = %q, want %q", resp.Runs[0].WorkflowDisplayName, "Weekly report")
	}
}

// fakeListWithNameStore returns a canned list for List() only (rest via the
// embedded stub) — proves ListScheduledRuns copies WorkflowDisplayName onto
// the proto response.
type fakeListWithNameStore struct {
	stubScheduledStore
	runs []*db.ScheduledRun
}

func (f *fakeListWithNameStore) List(_ context.Context, _, _ string) ([]*db.ScheduledRun, error) {
	return f.runs, nil
}
