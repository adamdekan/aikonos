package broker

// Tests for CP6b: RateWorkflowRun, ProposeWorkflowVersion, DecideWorkflowVersion
// — south (SandboxService, owner from owner_grant) and north (BrokerService,
// owner from callerIdentity).
//
// Pattern mirrors service_workflows_test.go (CP5b). Fake stores satisfy the
// new minimal interfaces; the existing fakeSkillPolicy / fakeFGA / ctxWithSubject
// helpers are reused from skill_gate_test.go / service_workflows_test.go.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/gatewaygrant"
	"github.com/adamdekan/aikonos/broker/internal/policy"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// mintGrantFor mints an owner grant for an arbitrary user (used by non-owner tests).
func mintGrantFor(t *testing.T, tenant, userID string) string {
	t.Helper()
	g, err := gatewaygrant.Mint(testGrantKey, tenant, userID, testGrantTTL)
	if err != nil {
		t.Fatalf("mintGrantFor(%s): %v", userID, err)
	}
	return g
}

// ── fakeWorkflowOwnerStore ────────────────────────────────────────────────────

// fakeWorkflowOwnerStore satisfies GetCurrent for owner-lookup tests. Rows
// keyed by lineageID. Combined with fakeWorkflowProposeStore/fakeWorkflowDecideStore
// via cp6bStore (below) since Deps.Workflows is now a single field (CP1).
type fakeWorkflowOwnerStore struct {
	stubWorkflowStore
	rows map[string]*db.WorkflowRow
}

func newFakeOwnerStore() *fakeWorkflowOwnerStore {
	return &fakeWorkflowOwnerStore{rows: map[string]*db.WorkflowRow{}}
}

func (f *fakeWorkflowOwnerStore) addRow(row db.WorkflowRow) {
	f.rows[row.LineageID.String()] = &row
}

func (f *fakeWorkflowOwnerStore) GetCurrent(_ context.Context, _ string, lineageID uuid.UUID) (*db.WorkflowRow, error) {
	r, ok := f.rows[lineageID.String()]
	if !ok {
		return nil, fmt.Errorf("workflow lineage %s not found", lineageID)
	}
	return r, nil
}

// ── fakeWorkflowProposeStore ──────────────────────────────────────────────────

type fakeWorkflowProposeStore struct {
	stubWorkflowStore
	proposed []db.WorkflowRow
}

func (f *fakeWorkflowProposeStore) ProposeVersion(_ context.Context, _ string, row db.WorkflowRow) (db.WorkflowRow, error) {
	row.ID = uuid.New()
	row.Version = 42 // deterministic fake version number
	row.ApprovalState = "proposed"
	f.proposed = append(f.proposed, row)
	return row, nil
}

// ── fakeWorkflowDecideStore ───────────────────────────────────────────────────

type approveCall struct {
	lineage uuid.UUID
	version int
}
type rejectCall struct {
	lineage uuid.UUID
	version int
	reason  string
}

type fakeWorkflowDecideStore struct {
	stubWorkflowStore
	approved []approveCall
	rejected []rejectCall
}

// ── cp6bStore ─────────────────────────────────────────────────────────────────

// cp6bStore composes the owner/propose/decide fakes above into one object
// satisfying workflowStore — Deps.Workflows is a single field (CP1 of the
// fable-rpc-twins collapse), so a test that previously wired independent
// workflowOwner / workflowPropose / workflowDecide overrides now wires one of
// these instead. Each field is optional; unset ones fall back to the embedded
// stub (never called by the RPC paths exercised in this file, since those
// paths always resolve owner via GetCurrent first).
type cp6bStore struct {
	stubWorkflowStore
	owner   *fakeWorkflowOwnerStore
	propose *fakeWorkflowProposeStore
	decide  *fakeWorkflowDecideStore
}

func (c *cp6bStore) GetCurrent(ctx context.Context, tenant string, lineageID uuid.UUID) (*db.WorkflowRow, error) {
	if c.owner != nil {
		return c.owner.GetCurrent(ctx, tenant, lineageID)
	}
	return c.stubWorkflowStore.GetCurrent(ctx, tenant, lineageID)
}

