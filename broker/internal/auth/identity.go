// broker/internal/auth/identity.go
// Caller identity carried through the request context.
//
// North-bound callers (frontend → broker) are identified by a verified OIDC
// token; south-bound callers (sandbox → broker) by their SPIFFE SVID. The
// interceptors in this package put an *Identity on the context; handlers and
// the audit interceptor read it back with IdentityFromContext.
package auth

import "context"

// Identity is the authenticated principal behind a gRPC call. For north-bound
// calls it is populated from validated OIDC claims; for south-bound calls only
// SpiffeID is set.
type Identity struct {
	// Subject is the verified OIDC `sub` claim — the stable, immutable authz
	// principal. For Entra this is the `oid` (non-reassignable GUID); for
	// Keycloak dev it is `preferred_username` (== email). Configured via
	// AIKONOS_OIDC_SUBJECT_CLAIM. Use PrincipalID() for authz and per-user
	// storage keys; never ActorID() for those purposes.
	Subject  string
	Email    string   // OIDC `email`/`preferred_username` — display label only
	// Name is the OIDC `name` claim — display label only, never an authz input.
	Name     string
	TenantID string   // tenant claim (north-bound)
	Scopes   []string // OIDC scopes/roles, if present
	SpiffeID string   // peer SPIFFE ID (south-bound; also set on north when mTLS peer is a known workload)
}

// PrincipalID returns the verified principal used for authorization decisions
// and per-user storage keys (workspace path, Vault key, FGA operand).
// Subject-first: Subject → Email → SpiffeID. Nil-safe (returns "").
//
// For Entra this is the immutable `oid`; for Keycloak dev it is the
// preferred_username (== email) — both controlled by AIKONOS_OIDC_SUBJECT_CLAIM.
func (i *Identity) PrincipalID() string {
	if i == nil {
		return ""
	}
	if i.Subject != "" {
		return i.Subject
	}
	if i.Email != "" {
		return i.Email
	}
	return i.SpiffeID
}

// ActorID returns the email-first display/audit label: Email → Subject → SpiffeID.
// Used only for audit log actor_user_id and human-readable display. Do NOT use
// for authorization decisions or per-user storage keys — use PrincipalID() instead.
func (i *Identity) ActorID() string {
	if i == nil {
		return ""
	}
	if i.Email != "" {
		return i.Email
	}
	if i.Subject != "" {
		return i.Subject
	}
	return i.SpiffeID
}

type ctxKey int

const identityKey ctxKey = iota

// WithIdentity returns a copy of ctx carrying id.
func WithIdentity(ctx context.Context, id *Identity) context.Context {
	return context.WithValue(ctx, identityKey, id)
}

// IdentityFromContext returns the Identity placed on ctx by the auth
// interceptors, if any.
func IdentityFromContext(ctx context.Context) (*Identity, bool) {
	id, ok := ctx.Value(identityKey).(*Identity)
	return id, ok && id != nil
}
