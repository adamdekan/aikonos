package broker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/db"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// ── CP2: group fan-out ────────────────────────────────────────────────────────

// trackingEnvelopeStore counts CreateEnvelope calls so tests can assert
// exactly how many fan-out deliveries were made. Satisfies taskStore
// (CreateEnvelope only; the rest via the embedded stub) for injection via
// Deps.Tasks.
type trackingEnvelopeStore struct {
	stubTaskStore
	count   int
	created []*db.Task
}

func (s *trackingEnvelopeStore) Create(_ context.Context, t *db.Task) error {
	s.created = append(s.created, t)
	return nil
}

func (s *trackingEnvelopeStore) CreateEnvelope(_ context.Context, _ *db.Envelope, _ db.EnvelopeState) (uuid.UUID, error) {
	s.count++
	return uuid.New(), nil
}

// TestSendEnvelope_GroupFanOut_DeliversToEachMember verifies that a group send
// creates one envelope per non-sender member.
func TestSendEnvelope_GroupFanOut_DeliversToEachMember(t *testing.T) {
	// alice is a delegatable_member of group:alpha; members: alice, bob, carol.
	// Fan-out must deliver to bob and carol (2 envelopes), not alice (self-excluded).
	f := &fakeFGA{
		listObjectsResultByUser: map[string][]string{
			"user:alice@example.com": {"group:alpha"},
			// bob and carol share the group so their per-member deliveries pass
			// the sharesDelegatableGroup fallback (since CheckFGA can_delegate_to_user
			// defaults to false in fakeFGA when not in checks map).
			"user:bob@example.com":   {"group:alpha"},
			"user:carol@example.com": {"group:alpha"},
		},
		reads: map[string][]fgaKey{
			"group:alpha": {
				memberTuple("alice@example.com", "group:alpha"),
				memberTuple("bob@example.com", "group:alpha"),
				memberTuple("carol@example.com", "group:alpha"),
			},
		},
	}
	fgaSrv := f.server(t)
	defer fgaSrv.Close()

	opaSrv := opaAlwaysAllow(t)
	defer opaSrv.Close()

	store := &trackingEnvelopeStore{}
	deps := testEnvelopeDeps(t, fgaSrv.URL, opaSrv.URL)
	deps.Tasks = store
	svc := NewBrokerService(deps)
	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")

	h, err := svc.SendEnvelope(ctx, &brokerv1.SendEnvelopeRequest{
		TenantId:   testTenantUUID,
		FromUserId: "alice@example.com",
		Recipient:  &brokerv1.EnvelopeRecipient{Target: &brokerv1.EnvelopeRecipient_GroupId{GroupId: "group:alpha"}},
		Task:       &brokerv1.EnvelopeTask{Intent: "fan this out"},
		Delegation: &brokerv1.EnvelopeDelegation{},
	})
	if err != nil {
		t.Fatalf("group fan-out: unexpected error: %v", err)
	}
	if h == nil {
		t.Fatal("group fan-out: nil handle")
	}
	// envelope_id is empty for group sends; trace_id must be set.
	if h.EnvelopeId != "" {
		t.Errorf("group send envelope_id should be empty, got %q", h.EnvelopeId)
	}
	if h.TraceId == "" {
		t.Error("group send trace_id must not be empty")
	}
	// Alice is excluded; bob and carol each get one envelope.
	if store.count != 2 {
		t.Errorf("expected 2 envelopes (bob + carol), created %d", store.count)
	}
}

