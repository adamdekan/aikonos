// broker/internal/broker/skills_admin.go
// North admin RPCs: ListSkills, UpsertSkill, DeleteSkill.
// CP3: validateSkillOverlay helper enforces I2/I3/I4.
// CP4: mutation RPCs + audit.
package broker

import (
	"context"
	"errors"
	"sort"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/effectclass"
	"github.com/adamdekan/aikonos/broker/internal/ids"
	"github.com/adamdekan/aikonos/broker/internal/toolregistry"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
	planv1 "github.com/adamdekan/aikonos/gen/go/plan/v1"
)

// capabilitySkills are FGA-gated authz skills (skill:<id>) that are NOT
// invokable tools, so they are absent from the tool registry. ListSkills
// emits them so the admin console treats grants like skill:scheduler as
// legitimate rather than "stale — not in registry". Empty scope: no Biscuit
// capability scope is involved (these are not InvokeTool tools).
var capabilitySkills = []string{"scheduler", "workflows", "vision", "personal-skills", "subagents"}

// ListSkills returns the full registered tool vocabulary (tool_id + required
// capability scope) merged with the tenant overlay state. It is read-only and
// carries no FGA mutation, so it works regardless of whether an OpenFGA store
// is configured — requireTenantAdmin passes via the allow-all stub when FGA is
// disabled. Registry entries are the base; overlay rows annotate (enabled,
// effect_class, display_name, description, executor_kind). Overlay rows whose
// tool_id is NOT in the registry (MCP-introduced custom skills) are appended.
func (s *BrokerService) ListSkills(ctx context.Context, req *brokerv1.ListSkillsRequest) (*brokerv1.ListSkillsResponse, error) {
	tenant, user, err := s.callerIdentity(ctx, req.TenantId, req.UserId)
	if err != nil {
		return nil, err
	}
	if err := s.requireTenantAdmin(ctx, user); err != nil {
		return nil, err
	}

	reg := s.toolReg()
	entries := reg.ListTools()

	// Lazy-load the tenant overlay into the registry cache so IsEnabled /
	// EffectClassForTenant are current. Load errors are tolerated (I7).
	if s.deps.OverlayLoader != nil {
		s.deps.OverlayLoader.ensureLoaded(ctx, tenant, s.deps.Logger)
	}

	// Fetch raw overlay rows for display fields (display_name, description,
	// executor_kind) that the registry cache does not hold. Map by tool_id for
	// O(1) lookup; nil when no overlay store is wired.
	overlayByID := map[string]db.SkillOverlay{}
	if s.deps.SkillOverlay != nil {
		rows, lErr := s.deps.SkillOverlay.ListByTenant(ctx, tenant)
		if lErr != nil {
			s.deps.Logger.Warn("ListSkills: overlay fetch failed", zap.Error(lErr))
		} else {
			for _, r := range rows {
				overlayByID[r.ToolID] = r
			}
		}
	}

	// Merge registry vocabulary with overlay state.
	seen := make(map[string]struct{}, len(entries))
	skills := make([]*brokerv1.AdminSkill, 0, len(entries)+len(capabilitySkills)+len(overlayByID))

	for _, e := range entries {
		seen[e.ToolID] = struct{}{}
		sk := &brokerv1.AdminSkill{
			ToolId:      e.ToolID,
			Scope:       e.Scope,
			Enabled:     true,
			ExecutorKind: "builtin",
		}
		// Derive authoritative effect_class from the registry global default.
		if ec, ok := reg.AuthoritativeEffectClass(e.ToolID); ok {
			sk.EffectClass = effectclass.RegoString(ec)
		}
		if row, ok := overlayByID[e.ToolID]; ok {
			sk.Enabled = row.Enabled
			sk.DisplayName = row.DisplayName
			sk.Description = row.Description
			sk.ExecutorKind = row.ExecutorKind
			if row.EffectClass != "" {
				sk.EffectClass = row.EffectClass
			}
		}
		skills = append(skills, sk)
	}

	// Capability-only skills (FGA-gated, not invokable tools).
	for _, id := range capabilitySkills {
		seen[id] = struct{}{}
		skills = append(skills, &brokerv1.AdminSkill{ToolId: id, Scope: "", Enabled: true})
	}

	// Overlay rows not in registry = MCP-introduced custom skills.
	for toolID, row := range overlayByID {
		if _, ok := seen[toolID]; ok {
			continue
		}
		skills = append(skills, &brokerv1.AdminSkill{
			ToolId:      toolID,
			Scope:       row.CapabilityScope,
			Enabled:     row.Enabled,
			EffectClass: row.EffectClass,
			DisplayName: row.DisplayName,
			Description: row.Description,
			ExecutorKind: row.ExecutorKind,
		})
	}

	sort.Slice(skills, func(i, j int) bool {
		return skills[i].ToolId < skills[j].ToolId
	})

	return &brokerv1.ListSkillsResponse{Skills: skills}, nil
}

