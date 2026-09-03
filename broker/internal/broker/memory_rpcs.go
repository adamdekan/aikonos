package broker

// memory_rpcs.go — the agent-memory management RPCs
// backing the settings-modal Memory surface: list groups, list/get concepts,
// and the three mutations (verify / deprecate / delete).
//
// Two things make this file security-relevant. First, VerifyMemoryConcept is the
// ONLY writer of a concept's `verified` list anywhere — that list is what earns
// a concept the human-reviewed trust tier, so no model-driven path may reach it.
// Second, every mutation runs under memorybundle.LockBundle, the same
// process-wide per-(tenant, segment) lock the memory.write tool takes: a second
// lock map in this package would not serialize against it and an interleaved
// index regeneration would silently drop a concept.
//
// Bundle I/O goes through Deps.Workspace.Local (a workspacefs.Backend) with the
// resolved scope segment passed as the `user` argument — never the per-user
// seam, which can only ever address the caller's own tree.

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/ids"
	"github.com/adamdekan/aikonos/broker/internal/memorybundle"
	"github.com/adamdekan/aikonos/broker/internal/workspacefs"
	"github.com/adamdekan/aikonos/broker/internal/workspacepath"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

const (
	// Synthetic segment prefixes — kept in lockstep with toolproxy's
	// memorySegment, which resolves the same three scopes for the tools.
	memoryAgentSegPrefix = "svc-"
	memoryGroupSegPrefix = "group-"

	// memoryHumanVerifier prefixes the `by` of a verification entry. TrustTier
	// keys the human-reviewed tier off exactly this prefix, so nothing but a
	// verification may be attributed with it — a log line reading
	// "deprecated … by human:x" claims a review that never happened.
	memoryHumanVerifier = "human:"

	// memoryUserActor prefixes every other management mutation's log actor,
	// matching the `generated.by` convention memory.write already writes.
	memoryUserActor = "user:"

	// memoryReadSkill is the FGA skill id auto-recall is gated on, and the
	// agent.Skills entry that opts an agent's own bundle into recall.
	memoryReadSkill = "memory.read"

	memoryVerifiedEvent   = "aikonos.memory.verified"
	memoryDeprecatedEvent = "aikonos.memory.deprecated"
	memoryDeletedEvent    = "aikonos.memory.deleted"

	memoryStaleAfterLayout = "2006-01-02"
)

// memoryAction is the authorization column a call falls in. The spec's matrix
// grants a group read to any member but a group mutation to managers only:
// "human-reviewed" on shared state must outrank "any member clicked a button".
type memoryAction int

const (
	memoryActionRead memoryAction = iota
	memoryActionManage
)

// memoryScopeRequest is the identity+scope preamble every memory RPC carries.
// The generated request types satisfy it structurally, so the preamble is
// written once rather than six times.
type memoryScopeRequest interface {
	GetTenantId() string
	GetUserId() string
	GetScope() string
	GetGroupId() string
	GetAgentId() string
}

// memoryConceptRef adds the concept id the four single-concept RPCs carry.
type memoryConceptRef interface {
	memoryScopeRequest
	GetId() string
}

// memoryPreamble binds the caller to the authenticated principal, authorizes the
// requested scope instance for the given action, and returns the workspace
// segment its bundle lives under.
func (s *BrokerService) memoryPreamble(ctx context.Context, req memoryScopeRequest, action memoryAction) (tenant, user, seg string, err error) {
	tenant, user, err = s.callerIdentity(ctx, req.GetTenantId(), req.GetUserId())
	if err != nil {
		return "", "", "", err
	}
	seg, err = s.authorizeMemoryScope(ctx, user, req.GetScope(), req.GetGroupId(), req.GetAgentId(), action)
	if err != nil {
		return "", "", "", err
	}
	if s.deps.Workspace.Local == nil {
		return "", "", "", status.Error(codes.FailedPrecondition, "workspace storage is not configured")
	}
	return tenant, user, seg, nil
}

