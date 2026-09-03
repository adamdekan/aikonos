package broker

// F9 tests: BeginWorkflowRun north RPC — the agent-bound workflow run gate.
//
// An unbound (personal) lineage returns an empty response and the caller keeps
// the legacy personal path. A bound lineage runs only for a caller who may
// operate its agent (can_use = usable_by or owner_user, tenant-admin fallback);
// on allow the broker mints an owner grant whose owner is the requesting USER
// (task ownership stays human) and returns the bound agent id.
//
// FGA is exercised through a real policy.Engine backed by fakeFGA so the
// MayOperateAgent CheckFGA call path is genuinely verified.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/gatewaygrant"
	"github.com/adamdekan/aikonos/broker/internal/policy"
	"github.com/adamdekan/aikonos/broker/internal/workflowsvc"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

const (
	beginRunUser  = "alice@example.com"
	beginRunOther = "bob@example.com" // a lineage owner who is NOT the caller
)

// beginRunStore returns a fixed row (or error) from GetCurrent; every other
// Store method is the inert stub.
type beginRunStore struct {
	stubWorkflowStore
	row *db.WorkflowRow
	err error
}

func (s beginRunStore) GetCurrent(context.Context, string, uuid.UUID) (*db.WorkflowRow, error) {
	return s.row, s.err
}

// newBeginRunSvc builds a BrokerService for BeginWorkflowRun tests. A nil fga
// leaves Policy nil (the FGA-disabled posture — no skill gate, no agent gate).
func newBeginRunSvc(t *testing.T, fga *fakeFGA, store workflowsvc.Store) *BrokerService {
	t.Helper()
	em, err := audit.NewEmitter(context.Background(), audit.Config{})
	if err != nil {
		t.Fatalf("audit.NewEmitter: %v", err)
	}
	var eng *policy.Engine
	if fga != nil {
		srv := fga.server(t)
		t.Cleanup(srv.Close)
		eng, err = policy.NewEngine(context.Background(), policy.Config{
			OPAEndpoint:     "http://unused",
			OpenFGAEndpoint: srv.URL,
			OpenFGAStoreID:  "store-1",
		})
		if err != nil {
			t.Fatalf("policy.NewEngine: %v", err)
		}
	}
	return NewBrokerService(Deps{
		Logger:          zap.NewNop(),
		Policy:          eng,
		Audit:           em,
		TenantID:        "aikonos-dev",
		Workflows:       store,
		GatewayGrantKey: testGrantKey,
		GatewayGrantTTL: testGrantTTL,
	})
}

func boundRow(agentID uuid.UUID) *db.WorkflowRow {
	return &db.WorkflowRow{OwnerUserID: beginRunUser, BoundAgentID: &agentID}
}

// TestBeginWorkflowRun_Unbound proves an unbound lineage returns an empty
// response (both fields blank) so the caller keeps the legacy personal path.
func TestBeginWorkflowRun_Unbound(t *testing.T) {
	fga := &fakeFGA{checks: map[string]bool{
		"user:" + beginRunUser + "|can_invoke|skill:workflows": true,
	}}
	svc := newBeginRunSvc(t, fga, beginRunStore{row: &db.WorkflowRow{OwnerUserID: beginRunUser}})

	ctx := ctxWithSubject(testWFTenant, beginRunUser, "")
	resp, err := svc.BeginWorkflowRun(ctx, &brokerv1.BeginWorkflowRunRequest{
		TenantId:  testWFTenant,
		LineageId: uuid.New().String(),
	})
	if err != nil {
		t.Fatalf("BeginWorkflowRun: %v", err)
	}
	if resp.OwnerGrant != "" || resp.BoundAgentId != "" {
		t.Fatalf("unbound: want empty response, got grant=%q agent=%q", resp.OwnerGrant, resp.BoundAgentId)
	}
}

