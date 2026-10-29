package scim

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/policy"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
)

const tenant = "11111111-1111-1111-1111-111111111111"

// memStore is an in-memory Store with the repo's uniqueness rules.
type memStore struct {
	users  []db.ScimUser
	groups []db.ScimGroup
}

func (m *memStore) ListUsers(context.Context, string) ([]db.ScimUser, error) {
	return append([]db.ScimUser(nil), m.users...), nil
}
func (m *memStore) GetUser(_ context.Context, _ string, id uuid.UUID) (db.ScimUser, error) {
	for _, u := range m.users {
		if u.ID == id {
			return u, nil
		}
	}
	return db.ScimUser{}, db.ErrNotFound
}
func (m *memStore) conflict(u *db.ScimUser) bool {
	for _, x := range m.users {
		if x.ID != u.ID && (strings.EqualFold(x.UserName, u.UserName) || x.ExternalID == u.ExternalID) {
			return true
		}
	}
	return false
}
func (m *memStore) CreateUser(_ context.Context, _ string, u *db.ScimUser) error {
	if m.conflict(u) {
		return db.ErrScimConflict
	}
	u.ID, u.CreatedAt, u.UpdatedAt = uuid.New(), time.Now(), time.Now()
	m.users = append(m.users, *u)
	return nil
}
func (m *memStore) UpdateUser(_ context.Context, _ string, u *db.ScimUser) error {
	if m.conflict(u) {
		return db.ErrScimConflict
	}
	for i := range m.users {
		if m.users[i].ID == u.ID {
			m.users[i] = *u
			return nil
		}
	}
	return db.ErrNotFound
}
func (m *memStore) DeleteUser(_ context.Context, _ string, id uuid.UUID) error {
	for i, u := range m.users {
		if u.ID == id {
			m.users = append(m.users[:i], m.users[i+1:]...)
			for j := range m.groups {
				var kept []uuid.UUID
				for _, x := range m.groups[j].Members {
					if x != id {
						kept = append(kept, x)
					}
				}
				m.groups[j].Members = kept
			}
			return nil
		}
	}
	return db.ErrNotFound
}
func (m *memStore) ListGroups(context.Context, string) ([]db.ScimGroup, error) {
	return append([]db.ScimGroup(nil), m.groups...), nil
}
func (m *memStore) GetGroup(_ context.Context, _ string, id uuid.UUID) (db.ScimGroup, error) {
	for _, g := range m.groups {
		if g.ID == id {
			return g, nil
		}
	}
	return db.ScimGroup{}, db.ErrNotFound
}
func (m *memStore) CreateGroup(_ context.Context, _ string, g *db.ScimGroup) error {
	for _, x := range m.groups {
		if x.FGAGroup == g.FGAGroup {
			return db.ErrScimConflict
		}
	}
	g.ID, g.CreatedAt, g.UpdatedAt = uuid.New(), time.Now(), time.Now()
	m.groups = append(m.groups, *g)
	return nil
}
func (m *memStore) UpdateGroup(_ context.Context, _ string, g *db.ScimGroup) error {
	for i := range m.groups {
		if m.groups[i].ID == g.ID {
			m.groups[i] = *g
			return nil
		}
	}
	return db.ErrNotFound
}
func (m *memStore) DeleteGroup(_ context.Context, _ string, id uuid.UUID) error {
	for i, g := range m.groups {
		if g.ID == id {
			m.groups = append(m.groups[:i], m.groups[i+1:]...)
			return nil
		}
	}
	return db.ErrNotFound
}

type memTuples struct {
	set  map[policy.Relation]bool
	fail bool
}

func (t *memTuples) WriteRelations(_ context.Context, rels ...policy.Relation) error {
	if t.fail {
		return fmt.Errorf("fga down")
	}
	for _, r := range rels {
		t.set[r] = true
	}
	return nil
}
func (t *memTuples) DeleteRelations(_ context.Context, rels ...policy.Relation) error {
	for _, r := range rels {
		delete(t.set, r)
	}
	return nil
}