// TestSendEnvelope_GroupFanOut_SkipsFailingMember verifies that a member whose
// per-member delivery fails (OPA deny) is skipped while other members still receive.
func TestSendEnvelope_GroupFanOut_SkipsFailingMember(t *testing.T) {
	// alice → group:alpha; members: alice, bob, carol.
	// carol's groups list is empty → sharesDelegatableGroup fails for carol →
	// fgaDecision="deny" → opaEchoFGADecision denies carol.
	// bob shares group:alpha → fgaDecision="allow" → OPA allows bob.
	f := &fakeFGA{
		listObjectsResultByUser: map[string][]string{
			"user:alice@example.com": {"group:alpha"},
			"user:bob@example.com":   {"group:alpha"},
			"user:carol@example.com": {}, // no shared group → delivery denied
		},
		reads: map[string][]fgaKey{
			"group:alpha": {
				memberTuple("alice@example.com", "group:alpha"),
				memberTuple("bob@example.com", "group:alpha"),
				memberTuple("carol@example.com", "group:alpha"),
			},
		},
	}
	fgaSrv := f.server(t)
	defer fgaSrv.Close()

	// OPA allows iff fga_send_decision=allow → carol is skipped, bob passes.
	opaSrv := opaEchoFGADecision(t)
	defer opaSrv.Close()

	store := &trackingEnvelopeStore{}
	deps := testEnvelopeDeps(t, fgaSrv.URL, opaSrv.URL)
	deps.Tasks = store
	svc := NewBrokerService(deps)
	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")

	h, err := svc.SendEnvelope(ctx, &brokerv1.SendEnvelopeRequest{
		TenantId:   testTenantUUID,
		FromUserId: "alice@example.com",
		Recipient:  &brokerv1.EnvelopeRecipient{Target: &brokerv1.EnvelopeRecipient_GroupId{GroupId: "group:alpha"}},
		Task:       &brokerv1.EnvelopeTask{Intent: "skip carol"},
		Delegation: &brokerv1.EnvelopeDelegation{},
	})
	if err != nil {
		t.Fatalf("partial fan-out should succeed (≥1 delivered): %v", err)
	}
	// Only bob was delivered to.
	if store.count != 1 {
		t.Errorf("expected 1 envelope (bob only), created %d", store.count)
	}
	_ = h
}