// TestBeginWorkflowRun_BoundAllowed proves a bound lineage the caller may
// operate returns a verifiable owner grant (owner = the requesting user) and
// the bound agent id.
func TestBeginWorkflowRun_BoundAllowed(t *testing.T) {
	agentID := uuid.New()
	fga := &fakeFGA{checks: map[string]bool{
		"user:" + beginRunUser + "|can_invoke|skill:workflows":        true,
		"user:" + beginRunUser + "|can_use|agent:" + agentID.String(): true,
	}}
	svc := newBeginRunSvc(t, fga, beginRunStore{row: boundRow(agentID)})

	ctx := ctxWithSubject(testWFTenant, beginRunUser, "")
	resp, err := svc.BeginWorkflowRun(ctx, &brokerv1.BeginWorkflowRunRequest{
		TenantId:  testWFTenant,
		LineageId: uuid.New().String(),
	})
	if err != nil {
		t.Fatalf("BeginWorkflowRun: %v", err)
	}
	if resp.OwnerGrant == "" {
		t.Fatal("bound+allowed: want non-empty owner grant")
	}
	gt, gu, verr := gatewaygrant.Verify(testGrantKey, resp.OwnerGrant)
	if verr != nil {
		t.Fatalf("grant does not verify: %v", verr)
	}
	if gt != testWFTenant {
		t.Errorf("grant tenant: want %q, got %q", testWFTenant, gt)
	}
	if gu != beginRunUser {
		t.Errorf("grant owner: want the requesting user %q, got %q", beginRunUser, gu)
	}
	if resp.BoundAgentId != agentID.String() {
		t.Errorf("bound_agent_id: want %q, got %q", agentID.String(), resp.BoundAgentId)
	}
}

// TestBeginWorkflowRun_BoundDenied proves a caller who may not operate the
// bound agent (no can_use, not admin) is refused with PermissionDenied and a
// human-readable message naming the agent.
func TestBeginWorkflowRun_BoundDenied(t *testing.T) {
	agentID := uuid.New()
	fga := &fakeFGA{checks: map[string]bool{
		// Skill gate passes so the deny provably comes from the agent gate.
		"user:" + beginRunUser + "|can_invoke|skill:workflows": true,
	}}
	svc := newBeginRunSvc(t, fga, beginRunStore{row: boundRow(agentID)})

	ctx := ctxWithSubject(testWFTenant, beginRunUser, "")
	_, err := svc.BeginWorkflowRun(ctx, &brokerv1.BeginWorkflowRunRequest{
		TenantId:  testWFTenant,
		LineageId: uuid.New().String(),
	})
	if got := status.Code(err); got != codes.PermissionDenied {
		t.Fatalf("want PermissionDenied, got %v (err: %v)", got, err)
	}
	if !strings.Contains(err.Error(), agentID.String()) {
		t.Errorf("deny message should name the agent %q, got %q", agentID.String(), err.Error())
	}
}

// TestBeginWorkflowRun_BoundFGADisabled proves that with FGA disabled the gate
// is allow-all and the grant is minted for a bound lineage.
func TestBeginWorkflowRun_BoundFGADisabled(t *testing.T) {
	agentID := uuid.New()
	svc := newBeginRunSvc(t, nil, beginRunStore{row: boundRow(agentID)}) // nil fga → Policy nil → FGA disabled

	ctx := ctxWithSubject(testWFTenant, beginRunUser, "")
	resp, err := svc.BeginWorkflowRun(ctx, &brokerv1.BeginWorkflowRunRequest{
		TenantId:  testWFTenant,
		LineageId: uuid.New().String(),
	})
	if err != nil {
		t.Fatalf("BeginWorkflowRun (FGA disabled): %v", err)
	}
	if resp.OwnerGrant == "" {
		t.Fatal("FGA disabled: want a minted grant")
	}
	if resp.BoundAgentId != agentID.String() {
		t.Errorf("bound_agent_id: want %q, got %q", agentID.String(), resp.BoundAgentId)
	}
}

// TestBeginWorkflowRun_UnknownLineage proves a missing lineage maps to NotFound.
func TestBeginWorkflowRun_UnknownLineage(t *testing.T) {
	fga := &fakeFGA{checks: map[string]bool{
		"user:" + beginRunUser + "|can_invoke|skill:workflows": true,
	}}
	svc := newBeginRunSvc(t, fga, beginRunStore{err: errStubNotOverridden})

	ctx := ctxWithSubject(testWFTenant, beginRunUser, "")
	_, err := svc.BeginWorkflowRun(ctx, &brokerv1.BeginWorkflowRunRequest{
		TenantId:  testWFTenant,
		LineageId: uuid.New().String(),
	})
	if got := status.Code(err); got != codes.NotFound {
		t.Fatalf("want NotFound, got %v (err: %v)", got, err)
	}
}

