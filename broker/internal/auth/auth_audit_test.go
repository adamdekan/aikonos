package auth

import (
	"context"
	"testing"

	"go.uber.org/zap"
	"google.golang.org/grpc/metadata"

	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
)

// recordingEmitter captures emitted audit events for assertion. Satisfies the
// unexported auditEmitter interface the auth interceptors accept.
type recordingEmitter struct{ events []*auditv1.AuditEvent }

func (r *recordingEmitter) Emit(_ context.Context, ev *auditv1.AuditEvent) error {
	r.events = append(r.events, ev)
	return nil
}

func (r *recordingEmitter) only(t *testing.T) *auditv1.AuditEvent {
	t.Helper()
	if len(r.events) != 1 {
		t.Fatalf("want exactly 1 audit event, got %d", len(r.events))
	}
	return r.events[0]
}

func TestTenantInterceptor_MissingTenant_EmitsAuthDenied(t *testing.T) {
	rec := &recordingEmitter{}
	ic := TenantInterceptor(rec, "test-tenant", zap.NewNop())

	ctx := WithIdentity(context.Background(), &Identity{Subject: "alice@example.com"})
	if _, err := ic(ctx, nil, testInfo, okHandler); err == nil {
		t.Fatal("want error for missing tenant claim, got nil")
	}
	ev := rec.only(t)
	if ev.EventType != "auth.tenant.missing" {
		t.Errorf("event_type: want auth.tenant.missing, got %q", ev.EventType)
	}
	if ev.Decision != auditv1.PolicyDecision_DENY {
		t.Errorf("decision: want DENY, got %v", ev.Decision)
	}
	if ev.ActorUserId != "alice@example.com" {
		t.Errorf("actor: want alice@example.com, got %q", ev.ActorUserId)
	}
	if ev.ResourceRef != testInfo.FullMethod {
		t.Errorf("resource_ref: want %q, got %q", testInfo.FullMethod, ev.ResourceRef)
	}
}

func TestTenantInterceptor_WithTenant_NoEvent(t *testing.T) {
	rec := &recordingEmitter{}
	ic := TenantInterceptor(rec, "test-tenant", zap.NewNop())

	ctx := WithIdentity(context.Background(), &Identity{Subject: "a", TenantID: "t-1"})
	if _, err := ic(ctx, nil, testInfo, okHandler); err != nil {
		t.Fatalf("tenant present should pass: %v", err)
	}
	if len(rec.events) != 0 {
		t.Fatalf("a successful call must emit no auth-denied event, got %d", len(rec.events))
	}
}

func TestOIDCInterceptor_SvcSubject_EmitsAuthDenied(t *testing.T) {
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
		t.Errorf("actor: got %q", ev.ActorUserId)
	}
}

func TestOIDCInterceptor_InvalidToken_EmitsAuthDenied(t *testing.T) {
	idp := newTestIDP(t)
	v := mustValidator(t, OIDCConfig{Issuer: idp.srv.URL, Audience: "aikonos-broker"})
	rec := &recordingEmitter{}
	ic := OIDCInterceptor(v, rec, "test-tenant", zap.NewNop())

	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("authorization", "Bearer not-a-real-jwt"))

	if _, err := ic(ctx, nil, testInfo, okHandler); err == nil {
		t.Fatal("want error for invalid token, got nil")
	}
	ev := rec.only(t)
	if ev.EventType != "auth.token.rejected" {
		t.Errorf("event_type: want auth.token.rejected, got %q", ev.EventType)
	}
	if ev.ActorUserId != "" {
		t.Errorf("actor must be empty when no identity was established, got %q", ev.ActorUserId)
	}
}

func TestOIDCInterceptor_MissingBearer_NoEvent(t *testing.T) {
	// A totally absent credential is pre-credential (transport-level) — not a
	// credential rejection, so it must NOT emit (avoids anonymous-probe spam).
	idp := newTestIDP(t)
	v := mustValidator(t, OIDCConfig{Issuer: idp.srv.URL, Audience: "aikonos-broker"})
	rec := &recordingEmitter{}
	ic := OIDCInterceptor(v, rec, "test-tenant", zap.NewNop())

	if _, err := ic(context.Background(), nil, testInfo, okHandler); err == nil {
		t.Fatal("want Unauthenticated, got nil")
	}
	if len(rec.events) != 0 {
		t.Fatalf("missing bearer must not emit an auth event, got %d", len(rec.events))
	}
}

func TestEmitAuthDenied_NilEmitter_NoPanic(t *testing.T) {
	// nil emitter is a no-op (audit disabled) — must not panic.
	emitAuthDenied(context.Background(), nil, "test-tenant", "auth.token.rejected", "/m", "", "", "invalid_token")
}