// ReadTuples matches like OpenFGA's Read: exact user and relation when set,
// and an object that is either exact or a bare "type:" prefix.
func (t *memTuples) ReadTuples(_ context.Context, f policy.ReadFilter) ([]policy.Relation, error) {
	var out []policy.Relation
	for r := range t.set {
		if f.User != "" && r.User != f.User || f.Relation != "" && r.Relation != f.Relation {
			continue
		}
		if strings.HasSuffix(f.Object, ":") && !strings.HasPrefix(r.Object, f.Object) ||
			!strings.HasSuffix(f.Object, ":") && f.Object != "" && r.Object != f.Object {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// memBlocks mirrors RevocationRepo.BlockUser: an active block is kept, except
// that a non-SCIM block takes over a SCIM one.
type memBlocks struct{ blocks map[string]db.UserBlock }

func (b *memBlocks) BlockUser(_ context.Context, _, subject, reason, by string) (bool, error) {
	if cur, ok := b.blocks[subject]; ok && (cur.By != BlockedBy || by == BlockedBy) {
		return false, nil
	}
	b.blocks[subject] = db.UserBlock{Reason: reason, By: by}
	return true, nil
}
func (b *memBlocks) UnblockUser(_ context.Context, _, subject, _ string) (bool, error) {
	_, ok := b.blocks[subject]
	delete(b.blocks, subject)
	return ok, nil
}
func (b *memBlocks) UserBlocks(context.Context, string) (map[string]db.UserBlock, error) {
	return b.blocks, nil
}

type memAudit struct{ events []string }

func (a *memAudit) Emit(_ context.Context, e *auditv1.AuditEvent) error {
	a.events = append(a.events, e.EventType+" "+e.ResourceRef+" by "+e.ActorUserId)
	return nil
}

type fixture struct {
	h      *Handler
	tuples *memTuples
	blocks *memBlocks
	audit  *memAudit
}

func newFixture() *fixture {
	f := &fixture{tuples: &memTuples{set: map[policy.Relation]bool{}}, blocks: &memBlocks{blocks: map[string]db.UserBlock{}}, audit: &memAudit{}}
	f.h = &Handler{Store: &memStore{}, Tuples: f.tuples, Blocks: f.blocks, Audit: f.audit, Logger: zap.NewNop()}
	return f
}

// call sends one request the way the gateway forwards it.
func (f *fixture) call(t *testing.T, method, target, body string) (int, map[string]any) {
	t.Helper()
	path, query, _ := strings.Cut(target, "?")
	if query != "" { // the gateway forwards the raw, encoded query string
		v, _ := url.ParseQuery(strings.ReplaceAll(query, " ", "%20"))
		query = v.Encode()
	}
	code, raw := f.h.Serve(context.Background(), Request{Tenant: tenant, Actor: "scim-token:t1", Method: method, Path: path, Query: query, Body: []byte(body)})
	var out map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("%s %s: body is not JSON: %s", method, target, raw)
		}
	}
	return code, out
}

func (f *fixture) member(subject, group string) bool {
	return f.tuples.set[policy.Relation{User: "user:" + subject, Relation: "member", Object: "group:" + group}]
}

// TestEntraSequence replays the request sequence Entra ID's provisioning
// service (and its SCIM validator) sends: look up by userName, create, patch
// with capitalised ops and string booleans, disable, group create, member
// add/remove, rename, delete.
func TestEntraSequence(t *testing.T) {
	f := newFixture()

	// 1. Lookup before create: an empty ListResponse.
	code, body := f.call(t, "GET", `/Users?filter=userName eq "alice@contoso.com"`, "")
	if code != 200 || body["totalResults"].(float64) != 0 {
		t.Fatalf("lookup: %d %v", code, body)
	}

	// 2. Create.
	code, body = f.call(t, "POST", "/Users", `{
	  "schemas":["urn:ietf:params:scim:schemas:core:2.0:User","urn:ietf:params:scim:schemas:extension:enterprise:2.0:User"],
	  "externalId":"oid-alice","userName":"alice@contoso.com","active":true,
	  "emails":[{"primary":true,"type":"work","value":"alice@contoso.com"}],
	  "meta":{"resourceType":"User"},"name":{"formatted":"Alice A","familyName":"A","givenName":"Alice"},"roles":[]}`)
	if code != 201 {
		t.Fatalf("create: %d %v", code, body)
	}
	alice := body["id"].(string)
	if body["active"] != true || body["externalId"] != "oid-alice" {
		t.Fatalf("create body: %v", body)
	}

	// Duplicate create: 409 uniqueness.
	code, body = f.call(t, "POST", "/Users", `{"externalId":"oid-alice","userName":"ALICE@contoso.com","active":true}`)
	if code != 409 || body["scimType"] != "uniqueness" {
		t.Fatalf("duplicate: %d %v", code, body)
	}

	// 3. Lookup finds her (userName is case-insensitive).
	code, body = f.call(t, "GET", `/Users?filter=userName eq "Alice@Contoso.com"`, "")
	if code != 200 || body["totalResults"].(float64) != 1 {
		t.Fatalf("lookup after create: %d %v", code, body)
	}

	// 4. Attribute patch: capitalised ops, filtered email path, unknown attrs ignored.
	code, body = f.call(t, "PATCH", "/Users/"+alice, `{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],"Operations":[
	  {"op":"Replace","path":"emails[type eq \"work\"].value","value":"alice.a@contoso.com"},
	  {"op":"Replace","path":"name.familyName","value":"Anders"},
	  {"op":"Add","path":"displayName","value":"Alice Anders"}]}`)
	if code != 200 || body["displayName"] != "Alice Anders" {
		t.Fatalf("patch: %d %v", code, body)
	}
	if e := body["emails"].([]any)[0].(map[string]any); e["value"] != "alice.a@contoso.com" {
		t.Fatalf("email not replaced: %v", body["emails"])
	}

	// 5. Group lookup, create, add member → FGA tuple.
	code, body = f.call(t, "GET", `/Groups?excludedAttributes=members&filter=displayName eq "Finance Team"`, "")
	if code != 200 || body["totalResults"].(float64) != 0 {
		t.Fatalf("group lookup: %d %v", code, body)
	}
	code, body = f.call(t, "POST", "/Groups", `{"schemas":["urn:ietf:params:scim:schemas:core:2.0:Group"],"externalId":"gid-1","displayName":"Finance Team","members":[]}`)
	if code != 201 {
		t.Fatalf("group create: %d %v", code, body)
	}
	group := body["id"].(string)
	code, _ = f.call(t, "PATCH", "/Groups/"+group, `{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],"Operations":[
	  {"op":"Add","path":"members","value":[{"$ref":null,"value":"`+alice+`"}]}]}`)
	if code != 200 || !f.member("oid-alice", "finance-team") {
		t.Fatalf("add member: %d, tuples %v", code, f.tuples.set)
	}
	// Re-sent add is idempotent.
	if code, _ = f.call(t, "PATCH", "/Groups/"+group, `{"Operations":[{"op":"Add","path":"members","value":[{"value":"`+alice+`"}]}]}`); code != 200 {
		t.Fatalf("re-add: %d", code)
	}
	_, body = f.call(t, "GET", "/Groups/"+group, "")
	if n := len(body["members"].([]any)); n != 1 {
		t.Fatalf("members after re-add: %d", n)
	}

	// Rename keeps the FGA group.
	code, body = f.call(t, "PATCH", "/Groups/"+group, `{"Operations":[{"op":"Replace","path":"displayName","value":"Finance"}]}`)
	if code != 200 || body["displayName"] != "Finance" || !f.member("oid-alice", "finance-team") {
		t.Fatalf("rename: %d %v %v", code, body, f.tuples.set)
	}

	// Remove member by filter path → tuple gone.
	code, _ = f.call(t, "PATCH", "/Groups/"+group, `{"Operations":[{"op":"Remove","path":"members[value eq \"`+alice+`\"]"}]}`)
	if code != 200 || f.member("oid-alice", "finance-team") {
		t.Fatalf("remove member: %d %v", code, f.tuples.set)
	}
	// Add again, remove with a value list.
	f.call(t, "PATCH", "/Groups/"+group, `{"Operations":[{"op":"Add","path":"members","value":[{"value":"`+alice+`"}]}]}`)
	f.call(t, "PATCH", "/Groups/"+group, `{"Operations":[{"op":"Remove","path":"members","value":[{"value":"`+alice+`"}]}]}`)
	if f.member("oid-alice", "finance-team") {
		t.Fatal("remove by value list left the tuple")
	}

	// 6. Disable (Entra sends a string boolean) → AG-06 block.
	code, body = f.call(t, "PATCH", "/Users/"+alice, `{"Operations":[{"op":"Replace","path":"active","value":"False"}]}`)
	if code != 200 || body["active"] != false {
		t.Fatalf("disable: %d %v", code, body)
	}
	if b, ok := f.blocks.blocks["oid-alice"]; !ok || b.By != BlockedBy {
		t.Fatalf("disable did not block: %v", f.blocks.blocks)
	}
	// Re-enable via a path-less replace lifts the SCIM block.
	code, _ = f.call(t, "PATCH", "/Users/"+alice, `{"Operations":[{"op":"replace","value":{"active":true}}]}`)
	if _, ok := f.blocks.blocks["oid-alice"]; code != 200 || ok {
		t.Fatalf("enable: %d, blocks %v", code, f.blocks.blocks)
	}

	// 7. Group delete drops its tuples; user delete blocks and forgets.
	f.call(t, "PATCH", "/Groups/"+group, `{"Operations":[{"op":"Add","path":"members","value":[{"value":"`+alice+`"}]}]}`)
	if code, _ = f.call(t, "DELETE", "/Groups/"+group, ""); code != 204 || f.member("oid-alice", "finance-team") {
		t.Fatalf("group delete: %d %v", code, f.tuples.set)
	}
	if code, _ = f.call(t, "DELETE", "/Users/"+alice, ""); code != 204 {
		t.Fatalf("user delete: %d", code)
	}
	if _, ok := f.blocks.blocks["oid-alice"]; !ok {
		t.Fatal("delete did not block")
	}
	if code, _ = f.call(t, "GET", "/Users/"+alice, ""); code != 404 {
		t.Fatalf("get after delete: %d", code)
	}

	// Every change is audited with the token as actor.
	want := []string{"user.created", "user.updated", "group.created", "group.updated", "user.deactivated", "user.reactivated", "group.deleted", "user.deleted"}
	joined := strings.Join(f.audit.events, "\n")
	for _, w := range want {
		if !strings.Contains(joined, "aikonos.broker.scim."+w+" ") {
			t.Errorf("missing audit event %s in\n%s", w, joined)
		}
	}
	if !strings.Contains(joined, "by scim-token:t1") {
		t.Errorf("audit actor is not the token: %s", joined)
	}
}

// An admin's own block survives an IdP reactivation.
func TestReactivationKeepsAdminBlock(t *testing.T) {
	f := newFixture()
	_, body := f.call(t, "POST", "/Users", `{"externalId":"oid-bob","userName":"bob@contoso.com","active":false}`)
	bob := body["id"].(string)
	if f.blocks.blocks["oid-bob"].By != BlockedBy {
		t.Fatal("create with active=false did not block")
	}
	f.blocks.blocks["oid-bob"] = db.UserBlock{Reason: "laptop stolen", By: "admin@contoso.com"}
	f.call(t, "PATCH", "/Users/"+bob, `{"Operations":[{"op":"Replace","path":"active","value":true}]}`)
	if f.blocks.blocks["oid-bob"].By != "admin@contoso.com" {
		t.Fatalf("reactivation lifted an admin block: %v", f.blocks.blocks)
	}
}

func TestUserValidation(t *testing.T) {
	f := newFixture()
	for name, body := range map[string]string{
		"no externalId":   `{"userName":"c@contoso.com"}`,
		"svc subject":     `{"userName":"c@contoso.com","externalId":"svc-x"}`,
		"no userName":     `{"externalId":"oid-c"}`,
		"active not bool": `{"userName":"c@contoso.com","externalId":"oid-c","active":"maybe"}`,
	} {
		if code, _ := f.call(t, "POST", "/Users", body); code != 400 {
			t.Errorf("%s: want 400, got %d", name, code)
		}
	}
	_, body := f.call(t, "POST", "/Users", `{"userName":"c@contoso.com","externalId":"oid-c"}`)
	code, out := f.call(t, "PATCH", "/Users/"+body["id"].(string), `{"Operations":[{"op":"Replace","path":"externalId","value":"oid-other"}]}`)
	if code != 400 || out["scimType"] != "mutability" {
		t.Fatalf("externalId change: %d %v", code, out)
	}
	if code, _ := f.call(t, "GET", `/Users?filter=name.givenName sw "x"`, ""); code != 400 {
		t.Fatalf("unsupported filter: %d", code)
	}
	if code, _ := f.call(t, "POST", "/Groups", `{"displayName":"x","members":[{"value":"`+uuid.NewString()+`"}]}`); code != 400 {
		t.Fatalf("unknown member: %d", code)
	}
}

// A failed FGA write leaves no group row behind, so a retry starts clean.
func TestGroupCreateRollsBackOnFGAFailure(t *testing.T) {
	f := newFixture()
	_, u := f.call(t, "POST", "/Users", `{"userName":"d@contoso.com","externalId":"oid-d"}`)
	f.tuples.fail = true
	code, _ := f.call(t, "POST", "/Groups", `{"displayName":"Ops","members":[{"value":"`+u["id"].(string)+`"}]}`)
	if code != 500 {
		t.Fatalf("want 500, got %d", code)
	}
	f.tuples.fail = false
	if code, _ = f.call(t, "POST", "/Groups", `{"displayName":"Ops","members":[{"value":"`+u["id"].(string)+`"}]}`); code != 201 || !f.member("oid-d", "ops") {
		t.Fatalf("retry: %d %v", code, f.tuples.set)
	}
}

func TestFGAGroupName(t *testing.T) {
	for in, want := range map[string]string{"Finance Team": "finance-team", "  R&D / EU ": "r-d-eu", "a.b_c": "a.b_c"} {
		if got, err := FGAGroupName(in); err != nil || got != want {
			t.Errorf("%q: got %q (%v), want %q", in, got, err, want)
		}
	}
	if _, err := FGAGroupName("!!!"); err == nil {
		t.Error("want an error for a name with no usable characters")
	}
}

func TestListPaging(t *testing.T) {
	f := newFixture()
	for i := 0; i < 3; i++ {
		f.call(t, "POST", "/Users", fmt.Sprintf(`{"userName":"u%d@c.com","externalId":"oid-%d"}`, i, i))
	}
	_, body := f.call(t, "GET", "/Users?startIndex=2&count=1", "")
	if body["totalResults"].(float64) != 3 || body["itemsPerPage"].(float64) != 1 || body["startIndex"].(float64) != 2 {
		t.Fatalf("paging: %v", body)
	}
}

// TestGroupCreateRefusesManagedGroup pins F4: an IdP group whose slug names a
// group that already has tuples (members, or grants to group#member) is not
// adopted, so a SCIM token cannot add members to an admin-managed group.
func TestGroupCreateRefusesManagedGroup(t *testing.T) {
	for name, seed := range map[string]policy.Relation{
		"has members":  {User: "user:oid-admin", Relation: "member", Object: "group:approvers"},
		"holds grants": {User: "group:approvers#member", Relation: "approver", Object: "tenant:aikonos-dev"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture()
			f.tuples.set[seed] = true
			_, u := f.call(t, "POST", "/Users", `{"userName":"e@contoso.com","externalId":"oid-e"}`)
			code, body := f.call(t, "POST", "/Groups", `{"displayName":"Approvers","members":[{"value":"`+u["id"].(string)+`"}]}`)
			if code != 409 || body["scimType"] != "uniqueness" {
				t.Fatalf("want 409 uniqueness, got %d %v", code, body)
			}
			if f.member("oid-e", "approvers") {
				t.Fatal("member tuple written to an admin-managed group")
			}
			if !strings.Contains(strings.Join(f.audit.events, "\n"), "aikonos.broker.scim.group.claim_refused group:approvers") {
				t.Fatalf("refusal not audited: %v", f.audit.events)
			}
		})
	}
}

