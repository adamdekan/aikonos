package broker

// CP3 tests: UpsertAgentSkill, ListAgentSkills, GetAgentSkill, DeleteAgentSkill.
// All DB-free via fakeAgentSkillRepo + real nop audit.Emitter + fakeFGA.
// Audit assertions follow the cp4 pattern: success without error == emit reached.

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/db"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// pgUniqueViolation returns the pgconn error that the real driver returns on
// SQLSTATE 23505 (unique_violation). Used by fakeAgentSkillRepo so the handler's
// errors.As(*pgconn.PgError) path is exercised in tests, not just in production.
func pgUniqueViolation() error {
	return &pgconn.PgError{Code: "23505", Message: "duplicate key value violates unique constraint"}
}

// ── fakeAgentSkillRepo ───────────────────────────────────────────────────────

type fakeAgentSkillRepo struct {
	mu          sync.Mutex
	rows        map[string]*db.AgentSkill // keyed by id string
	byName      map[string]string         // name → id
	upserts     int
	deletes     int
	errOnUpsert error // when non-nil, Upsert returns this (for duplicate-name simulation)
}

func newFakeAgentSkillRepo() *fakeAgentSkillRepo {
	return &fakeAgentSkillRepo{
		rows:   make(map[string]*db.AgentSkill),
		byName: make(map[string]string),
	}
}

func (f *fakeAgentSkillRepo) Upsert(_ context.Context, _ string, row db.AgentSkill) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.errOnUpsert != nil {
		return f.errOnUpsert
	}
	id := row.ID.String()
	// Enforce unique(name): reject with a real pgconn error so the handler's
	// errors.As(*pgconn.PgError) path is exercised exactly as with a live DB.
	if existing, ok := f.byName[row.Name]; ok && existing != id {
		return pgUniqueViolation()
	}
	// Remove old name mapping if updating.
	if old, ok := f.rows[id]; ok {
		delete(f.byName, old.Name)
	}
	cp := row
	f.rows[id] = &cp
	f.byName[row.Name] = id
	f.upserts++
	return nil
}

func (f *fakeAgentSkillRepo) Get(_ context.Context, _ string, id uuid.UUID) (*db.AgentSkill, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if row, ok := f.rows[id.String()]; ok {
		cp := *row
		return &cp, nil
	}
	return nil, fmt.Errorf("agent_skill %s: %w", id, db.ErrAgentSkillNotFound)
}

func (f *fakeAgentSkillRepo) GetByName(_ context.Context, _, name string) (*db.AgentSkill, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id, ok := f.byName[name]; ok {
		cp := *f.rows[id]
		return &cp, nil
	}
	return nil, fmt.Errorf("agent_skill %q: %w", name, db.ErrAgentSkillNotFound)
}

func (f *fakeAgentSkillRepo) ListByTenant(_ context.Context, _ string) ([]db.AgentSkill, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]db.AgentSkill, 0, len(f.rows))
	for _, r := range f.rows {
		out = append(out, *r)
	}
	return out, nil
}

func (f *fakeAgentSkillRepo) Delete(_ context.Context, _ string, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[id.String()]
	if !ok {
		return fmt.Errorf("agent_skill %s: %w", id, db.ErrAgentSkillNotFound)
	}
	delete(f.byName, row.Name)
	delete(f.rows, id.String())
	f.deletes++
	return nil
}

// ── helper: service wired for agent-skill admin tests ────────────────────────

func agentSkillAdminSvc(t *testing.T, repo agentSkillRepo) *BrokerService {
	t.Helper()
	fg := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := fg.server(t)
	t.Cleanup(srv.Close)

	em, err := audit.NewEmitter(context.Background(), audit.Config{})
	if err != nil {
		t.Fatalf("NewEmitter: %v", err)
	}

	reg := newTestToolRegistry()
	deps := testAdminDeps(t, srv.URL)
	deps.ToolRegistry = reg
	deps.Audit = em
	deps.AgentSkillBundles = repo

	return NewBrokerService(deps)
}

// ── UpsertAgentSkill: valid bundle accepted ──────────────────────────────────