// TestSendEnvelope_GroupFanOut_ZeroDeliverable_PermissionDenied verifies that
// when all members fail delivery (zero envelopes), the call returns PermissionDenied.
func TestSendEnvelope_GroupFanOut_ZeroDeliverable_PermissionDenied(t *testing.T) {
	// alice → group:alpha; members: alice, bob.
	// alice excluded (self). bob has no shared group → OPA deny → zero delivered.
	f := &fakeFGA{
		listObjectsResultByUser: map[string][]string{
			"user:alice@example.com": {"group:alpha"},
			"user:bob@example.com":   {}, // no shared group → fgaDecision=deny
		},
		reads: map[string][]fgaKey{
			"group:alpha": {
				memberTuple("alice@example.com", "group:alpha"),
				memberTuple("bob@example.com", "group:alpha"),
			},
		},
	}
	fgaSrv := f.server(t)
	defer fgaSrv.Close()

	opaSrv := opaEchoFGADecision(t)
	defer opaSrv.Close()

	store := &trackingEnvelopeStore{}
	deps := testEnvelopeDeps(t, fgaSrv.URL, opaSrv.URL)
	deps.Tasks = store
	svc := NewBrokerService(deps)
	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")

	_, err := svc.SendEnvelope(ctx, &brokerv1.SendEnvelopeRequest{
		TenantId:   testTenantUUID,
		FromUserId: "alice@example.com",
		Recipient:  &brokerv1.EnvelopeRecipient{Target: &brokerv1.EnvelopeRecipient_GroupId{GroupId: "group:alpha"}},
		Task:       &brokerv1.EnvelopeTask{Intent: "nobody gets this"},
		Delegation: &brokerv1.EnvelopeDelegation{},
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("zero deliverable: want PermissionDenied, got %v", err)
	}
	if store.count != 0 {
		t.Errorf("zero deliverable: expected 0 envelopes, created %d", store.count)
	}
}

// TestSendEnvelope_GroupFanOut_SenderNotMember_PermissionDenied verifies that a
// sender who is not a delegatable_member of the target group is rejected before
// any delivery.
func TestSendEnvelope_GroupFanOut_SenderNotMember_PermissionDenied(t *testing.T) {
	// alice is NOT in group:alpha → PermissionDenied before any fan-out.
	f := &fakeFGA{
		listObjectsResultByUser: map[string][]string{
			"user:alice@example.com": {}, // alice not in any group
		},
		reads: map[string][]fgaKey{
			"group:alpha": {
				memberTuple("bob@example.com", "group:alpha"),
				memberTuple("carol@example.com", "group:alpha"),
			},
		},
	}
	fgaSrv := f.server(t)
	defer fgaSrv.Close()

	opaSrv := opaAlwaysAllow(t)
	defer opaSrv.Close()

	store := &trackingEnvelopeStore{}
	deps := testEnvelopeDeps(t, fgaSrv.URL, opaSrv.URL)
	deps.Tasks = store
	svc := NewBrokerService(deps)
	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")

	_, err := svc.SendEnvelope(ctx, &brokerv1.SendEnvelopeRequest{
		TenantId:   testTenantUUID,
		FromUserId: "alice@example.com",
		Recipient:  &brokerv1.EnvelopeRecipient{Target: &brokerv1.EnvelopeRecipient_GroupId{GroupId: "group:alpha"}},
		Task:       &brokerv1.EnvelopeTask{Intent: "unauthorized group send"},
		Delegation: &brokerv1.EnvelopeDelegation{},
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("sender not member: want PermissionDenied, got %v", err)
	}
	// No deliveries must have been attempted.
	if store.count != 0 {
		t.Errorf("sender not member: expected 0 envelopes, got %d", store.count)
	}
}

// TestSendEnvelope_RoleRecipient_StillUnimplemented verifies the role arm is unchanged.
func TestSendEnvelope_RoleRecipient_StillUnimplemented(t *testing.T) {
	f := &fakeFGA{}
	fgaSrv := f.server(t)
	defer fgaSrv.Close()
	opaSrv := opaAlwaysAllow(t)
	defer opaSrv.Close()

	svc := NewBrokerService(testEnvelopeDeps(t, fgaSrv.URL, opaSrv.URL))
	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")

	_, err := svc.SendEnvelope(ctx, &brokerv1.SendEnvelopeRequest{
		TenantId:   testTenantUUID,
		FromUserId: "alice@example.com",
		Recipient:  &brokerv1.EnvelopeRecipient{Target: &brokerv1.EnvelopeRecipient_Role{Role: "admin"}},
		Task:       &brokerv1.EnvelopeTask{Intent: "role send"},
		Delegation: &brokerv1.EnvelopeDelegation{},
	})
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("role arm: want Unimplemented, got %v", err)
	}
}

// TestSendEnvelope_NilRecipient_InvalidArgument verifies that an empty recipient
// (no arm set) returns InvalidArgument.
func TestSendEnvelope_NilRecipient_InvalidArgument(t *testing.T) {
	f := &fakeFGA{}
	fgaSrv := f.server(t)
	defer fgaSrv.Close()
	opaSrv := opaAlwaysAllow(t)
	defer opaSrv.Close()

	svc := NewBrokerService(testEnvelopeDeps(t, fgaSrv.URL, opaSrv.URL))
	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")

	_, err := svc.SendEnvelope(ctx, &brokerv1.SendEnvelopeRequest{
		TenantId:   testTenantUUID,
		FromUserId: "alice@example.com",
		Recipient:  &brokerv1.EnvelopeRecipient{}, // no arm set
		Task:       &brokerv1.EnvelopeTask{Intent: "nowhere"},
		Delegation: &brokerv1.EnvelopeDelegation{},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("nil recipient: want InvalidArgument, got %v", err)
	}
}

// ── H-01: sender / inbox user bound to the authenticated identity ───────────

func TestSendEnvelope_ForgedFromUser_PermissionDenied(t *testing.T) {
	store := &trackingEnvelopeStore{}
	svc := &BrokerService{deps: Deps{Logger: zap.NewNop(), Tasks: store}}
	ctx := ctxWithIdentity(testTenantUUID, "eve@example.com")

	_, err := svc.SendEnvelope(ctx, &brokerv1.SendEnvelopeRequest{
		TenantId:   testTenantUUID,
		FromUserId: "admin@example.com", // forged: borrow the admin's delegable scopes
		Recipient:  &brokerv1.EnvelopeRecipient{Target: &brokerv1.EnvelopeRecipient_UserId{UserId: "eve@example.com"}},
		Task:       &brokerv1.EnvelopeTask{Intent: "escalate"},
		Delegation: &brokerv1.EnvelopeDelegation{AttenuatedScopes: []string{"admin:all"}},
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("want PermissionDenied for forged from_user_id, got %v", err)
	}
	if store.count != 0 {
		t.Fatalf("forged send persisted %d envelope(s)", store.count)
	}
}

func TestSendEnvelope_ForgedTenant_PermissionDenied(t *testing.T) {
	store := &trackingEnvelopeStore{}
	svc := &BrokerService{deps: Deps{Logger: zap.NewNop(), Tasks: store}}
	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")

	_, err := svc.SendEnvelope(ctx, &brokerv1.SendEnvelopeRequest{
		TenantId:   spoofedTenantUUID,
		FromUserId: "alice@example.com",
		Recipient:  &brokerv1.EnvelopeRecipient{Target: &brokerv1.EnvelopeRecipient_UserId{UserId: "bob@example.com"}},
		Task:       &brokerv1.EnvelopeTask{Intent: "cross-tenant"},
		Delegation: &brokerv1.EnvelopeDelegation{},
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("want PermissionDenied for forged tenant_id, got %v", err)
	}
	if store.count != 0 {
		t.Fatalf("forged send persisted %d envelope(s)", store.count)
	}
}

// inboxRecordingStore records the (tenant, user) ListInbox was queried with.
type inboxRecordingStore struct {
	stubTaskStore
	gotTenant, gotUser string
}

func (s *inboxRecordingStore) ListInbox(_ context.Context, tenantID, userID string, _ bool) ([]*db.Envelope, error) {
	s.gotTenant, s.gotUser = tenantID, userID
	return nil, nil
}

func TestListInboxEnvelopes_ForgedUser_PermissionDenied(t *testing.T) {
	store := &inboxRecordingStore{}
	svc := &BrokerService{deps: Deps{Logger: zap.NewNop(), Tasks: store}}
	ctx := ctxWithIdentity(testTenantUUID, "eve@example.com")

	_, err := svc.ListInboxEnvelopes(ctx, &brokerv1.ListInboxEnvelopesRequest{
		TenantId: testTenantUUID,
		UserId:   "alice@example.com", // forged: read alice's inbox
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("want PermissionDenied for forged user_id, got %v", err)
	}
	if store.gotUser != "" {
		t.Fatalf("inbox queried for %q despite the forged user_id", store.gotUser)
	}
}

func TestListInboxEnvelopes_ForgedTenant_PermissionDenied(t *testing.T) {
	store := &inboxRecordingStore{}
	svc := &BrokerService{deps: Deps{Logger: zap.NewNop(), Tasks: store}}
	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")

	_, err := svc.ListInboxEnvelopes(ctx, &brokerv1.ListInboxEnvelopesRequest{
		TenantId: spoofedTenantUUID,
		UserId:   "alice@example.com",
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("want PermissionDenied for forged tenant_id, got %v", err)
	}
	if store.gotTenant != "" {
		t.Fatalf("inbox queried under tenant %q despite the forged tenant", store.gotTenant)
	}
}

func TestListInboxEnvelopes_BindsIdentity(t *testing.T) {
	store := &inboxRecordingStore{}
	svc := &BrokerService{deps: Deps{Logger: zap.NewNop(), Tasks: store}}

	// Verified identity, request fields omitted → identity is the source.
	if _, err := svc.ListInboxEnvelopes(ctxWithIdentity(testTenantUUID, "alice@example.com"), &brokerv1.ListInboxEnvelopesRequest{}); err != nil {
		t.Fatalf("matching identity: %v", err)
	}
	if store.gotTenant != testTenantUUID || store.gotUser != "alice@example.com" {
		t.Fatalf("ListInbox got (%q, %q), want (%q, alice@example.com)", store.gotTenant, store.gotUser, testTenantUUID)
	}

	// OIDC disabled (no identity) → request fields are the source.
	if _, err := svc.ListInboxEnvelopes(context.Background(), &brokerv1.ListInboxEnvelopesRequest{
		TenantId: testTenantUUID, UserId: "bob@example.com",
	}); err != nil {
		t.Fatalf("dev mode: %v", err)
	}
	if store.gotUser != "bob@example.com" {
		t.Fatalf("dev mode ListInbox user = %q, want bob@example.com", store.gotUser)
	}
}

// TestSendEnvelope_AutoAccept_RecordsDelegatedBy pins AG-17 S7 on the
// auto-accept path: each spawned recipient task names the sender as
// delegated_by.
func TestSendEnvelope_AutoAccept_RecordsDelegatedBy(t *testing.T) {
	f := &fakeFGA{
		listObjectsResultByUser: map[string][]string{
			"user:alice@example.com": {"group:alpha"},
			"user:bob@example.com":   {"group:alpha"},
		},
		reads: map[string][]fgaKey{
			"group:alpha": {
				memberTuple("alice@example.com", "group:alpha"),
				memberTuple("bob@example.com", "group:alpha"),
			},
		},
	}
	fgaSrv := f.server(t)
	defer fgaSrv.Close()
	opaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"allow": true, "auto_accept": true}})
	}))
	defer opaSrv.Close()

	store := &trackingEnvelopeStore{}
	deps := testEnvelopeDeps(t, fgaSrv.URL, opaSrv.URL)
	deps.Tasks = store
	svc := NewBrokerService(deps)

	if _, err := svc.SendEnvelope(ctxWithIdentity(testTenantUUID, "alice@example.com"), &brokerv1.SendEnvelopeRequest{
		TenantId:   testTenantUUID,
		FromUserId: "alice@example.com",
		Recipient:  &brokerv1.EnvelopeRecipient{Target: &brokerv1.EnvelopeRecipient_GroupId{GroupId: "group:alpha"}},
		Task:       &brokerv1.EnvelopeTask{Intent: "auto"},
		Delegation: &brokerv1.EnvelopeDelegation{},
	}); err != nil {
		t.Fatalf("SendEnvelope: %v", err)
	}
	if len(store.created) != 1 {
		t.Fatalf("want 1 auto-accepted task, got %d", len(store.created))
	}
	task := store.created[0]
	if task.OwnerUserID != "bob@example.com" || task.OnBehalfOf().DelegatedBy != "alice@example.com" {
		t.Errorf("owner=%q delegated_by=%q, want bob@example.com / alice@example.com", task.OwnerUserID, task.OnBehalfOf().DelegatedBy)
	}
}