// TestGroupSubjectTypesMatchModel keeps groupSubjectTypes in step with the
// FGA model: every type with a relation that accepts group#member.
func TestGroupSubjectTypesMatchModel(t *testing.T) {
	raw, err := os.ReadFile("../../../policies/fga/model.fga")
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	typ := ""
	for _, line := range strings.Split(string(raw), "\n") {
		if name, ok := strings.CutPrefix(strings.TrimSpace(line), "type "); ok {
			typ = name
		} else if strings.Contains(line, "group#member") && (len(want) == 0 || want[len(want)-1] != typ) {
			want = append(want, typ)
		}
	}
	if !slices.Equal(groupSubjectTypes, want) {
		t.Fatalf("groupSubjectTypes = %v, model has %v", groupSubjectTypes, want)
	}
}

// TestUserExternalIDCanonical pins F133: an upper-case GUID externalId is
// stored, blocked and looked up in the lower-case form the token carries.
func TestUserExternalIDCanonical(t *testing.T) {
	f := newFixture()
	const upper, lower = "AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	code, body := f.call(t, "POST", "/Users", `{"userName":"g@contoso.com","externalId":"`+upper+`","active":false}`)
	if code != 201 || body["externalId"] != lower {
		t.Fatalf("create: %d %v", code, body)
	}
	if _, ok := f.blocks.blocks[lower]; !ok {
		t.Fatalf("block not on the token subject: %v", f.blocks.blocks)
	}
	if _, body = f.call(t, "GET", `/Users?filter=externalId eq "`+upper+`"`, ""); body["totalResults"].(float64) != 1 {
		t.Fatalf("lookup by the IdP's form: %v", body)
	}
	// Non-GUID subjects may be case-sensitive and are kept as sent.
	if _, body = f.call(t, "POST", "/Users", `{"userName":"h@contoso.com","externalId":"Oid-H"}`); body["externalId"] != "Oid-H" {
		t.Fatalf("non-GUID subject changed: %v", body)
	}
}

// TestAdminBlockSurvivesScimReactivation pins F134: an admin block on top of
// a SCIM block takes it over, so the IdP re-enabling the user lifts nothing.
func TestAdminBlockSurvivesScimReactivation(t *testing.T) {
	f := newFixture()
	_, body := f.call(t, "POST", "/Users", `{"externalId":"oid-i","userName":"i@contoso.com","active":false}`)
	if changed, _ := f.blocks.BlockUser(context.Background(), tenant, "oid-i", "incident", "admin@contoso.com"); !changed {
		t.Fatal("an admin block must take over a SCIM block")
	}
	f.call(t, "PATCH", "/Users/"+body["id"].(string), `{"Operations":[{"op":"Replace","path":"active","value":true}]}`)
	if f.blocks.blocks["oid-i"].By != "admin@contoso.com" {
		t.Fatalf("reactivation lifted the admin block: %v", f.blocks.blocks)
	}
}
