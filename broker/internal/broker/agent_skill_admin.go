// broker/internal/broker/agent_skill_admin.go
// North admin RPCs: UpsertAgentSkill, ListAgentSkills, GetAgentSkill, DeleteAgentSkill.
// Admin CRUD for skill bundles; files map stored as base64 extras; requireTenantAdmin
// gate; Audit.Emit on every mutation.
package broker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"regexp"
	"sort"

	"github.com/jackc/pgx/v5/pgconn"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/google/uuid"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/ids"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// agentSkillMaxRowBytes mirrors migration 028's agent_skill_size_cap CHECK
// (octet_length(body) + octet_length(extras::text) <= 5242880). Pre-validated
// here so an oversized bundle returns InvalidArgument, not a raw SQLSTATE.
const agentSkillMaxRowBytes = 5 * 1024 * 1024

// agentSkillRepo is the storage seam used by the admin RPCs. *db.AgentSkillRepo
// satisfies it; tests inject a fake.
type agentSkillRepo interface {
	Upsert(ctx context.Context, tenant string, row db.AgentSkill) error
	Get(ctx context.Context, tenant string, id uuid.UUID) (*db.AgentSkill, error)
	GetByName(ctx context.Context, tenant, name string) (*db.AgentSkill, error)
	ListByTenant(ctx context.Context, tenant string) ([]db.AgentSkill, error)
	Delete(ctx context.Context, tenant string, id uuid.UUID) error
}

// reservedSkillNames is the V1 set of built-in /command names that may never be
// used as tenant skill slugs. Extendable at upsert time with a one-line change;
// no migration required. Spec: agent-skill-bundles.md § Reserved built-in command names.
var reservedSkillNames = map[string]bool{
	"help":     true,
	"clear":    true,
	"new":      true,
	"reset":    true,
	"stop":     true,
	"settings": true,
	"skills":   true,
	"agents":   true,
	"login":    true,
	"logout":   true,
}

// skillNameRE enforces V1: slug must match ^[a-z0-9-]+$.
var skillNameRE = regexp.MustCompile(`^[a-z0-9-]+$`)

