package broker

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/auth"
)

// callerIdentity resolves the (tenant, user) a north-bound call acts on, binding
// both to the authenticated principal. It backs the connector RPCs (where user
// is the per-user Vault/workspace key) and the admin RPCs (where user is the
// subject the tenant-admin gate is evaluated against), so it must never let an
// untrusted request field choose the principal.
//
// When a verified OIDC identity is present (the in-cluster enforcement posture),
// it is authoritative: the request-supplied user_id may only echo the verified
// actor — a mismatch is rejected — so a token holder can neither store/read
// another user's connector tokens nor have the admin gate run against a forged
// identity. Only when no identity is on the context (OIDC-disabled dev mode) do
// the request fields become the source. Tenant must always be a UUID.
func (s *BrokerService) callerIdentity(ctx context.Context, reqTenant, reqUser string) (tenant, user string, err error) {
	id, hasID := auth.IdentityFromContext(ctx)

	tenant = reqTenant
	if hasID && id.TenantID != "" {
		// North-only: SPIFFE-only identities have TenantID == "" so this
		// block is never entered for south callers (no cross-tenant forge risk).
		if reqTenant != "" && reqTenant != id.TenantID {
			return "", "", status.Error(codes.PermissionDenied, "tenant_id does not match the authenticated identity")
		}
		tenant = id.TenantID
	}
	if _, perr := uuid.Parse(tenant); perr != nil {
		return "", "", status.Errorf(codes.InvalidArgument, "invalid tenant_id: %v", perr)
	}

	if hasID && id.PrincipalID() != "" {
		principal := id.PrincipalID()
		if reqUser != "" && reqUser != principal {
			return "", "", status.Error(codes.PermissionDenied, "user_id does not match the authenticated identity")
		}
		user = principal
	} else {
		user = reqUser
	}
	if user == "" {
		return "", "", status.Error(codes.InvalidArgument, "user_id required")
	}
	// svc- principals are service accounts for external agent access; they must
	// never appear as human callers on the north-bound surface.
	if strings.HasPrefix(user, "svc-") {
		return "", "", status.Error(codes.PermissionDenied, "svc- user_id not permitted on the north-bound API")
	}

	// Passive directory upsert — best-effort, never blocks the RPC.
	// At least one display field required; subject-only entries add nothing to the directory.
	if s.deps.UserDirectory != nil && hasID && id != nil &&
		id.Subject != "" && (id.Email != "" || id.Name != "") &&
		!strings.HasPrefix(id.Subject, "svc-") &&
		s.dirThrottle.allow(tenant, id.Subject) {
		go func() {
			ctx2, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := s.deps.UserDirectory.Upsert(ctx2, tenant, id.Subject, id.Email, id.Name); err != nil {
				s.deps.Logger.Warn("user_directory upsert failed", zap.String("subject", id.Subject), zap.Error(err))
			}
		}()
	}

	return tenant, user, nil
}

// requireTenantAdmin gates every admin RPC: the caller must hold the `admin`
// relation on this tenant in OpenFGA. Fails closed on a transport error.
func (s *BrokerService) requireTenantAdmin(ctx context.Context, user string) error {
	ok, err := s.deps.Policy.CheckFGA(ctx, "user:"+user, "admin", s.fgaTenantObject())
	if err != nil {
		s.emitAdminAccessDenied(ctx, user, "fga_error")
		return status.Error(codes.Internal, "authorization check failed")
	}
	if !ok {
		s.emitAdminAccessDenied(ctx, user, "not_admin")
		return status.Error(codes.PermissionDenied, "tenant admin required")
	}
	return nil
}
