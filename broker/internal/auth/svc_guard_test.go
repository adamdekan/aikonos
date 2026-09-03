package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4/jwt"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func makeSvcToken(t *testing.T, idp *testIDP, subject string) string {
	t.Helper()
	tok, err := jwt.Signed(idp.signer).Claims(jwt.Claims{
		Subject:  subject,
		Issuer:   idp.srv.URL,
		Audience: jwt.Audience{"aikonos-broker"},
		Expiry:   jwt.NewNumericDate(time.Now().Add(time.Hour)),
		IssuedAt: jwt.NewNumericDate(time.Now()),
	}).Claims(map[string]any{"email": "svc@internal"}).Serialize()
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return tok
}

// TestValidate_SvcSubjectRejected: Validate must return *SvcSubjectError for a
// token whose resolved subject starts with "svc-". No identity is returned.
func TestValidate_SvcSubjectRejected(t *testing.T) {
	idp := newTestIDP(t)
	v := mustValidator(t, OIDCConfig{Issuer: idp.srv.URL, Audience: "aikonos-broker"})

	rawTok := makeSvcToken(t, idp, "svc-some-service-principal")
	id, err := v.Validate(context.Background(), rawTok)
	if err == nil {
		t.Fatal("Validate must return an error for svc- subject, got nil")
	}
	if id != nil {
		t.Fatalf("Validate must return nil identity for svc- subject, got %+v", id)
	}
	var svcErr *SvcSubjectError
	if !errors.As(err, &svcErr) {
		t.Fatalf("error must be *SvcSubjectError, got %T: %v", err, err)
	}
	if svcErr.Subject != "svc-some-service-principal" {
		t.Errorf("SvcSubjectError.Subject = %q, want svc-some-service-principal", svcErr.Subject)
	}
	if svcErr.Email != "svc@internal" {
		t.Errorf("SvcSubjectError.Email = %q, want svc@internal", svcErr.Email)
	}
}

// TestValidate_OidOverrideToSvcRejected: when SubjectClaim "oid" overrides sub
// to a svc- value, Validate must reject — the check is on the final principal,
// not the raw sub.
func TestValidate_OidOverrideToSvcRejected(t *testing.T) {
	idp := newTestIDP(t)
	v := mustValidator(t, OIDCConfig{
		Issuer:       idp.srv.URL,
		Audience:     "aikonos-broker",
		SubjectClaim: "oid",
	})

	// sub is a human-looking value but oid resolves to svc-.
	tok := idp.mint(t, jwt.Claims{
		Issuer:   idp.srv.URL,
		Subject:  "human-sub",
		Audience: jwt.Audience{"aikonos-broker"},
		Expiry:   jwt.NewNumericDate(time.Now().Add(time.Hour)),
		IssuedAt: jwt.NewNumericDate(time.Now()),
	}, map[string]any{
		"oid":   "svc-agent-xyz",
		"email": "agent@internal",
	})

	id, err := v.Validate(context.Background(), tok)
	if err == nil {
		t.Fatal("Validate must reject when oid override resolves to svc-")
	}
	if id != nil {
		t.Fatalf("must return nil identity, got %+v", id)
	}
	var svcErr *SvcSubjectError
	if !errors.As(err, &svcErr) {
		t.Fatalf("error must be *SvcSubjectError, got %T", err)
	}
	if svcErr.Subject != "svc-agent-xyz" {
		t.Errorf("SvcSubjectError.Subject = %q, want svc-agent-xyz", svcErr.Subject)
	}
}

