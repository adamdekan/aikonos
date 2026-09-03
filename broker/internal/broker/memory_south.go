package broker

// memory_south.go — ListMemoryConceptsSouth, the SPIFFE-gated recall list the
// gateway calls once per turn.
//
// This is the single enablement chokepoint for auto-recall: it returns an EMPTY
// response — never an error — unless the user holds skill:memory.read, and it
// includes the agent bundle only when that agent's own Skills carry memory.read.
// Revoking either grant therefore silences recall on the next turn with no
// gateway change. Empty-not-error is deliberate: a chat turn must not fail
// because a memory grant was withdrawn or OpenFGA hiccuped.

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
	"github.com/adamdekan/aikonos/broker/internal/ids"
	"github.com/adamdekan/aikonos/broker/internal/memorybundle"
	"github.com/adamdekan/aikonos/broker/internal/toolproxy"
	"github.com/adamdekan/aikonos/broker/internal/workspacepath"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// memoryRecallOmittedEvent is fired per concept auto-recall drops. Audit-log
// only — no agui SSE event, so no contracts.json entry.
const memoryRecallOmittedEvent = "aikonos.memory.recall_omitted"

func (s *SandboxService) ListMemoryConceptsSouth(ctx context.Context, req *brokerv1.ListMemoryConceptsRequest) (*brokerv1.ListMemoryConceptsResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.memory.list_concepts_south")
	defer span.End()

	if err := s.requireGatewayPeer(ctx); err != nil {
		return nil, err
	}
	tenant, user := req.GetTenantId(), req.GetUserId()
	if tenant == "" || user == "" {
		return nil, status.Error(codes.InvalidArgument, "tenant_id and user_id required")
	}

	out := &brokerv1.ListMemoryConceptsResponse{}
	be := s.deps.Workspace.Local
	if be == nil || s.deps.Policy == nil {
		return out, nil
	}
	ok, err := s.deps.Policy.CheckFGA(ctx, "user:"+user, "can_invoke", "skill:"+memoryReadSkill)
	if err != nil || !ok {
		return out, nil
	}

	// The union is capped so a member of many groups cannot blow the gRPC message
	// bound (and lose recall entirely from then on). The fill order IS the
	// contract — own bundle, then groups by id, then the agent bundle, which a
	// full cap may crowd out — so what a user recalls stays deterministic instead
	// of depending on which bundle was listed first.
	remaining := memorybundle.MaxConcepts
	recall := func(seg, scope, groupID, agentID string) {
		if remaining <= 0 {
			return // don't read a bundle whose metas cannot fit anyway
		}
		metas := s.recallMetas(ctx, tenant, user, seg, scope, groupID, agentID)
		if len(metas) > remaining {
			metas = metas[:remaining]
		}
		out.Concepts = append(out.Concepts, metas...)
		remaining -= len(metas)
	}

	// The user's own bundle. Skipped for a synthetic subject: an external
	// agent-bound run's principal is svc-<agentId>, and treating that as a "user"
	// segment would reach the agent bundle through the branch that never checks
	// the agent's own memory.read opt-in.
	if !strings.HasPrefix(user, memoryAgentSegPrefix) && !strings.HasPrefix(user, memoryGroupSegPrefix) {
		recall(user, "user", "", "")
	}

	// Every group the user is a member of. Fail-open-to-empty on an FGA error,
	// like delegation discovery: recall is best-effort context, not a gate.
	refs, lerr := s.deps.Policy.ListObjects(ctx, "user:"+user, "member", "group")
	if lerr != nil {
		s.deps.Logger.Warn("ListMemoryConceptsSouth: group discovery failed, recalling without group memory", zap.Error(lerr))
		refs = nil
	}
	sort.Strings(refs)
	for _, ref := range refs {
		gid := strings.TrimPrefix(ref, "group:")
		if gid == "" || workspacepath.SanitizeSeg(gid) != gid {
			continue
		}
		recall(memoryGroupSegPrefix+gid, "group", gid, "")
	}

	if agentID := req.GetAgentId(); agentOptedIntoRecall(ctx, s.deps.Agents, tenant, agentID) {
		recall(memoryAgentSegPrefix+agentID, "agent", "", agentID)
	}

	return out, nil
}