// UpsertAgentSkill creates or replaces a tenant SKILL.md bundle row.
// Gate: callerIdentity → requireTenantAdmin → V1 name check → files
// validation → repo.Upsert → emit audit. allowed_tools is carried verbatim,
// unvalidated (, D5 — the narrowing machinery
// that used to gate it here is dead code, removed).
func (s *BrokerService) UpsertAgentSkill(ctx context.Context, req *brokerv1.UpsertAgentSkillRequest) (*brokerv1.UpsertAgentSkillResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.agent_skill.upsert")
	defer span.End()

	tenant, user, err := s.callerIdentity(ctx, req.TenantId, req.UserId)
	if err != nil {
		return nil, err
	}
	if err := s.requireTenantAdmin(ctx, user); err != nil {
		return nil, err
	}

	// Fail-closed if store not wired: return FailedPrecondition before any work.
	repo := s.deps.AgentSkillBundles
	if repo == nil {
		return nil, status.Error(codes.FailedPrecondition, "agent skill bundle store not configured")
	}

	// V1: name must match ^[a-z0-9-]+$ and not be reserved.
	if !skillNameRE.MatchString(req.Name) {
		return nil, status.Errorf(codes.InvalidArgument,
			"skill name %q is invalid: must match ^[a-z0-9-]+$ (V1)", req.Name)
	}
	if reservedSkillNames[req.Name] {
		return nil, status.Errorf(codes.InvalidArgument,
			"skill name %q is reserved and may not be used as a bundle slug (V1)", req.Name)
	}

	// Files: validate every path (relative, no traversal/absolute/backslash/
	// NUL, ≤ maxSkillBundleFiles entries), then marshal to the flat
	// {"<relpath>": "<base64>"} shape the extras JSONB column stores
	//.
	if len(req.Files) > maxSkillBundleFiles {
		return nil, status.Errorf(codes.InvalidArgument,
			"bundle carries %d files, exceeds the %d file cap", len(req.Files), maxSkillBundleFiles)
	}
	extrasMap := make(map[string]string, len(req.Files))
	for relPath, content := range req.Files {
		if !validSkillFilePath(relPath) {
			return nil, status.Errorf(codes.InvalidArgument, "file path %q is invalid", relPath)
		}
		extrasMap[relPath] = base64.StdEncoding.EncodeToString(content)
	}
	extras, marshalErr := json.Marshal(extrasMap)
	if marshalErr != nil {
		return nil, status.Errorf(codes.Internal, "marshal files: %v", marshalErr)
	}
	// Pre-validate against the migration-028 row cap before ever reaching the
	// DB, so an oversized bundle returns a named InvalidArgument instead of a
	// raw SQLSTATE.
	if len(req.Body)+len(extras) > agentSkillMaxRowBytes {
		return nil, status.Errorf(codes.InvalidArgument,
			"bundle body+files total %d bytes, exceeds the %d byte cap", len(req.Body)+len(extras), agentSkillMaxRowBytes)
	}

	// Resolve or generate the row ID. Callers may supply an existing ID (update)
	// or leave it empty (insert).
	var rowID uuid.UUID
	if req.Id != "" {
		if rowID, err = uuid.Parse(req.Id); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid id %q: %v", req.Id, err)
		}
	} else {
		rowID = uuid.New()
	}

	allowedTools, marshalErr := json.Marshal(req.AllowedTools)
	if marshalErr != nil {
		// []string cannot fail json.Marshal in practice; guard anyway.
		return nil, status.Errorf(codes.Internal, "marshal allowed_tools: %v", marshalErr)
	}

	// Keywords normalize server-side (lowercase/trim/dedup) so the gateway
	// matcher never has to re-derive canonical form; empty list is valid.
	// json.Marshal(nil []string) yields "null", not "[]" — normalize to an
	// empty slice first so the column always holds a JSON array.
	normalizedKeywords := db.NormalizeKeywords(req.Keywords)
	if normalizedKeywords == nil {
		normalizedKeywords = []string{}
	}
	keywords, marshalErr := json.Marshal(normalizedKeywords)
	if marshalErr != nil {
		return nil, status.Errorf(codes.Internal, "marshal keywords: %v", marshalErr)
	}

	row := db.AgentSkill{
		TenantID:               uuid.MustParse(tenant),
		ID:                     rowID,
		Name:                   req.Name,
		Description:            req.Description,
		Body:                   req.Body,
		AllowedTools:           allowedTools,
		ContextFork:            req.ContextFork,
		DisableModelInvocation: req.DisableModelInvocation,
		Extras:                 extras,
		Keywords:               keywords,
		CreatedBy:              user,
	}

	if err := repo.Upsert(ctx, tenant, row); err != nil {
		// Map pg unique-violation (SQLSTATE 23505) to InvalidArgument per spec V1.
		// String-match is a fallback for fakes; real pgx returns *pgconn.PgError.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, status.Errorf(codes.InvalidArgument,
				"skill name %q is already taken in this tenant (V1)", req.Name)
		}
		return nil, status.Errorf(codes.Internal, "upsert agent_skill: %v", err)
	}

	s.emitAgentSkillAudit(ctx, span.SpanContext().TraceID().String(), tenant, user,
		"aikonos.broker.agent_skill.upserted", rowID.String(), req.Name)

	return &brokerv1.UpsertAgentSkillResponse{
		Bundle: agentSkillToProto(row),
	}, nil
}

// ListAgentSkills returns all bundle rows for the caller's tenant.
// Gate: callerIdentity → requireTenantAdmin → repo.ListByTenant.
func (s *BrokerService) ListAgentSkills(ctx context.Context, req *brokerv1.ListAgentSkillsRequest) (*brokerv1.ListAgentSkillsResponse, error) {
	tenant, user, err := s.callerIdentity(ctx, req.TenantId, req.UserId)
	if err != nil {
		return nil, err
	}
	if err := s.requireTenantAdmin(ctx, user); err != nil {
		return nil, err
	}

	repo := s.deps.AgentSkillBundles
	if repo == nil {
		return nil, status.Error(codes.FailedPrecondition, "agent skill bundle store not configured")
	}

	rows, err := repo.ListByTenant(ctx, tenant)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list agent_skill: %v", err)
	}

	out := make([]*brokerv1.AgentSkillBundle, 0, len(rows))
	for _, r := range rows {
		out = append(out, agentSkillToProto(r))
	}
	return &brokerv1.ListAgentSkillsResponse{Bundles: out}, nil
}

// GetAgentSkill returns a single bundle row by id.
// Gate: callerIdentity → requireTenantAdmin → repo.Get.
func (s *BrokerService) GetAgentSkill(ctx context.Context, req *brokerv1.GetAgentSkillRequest) (*brokerv1.GetAgentSkillResponse, error) {
	tenant, user, err := s.callerIdentity(ctx, req.TenantId, req.UserId)
	if err != nil {
		return nil, err
	}
	if err := s.requireTenantAdmin(ctx, user); err != nil {
		return nil, err
	}

	repo := s.deps.AgentSkillBundles
	if repo == nil {
		return nil, status.Error(codes.FailedPrecondition, "agent skill bundle store not configured")
	}

	id, err := uuid.Parse(req.Id)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid id %q: %v", req.Id, err)
	}

	row, err := repo.Get(ctx, tenant, id)
	if err != nil {
		if errors.Is(err, db.ErrAgentSkillNotFound) {
			return nil, status.Errorf(codes.NotFound, "%v", err)
		}
		return nil, status.Errorf(codes.Internal, "get agent_skill: %v", err)
	}

	return &brokerv1.GetAgentSkillResponse{Bundle: agentSkillToProto(*row)}, nil
}

