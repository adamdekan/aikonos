package broker

// Tests for CP7: PublishWorkflow (north + south) and helpers.
//
// Pattern mirrors service_workflows_cp6b_test.go. Fakes satisfy the new
// workflowPublishStore interface; fakeWorkflowOwnerStore is reused from
// cp6b_test.go (same package). computeRequires (now workflowsvc.ComputeRequires)
// is tested standalone in workflowsvc — see that package's *_test.go files.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/policy"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// ── fakeWorkflowPublishStore ──────────────────────────────────────────────────

type publishedCall struct {
	lineage uuid.UUID
	version int
	groups  []string
	defJSON []byte
}

type successRatedCall struct {
	lineage uuid.UUID
	version int
}

// fakeWorkflowPublishStore satisfies the GetVersion/MarkSuccessRated/PublishVersion
// slice of workflowStore. Combined with fakeWorkflowOwnerStore via cp7Store (below)
// since Deps.Workflows is now a single field (CP1).
type fakeWorkflowPublishStore struct {
	stubWorkflowStore
	versionRow *db.WorkflowRow // returned by GetVersion; nil → error
	published  []publishedCall
	stamped    []successRatedCall
}

// ── cp7Store ──────────────────────────────────────────────────────────────────

// cp7Store composes fakeWorkflowOwnerStore + fakeWorkflowPublishStore into one
// object satisfying workflowStore (Deps.Workflows is a single field — CP1).
type cp7Store struct {
	stubWorkflowStore
	owner   *fakeWorkflowOwnerStore
	publish *fakeWorkflowPublishStore
}

func (c *cp7Store) GetCurrent(ctx context.Context, tenant string, lineageID uuid.UUID) (*db.WorkflowRow, error) {
	if c.owner != nil {
		return c.owner.GetCurrent(ctx, tenant, lineageID)
	}
	return c.stubWorkflowStore.GetCurrent(ctx, tenant, lineageID)
}

func (c *cp7Store) GetVersion(ctx context.Context, tenant string, lineageID uuid.UUID, version int) (*db.WorkflowRow, error) {
	if c.publish != nil {
		return c.publish.GetVersion(ctx, tenant, lineageID, version)
	}
	return c.stubWorkflowStore.GetVersion(ctx, tenant, lineageID, version)
}

func (c *cp7Store) MarkSuccessRated(ctx context.Context, tenant string, lineageID uuid.UUID, version int) error {
	if c.publish != nil {
		return c.publish.MarkSuccessRated(ctx, tenant, lineageID, version)
	}
	return c.stubWorkflowStore.MarkSuccessRated(ctx, tenant, lineageID, version)
}

func (c *cp7Store) PublishVersion(ctx context.Context, tenant string, lineageID uuid.UUID, version int, groups []string, definitionJSON []byte) error {
	if c.publish != nil {
		return c.publish.PublishVersion(ctx, tenant, lineageID, version, groups, definitionJSON)
	}
	return c.stubWorkflowStore.PublishVersion(ctx, tenant, lineageID, version, groups, definitionJSON)
}

func (f *fakeWorkflowPublishStore) GetVersion(_ context.Context, _ string, _ uuid.UUID, _ int) (*db.WorkflowRow, error) {
	if f.versionRow == nil {
		return nil, db.ErrVersionNotFound
	}
	return f.versionRow, nil
}

func (f *fakeWorkflowPublishStore) MarkSuccessRated(_ context.Context, _ string, lineageID uuid.UUID, version int) error {
	f.stamped = append(f.stamped, successRatedCall{lineageID, version})
	return nil
}

func (f *fakeWorkflowPublishStore) PublishVersion(_ context.Context, _ string, lineageID uuid.UUID, version int, groups []string, defJSON []byte) error {
	f.published = append(f.published, publishedCall{lineageID, version, groups, defJSON})
	return nil
}