func TestUpsertAgentSkill_ValidBundleAccepted(t *testing.T) {
	repo := newFakeAgentSkillRepo()
	svc := agentSkillAdminSvc(t, repo)

	resp, err := svc.UpsertAgentSkill(adminCtx(), &brokerv1.UpsertAgentSkillRequest{
		TenantId:     testTenantUUID,
		UserId:       "admin@example.com",
		Name:         "my-skill",
		Description:  "A test skill bundle.",
		Body:         "---\nname: my-skill\n---\n# My Skill\nDo things.",
		AllowedTools: []string{"web.fetch", "doc.read"},
	})
	if err != nil {
		t.Fatalf("UpsertAgentSkill valid bundle: %v", err)
	}
	if resp.Bundle == nil {
		t.Fatal("expected non-nil bundle in response")
	}
	if resp.Bundle.Name != "my-skill" {
		t.Errorf("name: got %q, want my-skill", resp.Bundle.Name)
	}
	if resp.Bundle.Id == "" {
		t.Error("expected non-empty id in response")
	}

	repo.mu.Lock()
	u := repo.upserts
	repo.mu.Unlock()
	if u != 1 {
		t.Errorf("expected 1 upsert, got %d", u)
	}
}

// ── UpsertAgentSkill: name regex violations → InvalidArgument ────────────────

func TestUpsertAgentSkill_NameRegexViolation_InvalidArgument(t *testing.T) {
	repo := newFakeAgentSkillRepo()
	svc := agentSkillAdminSvc(t, repo)

	for _, badName := range []string{"My-Skill", "a_b", "has space", "UPPER", ""} {
		_, err := svc.UpsertAgentSkill(adminCtx(), &brokerv1.UpsertAgentSkillRequest{
			TenantId: testTenantUUID,
			UserId:   "admin@example.com",
			Name:     badName,
			Body:     "---\n---\n",
		})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("name %q: want InvalidArgument, got %v", badName, err)
		}
	}

	repo.mu.Lock()
	u := repo.upserts
	repo.mu.Unlock()
	if u != 0 {
		t.Errorf("no upsert on invalid name, got %d", u)
	}
}

// ── UpsertAgentSkill: duplicate name → AlreadyExists or InvalidArgument ───────

func TestUpsertAgentSkill_DuplicateName_AlreadyExists(t *testing.T) {
	repo := newFakeAgentSkillRepo()
	svc := agentSkillAdminSvc(t, repo)

	// First upsert succeeds.
	_, err := svc.UpsertAgentSkill(adminCtx(), &brokerv1.UpsertAgentSkillRequest{
		TenantId: testTenantUUID,
		UserId:   "admin@example.com",
		Name:     "dupe-skill",
		Body:     "---\n---\n",
	})
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	// Inject a real pg unique-violation so the handler's errors.As path is hit.
	repo.mu.Lock()
	repo.errOnUpsert = pgUniqueViolation()
	repo.mu.Unlock()

	_, err = svc.UpsertAgentSkill(adminCtx(), &brokerv1.UpsertAgentSkillRequest{
		TenantId: testTenantUUID,
		UserId:   "admin@example.com",
		Name:     "dupe-skill",
		Body:     "---\n---\n",
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("duplicate name: want InvalidArgument, got %v", err)
	}
}

// ── UpsertAgentSkill: reserved names → InvalidArgument ───────────────────────

func TestUpsertAgentSkill_ReservedName_InvalidArgument(t *testing.T) {
	repo := newFakeAgentSkillRepo()
	svc := agentSkillAdminSvc(t, repo)

	reserved := []string{"help", "clear", "new", "reset", "stop", "settings", "skills", "agents", "login", "logout"}
	for _, name := range reserved {
		_, err := svc.UpsertAgentSkill(adminCtx(), &brokerv1.UpsertAgentSkillRequest{
			TenantId: testTenantUUID,
			UserId:   "admin@example.com",
			Name:     name,
			Body:     "---\n---\n",
		})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("reserved name %q: want InvalidArgument, got %v", name, err)
		}
	}

	repo.mu.Lock()
	u := repo.upserts
	repo.mu.Unlock()
	if u != 0 {
		t.Errorf("no upsert on reserved name, got %d", u)
	}
}

// ── UpsertAgentSkill: non-admin → PermissionDenied ───────────────────────────