// DeleteAgentSkill removes a bundle row.
// Gate: callerIdentity → requireTenantAdmin → repo.Delete → emit audit.
func (s *BrokerService) DeleteAgentSkill(ctx context.Context, req *brokerv1.DeleteAgentSkillRequest) (*brokerv1.DeleteAgentSkillResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.agent_skill.delete")
	defer span.End()

	tenant, user, err := s.callerIdentity(ctx, req.TenantId, req.UserId)
	if err != nil {
		return nil, err
	}
	if err := s.requireTenantAdmin(ctx, user); err != nil {
		return nil, err
	}

	repo := s.deps.AgentSkillBundles
	if repo == nil {
		return nil, status.Error(codes.FailedPrecondition, "agent skill bundle store not configured")
	}

	id, err := uuid.Parse(req.Id)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid id %q: %v", req.Id, err)
	}

	if err := repo.Delete(ctx, tenant, id); err != nil {
		if errors.Is(err, db.ErrAgentSkillNotFound) {
			return nil, status.Errorf(codes.NotFound, "%v", err)
		}
		return nil, status.Errorf(codes.Internal, "delete agent_skill: %v", err)
	}

	s.emitAgentSkillAudit(ctx, span.SpanContext().TraceID().String(), tenant, user,
		"aikonos.broker.agent_skill.deleted", id.String(), "")

	return &brokerv1.DeleteAgentSkillResponse{}, nil
}

// emitAgentSkillAudit mirrors emitSkillAudit for agent-skill bundle mutations.
// ResourceRef = "agentskill:<id>"; context map carries id and name.
func (s *BrokerService) emitAgentSkillAudit(ctx context.Context, traceID, tenant, actor, eventType, skillID, name string) {
	ev := &auditv1.AuditEvent{
		EventId:     ids.EventID(),
		TraceId:     traceID,
		TenantId:    tenant,
		OccurredAt:  timestampNow(),
		ActorUserId: actor,
		EventType:   eventType,
		Decision:    auditv1.PolicyDecision_ALLOW,
	}
	if skillID != "" {
		ev.ResourceRef = "agentskill:" + skillID
		ctxMap := map[string]any{"skill_id": skillID}
		if name != "" {
			ctxMap["name"] = name
		}
		if c, err := structpb.NewStruct(ctxMap); err == nil {
			ev.Context = c
		}
	}
	if err := s.deps.Audit.Emit(ctx, ev); err != nil {
		s.deps.Logger.Warn("agent_skill audit emit failed", zap.Error(err))
	}
}

// agentSkillExtras decodes row.Extras' flat {"<relpath>": "<base64>"} shape
//. Malformed JSON — never expected, this
// package is the sole writer — degrades to a nil map rather than propagating
// a decode error.
func agentSkillExtras(extras []byte) map[string]string {
	if len(extras) == 0 {
		return nil
	}
	var m map[string]string
	_ = json.Unmarshal(extras, &m)
	return m
}

// agentSkillToProto converts a db.AgentSkill to the wire message.
func agentSkillToProto(row db.AgentSkill) *brokerv1.AgentSkillBundle {
	var tools []string
	if len(row.AllowedTools) > 0 {
		_ = json.Unmarshal(row.AllowedTools, &tools)
	}
	var keywords []string
	if len(row.Keywords) > 0 {
		_ = json.Unmarshal(row.Keywords, &keywords)
	}
	extras := agentSkillExtras(row.Extras)
	filePaths := make([]string, 0, len(extras))
	for p := range extras {
		filePaths = append(filePaths, p)
	}
	sort.Strings(filePaths)
	return &brokerv1.AgentSkillBundle{
		Id:                     row.ID.String(),
		Name:                   row.Name,
		Description:            row.Description,
		Body:                   row.Body,
		AllowedTools:           tools,
		ContextFork:            row.ContextFork,
		DisableModelInvocation: row.DisableModelInvocation,
		CreatedBy:              row.CreatedBy,
		Keywords:               keywords,
		FilePaths:              filePaths,
	}
}