// authorizeMemoryScope applies the spec's authorization matrix and maps the
// scope onto its bundle segment. Fails closed: an unknown scope, a malformed
// group id or agent id, and an FGA error all deny before any I/O.
func (s *BrokerService) authorizeMemoryScope(ctx context.Context, user, scope, groupID, agentID string, action memoryAction) (string, error) {
	switch scope {
	case "user":
		// The user branch performs no authority check — self-access is the whole
		// grant — so a subject already shaped like a synthetic segment must not
		// reach it: it would alias onto a group bundle with no membership gate.
		// callerIdentity already rejects the svc- shape on this surface.
		if strings.HasPrefix(user, memoryGroupSegPrefix) {
			return "", status.Errorf(codes.PermissionDenied, "user id %q uses the reserved %q prefix", user, memoryGroupSegPrefix)
		}
		return user, nil
	case "group":
		if groupID == "" {
			return "", status.Error(codes.InvalidArgument, "group_id required for scope=group")
		}
		// Rejected rather than sanitized: silently aliasing "a/b" onto "a_b" would
		// let two distinct group ids share one bundle.
		if workspacepath.SanitizeSeg(groupID) != groupID {
			return "", status.Errorf(codes.InvalidArgument, "group_id %q is not a valid path segment", groupID)
		}
		if err := s.requireMemoryGroupRelation(ctx, user, groupID, action); err != nil {
			return "", err
		}
		return memoryGroupSegPrefix + groupID, nil
	case "agent":
		// Validated at the boundary so a non-uuid id cannot reach the DB as a
		// 22P02 dressed up as codes.Internal (same rule as agent_spec.go).
		if _, perr := uuid.Parse(agentID); perr != nil {
			return "", status.Errorf(codes.InvalidArgument, "invalid agent_id: %v", perr)
		}
		// An agent bundle is tenant-wide state with no human owner, so both
		// columns of the matrix are tenant-admin-only. requireTenantAdmin reads
		// Policy unguarded, so the nil check happens here — same fail-closed
		// posture as the group branch, rather than a nil dereference.
		if s.deps.Policy == nil {
			return "", status.Error(codes.PermissionDenied, "agent authorization is unavailable")
		}
		if err := s.requireTenantAdmin(ctx, user); err != nil {
			return "", err
		}
		return memoryAgentSegPrefix + agentID, nil
	default:
		return "", status.Errorf(codes.InvalidArgument, "scope must be %q, %q or %q, got %q", "user", "group", "agent", scope)
	}
}

// requireMemoryGroupRelation checks the group column: a read needs member OR
// manager, a mutation needs manager. Tenant admin is the fallback for both
// (requireTenantAdmin audits its own denials). An FGA transport error denies.
func (s *BrokerService) requireMemoryGroupRelation(ctx context.Context, user, groupID string, action memoryAction) error {
	if s.deps.Policy == nil {
		return status.Error(codes.PermissionDenied, "group authorization is unavailable")
	}
	relations := []string{"manager"}
	if action == memoryActionRead {
		relations = append(relations, "member")
	}
	object := "group:" + groupID
	for _, rel := range relations {
		ok, err := s.deps.Policy.CheckFGA(ctx, "user:"+user, rel, object)
		if err != nil {
			return status.Error(codes.Internal, "authorization check failed")
		}
		if ok {
			return nil
		}
	}
	return s.requireTenantAdmin(ctx, user)
}

// ── RPCs ──────────────────────────────────────────────────────────────────────

