package auth

import "testing"

// TestPrincipalID_SubjectFirst: Subject-first authz principal.
// An identity with Subject=oid, Email=email → PrincipalID returns the oid.
// This is the Entra production path: immutable oid is the authz subject.
func TestPrincipalID_SubjectFirst(t *testing.T) {
	id := &Identity{Subject: "oid-123", Email: "a@b.com", SpiffeID: "spiffe://x"}
	if got := id.PrincipalID(); got != "oid-123" {
		t.Errorf("PrincipalID: want oid-123, got %q", got)
	}
}

// TestPrincipalID_FallsBackToEmail: when Subject is empty, Email is the principal.
// This is the Keycloak dev path (preferred_username == email).
func TestPrincipalID_FallsBackToEmail(t *testing.T) {
	id := &Identity{Subject: "", Email: "a@b.com", SpiffeID: "spiffe://x"}
	if got := id.PrincipalID(); got != "a@b.com" {
		t.Errorf("PrincipalID: want a@b.com, got %q", got)
	}
}

// TestPrincipalID_FallsBackToSpiffeID: Subject and Email both empty → SpiffeID.
func TestPrincipalID_FallsBackToSpiffeID(t *testing.T) {
	id := &Identity{Subject: "", Email: "", SpiffeID: "spiffe://aikonos.com/svc"}
	if got := id.PrincipalID(); got != "spiffe://aikonos.com/svc" {
		t.Errorf("PrincipalID: want spiffe id, got %q", got)
	}
}

// TestPrincipalID_NilSafe: nil receiver returns "".
func TestPrincipalID_NilSafe(t *testing.T) {
	var id *Identity
	if got := id.PrincipalID(); got != "" {
		t.Errorf("PrincipalID on nil: want empty, got %q", got)
	}
}

// TestActorID_StillEmailFirst: proves the display/audit path is untouched.
// An identity with Subject=oid, Email=email → ActorID returns the email, not oid.
// PrincipalID returns oid. The two identifiers serve different purposes.
func TestActorID_StillEmailFirst(t *testing.T) {
	id := &Identity{Subject: "oid-123", Email: "a@b.com"}
	if got := id.ActorID(); got != "a@b.com" {
		t.Errorf("ActorID: want a@b.com (email-first display), got %q", got)
	}
	if got := id.PrincipalID(); got != "oid-123" {
		t.Errorf("PrincipalID: want oid-123 (subject-first authz), got %q", got)
	}
}