// validateSkillOverlay enforces I2/I3/I4 for a proposed overlay row.
// It derives executor_kind and scope (never accepts them from the caller) and
// resolves the authoritative effect class via ResolveOverlayClass.
//
// Returns (executorKind, derivedScope, resolvedClass, error).
// Error is always a gRPC status with codes.InvalidArgument on validation failure.
func (s *BrokerService) validateSkillOverlay(
	ctx context.Context,
	tenant, toolID, suppliedScope, declaredRego string,
) (executorKind, derivedScope string, resolvedClass planv1.EffectClass, err error) {
	reg := s.toolReg()

	// I2: determine executor_kind.
	// builtin iff toolID is in staticBaseline (IsBuiltin).
	// mcp iff toolID matches mcp:<conn>:<tool> AND the connector exists for this tenant.
	// Anything else (manifest-only, net-new, unknown) → InvalidArgument.
	if reg.IsBuiltin(toolID) {
		executorKind = "builtin"
	} else if connID, _, ok := toolregistry.ParseMcpToolID(toolID); ok {
		// Verify the connector exists for this tenant.
		if s.deps.Mcp.Connections == nil {
			return "", "", 0, status.Errorf(codes.InvalidArgument,
				"mcp connections not configured — cannot validate mcp tool_id %q", toolID)
		}
		if _, gerr := s.deps.Mcp.Connections.Get(ctx, tenant, connID); gerr != nil {
			return "", "", 0, status.Errorf(codes.InvalidArgument,
				"tool_id %q: connector %q does not exist for this tenant (no executor)", toolID, connID)
		}
		executorKind = "mcp"
	} else {
		return "", "", 0, status.Errorf(codes.InvalidArgument,
			"tool_id %q has no executor: only built-in (staticBaseline) and live mcp: tool ids are allowed", toolID)
	}

	// I3: derive scope; reject a supplied scope that disagrees.
	derivedScope, derr := reg.DeriveScope(toolID)
	if derr != nil {
		return "", "", 0, status.Errorf(codes.InvalidArgument, "%v", derr)
	}
	if suppliedScope != "" && suppliedScope != derivedScope {
		return "", "", 0, status.Errorf(codes.InvalidArgument,
			"tool_id %q: supplied scope %q conflicts with derived scope %q (I3)", toolID, suppliedScope, derivedScope)
	}

	// I4: resolve the authoritative effect class (enum-validate + floor + builtin-immutability).
	// When the caller omits effect_class, default to the baseline authoritative class so
	// ResolveOverlayClass does not reject an empty string at step 1.
	rego := declaredRego
	if rego == "" {
		if ec, ok := reg.AuthoritativeEffectClass(toolID); ok {
			rego = effectclass.RegoString(ec)
		} else {
			// MCP tool with no declared class: default to write_external floor (I4).
			rego = "write_external"
		}
	}
	resolvedClass, rerr := reg.ResolveOverlayClass(toolID, executorKind, rego)
	if rerr != nil {
		return "", "", 0, status.Errorf(codes.InvalidArgument, "%v", rerr)
	}

	return executorKind, derivedScope, resolvedClass, nil
}

// invalidateSkillOverlay clears the overlay cache for tenant so the next
// touch re-reads from the store. Called after every UpsertSkill / DeleteSkill.
func (s *BrokerService) invalidateSkillOverlay(tenant string) {
	if s.deps.OverlayLoader != nil {
		s.deps.OverlayLoader.invalidate(tenant)
	}
}

const (
	maxDescriptionLen  = 4096 // 4 KB
	maxDisplayNameLen  = 200
)