// ListMemoryGroups reports the groups whose memory bundle the caller may reach,
// with the relations that decide which actions the UI offers. Discovery only —
// every concept RPC re-checks — so a degraded FGA yields an empty picker rather
// than a client-facing error (the delegation-discovery posture).
func (s *BrokerService) ListMemoryGroups(ctx context.Context, req *brokerv1.ListMemoryGroupsRequest) (*brokerv1.ListMemoryGroupsResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.memory.list_groups")
	defer span.End()

	_, user, err := s.callerIdentity(ctx, req.GetTenantId(), req.GetUserId())
	if err != nil {
		return nil, err
	}
	out := &brokerv1.ListMemoryGroupsResponse{}
	if s.deps.Policy == nil {
		return out, nil
	}

	groups := map[string]*brokerv1.MemoryGroup{}
	for _, rel := range []string{"member", "manager"} {
		refs, lerr := s.deps.Policy.ListObjects(ctx, "user:"+user, rel, "group")
		if lerr != nil {
			continue // fail open to empty for this relation; the other still contributes
		}
		for _, ref := range refs {
			// ListObjects returns full object refs ("group:security-team"); every
			// bundle segment and every UI label uses the bare id.
			gid := strings.TrimPrefix(ref, "group:")
			if gid == "" || workspacepath.SanitizeSeg(gid) != gid {
				continue
			}
			g, ok := groups[gid]
			if !ok {
				g = &brokerv1.MemoryGroup{GroupId: gid}
				groups[gid] = g
			}
			if rel == "manager" {
				g.Manager = true
			} else {
				g.Member = true
			}
		}
	}
	for _, g := range groups {
		out.Groups = append(out.Groups, g)
	}
	sort.Slice(out.Groups, func(i, j int) bool { return out.Groups[i].GroupId < out.Groups[j].GroupId })
	return out, nil
}

// ListMemoryConcepts returns frontmatter metadata for every concept in one scope
// instance. Bodies are never included — that is GetMemoryConcept's job.
func (s *BrokerService) ListMemoryConcepts(ctx context.Context, req *brokerv1.ListMemoryConceptsRequest) (*brokerv1.ListMemoryConceptsResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.memory.list_concepts")
	defer span.End()

	tenant, _, seg, err := s.memoryPreamble(ctx, req, memoryActionRead)
	if err != nil {
		return nil, err
	}
	metas, err := memoryMetas(ctx, s.deps.Workspace.Local, tenant, seg, req.GetScope(), req.GetGroupId(), req.GetAgentId())
	if err != nil {
		return nil, err
	}
	return &brokerv1.ListMemoryConceptsResponse{Concepts: metas}, nil
}

// GetMemoryConcept returns one concept's metadata and its raw body. Unlike
// memory.read's concept mode the body is NOT injection-annotated: this is human
// display in the settings modal, not model context.
func (s *BrokerService) GetMemoryConcept(ctx context.Context, req *brokerv1.GetMemoryConceptRequest) (*brokerv1.GetMemoryConceptResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.memory.get_concept")
	defer span.End()

	tenant, _, seg, err := s.memoryPreamble(ctx, req, memoryActionRead)
	if err != nil {
		return nil, err
	}
	_, c, err := loadMemoryConcept(ctx, s.deps.Workspace.Local, tenant, seg, req.GetId())
	if err != nil {
		return nil, err
	}
	return &brokerv1.GetMemoryConceptResponse{
		Meta: memoryMeta(req.GetScope(), req.GetGroupId(), req.GetAgentId(), req.GetId(), c, time.Now().UTC()),
		Body: c.Body,
	}, nil
}

// VerifyMemoryConcept appends a human verification entry — the only write of
// `verified` in the system, and therefore the only way a concept reaches the
// human-reviewed trust tier.
func (s *BrokerService) VerifyMemoryConcept(ctx context.Context, req *brokerv1.VerifyMemoryConceptRequest) (*brokerv1.VerifyMemoryConceptResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.memory.verify_concept")
	defer span.End()

	tenant, user, seg, err := s.memoryPreamble(ctx, req, memoryActionManage)
	if err != nil {
		return nil, err
	}
	verifier := memoryHumanVerifier + user
	entry := map[string]any{"by": verifier, "at": time.Now().UTC().Format(time.RFC3339)}
	meta, err := s.amendMemoryConcept(ctx, tenant, user, verifier, seg, req, "verified", memoryVerifiedEvent, func(fm map[string]any) {
		// A non-list `verified` is unusable to TrustTier anyway, so a malformed
		// one is replaced rather than appended to.
		existing, _ := fm["verified"].([]any)
		fm["verified"] = append(existing, entry)
	})
	if err != nil {
		return nil, err
	}
	return &brokerv1.VerifyMemoryConceptResponse{Meta: meta}, nil
}