func TestUpsertAgentSkill_NonAdmin_PermissionDenied(t *testing.T) {
	repo := newFakeAgentSkillRepo()
	svc := agentSkillAdminSvc(t, repo)

	_, err := svc.UpsertAgentSkill(nonAdminCtx(), &brokerv1.UpsertAgentSkillRequest{
		TenantId: testTenantUUID,
		UserId:   "alice@example.com",
		Name:     "my-skill",
		Body:     "---\n---\n",
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-admin: want PermissionDenied, got %v", err)
	}

	repo.mu.Lock()
	u := repo.upserts
	repo.mu.Unlock()
	if u != 0 {
		t.Errorf("no upsert on non-admin, got %d", u)
	}
}

// ── capturingAuditSink: records emitted events for assertion ─────────────────

type capturingAuditSink struct {
	mu     sync.Mutex
	events []*auditv1.AuditEvent
}

func (c *capturingAuditSink) Emit(_ context.Context, ev *auditv1.AuditEvent) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, ev)
	return nil
}

func (c *capturingAuditSink) last() *auditv1.AuditEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.events) == 0 {
		return nil
	}
	return c.events[len(c.events)-1]
}

func (c *capturingAuditSink) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.events)
}

// agentSkillSvcWithSink builds a service wired with a capturing audit sink
// so tests can assert on emitted event fields.
func agentSkillSvcWithSink(t *testing.T, repo agentSkillRepo, sink *capturingAuditSink) *BrokerService {
	t.Helper()
	fg := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := fg.server(t)
	t.Cleanup(srv.Close)

	reg := newTestToolRegistry()
	deps := testAdminDeps(t, srv.URL)
	deps.ToolRegistry = reg
	deps.Audit = sink
	deps.AgentSkillBundles = repo
	return NewBrokerService(deps)
}

// ── UpsertAgentSkill: audit event carries correct type and actor ──────────────

func TestUpsertAgentSkill_AuditEmitted(t *testing.T) {
	repo := newFakeAgentSkillRepo()
	sink := &capturingAuditSink{}
	svc := agentSkillSvcWithSink(t, repo, sink)

	_, err := svc.UpsertAgentSkill(adminCtx(), &brokerv1.UpsertAgentSkillRequest{
		TenantId: testTenantUUID,
		UserId:   "admin@example.com",
		Name:     "audit-skill",
		Body:     "---\n---\n",
	})
	if err != nil {
		t.Fatalf("UpsertAgentSkill: %v", err)
	}
	if sink.count() == 0 {
		t.Fatal("expected audit event, got none")
	}
	ev := sink.last()
	if ev.EventType != "aikonos.broker.agent_skill.upserted" {
		t.Errorf("event_type: got %q, want aikonos.broker.agent_skill.upserted", ev.EventType)
	}
	if ev.ActorUserId != "admin@example.com" {
		t.Errorf("actor_user_id: got %q, want admin@example.com", ev.ActorUserId)
	}
}

// ── DeleteAgentSkill: audit event carries correct type and actor ──────────────

func TestDeleteAgentSkill_AuditEmitted(t *testing.T) {
	repo := newFakeAgentSkillRepo()
	sink := &capturingAuditSink{}
	svc := agentSkillSvcWithSink(t, repo, sink)

	id := uuid.New()
	repo.mu.Lock()
	repo.rows[id.String()] = &db.AgentSkill{
		ID:       id,
		TenantID: uuid.MustParse(testTenantUUID),
		Name:     "to-delete",
	}
	repo.byName["to-delete"] = id.String()
	repo.mu.Unlock()

	_, err := svc.DeleteAgentSkill(adminCtx(), &brokerv1.DeleteAgentSkillRequest{
		TenantId: testTenantUUID,
		UserId:   "admin@example.com",
		Id:       id.String(),
	})
	if err != nil {
		t.Fatalf("DeleteAgentSkill: %v", err)
	}
	if sink.count() == 0 {
		t.Fatal("expected audit event, got none")
	}
	ev := sink.last()
	if ev.EventType != "aikonos.broker.agent_skill.deleted" {
		t.Errorf("event_type: got %q, want aikonos.broker.agent_skill.deleted", ev.EventType)
	}
	if ev.ActorUserId != "admin@example.com" {
		t.Errorf("actor_user_id: got %q, want admin@example.com", ev.ActorUserId)
	}
}

// ── ListAgentSkills: admin happy path ────────────────────────────────────────