func (c *cp6bStore) ProposeVersion(ctx context.Context, tenant string, row db.WorkflowRow) (db.WorkflowRow, error) {
	if c.propose != nil {
		return c.propose.ProposeVersion(ctx, tenant, row)
	}
	return c.stubWorkflowStore.ProposeVersion(ctx, tenant, row)
}

func (c *cp6bStore) ApproveVersion(ctx context.Context, tenant string, lineageID uuid.UUID, version int) error {
	if c.decide != nil {
		return c.decide.ApproveVersion(ctx, tenant, lineageID, version)
	}
	return c.stubWorkflowStore.ApproveVersion(ctx, tenant, lineageID, version)
}

func (c *cp6bStore) RejectVersion(ctx context.Context, tenant string, lineageID uuid.UUID, version int, reason string) error {
	if c.decide != nil {
		return c.decide.RejectVersion(ctx, tenant, lineageID, version, reason)
	}
	return c.stubWorkflowStore.RejectVersion(ctx, tenant, lineageID, version, reason)
}

func (f *fakeWorkflowDecideStore) ApproveVersion(_ context.Context, _ string, lineageID uuid.UUID, version int) error {
	f.approved = append(f.approved, approveCall{lineageID, version})
	return nil
}

func (f *fakeWorkflowDecideStore) RejectVersion(_ context.Context, _ string, lineageID uuid.UUID, version int, reason string) error {
	f.rejected = append(f.rejected, rejectCall{lineageID, version, reason})
	return nil
}

// ── fakeCP6bEnvelopeStore ─────────────────────────────────────────────────────

// fakeCP6bEnvelopeStore satisfies taskStore (CreateEnvelope only; the rest
// via the embedded stub) for CP6b propose tests.
type fakeCP6bEnvelopeStore struct {
	stubTaskStore
	created []*db.Envelope
}

func (f *fakeCP6bEnvelopeStore) CreateEnvelope(_ context.Context, e *db.Envelope, _ db.EnvelopeState) (uuid.UUID, error) {
	f.created = append(f.created, e)
	return uuid.New(), nil
}

// ── SandboxService builder for CP6b ──────────────────────────────────────────

func newSandboxSvcForCP6b(
	t *testing.T,
	ownerStore *fakeWorkflowOwnerStore,
	proposeStore *fakeWorkflowProposeStore,
	decideStore *fakeWorkflowDecideStore,
	envStore taskStore,
	allow bool,
) *SandboxService {
	t.Helper()
	em, err := audit.NewEmitter(context.Background(), audit.Config{})
	if err != nil {
		t.Fatalf("audit.NewEmitter: %v", err)
	}
	svc := NewSandboxService(Deps{
		Logger:          zap.NewNop(),
		Audit:           em,
		GatewaySpiffeID: testGWSpiffeID,
		GatewayGrantKey: testGrantKey,
		GatewayGrantTTL: testGrantTTL,
		Workflows:       &cp6bStore{owner: ownerStore, propose: proposeStore, decide: decideStore},
		Tasks:           envStore,
	})
	svc.skillPolicy = &fakeSkillPolicy{enabled: true, granted: allow}
	return svc
}

// ── helpers ───────────────────────────────────────────────────────────────────

const testWFLineage = "11111111-2222-3333-4444-555555555555"

func testLineageUUID(t *testing.T) uuid.UUID {
	t.Helper()
	return uuid.MustParse(testWFLineage)
}

// ownerRow builds a WorkflowRow for the lineage owned by testWFOwner.
func ownerRow(lineageID uuid.UUID) db.WorkflowRow {
	return db.WorkflowRow{
		ID:               uuid.New(),
		LineageID:        lineageID,
		Version:          1,
		TenantID:         uuid.MustParse(testWFTenant),
		OwnerUserID:      testWFOwner,
		Name:             "my-workflow",
		Status:           "private",
		VisibilityKind:   "private",
		VisibilityGroups: []byte("[]"),
		Definition:       []byte(minimalValidWorkflowJSON()),
		ApprovalState:    "approved",
	}
}

// ── RateWorkflowRun tests — south ────────────────────────────────────────────