// DeprecateMemoryConcept marks a concept deprecated. Content and any recorded
// verifications are preserved — OKF supersedes rather than erases.
func (s *BrokerService) DeprecateMemoryConcept(ctx context.Context, req *brokerv1.DeprecateMemoryConceptRequest) (*brokerv1.DeprecateMemoryConceptResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.memory.deprecate_concept")
	defer span.End()

	tenant, user, seg, err := s.memoryPreamble(ctx, req, memoryActionManage)
	if err != nil {
		return nil, err
	}
	meta, err := s.amendMemoryConcept(ctx, tenant, user, memoryUserActor+user, seg, req, "deprecated", memoryDeprecatedEvent, func(fm map[string]any) {
		fm["status"] = "deprecated"
	})
	if err != nil {
		return nil, err
	}
	return &brokerv1.DeprecateMemoryConceptResponse{Meta: meta}, nil
}

// DeleteMemoryConcept hard-deletes a concept file. A recorded OKF deviation:
// the format prefers deprecate-over-delete, and this is the management-UI-only
// escape hatch for a concept that should never have existed.
func (s *BrokerService) DeleteMemoryConcept(ctx context.Context, req *brokerv1.DeleteMemoryConceptRequest) (*brokerv1.DeleteMemoryConceptResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.memory.delete_concept")
	defer span.End()

	tenant, user, seg, err := s.memoryPreamble(ctx, req, memoryActionManage)
	if err != nil {
		return nil, err
	}
	id := req.GetId()
	if verr := memorybundle.ValidateID(id); verr != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", verr)
	}

	defer memorybundle.LockBundle(tenant, seg)()

	if derr := s.deps.Workspace.Local.Delete(ctx, tenant, seg, memorybundle.ConceptPath(id)); derr != nil {
		return nil, fileError("delete memory concept", derr)
	}
	if err := s.finishMemoryMutation(ctx, tenant, user, memoryUserActor+user, seg, req, "deleted", memoryDeletedEvent); err != nil {
		return nil, err
	}
	return &brokerv1.DeleteMemoryConceptResponse{}, nil
}

// ── shared mutation path ──────────────────────────────────────────────────────

// amendMemoryConcept is the read-modify-write Verify and Deprecate share, held
// under the bundle lock for the whole concept+index+log sequence so a concurrent
// memory.write cannot interleave its own index regeneration.
func (s *BrokerService) amendMemoryConcept(ctx context.Context, tenant, user, actor, seg string, ref memoryConceptRef, verb, eventType string, mutate func(map[string]any)) (*brokerv1.MemoryConceptMeta, error) {
	be := s.deps.Workspace.Local
	id := ref.GetId()

	defer memorybundle.LockBundle(tenant, seg)()

	data, _, err := loadMemoryConcept(ctx, be, tenant, seg, id)
	if err != nil {
		return nil, err
	}
	amended, aerr := memorybundle.AmendFrontmatter(data, mutate)
	if aerr != nil {
		return nil, status.Errorf(codes.Internal, "amend memory concept: %v", aerr)
	}
	if _, werr := be.Write(ctx, tenant, seg, memorybundle.ConceptPath(id), amended); werr != nil {
		return nil, fileError("write memory concept", werr)
	}
	// Re-parsed rather than mutated in memory: the returned meta must describe
	// what actually landed on disk, including anything AmendFrontmatter normalized.
	c, perr := memorybundle.ParseConcept(amended)
	if perr != nil {
		return nil, status.Errorf(codes.Internal, "re-parse amended memory concept: %v", perr)
	}
	if err := s.finishMemoryMutation(ctx, tenant, user, actor, seg, ref, verb, eventType); err != nil {
		return nil, err
	}
	return memoryMeta(ref.GetScope(), ref.GetGroupId(), ref.GetAgentId(), id, c, time.Now().UTC()), nil
}

