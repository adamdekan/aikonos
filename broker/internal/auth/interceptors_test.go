package auth

import (
	"context"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4/jwt"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
)

func okHandler(_ context.Context, _ any) (any, error) { return "ok", nil }

var testInfo = &grpc.UnaryServerInfo{FullMethod: "/aikonos.broker.v1.BrokerService/CreateTask"}

func TestOIDCInterceptor_DevModePassthrough(t *testing.T) {
	v := mustValidator(t, OIDCConfig{}) // disabled
	ic := OIDCInterceptor(v, nil, "test-tenant", zap.NewNop())

	resp, err := ic(context.Background(), nil, testInfo, okHandler)
	if err != nil {
		t.Fatalf("dev-mode passthrough errored: %v", err)
	}
	if resp != "ok" {
		t.Fatalf("handler not called: %v", resp)
	}
}

func TestOIDCInterceptor_MissingToken(t *testing.T) {
	idp := newTestIDP(t)
	v := mustValidator(t, OIDCConfig{Issuer: idp.srv.URL, Audience: "aikonos-broker"})
	ic := OIDCInterceptor(v, nil, "test-tenant", zap.NewNop())

	_, err := ic(context.Background(), nil, testInfo, okHandler)
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("want Unauthenticated, got %v", err)
	}
}

func TestOIDCInterceptor_ValidTokenInjectsIdentity(t *testing.T) {
	idp := newTestIDP(t)
	v := mustValidator(t, OIDCConfig{Issuer: idp.srv.URL, Audience: "aikonos-broker", TenantClaim: "tenant_id"})
	ic := OIDCInterceptor(v, nil, "test-tenant", zap.NewNop())

	tok := idp.mint(t, jwt.Claims{
		Issuer:   idp.srv.URL,
		Subject:  "bob@example.com",
		Audience: jwt.Audience{"aikonos-broker"},
		Expiry:   jwt.NewNumericDate(time.Now().Add(time.Hour)),
	}, map[string]any{"email": "bob@example.com", "tenant_id": "t-1"})

	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("authorization", "Bearer "+tok))

	var got *Identity
	_, err := ic(ctx, nil, testInfo, func(c context.Context, _ any) (any, error) {
		got, _ = IdentityFromContext(c)
		return "ok", nil
	})
	if err != nil {
		t.Fatalf("Validate via interceptor: %v", err)
	}
	if got == nil || got.Subject != "bob@example.com" || got.TenantID != "t-1" {
		t.Fatalf("identity not injected: %+v", got)
	}
}

func TestTenantInterceptor(t *testing.T) {
	ic := TenantInterceptor(nil, "test-tenant", "", zap.NewNop())

	// No identity on context (dev mode): pass through.
	if _, err := ic(context.Background(), nil, testInfo, okHandler); err != nil {
		t.Fatalf("no-identity passthrough errored: %v", err)
	}

	// Identity with tenant: allowed.
	ctx := WithIdentity(context.Background(), &Identity{Subject: "a", TenantID: "t"})
	if _, err := ic(ctx, nil, testInfo, okHandler); err != nil {
		t.Fatalf("tenant present errored: %v", err)
	}

	// Identity without tenant: denied.
	ctx = WithIdentity(context.Background(), &Identity{Subject: "a"})
	if _, err := ic(ctx, nil, testInfo, okHandler); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("want PermissionDenied, got %v", err)
	}
}

// TestTenantInterceptor_ForeignTenantRefused pins F10/G10: the admin gate is
// evaluated on the configured tenant, so a token naming another tenant is
// refused (and audited) before any handler, whatever its subject.
func TestTenantInterceptor_ForeignTenantRefused(t *testing.T) {
	const own = "11111111-1111-1111-1111-111111111111"
	rec := &recordingEmitter{}
	ic := TenantInterceptor(rec, "test-tenant", own, zap.NewNop())

	ctx := WithIdentity(context.Background(), &Identity{Subject: "admin", TenantID: "22222222-2222-2222-2222-222222222222"})
	if _, err := ic(ctx, nil, testInfo, okHandler); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("foreign tenant: want PermissionDenied, got %v", err)
	}
	if ev := rec.only(t); ev.EventType != "auth.tenant.mismatch" || ev.Decision != auditv1.PolicyDecision_DENY {
		t.Fatalf("audit = %s/%s", ev.EventType, ev.Decision)
	}

	ctx = WithIdentity(context.Background(), &Identity{Subject: "admin", TenantID: "11111111-1111-1111-1111-111111111111"})
	if _, err := ic(ctx, nil, testInfo, okHandler); err != nil {
		t.Fatalf("own tenant: %v", err)
	}
}

// fakeServerStream is a ServerStream carrying only a context.
type fakeServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *fakeServerStream) Context() context.Context { return s.ctx }

