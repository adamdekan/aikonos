package audit

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
)

const archiveTestTenant = "123e4567-e89b-12d3-a456-426614174000"

func TestPolicyBundleKey(t *testing.T) {
	rev := "sha256:" + strings.Repeat("ab", 32)
	key, err := PolicyBundleKey(rev)
	if err != nil || key != PolicyBundlePrefix+strings.Repeat("ab", 32)+".json" {
		t.Fatalf("PolicyBundleKey(%s) = %q, %v", rev, key, err)
	}
	for _, bad := range []string{"", "sha256:", "sha256:abc", "md5:" + strings.Repeat("a", 64),
		"sha256:" + strings.Repeat("A", 64), "sha256:" + strings.Repeat("a", 63) + "/", "sha256:../" + strings.Repeat("a", 61)} {
		if _, err := PolicyBundleKey(bad); err == nil {
			t.Errorf("PolicyBundleKey(%q) accepted an invalid revision", bad)
		}
	}
}

func storedEvent(t *testing.T, tenant string, at time.Time) (*auditv1.AuditEvent, string) {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatal(err)
	}
	ev := &auditv1.AuditEvent{EventId: id.String(), TenantId: tenant, EventType: "aikonos.broker.policy.decision", OccurredAt: timestamppb.New(at)}
	return ev, objectKey(ev)
}

func TestReader_GetEvent_FindsTheEventByItsDay(t *testing.T) {
	ev, key := storedEvent(t, archiveTestTenant, time.Now().UTC())
	raw := mustMarshalEvent(ev)
	r := &Reader{store: &fakeStore{objects: map[string][]byte{key: raw}}}

	got, stored, ok, err := r.GetEvent(context.Background(), archiveTestTenant, ev.EventId)
	if err != nil || !ok || got.EventId != ev.EventId || string(stored) != string(raw) {
		t.Fatalf("GetEvent = %v, %q, %v, %v", got, stored, ok, err)
	}
}

func TestReader_GetEvent_FallsBackToListing(t *testing.T) {
	ev, _ := storedEvent(t, archiveTestTenant, time.Now().UTC())
	// Stored under a day far from the id's own timestamp.
	key := fmt.Sprintf("%s/2001/01/01/%s.json", archiveTestTenant, ev.EventId)
	r := &Reader{store: &fakeStore{objects: map[string][]byte{key: mustMarshalEvent(ev)}}}
	if got, _, _, err := r.GetEvent(context.Background(), archiveTestTenant, ev.EventId); err != nil || got.EventId != ev.EventId {
		t.Fatalf("GetEvent via listing = %v, %v", got, err)
	}
}

func TestReader_GetEvent_Rejections(t *testing.T) {
	other := "223e4567-e89b-12d3-a456-426614174000"
	ev, key := storedEvent(t, other, time.Now().UTC())
	// An object under this tenant's prefix that holds another tenant's event.
	smuggled := strings.Replace(key, other, archiveTestTenant, 1)
	r := &Reader{store: &fakeStore{objects: map[string][]byte{smuggled: mustMarshalEvent(ev)}}}
	ctx := context.Background()

	if _, _, _, err := r.GetEvent(ctx, archiveTestTenant, ev.EventId); err == nil {
		t.Error("an object holding another tenant's event must be rejected")
	}
	for _, bad := range []string{"../x", "not-a-uuid", strings.ToUpper(ev.EventId)} {
		if _, _, _, err := r.GetEvent(ctx, archiveTestTenant, bad); err == nil {
			t.Errorf("GetEvent(%q) accepted an invalid id", bad)
		}
	}
	missing, _ := uuid.NewV7()
	if _, _, _, err := r.GetEvent(ctx, archiveTestTenant, missing.String()); !errors.Is(err, ErrNotFound) {
		t.Errorf("a missing event: err = %v, want ErrNotFound", err)
	}
	if _, _, ok, _ := (&Reader{}).GetEvent(ctx, archiveTestTenant, missing.String()); ok {
		t.Error("an unconfigured reader reports ok=false")
	}
}

func TestReader_GetPolicyBundle(t *testing.T) {
	rev := "sha256:" + strings.Repeat("cd", 32)
	key, _ := PolicyBundleKey(rev)
	r := &Reader{store: &fakeStore{objects: map[string][]byte{key: []byte(`{"kind":"x"}`)}}}
	if data, ok, err := r.GetPolicyBundle(context.Background(), rev); err != nil || !ok || string(data) != `{"kind":"x"}` {
		t.Fatalf("GetPolicyBundle = %q, %v, %v", data, ok, err)
	}
	if _, _, err := r.GetPolicyBundle(context.Background(), "sha256:"+strings.Repeat("ef", 32)); err == nil {
		t.Error("a missing snapshot must be an error")
	}
	if _, _, err := r.GetPolicyBundle(context.Background(), "../../etc/passwd"); err == nil {
		t.Error("an invalid revision must be rejected before touching the store")
	}
}

func TestReader_VerifyEventSignature(t *testing.T) {
	ev, _ := storedEvent(t, archiveTestTenant, time.Now().UTC())
	mac := hmac.New(sha256.New, []byte("k1"))
	mac.Write(canonicalBytes(ev))
	ev.Signature = hex.EncodeToString(mac.Sum(nil))

	signed := &Reader{cfg: Config{SigningKey: "k1"}}
	if got := signed.VerifyEventSignature(context.Background(), ev); got != SignatureValid {
		t.Fatalf("valid signature reported %s", got)
	}
	ev.ResourceRef = "edited"
	if got := signed.VerifyEventSignature(context.Background(), ev); got != SignatureInvalid {
		t.Fatalf("edited event reported %s", got)
	}
	unsigned := &auditv1.AuditEvent{EventId: ev.EventId, TenantId: archiveTestTenant}
	if got := (&Reader{}).VerifyEventSignature(context.Background(), unsigned); got != SignatureUnsigned {
		t.Fatalf("unsigned event reported %s", got)
	}
}