// TestBeginWorkflowRun_SkillDenied proves the skill:workflows preamble gate
// still fires: a caller lacking it is refused before any agent lookup.
func TestBeginWorkflowRun_SkillDenied(t *testing.T) {
	fga := &fakeFGA{checks: map[string]bool{}} // nothing granted, no admins
	svc := newBeginRunSvc(t, fga, beginRunStore{row: boundRow(uuid.New())})

	ctx := ctxWithSubject(testWFTenant, beginRunUser, "")
	_, err := svc.BeginWorkflowRun(ctx, &brokerv1.BeginWorkflowRunRequest{
		TenantId:  testWFTenant,
		LineageId: uuid.New().String(),
	})
	if got := status.Code(err); got != codes.PermissionDenied {
		t.Fatalf("want PermissionDenied for missing skill:workflows, got %v (err: %v)", got, err)
	}
}

// otherOwnerRow builds a lineage owned by beginRunOther (not the caller), with
// the given visibility, optional JSONB group array, and optional agent binding.
func otherOwnerRow(vis, groupsJSON string, agentID *uuid.UUID) *db.WorkflowRow {
	r := &db.WorkflowRow{OwnerUserID: beginRunOther, VisibilityKind: vis, BoundAgentID: agentID}
	if groupsJSON != "" {
		r.VisibilityGroups = json.RawMessage(groupsJSON)
	}
	return r
}

// TestBeginWorkflowRun_NonOwnerPrivateBound_Denied proves the workflow-dimension
// gate is enforced independently of the agent gate: a non-owner who MAY operate
// the bound agent (can_use granted) is still refused a private lineage owned by
// someone else. Without the workflow gate the agent gate alone would allow it.
func TestBeginWorkflowRun_NonOwnerPrivateBound_Denied(t *testing.T) {
	agentID := uuid.New()
	fga := &fakeFGA{checks: map[string]bool{
		"user:" + beginRunUser + "|can_invoke|skill:workflows":        true,
		"user:" + beginRunUser + "|can_use|agent:" + agentID.String(): true, // agent gate WOULD pass
	}}
	svc := newBeginRunSvc(t, fga, beginRunStore{row: otherOwnerRow("private", "", &agentID)})

	ctx := ctxWithSubject(testWFTenant, beginRunUser, "")
	_, err := svc.BeginWorkflowRun(ctx, &brokerv1.BeginWorkflowRunRequest{
		TenantId:  testWFTenant,
		LineageId: uuid.New().String(),
	})
	if got := status.Code(err); got != codes.PermissionDenied {
		t.Fatalf("want PermissionDenied, got %v (err: %v)", got, err)
	}
	if !strings.Contains(err.Error(), "access to this workflow") {
		t.Errorf("deny should name the workflow-access failure, got %q", err.Error())
	}
}

// TestBeginWorkflowRun_NonOwnerSharedBoundMember_Allowed proves a non-owner who
// is a member of a visibility group of a shared bound workflow (and may operate
// the agent) gets a minted grant.
func TestBeginWorkflowRun_NonOwnerSharedBoundMember_Allowed(t *testing.T) {
	agentID := uuid.New()
	fga := &fakeFGA{checks: map[string]bool{
		"user:" + beginRunUser + "|can_invoke|skill:workflows":        true,
		"user:" + beginRunUser + "|can_use|agent:" + agentID.String(): true,
		"user:" + beginRunUser + "|member|group:eng":                  true,
	}}
	svc := newBeginRunSvc(t, fga, beginRunStore{row: otherOwnerRow("shared", `["eng"]`, &agentID)})

	ctx := ctxWithSubject(testWFTenant, beginRunUser, "")
	resp, err := svc.BeginWorkflowRun(ctx, &brokerv1.BeginWorkflowRunRequest{
		TenantId:  testWFTenant,
		LineageId: uuid.New().String(),
	})
	if err != nil {
		t.Fatalf("BeginWorkflowRun: %v", err)
	}
	if resp.OwnerGrant == "" {
		t.Fatal("shared+member+can_use: want a minted grant")
	}
	if resp.BoundAgentId != agentID.String() {
		t.Errorf("bound_agent_id: want %q, got %q", agentID.String(), resp.BoundAgentId)
	}
}