// TestStreamInterceptors_GateLikeUnary pins F136: a streaming RPC gets the
// north OIDC and tenant gate, and its handler sees the verified identity.
func TestStreamInterceptors_GateLikeUnary(t *testing.T) {
	idp := newTestIDP(t)
	v := mustValidator(t, OIDCConfig{Issuer: idp.srv.URL, Audience: "aikonos-broker", TenantClaim: "tenant_id"})
	oidc := OIDCStreamInterceptor(v, nil, "t", zap.NewNop())
	tenant := TenantStreamInterceptor(nil, "t", "", zap.NewNop())
	info := &grpc.StreamServerInfo{FullMethod: "/aikonos.broker.v1.BrokerService/StreamTaskEvents", IsServerStream: true}

	var seen *Identity
	handler := func(_ any, ss grpc.ServerStream) error {
		seen, _ = IdentityFromContext(ss.Context())
		return nil
	}
	run := func(ctx context.Context) error {
		return oidc(nil, &fakeServerStream{ctx: ctx}, info, func(srv any, ss grpc.ServerStream) error {
			return tenant(srv, ss, info, handler)
		})
	}

	if err := run(context.Background()); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("no bearer: want Unauthenticated, got %v", err)
	}
	noTenant := idp.mint(t, jwt.Claims{Issuer: idp.srv.URL, Subject: "bob", Audience: jwt.Audience{"aikonos-broker"},
		Expiry: jwt.NewNumericDate(time.Now().Add(time.Hour))}, nil)
	if err := run(metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+noTenant))); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("no tenant claim: want PermissionDenied, got %v", err)
	}
	ok := idp.mint(t, jwt.Claims{Issuer: idp.srv.URL, Subject: "bob", Audience: jwt.Audience{"aikonos-broker"},
		Expiry: jwt.NewNumericDate(time.Now().Add(time.Hour))}, map[string]any{"tenant_id": "t-1"})
	if err := run(metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+ok))); err != nil {
		t.Fatalf("valid token: %v", err)
	}
	if seen == nil || seen.Subject != "bob" || seen.TenantID != "t-1" {
		t.Fatalf("handler did not see the verified identity: %+v", seen)
	}
}

func TestAuditInterceptor_Emits(t *testing.T) {
	emitter, err := audit.NewEmitter(context.Background(), audit.Config{TenantID: "aikonos-dev"})
	if err != nil {
		t.Fatalf("emitter: %v", err)
	}
	ic := AuditInterceptor(emitter, "aikonos-dev", zap.NewNop())

	ctx := WithIdentity(context.Background(), &Identity{Subject: "alice@example.com", TenantID: "t-9"})
	resp, err := ic(ctx, nil, testInfo, okHandler)
	if err != nil || resp != "ok" {
		t.Fatalf("audit interceptor altered call: resp=%v err=%v", resp, err)
	}

	// A failing handler still passes the error through unchanged.
	boom := status.Error(codes.Internal, "boom")
	_, err = ic(ctx, nil, testInfo, func(context.Context, any) (any, error) { return nil, boom })
	if err != boom {
		t.Fatalf("audit interceptor swallowed handler error: %v", err)
	}
}

// fakeAuditEmitter records every event handed to Emit, for assertions the
// concrete *audit.Emitter (which talks to NATS) can't easily support in a
// unit test.
type fakeAuditEmitter struct {
	events []*auditv1.AuditEvent
}

func (f *fakeAuditEmitter) Emit(_ context.Context, ev *auditv1.AuditEvent) error {
	f.events = append(f.events, ev)
	return nil
}

// TestAuditInterceptor_PanicStillEmitsAndReturnsInternal verifies CP2 fix 3:
// a panicking handler must not bypass the audit emit, and the panic is
// converted to codes.Internal rather than crashing the server.
func TestAuditInterceptor_PanicStillEmitsAndReturnsInternal(t *testing.T) {
	fake := &fakeAuditEmitter{}
	ic := AuditInterceptor(fake, "aikonos-dev", zap.NewNop())

	ctx := WithIdentity(context.Background(), &Identity{Subject: "alice@example.com", TenantID: "t-9"})
	resp, err := ic(ctx, nil, testInfo, func(context.Context, any) (any, error) {
		panic("boom: handler exploded")
	})

	if resp != nil {
		t.Fatalf("expected nil response after a recovered panic, got %v", resp)
	}
	if status.Code(err) != codes.Internal {
		t.Fatalf("want codes.Internal after a recovered panic, got %v", err)
	}
	if len(fake.events) != 1 {
		t.Fatalf("expected exactly one audit event for the panicking call, got %d", len(fake.events))
	}
	ev := fake.events[0]
	if ev.Decision != auditv1.PolicyDecision_DENY {
		t.Errorf("expected DENY decision for a panicking handler, got %v", ev.Decision)
	}
	if ev.ActorUserId != "alice@example.com" || ev.TenantId != "t-9" {
		t.Errorf("expected identity from context to flow into the panic audit event, got %+v", ev)
	}
	if ev.ResourceRef != testInfo.FullMethod {
		t.Errorf("expected ResourceRef=%q, got %q", testInfo.FullMethod, ev.ResourceRef)
	}
}

func TestBearerFromContext(t *testing.T) {
	cases := []struct {
		name string
		md   metadata.MD
		ok   bool
		want string
	}{
		{"valid", metadata.Pairs("authorization", "Bearer abc.def"), true, "abc.def"},
		{"lowercase scheme", metadata.Pairs("authorization", "bearer xyz"), true, "xyz"},
		{"missing", metadata.MD{}, false, ""},
		{"not bearer", metadata.Pairs("authorization", "Basic abc"), false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := metadata.NewIncomingContext(context.Background(), tc.md)
			got, err := BearerFromContext(ctx)
			if tc.ok && (err != nil || got != tc.want) {
				t.Fatalf("got (%q,%v), want (%q,nil)", got, err, tc.want)
			}
			if !tc.ok && err == nil {
				t.Fatalf("expected error, got token %q", got)
			}
		})
	}
}