// TestValidate_HumanSubjectNotRejected: a normal human subject must still
// produce a valid identity — the svc- check must not false-positive.
func TestValidate_HumanSubjectNotRejected(t *testing.T) {
	idp := newTestIDP(t)
	v := mustValidator(t, OIDCConfig{Issuer: idp.srv.URL, Audience: "aikonos-broker"})

	tok := idp.mint(t, jwt.Claims{
		Issuer:   idp.srv.URL,
		Subject:  "alice@example.com",
		Audience: jwt.Audience{"aikonos-broker"},
		Expiry:   jwt.NewNumericDate(time.Now().Add(time.Hour)),
		IssuedAt: jwt.NewNumericDate(time.Now()),
	}, map[string]any{"email": "alice@example.com", "tenant_id": "t-1"})

	id, err := v.Validate(context.Background(), tok)
	if err != nil {
		t.Fatalf("human subject must not be rejected: %v", err)
	}
	if id == nil || id.Subject != "alice@example.com" {
		t.Fatalf("expected valid identity, got %+v", id)
	}
}

// TestOIDCInterceptor_SvcSubjectRejected: a token whose `sub` starts with "svc-"
// must be rejected with PermissionDenied by the OIDC interceptor.
func TestOIDCInterceptor_SvcSubjectRejected(t *testing.T) {
	idp := newTestIDP(t)
	v := mustValidator(t, OIDCConfig{Issuer: idp.srv.URL, Audience: "aikonos-broker"})
	ic := OIDCInterceptor(v, nil, "test-tenant", zap.NewNop())

	rawTok := makeSvcToken(t, idp, "svc-aaaaaaaa-0000-0000-0000-000000000001")
	md := metadata.New(map[string]string{"authorization": "Bearer " + rawTok})
	ctx := metadata.NewIncomingContext(context.Background(), md)

	_, err := ic(ctx, nil, testInfo, okHandler)
	if err == nil {
		t.Fatal("want error for svc- subject, got nil")
	}
	if code := status.Code(err); code != codes.PermissionDenied {
		t.Errorf("want PermissionDenied, got %v", code)
	}
}

// TestOIDCInterceptor_SvcSubject_AuditEvent: when OIDCInterceptor rejects a
// svc- token via the typed *SvcSubjectError path, it must emit an
// auth.subject.rejected event carrying the subject and email — audit fidelity
// is preserved through the typed-error rewiring.
func TestOIDCInterceptor_SvcSubject_AuditEvent(t *testing.T) {
	idp := newTestIDP(t)
	v := mustValidator(t, OIDCConfig{Issuer: idp.srv.URL, Audience: "aikonos-broker"})
	rec := &recordingEmitter{}
	ic := OIDCInterceptor(v, rec, "test-tenant", zap.NewNop())

	tok := makeSvcToken(t, idp, "svc-aaaaaaaa-0000-0000-0000-000000000001")
	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("authorization", "Bearer "+tok))

	if _, err := ic(ctx, nil, testInfo, okHandler); err == nil {
		t.Fatal("want error for svc- subject, got nil")
	}
	ev := rec.only(t)
	if ev.EventType != "auth.subject.rejected" {
		t.Errorf("event_type: want auth.subject.rejected, got %q", ev.EventType)
	}
	if ev.ActorUserId != "svc-aaaaaaaa-0000-0000-0000-000000000001" {
		t.Errorf("actor: got %q, want svc-aaaaaaaa-0000-0000-0000-000000000001", ev.ActorUserId)
	}
	if ev.ActorEmail != "svc@internal" {
		t.Errorf("actor_email: got %q, want svc@internal", ev.ActorEmail)
	}
}

// TestValidate_SvcSubject_DefenseInDepth: calling Validate directly (no
// interceptor) with a svc- token is rejected — the guard no longer depends
// on the interceptor being mounted.
func TestValidate_SvcSubject_DefenseInDepth(t *testing.T) {
	idp := newTestIDP(t)
	v := mustValidator(t, OIDCConfig{Issuer: idp.srv.URL, Audience: "aikonos-broker"})

	rawTok := makeSvcToken(t, idp, "svc-direct-caller")
	_, err := v.Validate(context.Background(), rawTok)
	if err == nil {
		t.Fatal("Validate must reject svc- tokens even without an interceptor")
	}
	var svcErr *SvcSubjectError
	if !errors.As(err, &svcErr) {
		t.Fatalf("error must be *SvcSubjectError, got %T: %v", err, err)
	}
}
