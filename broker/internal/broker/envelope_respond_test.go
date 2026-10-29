package broker

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/revocation"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// TestRespondToEnvelope_StaleAuthorityRefused pins F165: accepting an expired
// delegation, or one whose sender was blocked or revoked-all after sending,
// is refused before the envelope changes state or a task spawns.
func TestRespondToEnvelope_StaleAuthorityRefused(t *testing.T) {
	sent := time.Now().Add(-time.Hour)
	cases := []struct {
		name    string
		expires time.Time
		rev     func(*fakeRevStore)
		want    codes.Code
	}{
		{"expired", time.Now().Add(-time.Minute), func(*fakeRevStore) {}, codes.FailedPrecondition},
		{"sender blocked", time.Now().Add(time.Hour), func(r *fakeRevStore) { r.blocks[xferAlice] = db.UserBlock{} }, codes.PermissionDenied},
		{"sender revoked after send", time.Now().Add(time.Hour), func(r *fakeRevStore) { r.userNB[xferAlice] = sent.Add(time.Minute) }, codes.PermissionDenied},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newMemEnvelopeStore()
			deps, _ := xferDeps(t, "", t.TempDir(), store)
			rev := newFakeRevStore()
			tc.rev(rev)
			deps.Revocation = revocation.New(rev)
			id, _ := store.CreateEnvelope(context.Background(), &db.Envelope{
				FromUserID: xferAlice, ToTarget: map[string]any{"type": "user", "id": xferBob},
				TaskSpec: map[string]any{"prompt": "x"}, ExpiresAt: tc.expires,
			}, db.EnvelopeDelivered)
			store.envs[id.String()].CreatedAt = sent

			_, err := NewBrokerService(deps).RespondToEnvelope(ctxWithIdentity(testTenantUUID, xferBob), &brokerv1.RespondToEnvelopeRequest{
				TenantId: testTenantUUID, EnvelopeId: id.String(), Accepted: true,
			})
			if status.Code(err) != tc.want {
				t.Fatalf("want %s, got %v", tc.want, err)
			}
			if got := store.state(id.String()); got != db.EnvelopeDelivered {
				t.Fatalf("envelope state changed to %s", got)
			}
		})
	}
}