func TestListAgentSkills_AdminHappyPath(t *testing.T) {
	repo := newFakeAgentSkillRepo()
	svc := agentSkillAdminSvc(t, repo)

	for _, name := range []string{"skill-a", "skill-b"} {
		id := uuid.New()
		repo.mu.Lock()
		repo.rows[id.String()] = &db.AgentSkill{ID: id, TenantID: uuid.MustParse(testTenantUUID), Name: name}
		repo.byName[name] = id.String()
		repo.mu.Unlock()
	}

	resp, err := svc.ListAgentSkills(adminCtx(), &brokerv1.ListAgentSkillsRequest{
		TenantId: testTenantUUID,
		UserId:   "admin@example.com",
	})
	if err != nil {
		t.Fatalf("ListAgentSkills: %v", err)
	}
	if len(resp.Bundles) != 2 {
		t.Errorf("expected 2 bundles, got %d", len(resp.Bundles))
	}
}

// ── ListAgentSkills: non-admin → PermissionDenied ────────────────────────────

func TestListAgentSkills_NonAdmin_PermissionDenied(t *testing.T) {
	repo := newFakeAgentSkillRepo()
	svc := agentSkillAdminSvc(t, repo)

	_, err := svc.ListAgentSkills(nonAdminCtx(), &brokerv1.ListAgentSkillsRequest{
		TenantId: testTenantUUID,
		UserId:   "alice@example.com",
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-admin list: want PermissionDenied, got %v", err)
	}
}

// ── GetAgentSkill: admin happy path ──────────────────────────────────────────

func TestGetAgentSkill_AdminHappyPath(t *testing.T) {
	repo := newFakeAgentSkillRepo()
	svc := agentSkillAdminSvc(t, repo)

	id := uuid.New()
	repo.mu.Lock()
	repo.rows[id.String()] = &db.AgentSkill{ID: id, TenantID: uuid.MustParse(testTenantUUID), Name: "my-skill"}
	repo.byName["my-skill"] = id.String()
	repo.mu.Unlock()

	resp, err := svc.GetAgentSkill(adminCtx(), &brokerv1.GetAgentSkillRequest{
		TenantId: testTenantUUID,
		UserId:   "admin@example.com",
		Id:       id.String(),
	})
	if err != nil {
		t.Fatalf("GetAgentSkill: %v", err)
	}
	if resp.Bundle == nil {
		t.Fatal("expected non-nil bundle")
	}
	if resp.Bundle.Name != "my-skill" {
		t.Errorf("name: got %q, want my-skill", resp.Bundle.Name)
	}
}

// ── GetAgentSkill: not found → NotFound ──────────────────────────────────────

func TestGetAgentSkill_NotFound(t *testing.T) {
	repo := newFakeAgentSkillRepo()
	svc := agentSkillAdminSvc(t, repo)

	_, err := svc.GetAgentSkill(adminCtx(), &brokerv1.GetAgentSkillRequest{
		TenantId: testTenantUUID,
		UserId:   "admin@example.com",
		Id:       uuid.New().String(),
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("not-found: want NotFound, got %v", err)
	}
}

// ── DeleteAgentSkill: non-admin → PermissionDenied ───────────────────────────

func TestDeleteAgentSkill_NonAdmin_PermissionDenied(t *testing.T) {
	repo := newFakeAgentSkillRepo()
	svc := agentSkillAdminSvc(t, repo)

	_, err := svc.DeleteAgentSkill(nonAdminCtx(), &brokerv1.DeleteAgentSkillRequest{
		TenantId: testTenantUUID,
		UserId:   "alice@example.com",
		Id:       uuid.New().String(),
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-admin delete: want PermissionDenied, got %v", err)
	}
}

// ── UpsertAgentSkill: allowed_tools stored verbatim, never validated (D5) ────

// TestUpsertAgentSkill_AllowedToolsStoredVerbatim_NoValidation pins D5: the
// allowed_tools narrowing machinery is gone, so an entry that resolves to no
// known tool must no longer be rejected — the field is dormant, carried
// as-is for wire compatibility only.
func TestUpsertAgentSkill_AllowedToolsStoredVerbatim_NoValidation(t *testing.T) {
	repo := newFakeAgentSkillRepo()
	svc := agentSkillAdminSvc(t, repo)

	resp, err := svc.UpsertAgentSkill(adminCtx(), &brokerv1.UpsertAgentSkillRequest{
		TenantId:     testTenantUUID,
		UserId:       "admin@example.com",
		Name:         "bad-tools",
		Body:         "---\n---\n",
		AllowedTools: []string{"web.fetch", "nonexistent.tool", "mcp:noconn:search"},
	})
	if err != nil {
		t.Fatalf("allowed_tools must no longer be validated: %v", err)
	}
	want := []string{"web.fetch", "nonexistent.tool", "mcp:noconn:search"}
	if len(resp.Bundle.AllowedTools) != len(want) {
		t.Fatalf("allowed_tools = %v, want %v stored verbatim", resp.Bundle.AllowedTools, want)
	}
	for i := range want {
		if resp.Bundle.AllowedTools[i] != want[i] {
			t.Fatalf("allowed_tools = %v, want %v stored verbatim", resp.Bundle.AllowedTools, want)
		}
	}
}

// ── UpsertAgentSkill: keywords normalize (lowercase/trim/dedup) and round-trip ──

func TestUpsertAgentSkill_KeywordsNormalizeAndRoundTrip(t *testing.T) {
	repo := newFakeAgentSkillRepo()
	svc := agentSkillAdminSvc(t, repo)

	resp, err := svc.UpsertAgentSkill(adminCtx(), &brokerv1.UpsertAgentSkillRequest{
		TenantId: testTenantUUID,
		UserId:   "admin@example.com",
		Name:     "kw-skill",
		Body:     "---\n---\n",
		Keywords: []string{"Invoice", "  refund ", "INVOICE"},
	})
	if err != nil {
		t.Fatalf("UpsertAgentSkill: %v", err)
	}
	want := []string{"invoice", "refund"}
	if len(resp.Bundle.Keywords) != len(want) {
		t.Fatalf("keywords: got %v, want %v", resp.Bundle.Keywords, want)
	}
	for i := range want {
		if resp.Bundle.Keywords[i] != want[i] {
			t.Fatalf("keywords: got %v, want %v", resp.Bundle.Keywords, want)
		}
	}

	// Round-trip via GetAgentSkill confirms the normalized form persisted.
	getResp, err := svc.GetAgentSkill(adminCtx(), &brokerv1.GetAgentSkillRequest{
		TenantId: testTenantUUID,
		UserId:   "admin@example.com",
		Id:       resp.Bundle.Id,
	})
	if err != nil {
		t.Fatalf("GetAgentSkill: %v", err)
	}
	if len(getResp.Bundle.Keywords) != len(want) {
		t.Fatalf("round-tripped keywords: got %v, want %v", getResp.Bundle.Keywords, want)
	}
}

// ── UpsertAgentSkill: empty keywords is valid (never auto-loads) ─────────────

func TestUpsertAgentSkill_EmptyKeywords_Valid(t *testing.T) {
	repo := newFakeAgentSkillRepo()
	svc := agentSkillAdminSvc(t, repo)

	resp, err := svc.UpsertAgentSkill(adminCtx(), &brokerv1.UpsertAgentSkillRequest{
		TenantId: testTenantUUID,
		UserId:   "admin@example.com",
		Name:     "no-kw-skill",
		Body:     "---\n---\n",
	})
	if err != nil {
		t.Fatalf("UpsertAgentSkill: %v", err)
	}
	if len(resp.Bundle.Keywords) != 0 {
		t.Errorf("expected empty keywords, got %v", resp.Bundle.Keywords)
	}
}

// ── UpsertAgentSkill: files map validation + storage ──

func TestUpsertAgentSkill_FilesStoredAndFilePathsPopulated(t *testing.T) {
	repo := newFakeAgentSkillRepo()
	svc := agentSkillAdminSvc(t, repo)

	resp, err := svc.UpsertAgentSkill(adminCtx(), &brokerv1.UpsertAgentSkillRequest{
		TenantId:    testTenantUUID,
		UserId:      "admin@example.com",
		Name:        "tree-skill",
		Body:        "---\n---\n# Body",
		Description: "d",
		Files: map[string][]byte{
			"scripts/run.py":      []byte("print('hi')"),
			"references/notes.md": []byte("# notes"),
		},
	})
	if err != nil {
		t.Fatalf("UpsertAgentSkill with files: %v", err)
	}
	want := []string{"references/notes.md", "scripts/run.py"}
	if len(resp.Bundle.FilePaths) != len(want) {
		t.Fatalf("file_paths = %v, want %v", resp.Bundle.FilePaths, want)
	}
	for i := range want {
		if resp.Bundle.FilePaths[i] != want[i] {
			t.Fatalf("file_paths = %v, want %v (sorted ascending)", resp.Bundle.FilePaths, want)
		}
	}

	// Round-trip via GetAgentSkill confirms extras persisted, not just the
	// in-memory response.
	getResp, err := svc.GetAgentSkill(adminCtx(), &brokerv1.GetAgentSkillRequest{
		TenantId: testTenantUUID,
		UserId:   "admin@example.com",
		Id:       resp.Bundle.Id,
	})
	if err != nil {
		t.Fatalf("GetAgentSkill: %v", err)
	}
	if len(getResp.Bundle.FilePaths) != 2 {
		t.Fatalf("round-tripped file_paths = %v, want 2 entries", getResp.Bundle.FilePaths)
	}
}

func TestUpsertAgentSkill_NoFiles_EmptyFilePaths(t *testing.T) {
	repo := newFakeAgentSkillRepo()
	svc := agentSkillAdminSvc(t, repo)

	resp, err := svc.UpsertAgentSkill(adminCtx(), &brokerv1.UpsertAgentSkillRequest{
		TenantId: testTenantUUID,
		UserId:   "admin@example.com",
		Name:     "no-files-skill",
		Body:     "---\n---\n",
	})
	if err != nil {
		t.Fatalf("UpsertAgentSkill: %v", err)
	}
	if len(resp.Bundle.FilePaths) != 0 {
		t.Errorf("expected empty file_paths, got %v", resp.Bundle.FilePaths)
	}
}

func TestUpsertAgentSkill_InvalidFilePath_InvalidArgument(t *testing.T) {
	repo := newFakeAgentSkillRepo()
	svc := agentSkillAdminSvc(t, repo)

	badPaths := []string{"../escape.txt", "/etc/passwd", "a\\b", "references/../../escape.txt"}
	for _, p := range badPaths {
		_, err := svc.UpsertAgentSkill(adminCtx(), &brokerv1.UpsertAgentSkillRequest{
			TenantId: testTenantUUID,
			UserId:   "admin@example.com",
			Name:     "bad-path-skill",
			Body:     "---\n---\n",
			Files:    map[string][]byte{p: []byte("x")},
		})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("path %q: want InvalidArgument, got %v", p, err)
		}
	}

	repo.mu.Lock()
	u := repo.upserts
	repo.mu.Unlock()
	if u != 0 {
		t.Errorf("no upsert on invalid file path, got %d", u)
	}
}

func TestUpsertAgentSkill_TooManyFiles_InvalidArgument(t *testing.T) {
	repo := newFakeAgentSkillRepo()
	svc := agentSkillAdminSvc(t, repo)

	files := make(map[string][]byte, 101)
	for i := 0; i < 101; i++ {
		files[fmt.Sprintf("f%03d.txt", i)] = []byte("x")
	}
	_, err := svc.UpsertAgentSkill(adminCtx(), &brokerv1.UpsertAgentSkillRequest{
		TenantId: testTenantUUID,
		UserId:   "admin@example.com",
		Name:     "too-many-files",
		Body:     "---\n---\n",
		Files:    files,
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("over the 100-file cap: want InvalidArgument, got %v", err)
	}
}

func TestUpsertAgentSkill_FilesOverSizeCap_InvalidArgument(t *testing.T) {
	repo := newFakeAgentSkillRepo()
	svc := agentSkillAdminSvc(t, repo)

	// One ~6 MiB file pushes body+extras past the migration-028 5 MiB row cap.
	big := make([]byte, 6*1024*1024)
	_, err := svc.UpsertAgentSkill(adminCtx(), &brokerv1.UpsertAgentSkillRequest{
		TenantId: testTenantUUID,
		UserId:   "admin@example.com",
		Name:     "too-big-skill",
		Body:     "---\n---\n",
		Files:    map[string][]byte{"big.bin": big},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("over the 5 MiB row cap: want InvalidArgument, got %v", err)
	}
}
