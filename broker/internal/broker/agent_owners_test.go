package broker

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/revocation"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// AG-04: admins add and remove agent owners (users and groups); the last owner
// cannot be removed, and every owner change is audited.
func TestAgentOwners_AddRemoveLastOwnerAndAudit(t *testing.T) {
	const obj = "agent:" + agentAID
	f := &fakeFGA{
		admins: map[string]bool{"user:admin@example.com": true},
		reads: map[string][]fgaKey{obj: {
			{User: "user:alice@example.com", Relation: "owner_user", Object: obj},
			{User: "user:bob@example.com", Relation: "usable_by", Object: obj},
		}},
	}
	srv := f.server(t)
	defer srv.Close()
	deps := testAdminDeps(t, srv.URL)
	aud := &fakeInvokeAudit{}
	deps.Audit = aud
	svc := NewBrokerService(deps)
	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")

	// A group can own an agent.
	if _, err := svc.AssignRole(ctx, &brokerv1.AssignRoleRequest{Tuple: tuple("group:ops#member", "owner_group", obj)}); err != nil {
		t.Fatalf("assign owner_group: %v", err)
	}
	if e := findEvent(aud.events, "aikonos.broker.agent.owner.added"); e == nil || e.Context.AsMap()["user"] != "group:ops#member" {
		t.Fatalf("owner.added event = %v", e)
	}
	// owner_group takes a group member-set only.
	if _, err := svc.AssignRole(ctx, &brokerv1.AssignRoleRequest{Tuple: tuple("user:eve@example.com", "owner_group", obj)}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("user as owner_group: want InvalidArgument, got %v", err)
	}

	// alice is the only owner of record (the fake does not apply writes): refused.
	_, err := svc.RevokeRole(ctx, &brokerv1.RevokeRoleRequest{Tuple: tuple("user:alice@example.com", "owner_user", obj)})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("last owner: want FailedPrecondition, got %v", err)
	}
	if len(f.deletes) != 0 {
		t.Fatalf("refused removal must not delete: %v", f.deletes)
	}
	if e := findEvent(aud.events, "aikonos.broker.agent.owner.removed"); e == nil || e.Decision != auditv1.PolicyDecision_DENY {
		t.Fatalf("refusal must be audited as DENY, got %v", e)
	}

	// With a second owner, removing one is allowed and audited.
	f.reads[obj] = append(f.reads[obj], fgaKey{User: "group:ops#member", Relation: "owner_group", Object: obj})
	aud.events = nil
	if _, err := svc.RevokeRole(ctx, &brokerv1.RevokeRoleRequest{Tuple: tuple("user:alice@example.com", "owner_user", obj)}); err != nil {
		t.Fatalf("remove one of two owners: %v", err)
	}
	if len(f.deletes) != 1 {
		t.Fatalf("want one delete, got %v", f.deletes)
	}
	if e := findEvent(aud.events, "aikonos.broker.agent.owner.removed"); e == nil || e.Decision != auditv1.PolicyDecision_ALLOW {
		t.Fatalf("owner.removed event = %v", e)
	}

	// Owner reads fail closed.
	f.readErr = true
	if _, err := svc.RevokeRole(ctx, &brokerv1.RevokeRoleRequest{Tuple: tuple("group:ops#member", "owner_group", obj)}); status.Code(err) != codes.Internal {
		t.Fatalf("unreadable owners: want Internal, got %v", err)
	}
	if len(f.deletes) != 1 {
		t.Fatalf("refused removal must not delete: %v", f.deletes)
	}
}