// recallMetas lists one bundle's metas, logging and skipping a bundle it cannot
// read: one unreadable scope must not cost the turn every other scope's memory.
// Injection-flagged concepts are dropped here rather than annotated: auto-recall
// puts them in front of the model with no user action and no model decision, so
// there is nobody in that loop for a flag to reach. Dropping is per-concept — a
// clean concept in the same bundle still recalls.
func (s *SandboxService) recallMetas(ctx context.Context, tenant, user, seg, scope, groupID, agentID string) []*brokerv1.MemoryConceptMeta {
	metas, err := memoryMetas(ctx, s.deps.Workspace.Local, tenant, seg, scope, groupID, agentID)
	if err != nil {
		s.deps.Logger.Warn("ListMemoryConceptsSouth: bundle unreadable, skipped",
			zap.String("scope", scope), zap.String("segment", seg), zap.Error(err))
		return nil
	}
	kept := metas[:0]
	for _, m := range metas {
		if flags := toolproxy.ScanForInjection(recallVisibleText(m)); len(flags) > 0 {
			s.deps.Logger.Warn("ListMemoryConceptsSouth: concept omitted from recall, injection patterns matched",
				zap.String("scope", scope), zap.String("concept_id", m.GetId()), zap.Strings("flags", flags))
			s.emitRecallOmitted(ctx, tenant, user, seg, m, flags)
			continue
		}
		kept = append(kept, m)
	}
	return kept
}

// emitRecallOmitted records one concept auto-recall withheld. This is the only
// place in the memory path where the scan blocks instead of annotating, so it is
// the only one where a false positive would otherwise present as "my memory
// silently stopped being recalled" with no signal anywhere. Shape follows
// emitMemoryEvent (memory resource ref, scope/group_id/id context) — the closer
// precedent than emitResultFlagged, whose resource is a tool call — with the
// flags carried across from the annotate-only case, and DENY rather than ALLOW
// because the concept was withheld, not flagged in place. Fire-and-forget: an
// audit failure must not cost the turn its remaining memory.
func (s *SandboxService) emitRecallOmitted(ctx context.Context, tenant, user, seg string, m *brokerv1.MemoryConceptMeta, flags []string) {
	if s.deps.Audit == nil {
		return
	}
	anyFlags := make([]any, len(flags))
	for i, f := range flags {
		anyFlags[i] = f
	}
	ctxStruct, _ := structpb.NewStruct(map[string]any{
		"scope":           m.GetScope(),
		"group_id":        m.GetGroupId(),
		"id":              m.GetId(),
		"injection_flags": anyFlags,
	})
	if err := s.deps.Audit.Emit(ctx, &auditv1.AuditEvent{
		EventId:     ids.EventID(),
		TraceId:     trace.SpanContextFromContext(ctx).TraceID().String(),
		TenantId:    tenant,
		OccurredAt:  timestampNow(),
		ActorUserId: user,
		EventType:   memoryRecallOmittedEvent,
		ResourceRef: "aikonos:memory:" + m.GetScope() + "/" + seg + "/" + m.GetId(),
		Decision:    auditv1.PolicyDecision_DENY,
		Context:     ctxStruct,
	}); err != nil {
		audit.RecordEmitFailure(ctx, s.deps.Logger, err, memoryRecallOmittedEvent)
	}
}

// recallVisibleText is the concept text auto-recall puts in front of the model,
// which is the union of two renderers: buildMemoryPreamble
// (agent-gateway/src/routes/agui.ts) renders the id, title and description, and
// the semantic-recall tier's conceptText embeds title, description and tags. The
// id is here because it is rendered verbatim; today memoryMetas only yields
// ValidateID-clean ids (LoadConcept rejects the rest), so this is depth, not the
// last line — a future recall path that lists filenames without loading them
// would need no second fix. The body is not here — recall never carries one;
// memory.read fetches bodies under its own annotate-only scan.
func recallVisibleText(m *brokerv1.MemoryConceptMeta) string {
	return strings.Join(append([]string{m.GetId(), m.GetTitle(), m.GetDescription()}, m.GetTags()...), "\n")
}