// finishMemoryMutation regenerates the derived index, appends the log line and
// audits. Callers hold the bundle lock.
func (s *BrokerService) finishMemoryMutation(ctx context.Context, tenant, user, actor, seg string, ref memoryConceptRef, verb, eventType string) error {
	be := s.deps.Workspace.Local
	if _, err := memorybundle.RegenerateIndex(ctx, be, tenant, seg); err != nil {
		return status.Errorf(codes.Internal, "regenerate memory index: %v", err)
	}
	entry := fmt.Sprintf("%s `%s` by %s", verb, ref.GetId(), actor)
	if err := memorybundle.AppendLogEntry(ctx, be, tenant, seg, entry); err != nil {
		return status.Errorf(codes.Internal, "append memory log: %v", err)
	}
	s.emitMemoryEvent(ctx, tenant, user, seg, ref, eventType)
	return nil
}

// emitMemoryEvent records a management mutation. Fire-and-forget: the mutation
// already landed, so an audit failure is recorded and never returned
// (workflowsvc.Publish precedent).
func (s *BrokerService) emitMemoryEvent(ctx context.Context, tenant, user, seg string, ref memoryConceptRef, eventType string) {
	if s.deps.Audit == nil {
		return
	}
	ctxStruct, _ := structpb.NewStruct(map[string]any{
		"scope":    ref.GetScope(),
		"group_id": ref.GetGroupId(),
		"id":       ref.GetId(),
	})
	if err := s.deps.Audit.Emit(ctx, &auditv1.AuditEvent{
		EventId:     ids.EventID(),
		TraceId:     trace.SpanContextFromContext(ctx).TraceID().String(),
		TenantId:    tenant,
		OccurredAt:  timestampNow(),
		ActorUserId: user,
		EventType:   eventType,
		ResourceRef: "aikonos:memory:" + ref.GetScope() + "/" + seg + "/" + ref.GetId(),
		Decision:    auditv1.PolicyDecision_ALLOW,
		Context:     ctxStruct,
	}); err != nil {
		audit.RecordEmitFailure(ctx, s.deps.Logger, err, eventType)
	}
}

// ── bundle reads shared by the north RPCs and the south recall twin ───────────

// loadMemoryConcept reads and parses one concept, mapping failures onto codes the
// settings UI can act on: an unknown id is NotFound and a hand-corrupted file is
// FailedPrecondition — deletable, since Delete never parses.
func loadMemoryConcept(ctx context.Context, be workspacefs.Backend, tenant, seg, id string) ([]byte, memorybundle.Concept, error) {
	if err := memorybundle.ValidateID(id); err != nil {
		return nil, memorybundle.Concept{}, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	data, _, err := be.Read(ctx, tenant, seg, memorybundle.ConceptPath(id))
	if err != nil {
		return nil, memorybundle.Concept{}, fileError("read memory concept", err)
	}
	c, perr := memorybundle.ParseConcept(data)
	if perr != nil {
		return nil, memorybundle.Concept{}, status.Errorf(codes.FailedPrecondition, "%v", perr)
	}
	return data, c, nil
}

// memoryMetas builds one scope instance's metas. An unparseable concept is
// skipped rather than failing the listing — the posture RegenerateIndex already
// takes, so one hand-edited file cannot hide a whole bundle from its owner.
func memoryMetas(ctx context.Context, be workspacefs.Backend, tenant, seg, scope, groupID, agentID string) ([]*brokerv1.MemoryConceptMeta, error) {
	ids, err := memorybundle.ListConceptIDs(ctx, be, tenant, seg)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list memory bundle: %v", err)
	}
	now := time.Now().UTC()
	out := make([]*brokerv1.MemoryConceptMeta, 0, len(ids))
	for _, id := range ids {
		c, lerr := memorybundle.LoadConcept(ctx, be, tenant, seg, id)
		if lerr != nil {
			continue
		}
		out = append(out, memoryMeta(scope, groupID, agentID, id, c, now))
	}
	return out, nil
}

