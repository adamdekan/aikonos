package broker

import (
	"context"
	"sort"
	"strings"

	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/ids"
	"github.com/adamdekan/aikonos/broker/internal/policy"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// fgaTenantObject is the single place that maps the broker's configured tenant
// to its OpenFGA object string (the tenant is a UUID on the wire but a name in
// the FGA model, e.g. "tenant:aikonos-dev"). Single-tenant assumption: the
// process serves one tenant (Deps.TenantID); multi-tenant needs a UUID→name map.
func (s *BrokerService) fgaTenantObject() string {
	return "tenant:" + s.deps.TenantID
}

// fgaTenantObject mirrors BrokerService.fgaTenantObject for the south-bound
// service (both share Deps.TenantID). Used by the SPIFFE-gated ListWorkflows
// twin to pass the FGA tenant object into workflowsvc.List's MayOperateAgent
// gate (F9).
func (s *SandboxService) fgaTenantObject() string {
	return "tenant:" + s.deps.TenantID
}

// emitAdminAccessDenied fires a DENY audit event when requireTenantAdmin
// rejects a caller. Fire-and-forget; nil-guards deps.Audit.
func (s *BrokerService) emitAdminAccessDenied(ctx context.Context, user, reason string) {
	if s.deps.Audit == nil {
		return
	}
	traceID := trace.SpanContextFromContext(ctx).TraceID().String()
	ctxStruct, _ := structpb.NewStruct(map[string]any{"reason": reason})
	if err := s.deps.Audit.Emit(ctx, &auditv1.AuditEvent{
		EventId:     ids.EventID(),
		TraceId:     traceID,
		TenantId:    s.deps.TenantID,
		OccurredAt:  timestampNow(),
		ActorUserId: user,
		EventType:   "aikonos.broker.admin.access.denied",
		ResourceRef: "aikonos:tenant:" + s.deps.TenantID,
		Decision:    auditv1.PolicyDecision_DENY,
		Context:     ctxStruct,
	}); err != nil {
		audit.RecordEmitFailure(ctx, s.deps.Logger, err, "aikonos.broker.admin.access.denied")
	}
}

// manageRule is one curated (relation, object-type) the admin API may grant or
// revoke. Anything not in manageableRules is rejected — the admin surface must
// never write arbitrary tuples (e.g. workspace/task ACLs, or admin on another
// tenant). userKinds is the set of user-ref shapes the FGA model permits for
// this relation.
type manageRule struct {
	relation   string
	objectType string
	userKinds  map[string]bool
	section    brokerv1.AssignmentSection
}

const (
	kindUser        = "user"         // "user:alice@example.com"
	kindGroupMember = "group#member" // "group:security-team#member"
	kindAgent       = "agent"        // "agent:alice-agent"
)

var (
	userOrGroup = map[string]bool{kindUser: true, kindGroupMember: true}
	userOnly    = map[string]bool{kindUser: true}
	groupOnly   = map[string]bool{kindGroupMember: true}
	userOrAgent = map[string]bool{kindUser: true, kindAgent: true}
	agentOnly   = map[string]bool{kindAgent: true}
)

// manageableRules mirrors the directly-assignable type restrictions in
// policies/fga/model.fga. Computed relations (e.g. agent.can_delegate_to_user)
// are intentionally absent — they have no stored tuples to write or revoke.
var manageableRules = []manageRule{
	{"member", "group", userOrGroup, brokerv1.AssignmentSection_GROUP_MEMBERSHIP},
	{"manager", "group", userOnly, brokerv1.AssignmentSection_GROUP_MEMBERSHIP},
	// DL1: the delegation-cohort marker. Subject is the group's own member-set
	// (group:<name>#member); the non-wildcard ref keeps the wildcard guard intact.
	{"delegatable", "group", groupOnly, brokerv1.AssignmentSection_GROUP_MEMBERSHIP},
	{"admin", "tenant", userOnly, brokerv1.AssignmentSection_TENANT_ROLE},
	{"member", "tenant", userOrGroup, brokerv1.AssignmentSection_TENANT_ROLE},
	{"permitted_group", "skill", groupOnly, brokerv1.AssignmentSection_SKILL_ACCESS},
	{"can_use", "agentskill", groupOnly, brokerv1.AssignmentSection_SKILL_ACCESS},
	{"owner_user", "agent", userOnly, brokerv1.AssignmentSection_AGENT_RELATION},
	{"spawned_by", "agent", userOrAgent, brokerv1.AssignmentSection_AGENT_RELATION},
	{"usable_by", "agent", userOrGroup, brokerv1.AssignmentSection_AGENT_RELATION},
	{"permitted_agent", "mcp_connector", agentOnly, brokerv1.AssignmentSection_MCP_ACCESS},
}

// sectionForObject maps an object's type prefix to its curated assignment
// section. Objects of any other type (e.g. task:) are not part of the admin
// view and return ok=false.
func sectionForObject(object string) (brokerv1.AssignmentSection, bool) {
	switch {
	case strings.HasPrefix(object, "tenant:"):
		return brokerv1.AssignmentSection_TENANT_ROLE, true
	case strings.HasPrefix(object, "group:"):
		return brokerv1.AssignmentSection_GROUP_MEMBERSHIP, true
	case strings.HasPrefix(object, "skill:"):
		return brokerv1.AssignmentSection_SKILL_ACCESS, true
	case strings.HasPrefix(object, "agent:"):
		return brokerv1.AssignmentSection_AGENT_RELATION, true
	case strings.HasPrefix(object, "mcp_connector:"):
		return brokerv1.AssignmentSection_MCP_ACCESS, true
	case strings.HasPrefix(object, "agentskill:"):
		// Reuses SKILL_ACCESS: agentskill grants live in the same admin panel
		// section as tool-overlay skill grants; no dedicated enum value exists
		// in the frozen CP2 proto — adding one is CP2's responsibility.
		return brokerv1.AssignmentSection_SKILL_ACCESS, true
	}
	return 0, false
}

// managedObjectTypes are the object types the admin API may touch. Anything else
// (workspace/task/document/…) is out of scope and rejected.
var managedObjectTypes = map[string]bool{"group": true, "tenant": true, "skill": true, "agent": true, "mcp_connector": true, "agentskill": true}

func splitObject(object string) (typ, id string, ok bool) {
	i := strings.IndexByte(object, ':')
	if i <= 0 || i == len(object)-1 {
		return "", "", false
	}
	return object[:i], object[i+1:], true
}

func userKind(user string) string {
	switch {
	case strings.HasPrefix(user, "user:"):
		return kindUser
	case strings.HasPrefix(user, "group:") && strings.HasSuffix(user, "#member"):
		return kindGroupMember
	case strings.HasPrefix(user, "agent:"):
		return kindAgent
	default:
		return ""
	}
}

const maxTupleFieldLen = 256

// badTupleField rejects a tuple field that is empty, over-long, or carries
// whitespace — the broker validates shape itself because user/relation/object
// are sent verbatim to OpenFGA.
func badTupleField(s string) bool {
	return s == "" || len(s) > maxTupleFieldLen || strings.ContainsAny(s, " \t\r\n")
}

// hasWildcardID reports whether a ref's id segment is the OpenFGA public
// wildcard "*" (e.g. "user:*"), which would grant the relation to everyone. The
// optional "#member" userset suffix is stripped before inspecting the id.
func hasWildcardID(ref string) bool {
	if i := strings.IndexByte(ref, '#'); i >= 0 {
		ref = ref[:i]
	}
	if _, id, ok := strings.Cut(ref, ":"); ok {
		return id == "*"
	}
	return ref == "*"
}

// validateManagedObject is the floor for any admin write: well-formed fields, a
// managed object type, and tenant tuples pinned to this tenant. Both assign and
// revoke require it.
func (s *BrokerService) validateManagedObject(t *brokerv1.RoleTuple) (typ, id string, err error) {
	if t == nil {
		return "", "", status.Error(codes.InvalidArgument, "user, relation and object are required")
	}
	if badTupleField(t.User) || badTupleField(t.Relation) || badTupleField(t.Object) {
		return "", "", status.Error(codes.InvalidArgument, "tuple fields must be non-empty, <=256 chars, and free of whitespace")
	}
	// Never write the FGA public wildcard, and never let an object carry a
	// relation suffix ("type:id#relation") — either would silently widen the
	// grant far beyond the chosen subject/object.
	if hasWildcardID(t.User) || hasWildcardID(t.Object) {
		return "", "", status.Error(codes.InvalidArgument, "wildcard ids are not permitted")
	}
	if strings.ContainsRune(t.Object, '#') {
		return "", "", status.Error(codes.InvalidArgument, "object must not carry a relation suffix")
	}
	typ, id, ok := splitObject(t.Object)
	if !ok {
		return "", "", status.Error(codes.InvalidArgument, "object must be type:id")
	}
	if !managedObjectTypes[typ] {
		return "", "", status.Errorf(codes.InvalidArgument, "%q objects are not managed here", typ)
	}
	if typ == "tenant" && id != s.deps.TenantID {
		return "", "", status.Error(codes.PermissionDenied, "cannot manage another tenant")
	}
	return typ, id, nil
}

// validateAssignable is the strict chokepoint for granting a role: on top of the
// managed-object floor it enforces the relation allowlist and subject-kind rules
// from the FGA model, so the admin API can never create an over-broad or
// model-invalid grant.
func (s *BrokerService) validateAssignable(t *brokerv1.RoleTuple) (*manageRule, error) {
	typ, _, err := s.validateManagedObject(t)
	if err != nil {
		return nil, err
	}
	var rule *manageRule
	for i := range manageableRules {
		if manageableRules[i].relation == t.Relation && manageableRules[i].objectType == typ {
			rule = &manageableRules[i]
			break
		}
	}
	if rule == nil {
		return nil, status.Errorf(codes.InvalidArgument, "relation %q is not assignable on %q objects", t.Relation, typ)
	}
	if !rule.userKinds[userKind(t.User)] {
		return nil, status.Errorf(codes.InvalidArgument, "user %q is not a valid subject for %q", t.User, t.Relation)
	}
	return rule, nil
}

// validateRevocable is deliberately looser than assign: revoking only removes a
// grant (de-privileging is always safe), so an admin may clear any tuple the
// view surfaces on a managed object — including legacy/model-invalid grants the
// strict assign path would reject (e.g. a user directly on skill.permitted_group).
func (s *BrokerService) validateRevocable(t *brokerv1.RoleTuple) error {
	_, _, err := s.validateManagedObject(t)
	return err
}

func (s *BrokerService) ListAssignments(ctx context.Context, req *brokerv1.ListAssignmentsRequest) (*brokerv1.ListAssignmentsResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.admin.list_assignments")
	defer span.End()

	tenant, user, err := s.callerIdentity(ctx, req.TenantId, req.UserId)
	if err != nil {
		return nil, err
	}
	if err := s.requireTenantAdmin(ctx, user); err != nil {
		return nil, err
	}

	tuples, warnings := s.assembleAssignments(ctx)

	// Load the user directory once; nil-safe + error-tolerant (display labels only).
	// TODO: full-table read per admin page load; cache/paginate if directories grow large.
	var dirEntries map[string]db.UserDirectoryEntry
	if s.deps.UserDirectory != nil {
		if m, err := s.deps.UserDirectory.ListByTenant(ctx, tenant); err != nil {
			s.deps.Logger.Warn("ListAssignments: user_directory load failed", zap.Error(err))
		} else {
			dirEntries = m
		}
	}

	// Load agent names once; nil-safe + error-tolerant (display labels only).
	var agentNames map[string]string // bare uuid → name
	if s.deps.Agents != nil {
		if agents, err := s.deps.Agents.List(ctx, tenant); err != nil {
			s.deps.Logger.Warn("ListAssignments: agents load failed", zap.Error(err))
		} else {
			agentNames = make(map[string]string, len(agents))
			for _, a := range agents {
				agentNames[a.ID.String()] = a.Name
			}
		}
	}

	s.emitAdminAudit(ctx, span.SpanContext().TraceID().String(), tenant, user,
		"aikonos.broker.admin.assignments.listed", nil, auditv1.PolicyDecision_ALLOW)
	return &brokerv1.ListAssignmentsResponse{
		Tuples:     tuples,
		Principals: s.principalsFrom(tuples, dirEntries, agentNames),
		FgaEnabled: s.deps.Policy.FGAEnabled(),
		Warnings:   warnings,
	}, nil
}

func (s *BrokerService) AssignRole(ctx context.Context, req *brokerv1.AssignRoleRequest) (*brokerv1.AssignRoleResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.admin.assign_role")
	defer span.End()

	tenant, user, err := s.callerIdentity(ctx, req.TenantId, req.UserId)
	if err != nil {
		return nil, err
	}
	if err := s.requireTenantAdmin(ctx, user); err != nil {
		return nil, err
	}
	if !s.deps.Policy.FGAEnabled() {
		return nil, status.Error(codes.FailedPrecondition, "OpenFGA store not configured — cannot persist assignments")
	}
	if _, err := s.validateAssignable(req.Tuple); err != nil {
		return nil, err
	}
	if err := s.deps.Policy.WriteRelations(ctx, relationOf(req.Tuple)); err != nil {
		return nil, status.Errorf(codes.Internal, "assign failed: %v", err)
	}
	s.emitAdminAudit(ctx, span.SpanContext().TraceID().String(), tenant, user,
		"aikonos.broker.admin.role.assigned", req.Tuple, auditv1.PolicyDecision_ALLOW)
	return &brokerv1.AssignRoleResponse{Success: true}, nil
}

func (s *BrokerService) RevokeRole(ctx context.Context, req *brokerv1.RevokeRoleRequest) (*brokerv1.RevokeRoleResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.admin.revoke_role")
	defer span.End()

	tenant, user, err := s.callerIdentity(ctx, req.TenantId, req.UserId)
	if err != nil {
		return nil, err
	}
	if err := s.requireTenantAdmin(ctx, user); err != nil {
		return nil, err
	}
	if !s.deps.Policy.FGAEnabled() {
		return nil, status.Error(codes.FailedPrecondition, "OpenFGA store not configured — cannot persist assignments")
	}
	if err := s.validateRevocable(req.Tuple); err != nil {
		return nil, err
	}
	if err := s.deps.Policy.DeleteRelations(ctx, relationOf(req.Tuple)); err != nil {
		return nil, status.Errorf(codes.Internal, "revoke failed: %v", err)
	}
	s.emitAdminAudit(ctx, span.SpanContext().TraceID().String(), tenant, user,
		"aikonos.broker.admin.role.revoked", req.Tuple, auditv1.PolicyDecision_ALLOW)
	return &brokerv1.RevokeRoleResponse{Success: true}, nil
}

func relationOf(t *brokerv1.RoleTuple) policy.Relation {
	return policy.Relation{User: t.User, Relation: t.Relation, Object: t.Object}
}

// assembleAssignments reads every tuple in the store and buckets each into its
// curated section by object type. OpenFGA rejects the bare type-prefix Read the
// per-section approach would need (object id and user both empty → 400), so a
// single all-tuples read + in-process bucketing is the portable path. Rows for
// other tenants and non-curated object types (e.g. task:) are dropped. A read
// failure degrades the whole view to one warning rather than crashing.
func (s *BrokerService) assembleAssignments(ctx context.Context) (tuples []*brokerv1.RoleTuple, warnings []string) {
	rels, err := s.deps.Policy.ReadTuples(ctx, policy.ReadFilter{})
	if err != nil {
		s.deps.Logger.Warn("assembleAssignments: read failed", zap.Error(err))
		return nil, []string{"assignments unavailable"}
	}
	home := s.fgaTenantObject()
	for _, r := range rels {
		section, ok := sectionForObject(r.Object)
		if !ok {
			continue
		}
		if section == brokerv1.AssignmentSection_TENANT_ROLE && r.Object != home {
			continue
		}
		tuples = append(tuples, &brokerv1.RoleTuple{User: r.User, Relation: r.Relation, Object: r.Object, Section: section})
	}
	return tuples, warnings
}

// principalsFrom derives the user/group/agent directory from the observed
// tuples merged with the configured seed users and the user directory.
// dirEntries is the user directory loaded for this tenant (nil-safe: missing
// → fallback to id suffix). agentNames maps bare agent uuid → name (nil-safe:
// missing → id-suffix fallback). For user: principals the display name
// preference is: directory name → directory email → id-suffix fallback. For
// agent: principals: agentNames → id-suffix fallback. group: principals are
// unaffected.
//
// Every dirEntries subject is listed even when it holds no FGA tuple yet —
// otherwise a JIT-provisioned user whose login predates any matching
// provisioning rule (ClaimProvisioning is one-shot; see provisioner.go) would
// never appear here, leaving admins with no way to grant them a role through
// this RPC's response.
func (s *BrokerService) principalsFrom(tuples []*brokerv1.RoleTuple, dirEntries map[string]db.UserDirectoryEntry, agentNames map[string]string) []*brokerv1.Principal {
	seen := map[string]*brokerv1.Principal{}
	addRef := func(ref, source string) {
		id, kind := normalizePrincipal(ref)
		if id == "" {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = &brokerv1.Principal{Id: id, Kind: kind, DisplayName: principalDisplayName(id, kind, dirEntries, agentNames), Source: source}
	}
	for _, u := range s.deps.KnownUsers {
		addRef("user:"+u, "keycloak")
	}
	for _, t := range tuples {
		addRef(t.User, "fga")
		addRef(t.Object, "fga") // groups/agents are also targets the UI lists
	}
	for subject := range dirEntries {
		addRef("user:"+subject, "directory")
	}

	out := make([]*brokerv1.Principal, 0, len(seen))
	for _, p := range seen {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Id < out[j].Id
	})
	return out
}

// normalizePrincipal turns a tuple ref into a (canonical id, kind). A
// group#member user ref collapses to its group; skill/tenant objects are
// resources, not principals, and return "".
func normalizePrincipal(ref string) (id, kind string) {
	switch {
	case strings.HasPrefix(ref, "user:"):
		return ref, "user"
	case strings.HasPrefix(ref, "group:"):
		return strings.TrimSuffix(ref, "#member"), "group"
	case strings.HasPrefix(ref, "agent:"):
		return ref, "agent"
	default:
		return "", ""
	}
}

// principalDisplayName resolves the human-readable label for a principal.
// For user: refs it consults the directory (name → email → id-suffix fallback).
// For agent: refs it consults agentNames (bare uuid → name, id-suffix fallback).
// For group: refs it uses the id-suffix fallback unchanged.
func principalDisplayName(id, kind string, dir map[string]db.UserDirectoryEntry, agentNames map[string]string) string {
	if kind == "user" {
		// Strip "user:" prefix to get the bare subject for the directory lookup.
		if _, subject, ok := strings.Cut(id, ":"); ok {
			if e, ok := dir[subject]; ok {
				if e.Name != "" {
					return e.Name
				}
				if e.Email != "" {
					return e.Email
				}
			}
		}
	}
	if kind == "agent" {
		// Strip "agent:" prefix to get the bare uuid for the agents-table lookup.
		if _, agentID, ok := strings.Cut(id, ":"); ok {
			if name, ok := agentNames[agentID]; ok && name != "" {
				return name
			}
		}
	}
	// Fallback: strip the type prefix and use the id segment.
	if _, name, ok := splitObject(id); ok {
		return name
	}
	return id
}

func (s *BrokerService) emitAdminAudit(ctx context.Context, traceID, tenant, actor, eventType string, tuple *brokerv1.RoleTuple, decision auditv1.PolicyDecision) {
	if s.deps.Audit == nil {
		return
	}
	ev := &auditv1.AuditEvent{
		EventId:     ids.EventID(),
		TraceId:     traceID,
		TenantId:    tenant,
		OccurredAt:  timestampNow(),
		ActorUserId: actor,
		EventType:   eventType,
		Decision:    decision,
	}
	if tuple != nil {
		ev.ResourceRef = tuple.Object
		if ctxStruct, err := structpb.NewStruct(map[string]any{
			"user":     tuple.User,
			"relation": tuple.Relation,
			"object":   tuple.Object,
		}); err == nil {
			ev.Context = ctxStruct
		}
	}
	if err := s.deps.Audit.Emit(ctx, ev); err != nil {
		audit.RecordEmitFailure(ctx, s.deps.Logger, err, ev.EventType)
	}
}