func TestRateWorkflowRun_SkillDeny_PermissionDenied(t *testing.T) {
	lineageID := testLineageUUID(t)
	os := newFakeOwnerStore()
	os.addRow(ownerRow(lineageID))
	svc := newSandboxSvcForCP6b(t, os, nil, nil, nil, false /* deny */)

	_, err := svc.RateWorkflowRun(gatewayCtxForWorkflow(), &brokerv1.RateWorkflowRunRequest{
		TenantId:    testWFTenant,
		OwnerUserId: testWFOwner,
		OwnerGrant:  mintWFGrant(t),
		LineageId:   lineageID.String(),
		Version:     1,
		Rating:      brokerv1.WorkflowRating_RATING_BAD,
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("skill deny: want PermissionDenied, got %v", err)
	}
}

// TestRateWorkflowRun_NeverProposesAndSucceeds: rate (BAD or SUCCESS) is
// audit-only — it must succeed and must never touch the propose store.
func TestRateWorkflowRun_NeverProposesAndSucceeds(t *testing.T) {
	cases := []struct {
		name   string
		rating brokerv1.WorkflowRating
	}{
		{"BAD", brokerv1.WorkflowRating_RATING_BAD},
		{"SUCCESS", brokerv1.WorkflowRating_RATING_SUCCESS},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lineageID := testLineageUUID(t)
			os := newFakeOwnerStore()
			os.addRow(ownerRow(lineageID))
			// Inject a real proposeStore so we can assert it was never called.
			ps := &fakeWorkflowProposeStore{}
			svc := newSandboxSvcForCP6b(t, os, ps, nil, nil, true /* allow */)

			_, err := svc.RateWorkflowRun(gatewayCtxForWorkflow(), &brokerv1.RateWorkflowRunRequest{
				TenantId:    testWFTenant,
				OwnerUserId: testWFOwner,
				OwnerGrant:  mintWFGrant(t),
				LineageId:   lineageID.String(),
				Version:     1,
				Rating:      tc.rating,
				Note:        "test note",
			})
			if err != nil {
				t.Fatalf("RateWorkflowRun %s: unexpected error: %v", tc.name, err)
			}
			// Invariant: rate never creates a proposal.
			if len(ps.proposed) != 0 {
				t.Fatalf("RateWorkflowRun %s: ProposeVersion must not be called, got %d call(s)", tc.name, len(ps.proposed))
			}
		})
	}
}

// ── ProposeWorkflowVersion tests — south ─────────────────────────────────────