// G48/F250: oversized envelope fields are refused before any delivery.
func TestSendEnvelope_FieldCaps(t *testing.T) {
	f := &fakeFGA{}
	fgaSrv := f.server(t)
	defer fgaSrv.Close()
	opaSrv := opaAlwaysAllow(t)
	defer opaSrv.Close()

	store := &trackingEnvelopeStore{}
	deps := testEnvelopeDeps(t, fgaSrv.URL, opaSrv.URL)
	deps.Tasks = store
	svc := NewBrokerService(deps)
	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")
	bob := &brokerv1.EnvelopeRecipient{Target: &brokerv1.EnvelopeRecipient_UserId{UserId: "bob@example.com"}}

	for name, task := range map[string]*brokerv1.EnvelopeTask{
		"intent":          {Intent: strings.Repeat("a", maxPromptBytes+1)},
		"payload_ref":     {Intent: "x", PayloadRef: strings.Repeat("a", maxEnvelopePayloadRefBytes+1)},
		"required_skills": {Intent: "x", RequiredSkills: make([]string, maxEnvelopeListItems+1)},
	} {
		_, err := svc.SendEnvelope(ctx, &brokerv1.SendEnvelopeRequest{
			TenantId: testTenantUUID, Recipient: bob, Task: task, Delegation: &brokerv1.EnvelopeDelegation{},
		})
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("%s: got %v, want InvalidArgument", name, err)
		}
	}
	if store.count != 0 {
		t.Fatalf("an oversized envelope was stored")
	}
}