// TestBeginWorkflowRun_NonOwnerSharedBoundNonMember_Denied proves a non-owner
// who is NOT a member of any visibility group is refused even though the agent
// gate (can_use) would pass — the workflow gate is the one that denies.
func TestBeginWorkflowRun_NonOwnerSharedBoundNonMember_Denied(t *testing.T) {
	agentID := uuid.New()
	fga := &fakeFGA{checks: map[string]bool{
		"user:" + beginRunUser + "|can_invoke|skill:workflows":        true,
		"user:" + beginRunUser + "|can_use|agent:" + agentID.String(): true, // agent gate WOULD pass
		// no member grant for group:eng
	}}
	svc := newBeginRunSvc(t, fga, beginRunStore{row: otherOwnerRow("shared", `["eng"]`, &agentID)})

	ctx := ctxWithSubject(testWFTenant, beginRunUser, "")
	_, err := svc.BeginWorkflowRun(ctx, &brokerv1.BeginWorkflowRunRequest{
		TenantId:  testWFTenant,
		LineageId: uuid.New().String(),
	})
	if got := status.Code(err); got != codes.PermissionDenied {
		t.Fatalf("want PermissionDenied, got %v (err: %v)", got, err)
	}
	if !strings.Contains(err.Error(), "access to this workflow") {
		t.Errorf("deny should name the workflow-access failure, got %q", err.Error())
	}
}

// TestBeginWorkflowRun_OwnerPrivateBound_Allowed proves the owner of a private
// bound lineage is unaffected by the new workflow gate — grant still minted.
func TestBeginWorkflowRun_OwnerPrivateBound_Allowed(t *testing.T) {
	agentID := uuid.New()
	fga := &fakeFGA{checks: map[string]bool{
		"user:" + beginRunUser + "|can_invoke|skill:workflows":        true,
		"user:" + beginRunUser + "|can_use|agent:" + agentID.String(): true,
	}}
	// Owner == caller; visibility private.
	row := &db.WorkflowRow{OwnerUserID: beginRunUser, VisibilityKind: "private", BoundAgentID: &agentID}
	svc := newBeginRunSvc(t, fga, beginRunStore{row: row})

	ctx := ctxWithSubject(testWFTenant, beginRunUser, "")
	resp, err := svc.BeginWorkflowRun(ctx, &brokerv1.BeginWorkflowRunRequest{
		TenantId:  testWFTenant,
		LineageId: uuid.New().String(),
	})
	if err != nil {
		t.Fatalf("BeginWorkflowRun: %v", err)
	}
	if resp.OwnerGrant == "" {
		t.Fatal("owner+private+bound: want a minted grant")
	}
}

// TestBeginWorkflowRun_NonOwnerPrivateUnbound_Denied proves the workflow gate
// runs BEFORE the unbound branch: a non-owner's private UNBOUND lineage is
// refused, not returned as the empty personal-path response (which would let
// the gateway disclose+run it on the personal path).
func TestBeginWorkflowRun_NonOwnerPrivateUnbound_Denied(t *testing.T) {
	fga := &fakeFGA{checks: map[string]bool{
		"user:" + beginRunUser + "|can_invoke|skill:workflows": true,
	}}
	svc := newBeginRunSvc(t, fga, beginRunStore{row: otherOwnerRow("private", "", nil)}) // unbound

	ctx := ctxWithSubject(testWFTenant, beginRunUser, "")
	resp, err := svc.BeginWorkflowRun(ctx, &brokerv1.BeginWorkflowRunRequest{
		TenantId:  testWFTenant,
		LineageId: uuid.New().String(),
	})
	if err == nil {
		t.Fatalf("want PermissionDenied, got empty personal-path response: grant=%q agent=%q", resp.OwnerGrant, resp.BoundAgentId)
	}
	if got := status.Code(err); got != codes.PermissionDenied {
		t.Fatalf("want PermissionDenied, got %v (err: %v)", got, err)
	}
	if !strings.Contains(err.Error(), "access to this workflow") {
		t.Errorf("deny should name the workflow-access failure, got %q", err.Error())
	}
}