// UpsertSkill creates or updates a tenant skill overlay row.
// Gate: callerIdentity → requireTenantAdmin → validateSkillOverlay (I2/I3/I4)
// → repo.Upsert → invalidate cache → emit audit.
func (s *BrokerService) UpsertSkill(ctx context.Context, req *brokerv1.UpsertSkillRequest) (*brokerv1.UpsertSkillResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.skill.upsert")
	defer span.End()

	tenant, user, err := s.callerIdentity(ctx, req.TenantId, req.UserId)
	if err != nil {
		return nil, err
	}
	if err := s.requireTenantAdmin(ctx, user); err != nil {
		return nil, err
	}
	if req.ToolId == "" {
		return nil, status.Error(codes.InvalidArgument, "tool_id required")
	}
	// I8: length caps.
	if len(req.Description) > maxDescriptionLen {
		return nil, status.Errorf(codes.InvalidArgument,
			"description exceeds maximum length of %d bytes", maxDescriptionLen)
	}
	if len(req.DisplayName) > maxDisplayNameLen {
		return nil, status.Errorf(codes.InvalidArgument,
			"display_name exceeds maximum length of %d characters", maxDisplayNameLen)
	}

	if s.deps.SkillOverlay == nil {
		return nil, status.Error(codes.FailedPrecondition, "skill overlay store not configured")
	}

	execKind, derivedScope, resolvedClass, err := s.validateSkillOverlay(ctx, tenant, req.ToolId, req.Scope, req.EffectClass)
	if err != nil {
		return nil, err
	}

	store, ok := s.deps.SkillOverlay.(skillOverlayMutStore)
	if !ok || store == nil {
		return nil, status.Error(codes.FailedPrecondition, "skill overlay store not configured")
	}

	row := db.SkillOverlay{
		ToolID:          req.ToolId,
		ExecutorKind:    execKind,
		CapabilityScope: derivedScope,
		EffectClass:     effectclass.RegoString(resolvedClass),
		DisplayName:     req.DisplayName,
		Description:     req.Description,
		Enabled:         req.Enabled,
		CreatedBy:       user,
	}
	if err := store.Upsert(ctx, tenant, row); err != nil {
		return nil, status.Errorf(codes.Internal, "upsert skill overlay: %v", err)
	}

	s.invalidateSkillOverlay(tenant)

	s.emitSkillAudit(ctx, span.SpanContext().TraceID().String(), tenant, user,
		"aikonos.broker.skill.upserted", req.ToolId, row.EffectClass)

	return &brokerv1.UpsertSkillResponse{
		Skill: &brokerv1.AdminSkill{
			ToolId:      row.ToolID,
			Scope:       row.CapabilityScope,
			Enabled:     row.Enabled,
			EffectClass: row.EffectClass,
			DisplayName: row.DisplayName,
			Description: row.Description,
			ExecutorKind: row.ExecutorKind,
		},
	}, nil
}

// DeleteSkill removes a tenant skill overlay row, reverting the skill to its
// un-curated defaults. Gate: callerIdentity → requireTenantAdmin → repo.Delete
// → invalidate cache → emit audit.
func (s *BrokerService) DeleteSkill(ctx context.Context, req *brokerv1.DeleteSkillRequest) (*brokerv1.DeleteSkillResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.skill.delete")
	defer span.End()

	tenant, user, err := s.callerIdentity(ctx, req.TenantId, req.UserId)
	if err != nil {
		return nil, err
	}
	if err := s.requireTenantAdmin(ctx, user); err != nil {
		return nil, err
	}
	if req.ToolId == "" {
		return nil, status.Error(codes.InvalidArgument, "tool_id required")
	}
	if s.deps.SkillOverlay == nil {
		return nil, status.Error(codes.FailedPrecondition, "skill overlay store not configured")
	}

	store, ok := s.deps.SkillOverlay.(skillOverlayMutStore)
	if !ok || store == nil {
		return nil, status.Error(codes.FailedPrecondition, "skill overlay store not configured")
	}

	if err := store.Delete(ctx, tenant, req.ToolId); err != nil {
		if errors.Is(err, db.ErrSkillOverlayNotFound) {
			return nil, status.Errorf(codes.NotFound, "%v", err)
		}
		return nil, status.Errorf(codes.Internal, "delete skill overlay: %v", err)
	}

	s.invalidateSkillOverlay(tenant)

	s.emitSkillAudit(ctx, span.SpanContext().TraceID().String(), tenant, user,
		"aikonos.broker.skill.deleted", req.ToolId, "")

	return &brokerv1.DeleteSkillResponse{}, nil
}

// emitSkillAudit mirrors emitProvisioningAudit: ResourceRef = "skill:<tool_id>",
// context map carries tool_id and effect_class for traceability.
func (s *BrokerService) emitSkillAudit(ctx context.Context, traceID, tenant, actor, eventType, toolID, effectClassRego string) {
	ev := &auditv1.AuditEvent{
		EventId:     ids.EventID(),
		TraceId:     traceID,
		TenantId:    tenant,
		OccurredAt:  timestampNow(),
		ActorUserId: actor,
		EventType:   eventType,
		Decision:    auditv1.PolicyDecision_ALLOW,
	}
	if toolID != "" {
		ev.ResourceRef = "skill:" + toolID
		ctxMap := map[string]any{"tool_id": toolID}
		if effectClassRego != "" {
			ctxMap["effect_class"] = effectClassRego
		}
		if c, err := structpb.NewStruct(ctxMap); err == nil {
			ev.Context = c
		}
	}
	if err := s.deps.Audit.Emit(ctx, ev); err != nil {
		s.deps.Logger.Warn("skill audit emit failed", zap.Error(err))
	}
}
