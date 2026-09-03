package db

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
)

// ── Pure-logic tests (no Postgres required) ───────────────────────────────────

func TestWorkflowRepoConstructor(t *testing.T) {
	repo := NewWorkflowRepo(nil, nil)
	if repo == nil {
		t.Fatal("NewWorkflowRepo returned nil")
	}
}

// ── DB-backed tests (require TEST_DATABASE_URL; skip when absent) ─────────────
//
// Run against a migrated DB (migration 026 applied):
//
//	TEST_DATABASE_URL=postgres://aikonos:dev-password-change-me@localhost:5432/aikonos \
//	  go test ./broker/internal/db/... -run TestWorkflow -v

func openTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set — skipping DB-backed tests")
	}
	pool, err := NewPool(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open test pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// baseRow returns a minimal valid WorkflowRow for insertion.
func baseRow(tenantID uuid.UUID, lineageID uuid.UUID, owner string) WorkflowRow {
	return WorkflowRow{
		LineageID:        lineageID,
		TenantID:         tenantID,
		OwnerUserID:      owner,
		Name:             "test-workflow",
		Description:      "created in test",
		Status:           "private",
		VisibilityKind:   "private",
		VisibilityGroups: json.RawMessage(`[]`),
		Definition:       json.RawMessage(`{"steps":[]}`),
	}
}

// TestWorkflow_ThreeVersions verifies that three sequential CreateVersion calls
// produce immutable version rows 1..3, ListVersions returns all three newest-first,
// and GetCurrent returns version 3.
func TestWorkflow_ThreeVersions(t *testing.T) {
	pool := openTestPool(t)
	repo := NewWorkflowRepo(pool, zap.NewNop())
	ctx := context.Background()

	tenantID := uuid.New()
	lineageID := uuid.New()
	tenant := tenantID.String()

	for i := 1; i <= 3; i++ {
		row := baseRow(tenantID, lineageID, "user-a")
		row.Name = fmt.Sprintf("v%d", i)
		got, err := repo.CreateVersion(ctx, tenant, row)
		if err != nil {
			t.Fatalf("CreateVersion %d: %v", i, err)
		}
		if got.Version != i {
			t.Errorf("version %d: got Version=%d", i, got.Version)
		}
	}

	versions, err := repo.ListVersions(ctx, tenant, lineageID, 0, 0)
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(versions) != 3 {
		t.Fatalf("want 3 versions, got %d", len(versions))
	}
	// Newest first.
	if versions[0].Version != 3 {
		t.Errorf("first returned version: want 3, got %d", versions[0].Version)
	}

	current, err := repo.GetCurrent(ctx, tenant, lineageID)
	if err != nil {
		t.Fatalf("GetCurrent: %v", err)
	}
	if current.Version != 3 {
		t.Errorf("GetCurrent: want version 3, got %d", current.Version)
	}
}

// TestWorkflow_RLSIsolation verifies that tenant B cannot read or affect tenant A's
// workflow rows or version pins.
func TestWorkflow_RLSIsolation(t *testing.T) {
	pool := openTestPool(t)
	repo := NewWorkflowRepo(pool, zap.NewNop())
	ctx := context.Background()

	tenantA := uuid.New()
	tenantB := uuid.New()
	lineageID := uuid.New()
	userA := "user-a"

	row := baseRow(tenantA, lineageID, userA)
	if _, err := repo.CreateVersion(ctx, tenantA.String(), row); err != nil {
		t.Fatalf("CreateVersion as tenant A: %v", err)
	}

	// Tenant B should see nothing for lineage created by tenant A (ListVersions).
	versions, err := repo.ListVersions(ctx, tenantB.String(), lineageID, 0, 0)
	if err != nil {
		t.Fatalf("ListVersions as tenant B: %v", err)
	}
	if len(versions) != 0 {
		t.Errorf("RLS isolation ListVersions: tenant B should see 0 versions, got %d", len(versions))
	}

	// GetCurrent as tenant B must return an error.
	_, err = repo.GetCurrent(ctx, tenantB.String(), lineageID)
	if err == nil {
		t.Error("GetCurrent as tenant B: expected error (no rows), got nil")
	}

	// ListByOwner as tenant B must not return tenant A's workflow.
	byOwner, err := repo.ListByOwner(ctx, tenantB.String(), userA, "", 0)
	if err != nil {
		t.Fatalf("ListByOwner as tenant B: %v", err)
	}
	if len(byOwner) != 0 {
		t.Errorf("RLS isolation ListByOwner: tenant B should see 0 rows, got %d", len(byOwner))
	}

	// Pin set by tenant A must not be visible to tenant B.
	if err := repo.SetVersionPin(ctx, tenantA.String(), userA, lineageID, 1); err != nil {
		t.Fatalf("SetVersionPin as tenant A: %v", err)
	}
	_, ok, err := repo.GetVersionPin(ctx, tenantB.String(), userA, lineageID)
	if err != nil {
		t.Fatalf("GetVersionPin as tenant B: %v", err)
	}
	if ok {
		t.Error("RLS isolation GetVersionPin: tenant B should not see tenant A's pin")
	}

	// ClearVersionPin as tenant B must not affect tenant A's pin.
	if err := repo.ClearVersionPin(ctx, tenantB.String(), userA, lineageID); err != nil {
		t.Fatalf("ClearVersionPin as tenant B: %v", err)
	}
	// Tenant A's pin must still be intact.
	pinned, ok, err := repo.GetVersionPin(ctx, tenantA.String(), userA, lineageID)
	if err != nil {
		t.Fatalf("GetVersionPin as tenant A after B clear: %v", err)
	}
	if !ok {
		t.Error("RLS isolation: tenant A's pin was deleted by tenant B's ClearVersionPin")
	}
	if pinned != 1 {
		t.Errorf("RLS isolation: tenant A's pin should be 1, got %d", pinned)
	}
}

// TestWorkflow_VersionPin verifies the full pin lifecycle:
// pin v1 → ResolveVersionForUser returns v1 (downgrade); ClearVersionPin →
// ResolveVersionForUser falls back to the current (highest) version.
func TestWorkflow_VersionPin(t *testing.T) {
	pool := openTestPool(t)
	repo := NewWorkflowRepo(pool, zap.NewNop())
	ctx := context.Background()

	tenantID := uuid.New()
	lineageID := uuid.New()
	tenant := tenantID.String()
	userID := "pin-test-user"

	// Create two versions so current != v1.
	for i := 0; i < 2; i++ {
		if _, err := repo.CreateVersion(ctx, tenant, baseRow(tenantID, lineageID, userID)); err != nil {
			t.Fatalf("CreateVersion %d: %v", i+1, err)
		}
	}

	// Pin to v1 (downgrade).
	if err := repo.SetVersionPin(ctx, tenant, userID, lineageID, 1); err != nil {
		t.Fatalf("SetVersionPin: %v", err)
	}
	pinned, ok, err := repo.GetVersionPin(ctx, tenant, userID, lineageID)
	if err != nil {
		t.Fatalf("GetVersionPin: %v", err)
	}
	if !ok {
		t.Fatal("GetVersionPin: expected ok=true after SetVersionPin")
	}
	if pinned != 1 {
		t.Errorf("GetVersionPin: want 1, got %d", pinned)
	}

	resolved, err := repo.ResolveVersionForUser(ctx, tenant, userID, lineageID)
	if err != nil {
		t.Fatalf("ResolveVersionForUser (pinned): %v", err)
	}
	if resolved != 1 {
		t.Errorf("ResolveVersionForUser pinned: want 1, got %d", resolved)
	}

	// Clear the pin — should fall back to current (v2).
	if err := repo.ClearVersionPin(ctx, tenant, userID, lineageID); err != nil {
		t.Fatalf("ClearVersionPin: %v", err)
	}
	_, ok, err = repo.GetVersionPin(ctx, tenant, userID, lineageID)
	if err != nil {
		t.Fatalf("GetVersionPin after clear: %v", err)
	}
	if ok {
		t.Error("GetVersionPin after clear: expected ok=false")
	}

	resolved, err = repo.ResolveVersionForUser(ctx, tenant, userID, lineageID)
	if err != nil {
		t.Fatalf("ResolveVersionForUser (unpinned): %v", err)
	}
	if resolved != 2 {
		t.Errorf("ResolveVersionForUser unpinned: want 2 (current), got %d", resolved)
	}
}

// ── Approval-state tests (migration 026) ──────────────────────────────────────

// TestWorkflow_GetCurrentIgnoresProposed verifies that GetCurrent returns the
// highest approved version and ignores a newer proposed version.
func TestWorkflow_GetCurrentIgnoresProposed(t *testing.T) {
	pool := openTestPool(t)
	repo := NewWorkflowRepo(pool, zap.NewNop())
	ctx := context.Background()

	tenantID := uuid.New()
	lineageID := uuid.New()
	tenant := tenantID.String()

	// v1 — approved (owner save).
	v1, err := repo.CreateVersion(ctx, tenant, baseRow(tenantID, lineageID, "user-a"))
	if err != nil {
		t.Fatalf("CreateVersion v1: %v", err)
	}
	if v1.ApprovalState != "approved" {
		t.Errorf("v1 ApprovalState: want 'approved', got %q", v1.ApprovalState)
	}

	// v2 — proposed (self-improvement path).
	v2, err := repo.ProposeVersion(ctx, tenant, baseRow(tenantID, lineageID, "user-a"))
	if err != nil {
		t.Fatalf("ProposeVersion v2: %v", err)
	}
	if v2.ApprovalState != "proposed" {
		t.Errorf("v2 ApprovalState: want 'proposed', got %q", v2.ApprovalState)
	}
	if v2.Version != 2 {
		t.Errorf("v2 Version: want 2, got %d", v2.Version)
	}

	// GetCurrent must still return v1 — proposed v2 is invisible.
	current, err := repo.GetCurrent(ctx, tenant, lineageID)
	if err != nil {
		t.Fatalf("GetCurrent: %v", err)
	}
	if current.Version != 1 {
		t.Errorf("GetCurrent: want version 1 (approved), got %d", current.Version)
	}

	// ResolveVersionForUser (no pin) must also return v1.
	resolved, err := repo.ResolveVersionForUser(ctx, tenant, "user-a", lineageID)
	if err != nil {
		t.Fatalf("ResolveVersionForUser: %v", err)
	}
	if resolved != 1 {
		t.Errorf("ResolveVersionForUser: want 1 (approved current), got %d", resolved)
	}
}

// TestWorkflow_ApproveVersion verifies propose→approve flips version to current.
func TestWorkflow_ApproveVersion(t *testing.T) {
	pool := openTestPool(t)
	repo := NewWorkflowRepo(pool, zap.NewNop())
	ctx := context.Background()

	tenantID := uuid.New()
	lineageID := uuid.New()
	tenant := tenantID.String()

	// v1 approved baseline.
	if _, err := repo.CreateVersion(ctx, tenant, baseRow(tenantID, lineageID, "user-b")); err != nil {
		t.Fatalf("CreateVersion v1: %v", err)
	}

	// Propose v2.
	v2, err := repo.ProposeVersion(ctx, tenant, baseRow(tenantID, lineageID, "user-b"))
	if err != nil {
		t.Fatalf("ProposeVersion v2: %v", err)
	}

	// Approve it.
	if err := repo.ApproveVersion(ctx, tenant, lineageID, v2.Version); err != nil {
		t.Fatalf("ApproveVersion: %v", err)
	}

	// GetCurrent must now return v2.
	current, err := repo.GetCurrent(ctx, tenant, lineageID)
	if err != nil {
		t.Fatalf("GetCurrent after approve: %v", err)
	}
	if current.Version != 2 {
		t.Errorf("GetCurrent after approve: want 2, got %d", current.Version)
	}
	if current.ApprovalState != "approved" {
		t.Errorf("ApprovalState after approve: want 'approved', got %q", current.ApprovalState)
	}
	if current.ApprovedAt == nil {
		t.Error("ApprovedAt: want non-nil after approve")
	}

	// Double-approve must error (no longer 'proposed').
	if err := repo.ApproveVersion(ctx, tenant, lineageID, v2.Version); err == nil {
		t.Error("ApproveVersion on already-approved row: want error, got nil")
	}
}

// TestWorkflow_RejectVersion verifies that a rejected version stays excluded from
// GetCurrent.
func TestWorkflow_RejectVersion(t *testing.T) {
	pool := openTestPool(t)
	repo := NewWorkflowRepo(pool, zap.NewNop())
	ctx := context.Background()

	tenantID := uuid.New()
	lineageID := uuid.New()
	tenant := tenantID.String()

	// v1 approved baseline.
	if _, err := repo.CreateVersion(ctx, tenant, baseRow(tenantID, lineageID, "user-c")); err != nil {
		t.Fatalf("CreateVersion v1: %v", err)
	}

	// Propose v2.
	v2, err := repo.ProposeVersion(ctx, tenant, baseRow(tenantID, lineageID, "user-c"))
	if err != nil {
		t.Fatalf("ProposeVersion v2: %v", err)
	}

	// Reject it.
	if err := repo.RejectVersion(ctx, tenant, lineageID, v2.Version, "logic error in step 2"); err != nil {
		t.Fatalf("RejectVersion: %v", err)
	}

	// GetCurrent must still return v1 — rejected v2 is excluded.
	current, err := repo.GetCurrent(ctx, tenant, lineageID)
	if err != nil {
		t.Fatalf("GetCurrent after reject: %v", err)
	}
	if current.Version != 1 {
		t.Errorf("GetCurrent after reject: want 1, got %d", current.Version)
	}

	// ListVersions returns both — including the rejected one.
	versions, err := repo.ListVersions(ctx, tenant, lineageID, 0, 0)
	if err != nil {
		t.Fatalf("ListVersions after reject: %v", err)
	}
	if len(versions) != 2 {
		t.Fatalf("ListVersions: want 2 rows, got %d", len(versions))
	}
	// versions[0] is newest-first → v2 rejected.
	if versions[0].ApprovalState != "rejected" {
		t.Errorf("ListVersions[0] ApprovalState: want 'rejected', got %q", versions[0].ApprovalState)
	}
	if versions[0].ApprovalReason != "logic error in step 2" {
		t.Errorf("ApprovalReason: want 'logic error in step 2', got %q", versions[0].ApprovalReason)
	}
}

// TestWorkflow_ListProposals verifies that ListProposals returns only the owner's
// proposed rows and not approved/rejected ones or another owner's proposals.
func TestWorkflow_ListProposals(t *testing.T) {
	pool := openTestPool(t)
	repo := NewWorkflowRepo(pool, zap.NewNop())
	ctx := context.Background()

	tenantID := uuid.New()
	tenant := tenantID.String()
	lineageA := uuid.New()
	lineageB := uuid.New()

	// owner-d: one approved version + one proposed version on lineageA.
	if _, err := repo.CreateVersion(ctx, tenant, baseRow(tenantID, lineageA, "owner-d")); err != nil {
		t.Fatalf("CreateVersion lineageA v1: %v", err)
	}
	if _, err := repo.ProposeVersion(ctx, tenant, baseRow(tenantID, lineageA, "owner-d")); err != nil {
		t.Fatalf("ProposeVersion lineageA v2: %v", err)
	}

	// owner-e: one proposed version on lineageB (different owner).
	if _, err := repo.ProposeVersion(ctx, tenant, baseRow(tenantID, lineageB, "owner-e")); err != nil {
		t.Fatalf("ProposeVersion lineageB v1: %v", err)
	}

	// ListProposals for owner-d: must return exactly the one proposed row on lineageA.
	proposals, err := repo.ListProposals(ctx, tenant, "owner-d")
	if err != nil {
		t.Fatalf("ListProposals owner-d: %v", err)
	}
	if len(proposals) != 1 {
		t.Fatalf("ListProposals owner-d: want 1, got %d", len(proposals))
	}
	if proposals[0].ApprovalState != "proposed" {
		t.Errorf("ListProposals[0] ApprovalState: want 'proposed', got %q", proposals[0].ApprovalState)
	}
	if proposals[0].LineageID != lineageA {
		t.Errorf("ListProposals[0] LineageID: want lineageA")
	}

	// ListProposals for owner-e: one proposal on lineageB.
	proposalsE, err := repo.ListProposals(ctx, tenant, "owner-e")
	if err != nil {
		t.Fatalf("ListProposals owner-e: %v", err)
	}
	if len(proposalsE) != 1 {
		t.Fatalf("ListProposals owner-e: want 1, got %d", len(proposalsE))
	}
}

// TestWorkflow_DefaultApprovalState verifies that a WorkflowRow with an empty
// ApprovalState persists as 'approved' (the DB DEFAULT).
func TestWorkflow_DefaultApprovalState(t *testing.T) {
	pool := openTestPool(t)
	repo := NewWorkflowRepo(pool, zap.NewNop())
	ctx := context.Background()

	tenantID := uuid.New()
	lineageID := uuid.New()
	tenant := tenantID.String()

	row := baseRow(tenantID, lineageID, "user-f")
	// ApprovalState deliberately left empty — caller doesn't set it.
	out, err := repo.CreateVersion(ctx, tenant, row)
	if err != nil {
		t.Fatalf("CreateVersion: %v", err)
	}
	if out.ApprovalState != "approved" {
		t.Errorf("ApprovalState: want 'approved' for empty input, got %q", out.ApprovalState)
	}
}

// TestWorkflow_ListByOwner_ProposedNotRepresentative verifies that ListByOwner
// represents each lineage by its highest *approved* version. A proposed version
// higher than the current approved one must not surface as the representative row.
func TestWorkflow_ListByOwner_ProposedNotRepresentative(t *testing.T) {
	pool := openTestPool(t)
	repo := NewWorkflowRepo(pool, zap.NewNop())
	ctx := context.Background()

	tenantID := uuid.New()
	lineageID := uuid.New()
	tenant := tenantID.String()
	// Unique owner per run: ListByOwner filters by owner_user_id and RLS is
	// bypassed for the test role, so a fixed owner id would collide with rows
	// left by prior runs against the shared dev DB.
	owner := "owner-" + uuid.NewString()

	// v1 approved (CreateVersion defaults approval_state='approved').
	v1, err := repo.CreateVersion(ctx, tenant, baseRow(tenantID, lineageID, owner))
	if err != nil {
		t.Fatalf("CreateVersion v1: %v", err)
	}
	// v2 proposed — not yet approved; must NOT become the representative row.
	if _, err := repo.ProposeVersion(ctx, tenant, baseRow(tenantID, lineageID, owner)); err != nil {
		t.Fatalf("ProposeVersion v2: %v", err)
	}

	rows, err := repo.ListByOwner(ctx, tenant, owner, "", 0)
	if err != nil {
		t.Fatalf("ListByOwner: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ListByOwner: want 1 lineage row, got %d", len(rows))
	}
	got := rows[0]
	if got.Version != v1.Version {
		t.Errorf("ListByOwner: want version %d (highest approved), got %d", v1.Version, got.Version)
	}
	if got.ApprovalState != "approved" {
		t.Errorf("ListByOwner ApprovalState: want 'approved', got %q", got.ApprovalState)
	}
}

// TestWorkflow_ListByOwner_TwoApprovedVersions verifies that when multiple
// approved versions exist the highest one is returned as the representative.
func TestWorkflow_ListByOwner_TwoApprovedVersions(t *testing.T) {
	pool := openTestPool(t)
	repo := NewWorkflowRepo(pool, zap.NewNop())
	ctx := context.Background()

	tenantID := uuid.New()
	lineageID := uuid.New()
	tenant := tenantID.String()
	// Unique owner per run (see ProposedNotRepresentative for why).
	owner := "owner-" + uuid.NewString()

	// v1 approved.
	if _, err := repo.CreateVersion(ctx, tenant, baseRow(tenantID, lineageID, owner)); err != nil {
		t.Fatalf("CreateVersion v1: %v", err)
	}
	// v2 approved.
	v2, err := repo.CreateVersion(ctx, tenant, baseRow(tenantID, lineageID, owner))
	if err != nil {
		t.Fatalf("CreateVersion v2: %v", err)
	}

	rows, err := repo.ListByOwner(ctx, tenant, owner, "", 0)
	if err != nil {
		t.Fatalf("ListByOwner: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ListByOwner: want 1 lineage row, got %d", len(rows))
	}
	got := rows[0]
	if got.Version != v2.Version {
		t.Errorf("ListByOwner: want version %d (highest approved), got %d", v2.Version, got.Version)
	}
	if got.ApprovalState != "approved" {
		t.Errorf("ListByOwner ApprovalState: want 'approved', got %q", got.ApprovalState)
	}
}

// TestWorkflow_BoundAgentIDRoundTrip verifies bound_agent_id (migration 038)
// persists through CreateVersion and reads back via GetCurrent. Requires a
// migrated DB (migration 038 applied); skips without TEST_DATABASE_URL.
func TestWorkflow_BoundAgentIDRoundTrip(t *testing.T) {
	pool := openTestPool(t)
	repo := NewWorkflowRepo(pool, zap.NewNop())
	ctx := context.Background()

	tenantID := uuid.New()
	lineageID := uuid.New()
	agentID := uuid.New()
	tenant := tenantID.String()

	row := baseRow(tenantID, lineageID, "user-a")
	row.BoundAgentID = &agentID
	if _, err := repo.CreateVersion(ctx, tenant, row); err != nil {
		t.Fatalf("CreateVersion: %v", err)
	}

	current, err := repo.GetCurrent(ctx, tenant, lineageID)
	if err != nil {
		t.Fatalf("GetCurrent: %v", err)
	}
	if current.BoundAgentID == nil || *current.BoundAgentID != agentID {
		t.Errorf("bound_agent_id round-trip: want %s, got %v", agentID, current.BoundAgentID)
	}
}

// TestWorkflow_ListByOwner_KeysetAndLimit verifies the SQL keyset+LIMIT bounding
// (batch-1 pagination): with three owned lineages, a limit caps distinct
// lineages and an afterLineage cursor skips everything at or before it.
func TestWorkflow_ListByOwner_KeysetAndLimit(t *testing.T) {
	pool := openTestPool(t)
	repo := NewWorkflowRepo(pool, zap.NewNop())
	ctx := context.Background()

	tenantID := uuid.New()
	tenant := tenantID.String()
	owner := "keyset-owner-" + uuid.NewString() // unique so RLS+owner filter isolate this test

	var lineages []uuid.UUID
	for i := 0; i < 3; i++ {
		lid := uuid.New()
		lineages = append(lineages, lid)
		if _, err := repo.CreateVersion(ctx, tenant, baseRow(tenantID, lid, owner)); err != nil {
			t.Fatalf("CreateVersion: %v", err)
		}
	}
	sort.Slice(lineages, func(i, j int) bool { return lineages[i].String() < lineages[j].String() })

	// limit=2 → first two lineages in ascending order.
	page, err := repo.ListByOwner(ctx, tenant, owner, "", 2)
	if err != nil {
		t.Fatalf("ListByOwner limit=2: %v", err)
	}
	if len(page) != 2 {
		t.Fatalf("limit=2: want 2 rows, got %d", len(page))
	}
	if page[0].LineageID != lineages[0] || page[1].LineageID != lineages[1] {
		t.Errorf("limit=2 order: want [%s %s], got [%s %s]", lineages[0], lineages[1], page[0].LineageID, page[1].LineageID)
	}

	// cursor = first lineage → skip it, keyset returns the remaining two.
	after, err := repo.ListByOwner(ctx, tenant, owner, lineages[0].String(), 0)
	if err != nil {
		t.Fatalf("ListByOwner keyset: %v", err)
	}
	if len(after) != 2 || after[0].LineageID != lineages[1] {
		t.Errorf("keyset after %s: want 2 rows starting %s, got %d rows", lineages[0], lineages[1], len(after))
	}
}

// TestWorkflow_ListVersions_KeysetAndLimit verifies ListVersions' SQL
// keyset (beforeVersion) + LIMIT bounding against a live DB.
func TestWorkflow_ListVersions_KeysetAndLimit(t *testing.T) {
	pool := openTestPool(t)
	repo := NewWorkflowRepo(pool, zap.NewNop())
	ctx := context.Background()

	tenantID := uuid.New()
	tenant := tenantID.String()
	lineageID := uuid.New()
	for i := 1; i <= 3; i++ {
		if _, err := repo.CreateVersion(ctx, tenant, baseRow(tenantID, lineageID, "user-a")); err != nil {
			t.Fatalf("CreateVersion %d: %v", i, err)
		}
	}

	// limit=2, no cursor → [3, 2] (newest first).
	page, err := repo.ListVersions(ctx, tenant, lineageID, 0, 2)
	if err != nil {
		t.Fatalf("ListVersions limit=2: %v", err)
	}
	if len(page) != 2 || page[0].Version != 3 || page[1].Version != 2 {
		t.Fatalf("limit=2: want [3 2], got %v", page)
	}

	// beforeVersion=2 → only version 1 remains.
	after, err := repo.ListVersions(ctx, tenant, lineageID, 2, 0)
	if err != nil {
		t.Fatalf("ListVersions beforeVersion=2: %v", err)
	}
	if len(after) != 1 || after[0].Version != 1 {
		t.Fatalf("beforeVersion=2: want [1], got %v", after)
	}
}
