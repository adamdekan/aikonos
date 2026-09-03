package broker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/google/uuid"
	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/auth"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/policy"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// fakeFGA stands in for OpenFGA: /check allows the admin relation only for users
// in admins; /read returns tuples keyed by the requested object filter; /write
// captures the raw body; /list-objects returns listObjectsResult and records the
// queried user in listObjectsUser.
type fakeFGA struct {
	admins            map[string]bool
	checks            map[string]bool     // composite "user|relation|object" → allowed (consulted before admins)
	reads             map[string][]fgaKey // object filter (e.g. "group:") → tuples
	mu                sync.Mutex
	writes            []map[string]any
	deletes           []map[string]any
	checkErr          bool     // when true, /check returns HTTP 500 (simulates transport error)
	errOnObject       string   // when set, /check returns HTTP 500 only for this object (per-object transport error)
	writeErr          bool     // when true, /write returns HTTP 500 (simulates FGA write failure)
	deleteOnlyErr     bool     // when true, /write returns HTTP 500 only when the body contains Deletes
	listObjectsResult       []string            // objects returned by /list-objects (fallback when listObjectsResultByUser has no entry)
	listObjectsResultByUser map[string][]string // per-user objects returned by /list-objects (takes priority over listObjectsResult)
	listObjectsByRelation   map[string][]string // per-relation objects returned by /list-objects (highest priority; for callers that query several relations)
	listObjectsErr          bool                // when true, /list-objects returns HTTP 500
	listObjectsUser         string              // last user arg sent to /list-objects
	listObjectsCalls        int                 // number of /list-objects requests served (cache-hit assertions)
}

type fgaKey struct {
	User     string `json:"user"`
	Relation string `json:"relation"`
	Object   string `json:"object"`
}

func (f *fakeFGA) server(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case strings.HasSuffix(r.URL.Path, "/check"):
			if f.checkErr {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			var req struct {
				TupleKey struct{ User, Relation, Object string } `json:"tuple_key"`
			}
			_ = json.Unmarshal(body, &req)
			if f.errOnObject != "" && req.TupleKey.Object == f.errOnObject {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			compositeKey := req.TupleKey.User + "|" + req.TupleKey.Relation + "|" + req.TupleKey.Object
			if allowed, ok := f.checks[compositeKey]; ok {
				_ = json.NewEncoder(w).Encode(map[string]bool{"allowed": allowed})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]bool{"allowed": f.admins[req.TupleKey.User]})
		case strings.HasSuffix(r.URL.Path, "/read"):
			// Faithful to OpenFGA: an absent/empty tuple_key reads every tuple;
			// an exact object filter returns just that object's tuples.
			var req struct {
				TupleKey *struct{ Object string } `json:"tuple_key"`
			}
			_ = json.Unmarshal(body, &req)
			var keys []fgaKey
			if req.TupleKey == nil || req.TupleKey.Object == "" {
				for _, ks := range f.reads {
					keys = append(keys, ks...)
				}
			} else {
				keys = f.reads[req.TupleKey.Object]
			}
			wrapped := make([]map[string]fgaKey, 0, len(keys))
			for _, k := range keys {
				wrapped = append(wrapped, map[string]fgaKey{"key": k})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"tuples": wrapped})
		case strings.HasSuffix(r.URL.Path, "/list-objects"):
			if f.listObjectsErr {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			var req struct {
				User     string `json:"user"`
				Relation string `json:"relation"`
			}
			_ = json.Unmarshal(body, &req)
			f.mu.Lock()
			f.listObjectsUser = req.User
			f.listObjectsCalls++
			var objects []string
			if byRel, ok := f.listObjectsByRelation[req.Relation]; ok {
				objects = byRel
			} else if byUser, ok := f.listObjectsResultByUser[req.User]; ok {
				objects = byUser
			} else {
				objects = f.listObjectsResult
			}
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"objects": objects})
		case strings.HasSuffix(r.URL.Path, "/write"):
			if f.writeErr {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			var req struct {
				Writes *struct {
					TupleKeys []map[string]any `json:"tuple_keys"`
				} `json:"writes"`
				Deletes *struct {
					TupleKeys []map[string]any `json:"tuple_keys"`
				} `json:"deletes"`
			}
			_ = json.Unmarshal(body, &req)
			if f.deleteOnlyErr && req.Deletes != nil {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			f.mu.Lock()
			if req.Writes != nil {
				f.writes = append(f.writes, req.Writes.TupleKeys...)
			}
			if req.Deletes != nil {
				f.deletes = append(f.deletes, req.Deletes.TupleKeys...)
			}
			f.mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		default:
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotImplemented)
		}
	}))
}