// memoryMeta flattens one concept's frontmatter onto the wire message, deriving
// trust tier and staleness (neither is ever stored).
func memoryMeta(scope, groupID, agentID, id string, c memorybundle.Concept, now time.Time) *brokerv1.MemoryConceptMeta {
	// group_id/agent_id are scope discriminators, not passthroughs: a request's
	// unused one is dropped so a caller-chosen value can never come back looking
	// like the concept's real home.
	if scope != "group" {
		groupID = ""
	}
	if scope != "agent" {
		agentID = ""
	}
	fm := c.Frontmatter
	status := fmText(fm["status"])
	if status == "" {
		status = "stable"
	}
	generated, _ := fmMap(fm["generated"])
	return &brokerv1.MemoryConceptMeta{
		Id:          id,
		Scope:       scope,
		GroupId:     groupID,
		AgentId:     agentID,
		Type:        fmText(fm["type"]),
		Title:       fmText(fm["title"]),
		Description: fmText(fm["description"]),
		Tags:        fmTexts(fm["tags"]),
		Status:      status,
		TrustTier:   memorybundle.TrustTier(fm),
		Stale:       memorybundle.IsStale(fm, now),
		StaleAfter:  fmDate(fm["stale_after"]),
		GeneratedBy: fmText(generated["by"]),
		GeneratedAt: fmTimestamp(generated["at"]),
	}
}

// fmText reads a string frontmatter scalar. Non-strings render empty rather than
// panicking: frontmatter is caller-written YAML and any key may hold any shape.
func fmText(v any) string {
	s, _ := v.(string)
	return s
}

func fmTexts(v any) []string {
	items, _ := v.([]any)
	out := make([]string, 0, len(items))
	for _, it := range items {
		if s, ok := it.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// fmMap normalizes a nested frontmatter mapping. yaml.v3 yields map[string]any
// for string-keyed maps but map[any]any is representable, so both are accepted.
func fmMap(v any) (map[string]any, bool) {
	switch t := v.(type) {
	case map[string]any:
		return t, true
	case map[any]any:
		out := make(map[string]any, len(t))
		for k, vv := range t {
			out[fmt.Sprint(k)] = vv
		}
		return out, true
	default:
		return map[string]any{}, false
	}
}

// fmDate renders a YYYY-MM-DD frontmatter field. yaml.v3 resolves an unquoted
// date scalar to time.Time, so both representations must render back to text.
func fmDate(v any) string {
	if t, ok := v.(time.Time); ok {
		return t.UTC().Format(memoryStaleAfterLayout)
	}
	return fmText(v)
}

// fmTimestamp renders a timestamp frontmatter field (generated.at) as RFC3339.
func fmTimestamp(v any) string {
	if t, ok := v.(time.Time); ok {
		return t.UTC().Format(time.RFC3339)
	}
	return fmText(v)
}

// agentOptedIntoRecall reports whether the agent's own configured skills include
// memory.read — the per-agent enablement toggle. agent.Skills is authoritative:
// an agent without it never contributes memory to a recall, whatever group
// grants its runner holds.
func agentOptedIntoRecall(ctx context.Context, agents AgentStore, tenant, agentID string) bool {
	if agents == nil || agentID == "" {
		return false
	}
	if _, err := uuid.Parse(agentID); err != nil {
		return false
	}
	a, err := agents.Get(ctx, tenant, agentID)
	if err != nil || a == nil {
		return false
	}
	return slices.Contains(a.Skills, memoryReadSkill)
}