func TestProposeWorkflowVersion_SkillDeny_PermissionDenied(t *testing.T) {
	lineageID := testLineageUUID(t)
	os := newFakeOwnerStore()
	os.addRow(ownerRow(lineageID))
	ps := &fakeWorkflowProposeStore{}
	svc := newSandboxSvcForCP6b(t, os, ps, nil, nil, false /* deny */)

	_, err := svc.ProposeWorkflowVersion(gatewayCtxForWorkflow(), &brokerv1.ProposeWorkflowVersionRequest{
		TenantId:       testWFTenant,
		OwnerUserId:    testWFOwner,
		OwnerGrant:     mintWFGrant(t),
		LineageId:      lineageID.String(),
		DefinitionJson: minimalValidWorkflowJSON(),
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("skill deny: want PermissionDenied, got %v", err)
	}
}

// TestProposeWorkflowVersion_NonOwner_PermissionDenied: caller whose grant is
// valid but resolves to a different user than the lineage owner is rejected.
func TestProposeWorkflowVersion_NonOwner_PermissionDenied(t *testing.T) {
	lineageID := testLineageUUID(t)
	os := newFakeOwnerStore()
	os.addRow(ownerRow(lineageID)) // owned by testWFOwner = "alice@example.com"

	ps := &fakeWorkflowProposeStore{}
	em, _ := audit.NewEmitter(context.Background(), audit.Config{})
	svc := NewSandboxService(Deps{
		Logger:          zap.NewNop(),
		Audit:           em,
		GatewaySpiffeID: testGWSpiffeID,
		GatewayGrantKey: testGrantKey,
		GatewayGrantTTL: time.Hour,
		Workflows:       &cp6bStore{owner: os, propose: ps},
	})
	svc.skillPolicy = &fakeSkillPolicy{enabled: true, granted: true}

	nonOwnerGrant := mintGrantFor(t, testWFTenant, "eve@example.com")
	_, err := svc.ProposeWorkflowVersion(gatewayCtxForWorkflow(), &brokerv1.ProposeWorkflowVersionRequest{
		TenantId:       testWFTenant,
		OwnerUserId:    "eve@example.com",
		OwnerGrant:     nonOwnerGrant,
		LineageId:      lineageID.String(),
		DefinitionJson: minimalValidWorkflowJSON(),
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-owner propose: want PermissionDenied, got %v", err)
	}
	if len(ps.proposed) != 0 {
		t.Fatal("non-owner: ProposeVersion must not have been called")
	}
}

// TestProposeWorkflowVersion_Owner_CreatesProposalAndEnvelope: happy path.
func TestProposeWorkflowVersion_Owner_CreatesProposalAndEnvelope(t *testing.T) {
	lineageID := testLineageUUID(t)
	os := newFakeOwnerStore()
	os.addRow(ownerRow(lineageID))
	ps := &fakeWorkflowProposeStore{}
	env := &fakeCP6bEnvelopeStore{}
	svc := newSandboxSvcForCP6b(t, os, ps, nil, env, true /* allow */)

	resp, err := svc.ProposeWorkflowVersion(gatewayCtxForWorkflow(), &brokerv1.ProposeWorkflowVersionRequest{
		TenantId:       testWFTenant,
		OwnerUserId:    testWFOwner,
		OwnerGrant:     mintWFGrant(t),
		LineageId:      lineageID.String(),
		DefinitionJson: minimalValidWorkflowJSON(),
	})
	if err != nil {
		t.Fatalf("ProposeWorkflowVersion: unexpected error: %v", err)
	}
	if resp.Version != 42 {
		t.Errorf("version: want 42 (fake), got %d", resp.Version)
	}
	if len(ps.proposed) != 1 {
		t.Fatalf("ProposeVersion call count: want 1, got %d", len(ps.proposed))
	}
	if ps.proposed[0].ApprovalState != "proposed" {
		t.Errorf("ApprovalState: want \"proposed\", got %q", ps.proposed[0].ApprovalState)
	}
	if len(env.created) != 1 {
		t.Fatalf("envelope count: want 1, got %d", len(env.created))
	}
}

func TestProposeWorkflowVersion_InvalidJSON_InvalidArgument(t *testing.T) {
	lineageID := testLineageUUID(t)
	os := newFakeOwnerStore()
	os.addRow(ownerRow(lineageID))
	ps := &fakeWorkflowProposeStore{}
	svc := newSandboxSvcForCP6b(t, os, ps, nil, nil, true /* allow */)

	_, err := svc.ProposeWorkflowVersion(gatewayCtxForWorkflow(), &brokerv1.ProposeWorkflowVersionRequest{
		TenantId:       testWFTenant,
		OwnerUserId:    testWFOwner,
		OwnerGrant:     mintWFGrant(t),
		LineageId:      lineageID.String(),
		DefinitionJson: "not-valid-json",
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("invalid JSON: want InvalidArgument, got %v", err)
	}
}

// ── DecideWorkflowVersion tests — south ──────────────────────────────────────

func TestDecideWorkflowVersion_SkillDeny_PermissionDenied(t *testing.T) {
	lineageID := testLineageUUID(t)
	os := newFakeOwnerStore()
	os.addRow(ownerRow(lineageID))
	ds := &fakeWorkflowDecideStore{}
	svc := newSandboxSvcForCP6b(t, os, nil, ds, nil, false /* deny */)

	_, err := svc.DecideWorkflowVersion(gatewayCtxForWorkflow(), &brokerv1.DecideWorkflowVersionRequest{
		TenantId:    testWFTenant,
		OwnerUserId: testWFOwner,
		OwnerGrant:  mintWFGrant(t),
		LineageId:   lineageID.String(),
		Version:     2,
		Approved:    true,
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("skill deny: want PermissionDenied, got %v", err)
	}
}

func TestDecideWorkflowVersion_NonOwner_PermissionDenied(t *testing.T) {
	lineageID := testLineageUUID(t)
	os := newFakeOwnerStore()
	os.addRow(ownerRow(lineageID)) // owned by testWFOwner
	ds := &fakeWorkflowDecideStore{}
	em, _ := audit.NewEmitter(context.Background(), audit.Config{})
	svc := NewSandboxService(Deps{
		Logger:          zap.NewNop(),
		Audit:           em,
		GatewaySpiffeID: testGWSpiffeID,
		GatewayGrantKey: testGrantKey,
		GatewayGrantTTL: time.Hour,
		Workflows:       &cp6bStore{owner: os, decide: ds},
	})
	svc.skillPolicy = &fakeSkillPolicy{enabled: true, granted: true}

	nonOwnerGrant := mintGrantFor(t, testWFTenant, "eve@example.com")
	_, err := svc.DecideWorkflowVersion(gatewayCtxForWorkflow(), &brokerv1.DecideWorkflowVersionRequest{
		TenantId:    testWFTenant,
		OwnerUserId: "eve@example.com",
		OwnerGrant:  nonOwnerGrant,
		LineageId:   lineageID.String(),
		Version:     2,
		Approved:    true,
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-owner decide: want PermissionDenied, got %v", err)
	}
	if len(ds.approved) != 0 || len(ds.rejected) != 0 {
		t.Fatal("non-owner: decide store must not have been called")
	}
}

func TestDecideWorkflowVersion_Approve_CallsApproveVersion(t *testing.T) {
	lineageID := testLineageUUID(t)
	os := newFakeOwnerStore()
	os.addRow(ownerRow(lineageID))
	ds := &fakeWorkflowDecideStore{}
	svc := newSandboxSvcForCP6b(t, os, nil, ds, nil, true /* allow */)

	resp, err := svc.DecideWorkflowVersion(gatewayCtxForWorkflow(), &brokerv1.DecideWorkflowVersionRequest{
		TenantId:    testWFTenant,
		OwnerUserId: testWFOwner,
		OwnerGrant:  mintWFGrant(t),
		LineageId:   lineageID.String(),
		Version:     2,
		Approved:    true,
	})
	if err != nil {
		t.Fatalf("approve: unexpected error: %v", err)
	}
	if resp.ApprovalState != "approved" {
		t.Errorf("ApprovalState: want \"approved\", got %q", resp.ApprovalState)
	}
	if len(ds.approved) != 1 {
		t.Fatalf("ApproveVersion calls: want 1, got %d", len(ds.approved))
	}
	if ds.approved[0].version != 2 {
		t.Errorf("approved version: want 2, got %d", ds.approved[0].version)
	}
}

func TestDecideWorkflowVersion_Reject_CallsRejectVersion(t *testing.T) {
	lineageID := testLineageUUID(t)
	os := newFakeOwnerStore()
	os.addRow(ownerRow(lineageID))
	ds := &fakeWorkflowDecideStore{}
	svc := newSandboxSvcForCP6b(t, os, nil, ds, nil, true /* allow */)

	resp, err := svc.DecideWorkflowVersion(gatewayCtxForWorkflow(), &brokerv1.DecideWorkflowVersionRequest{
		TenantId:    testWFTenant,
		OwnerUserId: testWFOwner,
		OwnerGrant:  mintWFGrant(t),
		LineageId:   lineageID.String(),
		Version:     2,
		Approved:    false,
		Reason:      "plan broke prod",
	})
	if err != nil {
		t.Fatalf("reject: unexpected error: %v", err)
	}
	if resp.ApprovalState != "rejected" {
		t.Errorf("ApprovalState: want \"rejected\", got %q", resp.ApprovalState)
	}
	if len(ds.rejected) != 1 {
		t.Fatalf("RejectVersion calls: want 1, got %d", len(ds.rejected))
	}
	if ds.rejected[0].reason != "plan broke prod" {
		t.Errorf("reason: want %q, got %q", "plan broke prod", ds.rejected[0].reason)
	}
}

// ── North-path (BrokerService — OIDC bearer) tests ───────────────────────────

// newBrokerSvcForCP6b builds a BrokerService with the CP6b stores and FGA allow/deny.
func newBrokerSvcForCP6b(
	t *testing.T,
	ownerStore *fakeWorkflowOwnerStore,
	proposeStore *fakeWorkflowProposeStore,
	decideStore *fakeWorkflowDecideStore,
	envStore taskStore,
	allow bool,
) *BrokerService {
	t.Helper()
	var checks map[string]bool
	if allow {
		checks = map[string]bool{
			"user:" + testWFOwner + "|can_invoke|skill:workflows": true,
		}
	} else {
		checks = map[string]bool{}
	}
	fga := &fakeFGA{checks: checks}
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
	em, err := audit.NewEmitter(context.Background(), audit.Config{})
	if err != nil {
		t.Fatalf("audit.NewEmitter: %v", err)
	}
	return NewBrokerService(Deps{
		Logger:    zap.NewNop(),
		Policy:    eng,
		Audit:     em,
		TenantID:  "aikonos-dev",
		Workflows: &cp6bStore{owner: ownerStore, propose: proposeStore, decide: decideStore},
		Tasks:     envStore,
	})
}

// TestNorthCP6b_SkillDenied_AllRPCs: BrokerService skill gate on all three RPCs.
func TestNorthCP6b_SkillDenied_AllRPCs(t *testing.T) {
	lineageID := testLineageUUID(t)
	os := newFakeOwnerStore()
	os.addRow(ownerRow(lineageID))
	svc := newBrokerSvcForCP6b(t, os, nil, nil, nil, false /* deny */)
	ctx := ctxWithSubject(testWFTenant, testWFOwner, "")

	t.Run("RateWorkflowRun", func(t *testing.T) {
		_, err := svc.RateWorkflowRun(ctx, &brokerv1.RateWorkflowRunRequest{
			TenantId:    testWFTenant,
			OwnerUserId: testWFOwner,
			LineageId:   lineageID.String(),
			Version:     1,
			Rating:      brokerv1.WorkflowRating_RATING_BAD,
		})
		if got := status.Code(err); got != codes.PermissionDenied {
			t.Fatalf("want PermissionDenied, got %v (err: %v)", got, err)
		}
	})

	t.Run("ProposeWorkflowVersion", func(t *testing.T) {
		_, err := svc.ProposeWorkflowVersion(ctx, &brokerv1.ProposeWorkflowVersionRequest{
			TenantId:       testWFTenant,
			OwnerUserId:    testWFOwner,
			LineageId:      lineageID.String(),
			DefinitionJson: minimalValidWorkflowJSON(),
		})
		if got := status.Code(err); got != codes.PermissionDenied {
			t.Fatalf("want PermissionDenied, got %v (err: %v)", got, err)
		}
	})

	t.Run("DecideWorkflowVersion", func(t *testing.T) {
		_, err := svc.DecideWorkflowVersion(ctx, &brokerv1.DecideWorkflowVersionRequest{
			TenantId:    testWFTenant,
			OwnerUserId: testWFOwner,
			LineageId:   lineageID.String(),
			Version:     2,
			Approved:    true,
		})
		if got := status.Code(err); got != codes.PermissionDenied {
			t.Fatalf("want PermissionDenied, got %v (err: %v)", got, err)
		}
	})
}

// TestNorthCP6b_ProposeWorkflowVersion_NonOwner_PermissionDenied.
func TestNorthCP6b_ProposeWorkflowVersion_NonOwner_PermissionDenied(t *testing.T) {
	lineageID := testLineageUUID(t)
	os := newFakeOwnerStore()
	os.addRow(ownerRow(lineageID)) // owned by testWFOwner

	ps := &fakeWorkflowProposeStore{}
	// FGA grants skill:workflows to "eve" so only the owner check causes denial.
	fga := &fakeFGA{checks: map[string]bool{
		"user:eve@example.com|can_invoke|skill:workflows": true,
	}}
	srv := fga.server(t)
	t.Cleanup(srv.Close)
	eng, _ := policy.NewEngine(context.Background(), policy.Config{
		OPAEndpoint:     "http://unused",
		OpenFGAEndpoint: srv.URL,
		OpenFGAStoreID:  "store-1",
	})
	em, _ := audit.NewEmitter(context.Background(), audit.Config{})
	svc := NewBrokerService(Deps{
		Logger:    zap.NewNop(),
		Policy:    eng,
		Audit:     em,
		TenantID:  "aikonos-dev",
		Workflows: &cp6bStore{owner: os, propose: ps},
	})

	ctx := ctxWithSubject(testWFTenant, "eve@example.com", "")
	_, err := svc.ProposeWorkflowVersion(ctx, &brokerv1.ProposeWorkflowVersionRequest{
		TenantId:       testWFTenant,
		OwnerUserId:    "eve@example.com",
		LineageId:      lineageID.String(),
		DefinitionJson: minimalValidWorkflowJSON(),
	})
	if got := status.Code(err); got != codes.PermissionDenied {
		t.Fatalf("north non-owner propose: want PermissionDenied, got %v (err: %v)", got, err)
	}
	if len(ps.proposed) != 0 {
		t.Fatal("ProposeVersion must not have been called")
	}
}

// TestNorthCP6b_DecideWorkflowVersion_NonOwner_PermissionDenied.
func TestNorthCP6b_DecideWorkflowVersion_NonOwner_PermissionDenied(t *testing.T) {
	lineageID := testLineageUUID(t)
	os := newFakeOwnerStore()
	os.addRow(ownerRow(lineageID)) // owned by testWFOwner

	ds := &fakeWorkflowDecideStore{}
	fga := &fakeFGA{checks: map[string]bool{
		"user:eve@example.com|can_invoke|skill:workflows": true,
	}}
	srv := fga.server(t)
	t.Cleanup(srv.Close)
	eng, _ := policy.NewEngine(context.Background(), policy.Config{
		OPAEndpoint:     "http://unused",
		OpenFGAEndpoint: srv.URL,
		OpenFGAStoreID:  "store-1",
	})
	em, _ := audit.NewEmitter(context.Background(), audit.Config{})
	svc := NewBrokerService(Deps{
		Logger:    zap.NewNop(),
		Policy:    eng,
		Audit:     em,
		TenantID:  "aikonos-dev",
		Workflows: &cp6bStore{owner: os, decide: ds},
	})

	ctx := ctxWithSubject(testWFTenant, "eve@example.com", "")
	_, err := svc.DecideWorkflowVersion(ctx, &brokerv1.DecideWorkflowVersionRequest{
		TenantId:    testWFTenant,
		OwnerUserId: "eve@example.com",
		LineageId:   lineageID.String(),
		Version:     2,
		Approved:    true,
	})
	if got := status.Code(err); got != codes.PermissionDenied {
		t.Fatalf("north non-owner decide: want PermissionDenied, got %v (err: %v)", got, err)
	}
	if len(ds.approved) != 0 {
		t.Fatal("ApproveVersion must not have been called")
	}
}

// TestNorthCP6b_Propose_Owner_CreatesProposalAndEnvelope: north happy path.
func TestNorthCP6b_Propose_Owner_CreatesProposalAndEnvelope(t *testing.T) {
	lineageID := testLineageUUID(t)
	os := newFakeOwnerStore()
	os.addRow(ownerRow(lineageID))
	ps := &fakeWorkflowProposeStore{}
	env := &fakeCP6bEnvelopeStore{}

	svc := newBrokerSvcForCP6b(t, os, ps, nil, env, true /* allow */)
	ctx := ctxWithSubject(testWFTenant, testWFOwner, "")

	resp, err := svc.ProposeWorkflowVersion(ctx, &brokerv1.ProposeWorkflowVersionRequest{
		TenantId:       testWFTenant,
		OwnerUserId:    testWFOwner,
		LineageId:      lineageID.String(),
		DefinitionJson: minimalValidWorkflowJSON(),
	})
	if err != nil {
		t.Fatalf("north propose: unexpected error: %v", err)
	}
	if resp.Version != 42 {
		t.Errorf("version: want 42 (fake), got %d", resp.Version)
	}
	if len(ps.proposed) != 1 {
		t.Fatalf("ProposeVersion calls: want 1, got %d", len(ps.proposed))
	}
	if len(env.created) != 1 {
		t.Fatalf("envelope count: want 1, got %d", len(env.created))
	}
}