func testAdminDeps(t *testing.T, fgaURL string) Deps {
	t.Helper()
	em, err := audit.NewEmitter(context.Background(), audit.Config{})
	if err != nil {
		t.Fatal(err)
	}
	cfg := policy.Config{OPAEndpoint: "http://unused", OpenFGAEndpoint: fgaURL}
	if fgaURL != "" {
		cfg.OpenFGAStoreID = "store-1"
	}
	eng, err := policy.NewEngine(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return Deps{
		Logger:     zap.NewNop(),
		Audit:      em,
		Policy:     eng,
		TenantID:   "aikonos-dev",
		KnownUsers: []string{"admin@example.com", "alice@example.com", "bob@example.com"},
	}
}

func tuple(user, relation, object string) *brokerv1.RoleTuple {
	return &brokerv1.RoleTuple{User: user, Relation: relation, Object: object}
}

func TestAssignRole_NonAdminDenied(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	svc := NewBrokerService(testAdminDeps(t, srv.URL))

	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")
	_, err := svc.AssignRole(ctx, &brokerv1.AssignRoleRequest{
		UserId: "alice@example.com",
		Tuple:  tuple("user:bob@example.com", "member", "group:security-team"),
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-admin should be denied, got %v", err)
	}
	if len(f.writes) != 0 {
		t.Errorf("denied assign must not write, got %+v", f.writes)
	}
}

func TestAssignRole_AdminAllowed(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	svc := NewBrokerService(testAdminDeps(t, srv.URL))

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	resp, err := svc.AssignRole(ctx, &brokerv1.AssignRoleRequest{
		UserId: "admin@example.com",
		Tuple:  tuple("user:bob@example.com", "member", "group:security-team"),
	})
	if err != nil || !resp.Success {
		t.Fatalf("admin assign: resp=%v err=%v", resp, err)
	}
	if len(f.writes) != 1 || f.writes[0]["object"] != "group:security-team" || f.writes[0]["relation"] != "member" {
		t.Errorf("write tuple wrong: %+v", f.writes)
	}
}

func TestAssignRole_RejectsForeignTenant(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	svc := NewBrokerService(testAdminDeps(t, srv.URL))

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	_, err := svc.AssignRole(ctx, &brokerv1.AssignRoleRequest{
		UserId: "admin@example.com",
		Tuple:  tuple("user:mallory@example.com", "admin", "tenant:other"),
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("foreign-tenant assign should be PermissionDenied, got %v", err)
	}
	if len(f.writes) != 0 {
		t.Errorf("must not write a foreign-tenant tuple, got %+v", f.writes)
	}
}

func TestAssignRole_RejectsUnmanageableRelation(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	svc := NewBrokerService(testAdminDeps(t, srv.URL))

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	_, err := svc.AssignRole(ctx, &brokerv1.AssignRoleRequest{
		UserId: "admin@example.com",
		Tuple:  tuple("user:bob@example.com", "can_read", "workspace:shared"),
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("unmanageable relation should be InvalidArgument, got %v", err)
	}
}

func TestAssignRole_RejectsBadSubjectKind(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	svc := NewBrokerService(testAdminDeps(t, srv.URL))

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	// tenant.admin only accepts a user, not a group#member.
	_, err := svc.AssignRole(ctx, &brokerv1.AssignRoleRequest{
		UserId: "admin@example.com",
		Tuple:  tuple("group:security-team#member", "admin", "tenant:aikonos-dev"),
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("group subject for tenant.admin should be InvalidArgument, got %v", err)
	}
}

func TestRevokeRole_AdminAllowed(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	svc := NewBrokerService(testAdminDeps(t, srv.URL))

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	resp, err := svc.RevokeRole(ctx, &brokerv1.RevokeRoleRequest{
		UserId: "admin@example.com",
		Tuple:  tuple("user:bob@example.com", "member", "group:security-team"),
	})
	if err != nil || !resp.Success {
		t.Fatalf("admin revoke: resp=%v err=%v", resp, err)
	}
	if len(f.deletes) != 1 || f.deletes[0]["object"] != "group:security-team" {
		t.Errorf("delete tuple wrong: %+v", f.deletes)
	}
	if len(f.writes) != 0 {
		t.Errorf("revoke must not write, got %+v", f.writes)
	}
}

func TestListAssignments_BucketsAndPrincipals(t *testing.T) {
	f := &fakeFGA{
		admins: map[string]bool{"user:admin@example.com": true},
		reads: map[string][]fgaKey{
			"tenant:aikonos-dev": {
				{User: "user:admin@example.com", Relation: "admin", Object: "tenant:aikonos-dev"},
				{User: "user:alice@example.com", Relation: "member", Object: "tenant:aikonos-dev"},
			},
			"group:": {
				{User: "user:alice@example.com", Relation: "member", Object: "group:security-team"},
			},
			"skill:": {
				{User: "group:security-team#member", Relation: "permitted_group", Object: "skill:web.fetch"},
			},
			"agent:": {
				{User: "user:alice@example.com", Relation: "owner_user", Object: "agent:alice-agent"},
			},
		},
	}
	srv := f.server(t)
	defer srv.Close()
	svc := NewBrokerService(testAdminDeps(t, srv.URL))

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	resp, err := svc.ListAssignments(ctx, &brokerv1.ListAssignmentsRequest{UserId: "admin@example.com"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !resp.FgaEnabled {
		t.Error("fga_enabled should be true with a live store")
	}

	bySection := map[brokerv1.AssignmentSection]int{}
	for _, tp := range resp.Tuples {
		bySection[tp.Section]++
	}
	if bySection[brokerv1.AssignmentSection_TENANT_ROLE] != 2 {
		t.Errorf("tenant section = %d, want 2", bySection[brokerv1.AssignmentSection_TENANT_ROLE])
	}
	if bySection[brokerv1.AssignmentSection_GROUP_MEMBERSHIP] != 1 ||
		bySection[brokerv1.AssignmentSection_SKILL_ACCESS] != 1 ||
		bySection[brokerv1.AssignmentSection_AGENT_RELATION] != 1 {
		t.Errorf("section buckets wrong: %+v", bySection)
	}

	// Principals: users + the group (collapsed from group#member) + the agent,
	// deduped across tuples, plus the KnownUsers seed.
	kinds := map[string]string{}
	for _, p := range resp.Principals {
		kinds[p.Id] = p.Kind
	}
	if kinds["group:security-team"] != "group" {
		t.Errorf("expected group:security-team principal (group), got %+v", kinds)
	}
	if kinds["agent:alice-agent"] != "agent" {
		t.Errorf("expected agent:alice-agent principal, got %+v", kinds)
	}
	if kinds["user:bob@example.com"] != "user" {
		t.Error("expected seeded user:bob@example.com principal from KnownUsers")
	}
}

// TestPrincipalsFrom_IncludesDirectoryOnlyUser proves a JIT-provisioned user
// with zero FGA tuples still surfaces as a principal, sourced from the user
// directory — otherwise a login that predates any matching provisioning rule
// (ClaimProvisioning is one-shot, see provisioner.go) leaves that user
// permanently invisible to admins, with no way to grant them a role from
// this RPC's response.
func TestPrincipalsFrom_IncludesDirectoryOnlyUser(t *testing.T) {
	svc := NewBrokerService(testAdminDeps(t, ""))
	dirEntries := map[string]db.UserDirectoryEntry{
		"11111111-0000-0000-0000-000000000099": {Email: "new.hire@example.com", Name: "New Hire"},
	}

	principals := svc.principalsFrom(nil, dirEntries, nil)

	var found *brokerv1.Principal
	for _, p := range principals {
		if p.Id == "user:11111111-0000-0000-0000-000000000099" {
			found = p
			break
		}
	}
	if found == nil {
		t.Fatalf("directory-only user not in principals; got %+v", principals)
	}
	if found.Kind != "user" {
		t.Errorf("Kind = %q, want %q", found.Kind, "user")
	}
	if found.Source != "directory" {
		t.Errorf("Source = %q, want %q", found.Source, "directory")
	}
	if found.DisplayName != "New Hire" {
		t.Errorf("DisplayName = %q, want %q", found.DisplayName, "New Hire")
	}
}

func TestListAssignments_DegradesOnReadError(t *testing.T) {
	// A read that 500s must not crash the console: ListAssignments still
	// succeeds, with empty tuples and a single warning.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/check"):
			_, _ = w.Write([]byte(`{"allowed":true}`))
		case strings.HasSuffix(r.URL.Path, "/read"):
			http.Error(w, "boom", http.StatusInternalServerError)
		default:
			http.Error(w, "x", http.StatusNotImplemented)
		}
	}))
	defer srv.Close()
	svc := NewBrokerService(testAdminDeps(t, srv.URL))

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	resp, err := svc.ListAssignments(ctx, &brokerv1.ListAssignmentsRequest{UserId: "admin@example.com"})
	if err != nil {
		t.Fatalf("degraded list must still succeed: %v", err)
	}
	if len(resp.Tuples) != 0 {
		t.Errorf("expected no tuples on read failure, got %+v", resp.Tuples)
	}
	if len(resp.Warnings) != 1 {
		t.Errorf("expected a single warning, got %+v", resp.Warnings)
	}
}

func TestListAssignments_DropsForeignTenantAndNonCurated(t *testing.T) {
	// A single read-all returns tuples across object types; the home-tenant
	// filter drops other tenants' tenant: rows and non-curated types (task:).
	f := &fakeFGA{
		admins: map[string]bool{"user:admin@example.com": true},
		reads: map[string][]fgaKey{
			"all": {
				{User: "user:admin@example.com", Relation: "admin", Object: "tenant:aikonos-dev"},
				{User: "user:mallory@evil.dev", Relation: "admin", Object: "tenant:other"},
				{User: "user:alice@example.com", Relation: "member", Object: "group:security-team"},
				{User: "user:alice@example.com", Relation: "owner", Object: "task:abc-123"},
			},
		},
	}
	srv := f.server(t)
	defer srv.Close()
	svc := NewBrokerService(testAdminDeps(t, srv.URL))

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	resp, err := svc.ListAssignments(ctx, &brokerv1.ListAssignmentsRequest{UserId: "admin@example.com"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, tp := range resp.Tuples {
		if tp.Object == "tenant:other" || strings.HasPrefix(tp.Object, "task:") {
			t.Errorf("foreign-tenant / non-curated tuple leaked: %+v", tp)
		}
	}
	if len(resp.Tuples) != 2 {
		t.Errorf("expected 2 curated home-tenant tuples (tenant admin + group member), got %d: %+v", len(resp.Tuples), resp.Tuples)
	}
}

func TestRevokeRole_AllowsLegacyTupleAssignWouldReject(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	svc := NewBrokerService(testAdminDeps(t, srv.URL))

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	// A user directly on skill.permitted_group (model wants group#member): the
	// seed actually contains one. Assign must reject it...
	legacy := tuple("user:alice@example.com", "permitted_group", "skill:email.draft")
	if _, err := svc.AssignRole(ctx, &brokerv1.AssignRoleRequest{UserId: "admin@example.com", Tuple: legacy}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("assign of user-on-skill should be InvalidArgument, got %v", err)
	}
	// ...but revoke must allow clearing it (de-privileging is always safe).
	resp, err := svc.RevokeRole(ctx, &brokerv1.RevokeRoleRequest{UserId: "admin@example.com", Tuple: legacy})
	if err != nil || !resp.Success {
		t.Fatalf("revoke of legacy tuple should succeed: resp=%v err=%v", resp, err)
	}
	if len(f.deletes) != 1 || f.deletes[0]["object"] != "skill:email.draft" {
		t.Errorf("delete tuple wrong: %+v", f.deletes)
	}
}

func TestRevokeRole_StillRejectsForeignTenant(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	svc := NewBrokerService(testAdminDeps(t, srv.URL))

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	_, err := svc.RevokeRole(ctx, &brokerv1.RevokeRoleRequest{
		UserId: "admin@example.com",
		Tuple:  tuple("user:mallory@example.com", "admin", "tenant:other"),
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("foreign-tenant revoke should be PermissionDenied, got %v", err)
	}
	if len(f.deletes) != 0 {
		t.Errorf("must not delete a foreign-tenant tuple, got %+v", f.deletes)
	}
}

func TestListAssignments_DevStubEmpty(t *testing.T) {
	// No FGA store → CheckFGA allow-all (admin gate passes), reads return empty.
	svc := NewBrokerService(testAdminDeps(t, ""))
	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	resp, err := svc.ListAssignments(ctx, &brokerv1.ListAssignmentsRequest{UserId: "admin@example.com"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if resp.FgaEnabled {
		t.Error("fga_enabled should be false in dev stub")
	}
	if len(resp.Tuples) != 0 {
		t.Errorf("dev stub should yield no tuples, got %+v", resp.Tuples)
	}
	// Principals still seed from KnownUsers (the future-directory seam).
	if len(resp.Principals) == 0 {
		t.Error("expected seeded principals from KnownUsers")
	}
}

func TestAssignRole_DevStubRefusesMutation(t *testing.T) {
	// Dev stub: admin gate is allow-all, but a mutation must not silently no-op.
	svc := NewBrokerService(testAdminDeps(t, ""))
	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	_, err := svc.AssignRole(ctx, &brokerv1.AssignRoleRequest{
		UserId: "admin@example.com",
		Tuple:  tuple("user:bob@example.com", "member", "group:security-team"),
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("dev-stub assign should be FailedPrecondition, got %v", err)
	}
}

func TestAdminIdentity_RejectsNonUUIDTenant(t *testing.T) {
	svc := NewBrokerService(testAdminDeps(t, ""))
	ctx := ctxWithIdentity("not-a-uuid", "admin@example.com")
	_, err := svc.ListAssignments(ctx, &brokerv1.ListAssignmentsRequest{UserId: "admin@example.com"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument for non-uuid tenant, got %v", err)
	}
}

// A non-admin token holder must not be able to have the admin gate evaluated
// against a forged user_id: the acting principal is bound to the verified OIDC
// subject, so claiming user_id=admin while authenticated as alice is rejected
// before any FGA check or mutation.
func TestAssignRole_RejectsUserIdSpoofing(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	svc := NewBrokerService(testAdminDeps(t, srv.URL))

	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com") // authenticated as a plain member
	_, err := svc.AssignRole(ctx, &brokerv1.AssignRoleRequest{
		UserId: "admin@example.com", // ...but claiming to be the admin
		Tuple:  tuple("user:alice@example.com", "admin", "tenant:aikonos-dev"),
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("spoofed user_id should be PermissionDenied, got %v", err)
	}
	if len(f.writes) != 0 {
		t.Errorf("spoofed assign must not write, got %+v", f.writes)
	}
}

func TestListAssignments_RejectsUserIdSpoofing(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	svc := NewBrokerService(testAdminDeps(t, srv.URL))

	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")
	_, err := svc.ListAssignments(ctx, &brokerv1.ListAssignmentsRequest{UserId: "admin@example.com"})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("spoofed user_id should be PermissionDenied, got %v", err)
	}
}

// TestListAssignments_AdminGateChecksSubjectOid proves the tenant-admin FGA gate
// is evaluated against the OIDC Subject (the OID), not the Email, when the two
// differ — the zero-trust fix for Entra-shaped identities.
func TestListAssignments_AdminGateChecksSubjectOid(t *testing.T) {
	const oid = "cccccccc-oid"
	const email = "adam@example.com"

	// Round 1: gate allows only user:<oid> → must succeed (proving the gate used Subject).
	t.Run("allows when oid is admin", func(t *testing.T) {
		f := &fakeFGA{admins: map[string]bool{"user:" + oid: true}}
		srv := f.server(t)
		defer srv.Close()
		svc := NewBrokerService(testAdminDeps(t, srv.URL))

		ctx := auth.WithIdentity(context.Background(), &auth.Identity{
			Subject:  oid,
			Email:    email,
			TenantID: testTenantUUID,
		})
		_, err := svc.ListAssignments(ctx, &brokerv1.ListAssignmentsRequest{UserId: oid})
		if err != nil {
			t.Fatalf("gate should pass for user:%s (oid), got %v", oid, err)
		}
	})

	// Round 2: gate allows only user:<email> → must be PermissionDenied (proving
	// the gate did NOT use the email as the authz principal).
	t.Run("denied when only email is admin", func(t *testing.T) {
		f := &fakeFGA{admins: map[string]bool{"user:" + email: true}}
		srv := f.server(t)
		defer srv.Close()
		svc := NewBrokerService(testAdminDeps(t, srv.URL))

		ctx := auth.WithIdentity(context.Background(), &auth.Identity{
			Subject:  oid,
			Email:    email,
			TenantID: testTenantUUID,
		})
		_, err := svc.ListAssignments(ctx, &brokerv1.ListAssignmentsRequest{UserId: oid})
		if status.Code(err) != codes.PermissionDenied {
			t.Fatalf("gate should deny when only email is admin (used oid for authz), got %v", err)
		}
	})
}

// TestListAssignments_AgentDisplayName verifies that when the Agents store has a
// record for an agent uuid, the corresponding principal's DisplayName is the
// agent's Name rather than the bare uuid.
func TestListAssignments_AgentDisplayName(t *testing.T) {
	agentUUID := "11111111-0000-0000-0000-000000000001"
	f := &fakeFGA{
		admins: map[string]bool{"user:admin@example.com": true},
		reads: map[string][]fgaKey{
			"all": {
				{User: "agent:" + agentUUID, Relation: "permitted_agent", Object: "mcp_connector:x"},
			},
		},
	}
	srv := f.server(t)
	defer srv.Close()

	deps := testAdminDeps(t, srv.URL)
	store := &fakeAgentStore{}
	agentID, err := uuid.Parse(agentUUID)
	if err != nil {
		t.Fatal(err)
	}
	tenantID, err := uuid.Parse(testTenantUUID)
	if err != nil {
		t.Fatal(err)
	}
	_ = store.Create(context.Background(), &db.Agent{
		ID:       agentID,
		TenantID: tenantID,
		Name:     "echo-agent",
	})
	deps.Agents = store

	svc := NewBrokerService(deps)
	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	resp, err := svc.ListAssignments(ctx, &brokerv1.ListAssignmentsRequest{UserId: "admin@example.com"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	var found *brokerv1.Principal
	for _, p := range resp.Principals {
		if p.Id == "agent:"+agentUUID {
			found = p
			break
		}
	}
	if found == nil {
		t.Fatalf("agent principal not in response; principals=%+v", resp.Principals)
	}
	if found.DisplayName != "echo-agent" {
		t.Errorf("DisplayName = %q, want %q", found.DisplayName, "echo-agent")
	}
}

// TestListAssignments_AgentDisplayNameFallback verifies that when there is no
// Agents store entry for an agent uuid, DisplayName falls back to the bare uuid.
func TestListAssignments_AgentDisplayNameFallback(t *testing.T) {
	agentUUID := "22222222-0000-0000-0000-000000000002"
	f := &fakeFGA{
		admins: map[string]bool{"user:admin@example.com": true},
		reads: map[string][]fgaKey{
			"all": {
				{User: "agent:" + agentUUID, Relation: "permitted_agent", Object: "mcp_connector:y"},
			},
		},
	}
	srv := f.server(t)
	defer srv.Close()

	// Agents store is nil → no lookup possible.
	svc := NewBrokerService(testAdminDeps(t, srv.URL))
	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	resp, err := svc.ListAssignments(ctx, &brokerv1.ListAssignmentsRequest{UserId: "admin@example.com"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	var found *brokerv1.Principal
	for _, p := range resp.Principals {
		if p.Id == "agent:"+agentUUID {
			found = p
			break
		}
	}
	if found == nil {
		t.Fatalf("agent principal not in response; principals=%+v", resp.Principals)
	}
	if found.DisplayName != agentUUID {
		t.Errorf("DisplayName = %q, want uuid fallback %q", found.DisplayName, agentUUID)
	}
}

func TestAssignRole_RejectsWildcardSubject(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	svc := NewBrokerService(testAdminDeps(t, srv.URL))

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	_, err := svc.AssignRole(ctx, &brokerv1.AssignRoleRequest{
		UserId: "admin@example.com",
		Tuple:  tuple("user:*", "member", "group:security-team"),
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("wildcard subject should be InvalidArgument, got %v", err)
	}
	if len(f.writes) != 0 {
		t.Errorf("wildcard assign must not write, got %+v", f.writes)
	}
}

// TestAssignRole_AgentSkill_Accepted verifies that an admin can grant
// can_use on an agentskill object to a group (the only permitted subject kind).
func TestAssignRole_AgentSkill_Accepted(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	svc := NewBrokerService(testAdminDeps(t, srv.URL))

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	resp, err := svc.AssignRole(ctx, &brokerv1.AssignRoleRequest{
		UserId: "admin@example.com",
		Tuple:  tuple("group:security-team#member", "can_use", "agentskill:research-assistant"),
	})
	if err != nil || !resp.Success {
		t.Fatalf("agentskill can_use assign failed: resp=%v err=%v", resp, err)
	}
	if len(f.writes) != 1 {
		t.Fatalf("expected 1 write, got %d: %+v", len(f.writes), f.writes)
	}
	if f.writes[0]["object"] != "agentskill:research-assistant" || f.writes[0]["relation"] != "can_use" {
		t.Errorf("write tuple wrong: %+v", f.writes[0])
	}
}

// TestAssignRole_AgentSkill_UnknownRelationRejected verifies that a relation
// other than can_use on an agentskill object is rejected as InvalidArgument.
func TestAssignRole_AgentSkill_UnknownRelationRejected(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	svc := NewBrokerService(testAdminDeps(t, srv.URL))

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	_, err := svc.AssignRole(ctx, &brokerv1.AssignRoleRequest{
		UserId: "admin@example.com",
		Tuple:  tuple("group:security-team#member", "can_invoke", "agentskill:research-assistant"),
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("unknown relation on agentskill should be InvalidArgument, got %v", err)
	}
	if len(f.writes) != 0 {
		t.Errorf("rejected assign must not write, got %+v", f.writes)
	}
}

// TestAssignRole_AgentSkill_UserSubjectRejected verifies that a plain user
// (not group#member) cannot be assigned can_use on an agentskill — the FGA
// model only permits group#member as the subject for this relation.
func TestAssignRole_AgentSkill_UserSubjectRejected(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	svc := NewBrokerService(testAdminDeps(t, srv.URL))

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	_, err := svc.AssignRole(ctx, &brokerv1.AssignRoleRequest{
		UserId: "admin@example.com",
		Tuple:  tuple("user:alice@example.com", "can_use", "agentskill:research-assistant"),
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("user subject for agentskill.can_use should be InvalidArgument, got %v", err)
	}
	if len(f.writes) != 0 {
		t.Errorf("rejected assign must not write, got %+v", f.writes)
	}
}

// TestRevokeRole_AgentSkill_RoundTrip verifies that an admin can revoke a
// can_use grant on an agentskill (the loose revoke path accepts managed types).
func TestRevokeRole_AgentSkill_RoundTrip(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	svc := NewBrokerService(testAdminDeps(t, srv.URL))

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	resp, err := svc.RevokeRole(ctx, &brokerv1.RevokeRoleRequest{
		UserId: "admin@example.com",
		Tuple:  tuple("group:security-team#member", "can_use", "agentskill:research-assistant"),
	})
	if err != nil || !resp.Success {
		t.Fatalf("agentskill can_use revoke failed: resp=%v err=%v", resp, err)
	}
	if len(f.deletes) != 1 || f.deletes[0]["object"] != "agentskill:research-assistant" {
		t.Errorf("delete tuple wrong: %+v", f.deletes)
	}
	if len(f.writes) != 0 {
		t.Errorf("revoke must not write, got %+v", f.writes)
	}
}

// TestListAssignments_AgentSkillBucketsToSkillAccess verifies that agentskill
// tuples are bucketed into SKILL_ACCESS (reusing the existing section since no
// dedicated proto enum value exists in the frozen CP2 proto).
func TestListAssignments_AgentSkillBucketsToSkillAccess(t *testing.T) {
	f := &fakeFGA{
		admins: map[string]bool{"user:admin@example.com": true},
		reads: map[string][]fgaKey{
			"all": {
				{User: "user:admin@example.com", Relation: "admin", Object: "tenant:aikonos-dev"},
				{User: "group:security-team#member", Relation: "can_use", Object: "agentskill:research-assistant"},
				{User: "group:security-team#member", Relation: "permitted_group", Object: "skill:web.fetch"},
			},
		},
	}
	srv := f.server(t)
	defer srv.Close()
	svc := NewBrokerService(testAdminDeps(t, srv.URL))

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	resp, err := svc.ListAssignments(ctx, &brokerv1.ListAssignmentsRequest{UserId: "admin@example.com"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	var foundAgentSkill bool
	for _, tp := range resp.Tuples {
		if tp.Object == "agentskill:research-assistant" {
			foundAgentSkill = true
			if tp.Section != brokerv1.AssignmentSection_SKILL_ACCESS {
				t.Errorf("agentskill tuple section = %v, want SKILL_ACCESS", tp.Section)
			}
		}
	}
	if !foundAgentSkill {
		t.Error("agentskill:research-assistant tuple not present in ListAssignments response")
	}
}