// AG-04: ListAgents flags an agent with no owner or only blocked user owners;
// a group owner or one active user owner keeps it accountable.
func TestListAgents_OrphanedFlag(t *testing.T) {
	const (
		blockedOnly = "aaaaaaaa-0000-0000-0000-000000000011"
		oneActive   = "aaaaaaaa-0000-0000-0000-000000000012"
		groupOwned  = "aaaaaaaa-0000-0000-0000-000000000013"
		noOwner     = "aaaaaaaa-0000-0000-0000-000000000014"
	)
	store := &fakeAgentStore{}
	for _, id := range []string{blockedOnly, oneActive, groupOwned, noOwner} {
		store.agents = append(store.agents, makeTestAgent(t, agentTestTenantUUID, id, id))
	}
	owner := func(id, user, rel string) fgaKey { return fgaKey{User: user, Relation: rel, Object: "agent:" + id} }
	f := &fakeFGA{
		admins: map[string]bool{"user:admin@example.com": true},
		reads: map[string][]fgaKey{
			"agent:" + blockedOnly: {owner(blockedOnly, "user:bob", "owner_user"), owner(blockedOnly, "user:carol", "usable_by")},
			"agent:" + oneActive:   {owner(oneActive, "user:bob", "owner_user"), owner(oneActive, "user:carol", "owner_user")},
			"agent:" + groupOwned:  {owner(groupOwned, "user:bob", "owner_user"), owner(groupOwned, "group:ops#member", "owner_group")},
		},
	}
	deps := newListMyAgentsDeps(t, f, store)
	rev := newFakeRevStore()
	rev.blocks["bob"] = db.UserBlock{Reason: "left"}
	deps.Revocation = revocation.New(rev)
	svc := NewBrokerService(deps)

	resp, err := svc.ListAgents(ctxWithIdentity(agentTestTenantUUID, "admin@example.com"), &brokerv1.ListAgentsRequest{})
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	want := map[string]bool{blockedOnly: true, oneActive: false, groupOwned: false, noOwner: true}
	for _, a := range resp.Agents {
		if a.Orphaned != want[a.Id] {
			t.Errorf("agent %s orphaned = %v, want %v", a.Id, a.Orphaned, want[a.Id])
		}
		if a.Id == blockedOnly && (len(a.UsableBy) != 1 || a.UsableBy[0] != "user:carol") {
			t.Errorf("usable_by = %v, want only user:carol", a.UsableBy)
		}
	}
}

// AG-04: the startup backfill grants the creator owner_user on agents without
// any owner tuple, skips owned and creator-less agents, and audits each grant.
func TestBackfillAgentOwners(t *testing.T) {
	const (
		unowned   = "aaaaaaaa-0000-0000-0000-000000000021"
		owned     = "aaaaaaaa-0000-0000-0000-000000000022"
		noCreator = "aaaaaaaa-0000-0000-0000-000000000023"
	)
	store := &fakeAgentStore{}
	for id, by := range map[string]string{unowned: "alice", owned: "bob", noCreator: ""} {
		a := makeTestAgent(t, agentTestTenantUUID, id, id)
		a.CreatedBy = by
		store.agents = append(store.agents, a)
	}
	f := &fakeFGA{reads: map[string][]fgaKey{
		"agent:" + owned: {{User: "group:ops#member", Relation: "owner_group", Object: "agent:" + owned}},
	}}
	deps := newListMyAgentsDeps(t, f, store)
	aud := &fakeInvokeAudit{}
	deps.Audit = aud
	svc := NewBrokerService(deps)

	n, err := svc.BackfillAgentOwners(context.Background(), agentTestTenantUUID)
	if err != nil || n != 1 {
		t.Fatalf("backfill: n=%d err=%v", n, err)
	}
	if len(f.writes) != 1 {
		t.Fatalf("want one write, got %v", f.writes)
	}
	if u, rel, obj := tupleKey(f.writes[0]); u != "user:alice" || rel != "owner_user" || obj != "agent:"+unowned {
		t.Fatalf("wrote %s %s %s", u, rel, obj)
	}
	if e := findEvent(aud.events, "aikonos.broker.agent.owner.added"); e == nil || e.ActorUserId != "system:owner-backfill" {
		t.Fatalf("backfill audit event = %v", e)
	}
}
