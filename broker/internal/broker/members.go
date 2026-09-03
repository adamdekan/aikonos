package broker

import (
	"context"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/policy"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// ListMembers (A3) returns the unified members roster: every directory row for
// the tenant with provisioning origin and admin status. Read-only; tenant-admin
// gated. Admin status is resolved with a single FGA ReadTuples call for the
// tenant "admin" relation, not one CheckFGA per user.
func (s *BrokerService) ListMembers(ctx context.Context, req *brokerv1.ListMembersRequest) (*brokerv1.ListMembersResponse, error) {
	tenant, user, err := s.callerIdentity(ctx, req.TenantId, "")
	if err != nil {
		return nil, err
	}
	if err := s.requireTenantAdmin(ctx, user); err != nil {
		return nil, err
	}
	if s.deps.UserDirectory == nil {
		return &brokerv1.ListMembersResponse{}, nil
	}

	entries, err := s.deps.UserDirectory.ListMembers(ctx, tenant)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list members: %v", err)
	}

	admins := s.adminSubjects(ctx)

	out := make([]*brokerv1.MemberInfo, 0, len(entries))
	for _, e := range entries {
		m := &brokerv1.MemberInfo{
			Subject: e.Subject,
			Email:   e.Email,
			Name:    e.Name,
			IsAdmin: admins[e.Subject],
		}
		if !e.LastSeen.IsZero() {
			m.LastSeen = e.LastSeen.UTC().Format("2006-01-02T15:04:05Z07:00")
		}
		if e.ProvisionedAt != nil {
			m.Provisioned = true
			m.ProvisionedAt = e.ProvisionedAt.UTC().Format("2006-01-02T15:04:05Z07:00")
		}
		out = append(out, m)
	}
	return &brokerv1.ListMembersResponse{Members: out}, nil
}

// adminSubjects returns the set of subjects holding the tenant admin relation.
// One FGA read; empty (all non-admin) when FGA is disabled or the read fails —
// the roster is display-only, so a read failure degrades to "unknown admin"
// rather than failing the whole listing.
func (s *BrokerService) adminSubjects(ctx context.Context) map[string]bool {
	admins := map[string]bool{}
	if s.deps.Policy == nil || !s.deps.Policy.FGAEnabled() {
		return admins
	}
	tuples, err := s.deps.Policy.ReadTuples(ctx, policy.ReadFilter{
		Relation: "admin",
		Object:   s.fgaTenantObject(),
	})
	if err != nil {
		s.deps.Logger.Warn("ListMembers: admin-set read failed; roster admin flags omitted")
		return admins
	}
	for _, t := range tuples {
		admins[strings.TrimPrefix(t.User, "user:")] = true
	}
	return admins
}