// minimalWorkflowRowForPublish builds a WorkflowRow whose Definition is a
// minimal valid workflow with two steps so computeRequires can be exercised.
func minimalWorkflowRowForPublish(lineageID uuid.UUID, version int) *db.WorkflowRow {
	wf := map[string]any{
		"apiVersion": "aikonos.com/v1",
		"kind":       "Workflow",
		"metadata": map[string]any{
			"name": "pub-workflow",
			"visibility": map[string]any{
				"kind": "private",
			},
		},
		"steps": []any{
			map[string]any{"skill": "doc.read"},
			map[string]any{"skill": "web.fetch"},
		},
	}
	b, _ := json.Marshal(wf)
	return &db.WorkflowRow{
		ID:               uuid.New(),
		LineageID:        lineageID,
		Version:          version,
		TenantID:         uuid.MustParse(testWFTenant),
		OwnerUserID:      testWFOwner,
		Name:             "pub-workflow",
		Status:           "private",
		VisibilityKind:   "private",
		VisibilityGroups: []byte("[]"),
		Definition:       b,
		ApprovalState:    "approved",
	}
}

const testGroupID = "group-alpha"

// ── SandboxService builder for CP7 ───────────────────────────────────────────

func newSandboxSvcForCP7(
	t *testing.T,
	ownerStore *fakeWorkflowOwnerStore,
	publishStore *fakeWorkflowPublishStore,
	allow bool,
	fga *fakeFGA,
) *SandboxService {
	t.Helper()
	em, err := audit.NewEmitter(context.Background(), audit.Config{})
	if err != nil {
		t.Fatalf("audit.NewEmitter: %v", err)
	}
	// Build a skillPolicy that delegates group-membership checks through the
	// same fakeFGA when provided.
	var sp skillGatePolicy
	if fga != nil {
		sp = fga.asSkillPolicy()
	} else {
		sp = &fakeSkillPolicy{enabled: true, granted: allow}
	}
	svc := NewSandboxService(Deps{
		Logger:          zap.NewNop(),
		Audit:           em,
		GatewaySpiffeID: testGWSpiffeID,
		GatewayGrantKey: testGrantKey,
		GatewayGrantTTL: testGrantTTL,
		Workflows:       &cp7Store{owner: ownerStore, publish: publishStore},
		ToolRegistry:    newTestToolRegistry(),
	})
	svc.skillPolicy = sp
	return svc
}

// fakeFGA.asSkillPolicy wraps a fakeFGA as a skillGatePolicy so south-path
// group-membership checks (CheckFGA) work through the same check map.
func (f *fakeFGA) asSkillPolicy() skillGatePolicy {
	return &fakeFGASkillPolicy{fga: f}
}

// fakeFGASkillPolicy adapts fakeFGA to the skillGatePolicy interface used by
// the south path (skill gate + publishCore group check).
type fakeFGASkillPolicy struct {
	fga *fakeFGA
}

func (p *fakeFGASkillPolicy) FGAEnabled() bool { return true }
func (p *fakeFGASkillPolicy) CheckFGA(ctx context.Context, user, relation, object string) (bool, error) {
	key := user + "|" + relation + "|" + object
	ok := p.fga.checks[key]
	return ok, nil
}

// computeRequires (now workflowsvc.ComputeRequires) unit tests moved to
// workflowsvc.TestComputeRequires_* (workflowsvc-extraction CP2) — direct
// calls to the former computeRequires, one of the six functions CP1 kept
// shimmed. Nothing else in this file used them.

// ── PublishWorkflow south tests ───────────────────────────────────────────────

