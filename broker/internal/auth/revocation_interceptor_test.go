package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4/jwt"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/revocation"
)

// revStore is a revocation.Store whose block flag another "replica" flips.
type revStore struct {
	blocked bool
	userNB  time.Time
	err     error
}

func (s *revStore) UserState(context.Context, string, string) (bool, time.Time, error) {
	return s.blocked, s.userNB, s.err
}
func (s *revStore) AgentNotBefore(context.Context, string, string) (time.Time, error) {
	return time.Time{}, nil
}
func (s *revStore) BlockUser(context.Context, string, string, string, string) (bool, error) {
	return true, nil
}
func (s *revStore) UnblockUser(context.Context, string, string, string) (bool, error) {
	return true, nil
}
func (s *revStore) UserBlocks(context.Context, string) (map[string]db.UserBlock, error) {
	return nil, nil
}
func (s *revStore) RevokeUserSessions(context.Context, string, string, time.Time) error { return nil }
func (s *revStore) RevokeAgentSessions(context.Context, string, string, time.Time) error {
	return nil
}

// AG-06 acceptance: blocking a user makes the next north RPC with a
// still-valid IdP token fail within the cache TTL (<= 60 s), and a revoke-all
// refuses a token issued before the epoch. Runs the real north chain:
// OIDC -> tenant -> revocation.
func TestRevocationInterceptor_BlockAndEpoch(t *testing.T) {
	idp := newTestIDP(t)
	v := mustValidator(t, OIDCConfig{Issuer: idp.srv.URL, Audience: "aikonos-broker", TenantClaim: "tenant_id"})
	iat := time.Now().Add(-time.Minute).Truncate(time.Second)
	tok := idp.mint(t, jwt.Claims{
		Issuer: idp.srv.URL, Subject: "bob", Audience: jwt.Audience{"aikonos-broker"},
		Expiry: jwt.NewNumericDate(time.Now().Add(time.Hour)), IssuedAt: jwt.NewNumericDate(iat),
	}, map[string]any{"email": "bob@example.com", "tenant_id": "11111111-1111-1111-1111-111111111111"})

	store := &revStore{}
	checker := revocation.New(store)
	clock := time.Now()
	checker.Now = func() time.Time { return clock }
	rec := &recordingEmitter{}
	chain := []grpc.UnaryServerInterceptor{
		OIDCInterceptor(v, rec, "t", zap.NewNop()),
		TenantInterceptor(rec, "t", zap.NewNop()),
		RevocationInterceptor(checker, rec, zap.NewNop()),
	}
	call := func() error {
		ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+tok))
		var run func(i int, ctx context.Context) (any, error)
		run = func(i int, ctx context.Context) (any, error) {
			if i == len(chain) {
				return okHandler(ctx, nil)
			}
			return chain[i](ctx, nil, testInfo, func(ctx context.Context, _ any) (any, error) { return run(i+1, ctx) })
		}
		_, err := run(0, ctx)
		return err
	}

	if err := call(); err != nil {
		t.Fatalf("unblocked: %v", err)
	}
	store.blocked = true // written on another replica
	clock = clock.Add(revocation.TTL)
	if err := call(); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("blocked user within TTL: want PermissionDenied, got %v", err)
	}
	if ev := rec.only(t); ev.EventType != "auth.subject.blocked" || ev.ActorUserId != "bob" {
		t.Fatalf("audit = %s actor %q", ev.EventType, ev.ActorUserId)
	}

	store.blocked, rec.events = false, nil
	store.userNB = iat.Add(time.Second) // revoke-all after the token was issued
	clock = clock.Add(revocation.TTL)
	if err := call(); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("token before epoch: want Unauthenticated, got %v", err)
	}
	if ev := rec.only(t); ev.EventType != "auth.token.revoked" {
		t.Fatalf("audit = %s", ev.EventType)
	}

	store.userNB = iat // a token issued at the epoch is valid
	clock = clock.Add(revocation.TTL)
	if err := call(); err != nil {
		t.Fatalf("token at epoch: %v", err)
	}

	store.err = errors.New("db down")
	clock = clock.Add(revocation.TTL)
	if err := call(); status.Code(err) != codes.Unavailable {
		t.Fatalf("read failure: want Unavailable (fail closed), got %v", err)
	}
}

// No identity (OIDC disabled, dev) passes through untouched.
func TestRevocationInterceptor_NoIdentityPassthrough(t *testing.T) {
	ic := RevocationInterceptor(revocation.New(&revStore{blocked: true}), &recordingEmitter{}, zap.NewNop())
	if _, err := ic(context.Background(), nil, testInfo, okHandler); err != nil {
		t.Fatalf("no identity: %v", err)
	}
}