// F250: a group send past maxEnvelopeFanOut members is refused outright.
func TestSendEnvelope_GroupFanOutCap(t *testing.T) {
	tuples := []fgaKey{memberTuple("alice@example.com", "group:big")}
	for i := 0; i <= maxEnvelopeFanOut; i++ {
		tuples = append(tuples, memberTuple(fmt.Sprintf("u%d@example.com", i), "group:big"))
	}
	f := &fakeFGA{
		listObjectsResultByUser: map[string][]string{"user:alice@example.com": {"group:big"}},
		reads:                   map[string][]fgaKey{"group:big": tuples},
	}
	fgaSrv := f.server(t)
	defer fgaSrv.Close()
	opaSrv := opaAlwaysAllow(t)
	defer opaSrv.Close()

	store := &trackingEnvelopeStore{}
	deps := testEnvelopeDeps(t, fgaSrv.URL, opaSrv.URL)
	deps.Tasks = store
	svc := NewBrokerService(deps)

	_, err := svc.SendEnvelope(ctxWithIdentity(testTenantUUID, "alice@example.com"), &brokerv1.SendEnvelopeRequest{
		TenantId:   testTenantUUID,
		Recipient:  &brokerv1.EnvelopeRecipient{Target: &brokerv1.EnvelopeRecipient_GroupId{GroupId: "group:big"}},
		Task:       &brokerv1.EnvelopeTask{Intent: "fan this out"},
		Delegation: &brokerv1.EnvelopeDelegation{},
	})
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("got %v, want ResourceExhausted", err)
	}
	if store.count != 0 {
		t.Fatalf("created %d envelopes past the fan-out cap", store.count)
	}
}
