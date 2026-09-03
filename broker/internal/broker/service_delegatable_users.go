package broker

import (
	"context"
	"sort"
	"strings"

	"go.uber.org/zap"

	"github.com/adamdekan/aikonos/broker/internal/policy"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// ListDelegatableUsers returns the users and groups the caller may delegate to.
// Users: the union of members across every delegatable group the caller belongs to,
// minus the caller. Groups: one entry per delegatable group with member_count
// = members excluding the caller (groups with zero other members are omitted).
//
// Discovery only — never trusted for authorization (SendEnvelope re-checks). Fails
// closed: any FGA error yields an empty response, never a client-facing error, because
// a degraded directory must not break the chat composer.
func (s *BrokerService) ListDelegatableUsers(ctx context.Context, req *brokerv1.ListDelegatableUsersRequest) (*brokerv1.ListDelegatableUsersResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.envelope.delegatable_users")
	defer span.End()

	tenant, user, err := s.callerIdentity(ctx, req.TenantId, req.UserId)
	if err != nil {
		return nil, err
	}

	empty := &brokerv1.ListDelegatableUsersResponse{}

	myGroups, err := s.deps.Policy.ListObjects(ctx, "user:"+user, "delegatable_member", "group")
	if err != nil || len(myGroups) == 0 {
		return empty, nil
	}

	// Union the user members of every delegatable group the caller is in. EqualFold
	// mirrors the self-delegation guard in SendEnvelope (OIDC subjects are lowercased
	// upstream; defense in depth against a mixed-case subject self-listing).
	peers := make(map[string]struct{})
	var groups []*brokerv1.DelegatableGroup
	for _, g := range myGroups {
		members, rerr := s.groupMembersExcluding(ctx, g, user)
		if rerr != nil {
			continue // fail closed for this group; other groups still contribute
		}
		for _, m := range members {
			peers[m] = struct{}{}
		}
		if len(members) == 0 {
			continue // omit groups with no other members
		}
		groups = append(groups, &brokerv1.DelegatableGroup{
			GroupId:     g,
			DisplayName: g, // best-effort; no group directory wired yet
			MemberCount: int32(len(members)),
		})
	}
	// Sort groups for deterministic output.
	sort.Slice(groups, func(i, j int) bool { return groups[i].GroupId < groups[j].GroupId })

	names := s.userDisplayNames(ctx, tenant)
	out := make([]*brokerv1.DelegatableUser, 0, len(peers))
	for id := range peers {
		display := id
		if n := names[id]; n != "" {
			display = n
		}
		out = append(out, &brokerv1.DelegatableUser{UserId: id, DisplayName: display})
	}
	// Sort for a stable popover order and deterministic tests.
	sort.Slice(out, func(i, j int) bool { return out[i].UserId < out[j].UserId })

	return &brokerv1.ListDelegatableUsersResponse{Users: out, Groups: groups}, nil
}

// groupMembersExcluding returns the user subjects that are members of groupID,
// excluding the given caller subject (EqualFold). CP2 reuses this helper for
// fan-out in SendEnvelope. Returns nil on FGA error (fail closed).
func (s *BrokerService) groupMembersExcluding(ctx context.Context, groupID, excludeUser string) ([]string, error) {
	tuples, err := s.deps.Policy.ReadTuples(ctx, policy.ReadFilter{Relation: "member", Object: groupID})
	if err != nil {
		return nil, err
	}
	var members []string
	for _, tpl := range tuples {
		if tpl.Relation != "member" {
			continue
		}
		subj, ok := strings.CutPrefix(tpl.User, "user:")
		if !ok || subj == "" || strings.EqualFold(subj, excludeUser) {
			continue
		}
		members = append(members, subj)
	}
	return members, nil
}

// userDisplayNames returns subject→display-name from the directory, best effort.
// nil-safe + error-tolerant: labels are cosmetic, so a miss falls back to user id.
func (s *BrokerService) userDisplayNames(ctx context.Context, tenant string) map[string]string {
	if s.deps.UserDirectory == nil {
		return nil
	}
	entries, err := s.deps.UserDirectory.ListByTenant(ctx, tenant)
	if err != nil {
		s.deps.Logger.Warn("ListDelegatableUsers: user_directory load failed", zap.Error(err))
		return nil
	}
	names := make(map[string]string, len(entries))
	for _, e := range entries {
		if e.Name != "" {
			names[e.Subject] = e.Name
		}
	}
	return names
}