// TestPublishWorkflow_SuccessRatedThenPublish_Succeeds: publish to an owned
// group succeeds. Verifies row becomes published/shared and definition gains
// requires.
func TestPublishWorkflow_SuccessRatedThenPublish_Succeeds(t *testing.T) {
	lineageID := testLineageUUID(t)

	ownerStore := newFakeOwnerStore()
	ownerStore.addRow(ownerRow(lineageID))

	ps := &fakeWorkflowPublishStore{
		versionRow: minimalWorkflowRowForPublish(lineageID, 1),
	}

	fga := &fakeFGA{checks: map[string]bool{
		"user:" + testWFOwner + "|can_invoke|skill:workflows":  true,
		"user:" + testWFOwner + "|member|group:" + testGroupID: true,
	}}
	svc := newSandboxSvcForCP7(t, ownerStore, ps, true, fga)

	resp, err := svc.PublishWorkflow(gatewayCtxForWorkflow(), &brokerv1.PublishWorkflowRequest{
		TenantId:    testWFTenant,
		OwnerUserId: testWFOwner,
		OwnerGrant:  mintWFGrant(t),
		LineageId:   lineageID.String(),
		Version:     1,
		GroupIds:    []string{testGroupID},
	})
	if err != nil {
		t.Fatalf("PublishWorkflow: unexpected error: %v", err)
	}
	if resp.VisibilityKind != "shared" {
		t.Errorf("VisibilityKind: want \"shared\", got %q", resp.VisibilityKind)
	}
	if len(resp.Groups) != 1 || resp.Groups[0] != testGroupID {
		t.Errorf("Groups: want [%s], got %v", testGroupID, resp.Groups)
	}

	// PublishVersion was called
	if len(ps.published) != 1 {
		t.Fatalf("PublishVersion calls: want 1, got %d", len(ps.published))
	}
	if len(ps.published[0].groups) != 1 || ps.published[0].groups[0] != testGroupID {
		t.Errorf("published groups: want [%s], got %v", testGroupID, ps.published[0].groups)
	}

	// The re-serialized definition must contain the requires block with skills.
	var def map[string]any
	if err := json.Unmarshal(ps.published[0].defJSON, &def); err != nil {
		t.Fatalf("unmarshal published definition: %v", err)
	}
	reqAny, ok := def["requires"]
	if !ok {
		t.Fatal("published definition must contain 'requires' field")
	}
	reqMap, ok := reqAny.(map[string]any)
	if !ok {
		t.Fatalf("requires must be an object, got %T", reqAny)
	}
	skillsAny, ok := reqMap["skills"]
	if !ok {
		t.Fatal("requires.skills must be present")
	}
	skills, ok := skillsAny.([]any)
	if !ok || len(skills) < 2 {
		t.Errorf("requires.skills: want 2 entries, got %v", skillsAny)
	}
}

// TestPublishWorkflow_NonMemberGroup_PermissionDenied: owner not a member of
// the target group → PermissionDenied.
func TestPublishWorkflow_NonMemberGroup_PermissionDenied(t *testing.T) {
	lineageID := testLineageUUID(t)

	ownerStore := newFakeOwnerStore()
	ownerStore.addRow(ownerRow(lineageID))

	ps := &fakeWorkflowPublishStore{
		versionRow: minimalWorkflowRowForPublish(lineageID, 1),
	}

	// skill:workflows granted but NOT a member of the group
	fga := &fakeFGA{checks: map[string]bool{
		"user:" + testWFOwner + "|can_invoke|skill:workflows": true,
		// group member relation intentionally absent
	}}
	svc := newSandboxSvcForCP7(t, ownerStore, ps, true, fga)

	_, err := svc.PublishWorkflow(gatewayCtxForWorkflow(), &brokerv1.PublishWorkflowRequest{
		TenantId:    testWFTenant,
		OwnerUserId: testWFOwner,
		OwnerGrant:  mintWFGrant(t),
		LineageId:   lineageID.String(),
		Version:     1,
		GroupIds:    []string{testGroupID},
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-member group: want PermissionDenied, got %v", err)
	}
	if len(ps.published) != 0 {
		t.Error("PublishVersion must not have been called")
	}
}

// TestPublishWorkflow_SkillDeny_PermissionDenied.
func TestPublishWorkflow_SkillDeny_PermissionDenied(t *testing.T) {
	lineageID := testLineageUUID(t)

	ownerStore := newFakeOwnerStore()
	ownerStore.addRow(ownerRow(lineageID))
	ps := &fakeWorkflowPublishStore{versionRow: minimalWorkflowRowForPublish(lineageID, 1)}

	svc := newSandboxSvcForCP7(t, ownerStore, ps, false /* deny */, nil)

	_, err := svc.PublishWorkflow(gatewayCtxForWorkflow(), &brokerv1.PublishWorkflowRequest{
		TenantId:    testWFTenant,
		OwnerUserId: testWFOwner,
		OwnerGrant:  mintWFGrant(t),
		LineageId:   lineageID.String(),
		Version:     1,
		GroupIds:    []string{testGroupID},
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("skill deny: want PermissionDenied, got %v", err)
	}
}

// TestPublishWorkflow_NonOwner_PermissionDenied: non-owner grant → PermissionDenied.
func TestPublishWorkflow_NonOwner_PermissionDenied(t *testing.T) {
	lineageID := testLineageUUID(t)

	ownerStore := newFakeOwnerStore()
	ownerStore.addRow(ownerRow(lineageID)) // owned by testWFOwner

	ps := &fakeWorkflowPublishStore{versionRow: minimalWorkflowRowForPublish(lineageID, 1)}

	fga := &fakeFGA{checks: map[string]bool{
		"user:eve@example.com|can_invoke|skill:workflows":  true,
		"user:eve@example.com|member|group:" + testGroupID: true,
	}}
	em, _ := audit.NewEmitter(context.Background(), audit.Config{})
	svc := NewSandboxService(Deps{
		Logger:          zap.NewNop(),
		Audit:           em,
		GatewaySpiffeID: testGWSpiffeID,
		GatewayGrantKey: testGrantKey,
		GatewayGrantTTL: time.Hour,
		Workflows:       &cp7Store{owner: ownerStore, publish: ps},
		ToolRegistry:    newTestToolRegistry(),
	})
	svc.skillPolicy = fga.asSkillPolicy()

	nonOwnerGrant := mintGrantFor(t, testWFTenant, "eve@example.com")
	_, err := svc.PublishWorkflow(gatewayCtxForWorkflow(), &brokerv1.PublishWorkflowRequest{
		TenantId:    testWFTenant,
		OwnerUserId: "eve@example.com",
		OwnerGrant:  nonOwnerGrant,
		LineageId:   lineageID.String(),
		Version:     1,
		GroupIds:    []string{testGroupID},
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-owner: want PermissionDenied, got %v", err)
	}
	if len(ps.published) != 0 {
		t.Error("PublishVersion must not have been called for non-owner")
	}
}

// ── RateWorkflowRun SUCCESS stamps the marker (south) ────────────────────────

// TestRateWorkflowRun_Success_StampsMarker: SUCCESS rating calls MarkSuccessRated.
func TestRateWorkflowRun_Success_StampsMarker(t *testing.T) {
	lineageID := testLineageUUID(t)
	ownerStore := newFakeOwnerStore()
	ownerStore.addRow(ownerRow(lineageID))

	ps := &fakeWorkflowPublishStore{versionRow: minimalWorkflowRowForPublish(lineageID, 1)}

	em, _ := audit.NewEmitter(context.Background(), audit.Config{})
	svc := NewSandboxService(Deps{
		Logger:          zap.NewNop(),
		Audit:           em,
		GatewaySpiffeID: testGWSpiffeID,
		GatewayGrantKey: testGrantKey,
		GatewayGrantTTL: testGrantTTL,
		Workflows:       &cp7Store{owner: ownerStore, publish: ps},
	})
	svc.skillPolicy = &fakeSkillPolicy{enabled: true, granted: true}

	_, err := svc.RateWorkflowRun(gatewayCtxForWorkflow(), &brokerv1.RateWorkflowRunRequest{
		TenantId:    testWFTenant,
		OwnerUserId: testWFOwner,
		OwnerGrant:  mintWFGrant(t),
		LineageId:   lineageID.String(),
		Version:     1,
		Rating:      brokerv1.WorkflowRating_RATING_SUCCESS,
	})
	if err != nil {
		t.Fatalf("RateWorkflowRun SUCCESS: unexpected error: %v", err)
	}
	if len(ps.stamped) != 1 {
		t.Fatalf("MarkSuccessRated calls: want 1, got %d", len(ps.stamped))
	}
	if ps.stamped[0].version != 1 {
		t.Errorf("stamped version: want 1, got %d", ps.stamped[0].version)
	}
}

// TestRateWorkflowRun_Bad_DoesNotStamp: BAD rating must NOT call MarkSuccessRated.
func TestRateWorkflowRun_Bad_DoesNotStamp(t *testing.T) {
	lineageID := testLineageUUID(t)
	ownerStore := newFakeOwnerStore()
	ownerStore.addRow(ownerRow(lineageID))

	ps := &fakeWorkflowPublishStore{versionRow: minimalWorkflowRowForPublish(lineageID, 1)}

	em, _ := audit.NewEmitter(context.Background(), audit.Config{})
	svc := NewSandboxService(Deps{
		Logger:          zap.NewNop(),
		Audit:           em,
		GatewaySpiffeID: testGWSpiffeID,
		GatewayGrantKey: testGrantKey,
		GatewayGrantTTL: testGrantTTL,
		Workflows:       &cp7Store{owner: ownerStore, publish: ps},
	})
	svc.skillPolicy = &fakeSkillPolicy{enabled: true, granted: true}

	_, err := svc.RateWorkflowRun(gatewayCtxForWorkflow(), &brokerv1.RateWorkflowRunRequest{
		TenantId:    testWFTenant,
		OwnerUserId: testWFOwner,
		OwnerGrant:  mintWFGrant(t),
		LineageId:   lineageID.String(),
		Version:     1,
		Rating:      brokerv1.WorkflowRating_RATING_BAD,
	})
	if err != nil {
		t.Fatalf("RateWorkflowRun BAD: unexpected error: %v", err)
	}
	if len(ps.stamped) != 0 {
		t.Errorf("BAD rating: MarkSuccessRated must not be called, got %d call(s)", len(ps.stamped))
	}
}

// ── North-path (BrokerService — OIDC bearer) tests ───────────────────────────

// newBrokerSvcForCP7 builds a BrokerService with PublishWorkflow stores and FGA.
func newBrokerSvcForCP7(
	t *testing.T,
	ownerStore *fakeWorkflowOwnerStore,
	publishStore *fakeWorkflowPublishStore,
	fgaChecks map[string]bool,
) *BrokerService {
	t.Helper()
	fga := &fakeFGA{checks: fgaChecks}
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
		Logger:       zap.NewNop(),
		Policy:       eng,
		Audit:        em,
		TenantID:     "aikonos-dev",
		Workflows:    &cp7Store{owner: ownerStore, publish: publishStore},
		ToolRegistry: newTestToolRegistry(),
	})
}

// TestNorthCP7_SkillDenied_PermissionDenied.
func TestNorthCP7_SkillDenied_PermissionDenied(t *testing.T) {
	lineageID := testLineageUUID(t)
	ownerStore := newFakeOwnerStore()
	ownerStore.addRow(ownerRow(lineageID))
	ps := &fakeWorkflowPublishStore{versionRow: minimalWorkflowRowForPublish(lineageID, 1)}

	svc := newBrokerSvcForCP7(t, ownerStore, ps, map[string]bool{
		// skill:workflows NOT granted
	})
	ctx := ctxWithSubject(testWFTenant, testWFOwner, "")

	_, err := svc.PublishWorkflow(ctx, &brokerv1.PublishWorkflowRequest{
		TenantId:    testWFTenant,
		OwnerUserId: testWFOwner,
		LineageId:   lineageID.String(),
		Version:     1,
		GroupIds:    []string{testGroupID},
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("north skill deny: want PermissionDenied, got %v", err)
	}
}

// TestNorthCP7_NonOwner_PermissionDenied.
func TestNorthCP7_NonOwner_PermissionDenied(t *testing.T) {
	lineageID := testLineageUUID(t)
	ownerStore := newFakeOwnerStore()
	ownerStore.addRow(ownerRow(lineageID)) // owned by testWFOwner

	ps := &fakeWorkflowPublishStore{versionRow: minimalWorkflowRowForPublish(lineageID, 1)}

	svc := newBrokerSvcForCP7(t, ownerStore, ps, map[string]bool{
		"user:eve@example.com|can_invoke|skill:workflows":  true,
		"user:eve@example.com|member|group:" + testGroupID: true,
	})
	ctx := ctxWithSubject(testWFTenant, "eve@example.com", "")

	_, err := svc.PublishWorkflow(ctx, &brokerv1.PublishWorkflowRequest{
		TenantId:    testWFTenant,
		OwnerUserId: "eve@example.com",
		LineageId:   lineageID.String(),
		Version:     1,
		GroupIds:    []string{testGroupID},
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("north non-owner: want PermissionDenied, got %v", err)
	}
	if len(ps.published) != 0 {
		t.Error("PublishVersion must not have been called")
	}
}

// TestNorthCP7_NonMemberGroup_PermissionDenied.
func TestNorthCP7_NonMemberGroup_PermissionDenied(t *testing.T) {
	lineageID := testLineageUUID(t)
	ownerStore := newFakeOwnerStore()
	ownerStore.addRow(ownerRow(lineageID))

	ps := &fakeWorkflowPublishStore{
		versionRow: minimalWorkflowRowForPublish(lineageID, 1),
	}

	svc := newBrokerSvcForCP7(t, ownerStore, ps, map[string]bool{
		"user:" + testWFOwner + "|can_invoke|skill:workflows": true,
		// group member NOT in checks
	})
	ctx := ctxWithSubject(testWFTenant, testWFOwner, "")

	_, err := svc.PublishWorkflow(ctx, &brokerv1.PublishWorkflowRequest{
		TenantId:    testWFTenant,
		OwnerUserId: testWFOwner,
		LineageId:   lineageID.String(),
		Version:     1,
		GroupIds:    []string{testGroupID},
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("north non-member: want PermissionDenied, got %v", err)
	}
}

// TestNorthCP7_OwnerSuccessRatedPublish_Succeeds: north happy path.
func TestNorthCP7_OwnerSuccessRatedPublish_Succeeds(t *testing.T) {
	lineageID := testLineageUUID(t)
	ownerStore := newFakeOwnerStore()
	ownerStore.addRow(ownerRow(lineageID))

	ps := &fakeWorkflowPublishStore{
		versionRow: minimalWorkflowRowForPublish(lineageID, 1),
	}

	svc := newBrokerSvcForCP7(t, ownerStore, ps, map[string]bool{
		"user:" + testWFOwner + "|can_invoke|skill:workflows":  true,
		"user:" + testWFOwner + "|member|group:" + testGroupID: true,
	})
	ctx := ctxWithSubject(testWFTenant, testWFOwner, "")

	resp, err := svc.PublishWorkflow(ctx, &brokerv1.PublishWorkflowRequest{
		TenantId:    testWFTenant,
		OwnerUserId: testWFOwner,
		LineageId:   lineageID.String(),
		Version:     1,
		GroupIds:    []string{testGroupID},
	})
	if err != nil {
		t.Fatalf("north PublishWorkflow: unexpected error: %v", err)
	}
	if resp.VisibilityKind != "shared" {
		t.Errorf("VisibilityKind: want \"shared\", got %q", resp.VisibilityKind)
	}
	if len(ps.published) != 1 {
		t.Fatalf("PublishVersion calls: want 1, got %d", len(ps.published))
	}
	if len(ps.published[0].groups) != 1 || ps.published[0].groups[0] != testGroupID {
		t.Errorf("published groups: want [%s], got %v", testGroupID, ps.published[0].groups)
	}
}

// ── Item 1: version-not-found path returns NotFound, not FailedPrecondition ──

// TestPublishWorkflow_VersionNotFound_NotFound: PublishVersion signals
// ErrVersionNotFound → publishCore must surface codes.NotFound (not FailedPrecondition).
func TestPublishWorkflow_VersionNotFound_NotFound(t *testing.T) {
	lineageID := testLineageUUID(t)

	ownerStore := newFakeOwnerStore()
	ownerStore.addRow(ownerRow(lineageID))

	// versionRow nil → GetVersion returns ErrVersionNotFound → PublishVersion
	// also returns ErrVersionNotFound (fake propagates it).
	ps := &fakeWorkflowPublishStore{versionRow: nil}

	fga := &fakeFGA{checks: map[string]bool{
		"user:" + testWFOwner + "|can_invoke|skill:workflows":  true,
		"user:" + testWFOwner + "|member|group:" + testGroupID: true,
	}}
	svc := newSandboxSvcForCP7(t, ownerStore, ps, true, fga)

	_, err := svc.PublishWorkflow(gatewayCtxForWorkflow(), &brokerv1.PublishWorkflowRequest{
		TenantId:    testWFTenant,
		OwnerUserId: testWFOwner,
		OwnerGrant:  mintWFGrant(t),
		LineageId:   lineageID.String(),
		Version:     99, // does not exist
		GroupIds:    []string{testGroupID},
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("version not found: want NotFound, got %v", err)
	}
}

// TestNorthCP7_VersionNotFound_NotFound: same path through north handler.
func TestNorthCP7_VersionNotFound_NotFound(t *testing.T) {
	lineageID := testLineageUUID(t)
	ownerStore := newFakeOwnerStore()
	ownerStore.addRow(ownerRow(lineageID))

	ps := &fakeWorkflowPublishStore{versionRow: nil}

	svc := newBrokerSvcForCP7(t, ownerStore, ps, map[string]bool{
		"user:" + testWFOwner + "|can_invoke|skill:workflows":  true,
		"user:" + testWFOwner + "|member|group:" + testGroupID: true,
	})
	ctx := ctxWithSubject(testWFTenant, testWFOwner, "")

	_, err := svc.PublishWorkflow(ctx, &brokerv1.PublishWorkflowRequest{
		TenantId:    testWFTenant,
		OwnerUserId: testWFOwner,
		LineageId:   lineageID.String(),
		Version:     99,
		GroupIds:    []string{testGroupID},
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("north version not found: want NotFound, got %v", err)
	}
}

// ── Item 2: north-path RateWorkflowRun stamp tests ───────────────────────────

// TestNorthCP7_RateWorkflowRun_Success_StampsMarker: BrokerService.RateWorkflowRun
// with RATING_SUCCESS calls MarkSuccessRated via Deps.Workflows.
func TestNorthCP7_RateWorkflowRun_Success_StampsMarker(t *testing.T) {
	lineageID := testLineageUUID(t)
	ownerStore := newFakeOwnerStore()
	ownerStore.addRow(ownerRow(lineageID))

	ps := &fakeWorkflowPublishStore{versionRow: minimalWorkflowRowForPublish(lineageID, 1)}

	svc := newBrokerSvcForCP7(t, ownerStore, ps, map[string]bool{
		"user:" + testWFOwner + "|can_invoke|skill:workflows": true,
	})
	ctx := ctxWithSubject(testWFTenant, testWFOwner, "")

	_, err := svc.RateWorkflowRun(ctx, &brokerv1.RateWorkflowRunRequest{
		TenantId:    testWFTenant,
		OwnerUserId: testWFOwner,
		LineageId:   lineageID.String(),
		Version:     1,
		Rating:      brokerv1.WorkflowRating_RATING_SUCCESS,
	})
	if err != nil {
		t.Fatalf("north RateWorkflowRun SUCCESS: unexpected error: %v", err)
	}
	if len(ps.stamped) != 1 {
		t.Fatalf("MarkSuccessRated calls: want 1, got %d", len(ps.stamped))
	}
	if ps.stamped[0].version != 1 {
		t.Errorf("stamped version: want 1, got %d", ps.stamped[0].version)
	}
}

// TestNorthCP7_RateWorkflowRun_Bad_DoesNotStamp: RATING_BAD must not call
// MarkSuccessRated on the north path.
func TestNorthCP7_RateWorkflowRun_Bad_DoesNotStamp(t *testing.T) {
	lineageID := testLineageUUID(t)
	ownerStore := newFakeOwnerStore()
	ownerStore.addRow(ownerRow(lineageID))

	ps := &fakeWorkflowPublishStore{versionRow: minimalWorkflowRowForPublish(lineageID, 1)}

	svc := newBrokerSvcForCP7(t, ownerStore, ps, map[string]bool{
		"user:" + testWFOwner + "|can_invoke|skill:workflows": true,
	})
	ctx := ctxWithSubject(testWFTenant, testWFOwner, "")

	_, err := svc.RateWorkflowRun(ctx, &brokerv1.RateWorkflowRunRequest{
		TenantId:    testWFTenant,
		OwnerUserId: testWFOwner,
		LineageId:   lineageID.String(),
		Version:     1,
		Rating:      brokerv1.WorkflowRating_RATING_BAD,
	})
	if err != nil {
		t.Fatalf("north RateWorkflowRun BAD: unexpected error: %v", err)
	}
	if len(ps.stamped) != 0 {
		t.Errorf("BAD rating north: MarkSuccessRated must not be called, got %d call(s)", len(ps.stamped))
	}
}
