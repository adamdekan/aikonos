package workflowsvc

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/ids"
	"github.com/adamdekan/aikonos/broker/internal/toolregistry"
	"github.com/adamdekan/aikonos/broker/internal/workflow"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// Save persists a workflow version — brand-new (empty LineageId) or an edit
// of an existing lineage (inherits status/visibility from the current
// approved version so a published workflow stays published after an owner
// edit). Was saveWorkflowCore in broker/internal/broker/service_workflows.go.
func Save(
	ctx context.Context,
	tenantID, ownerUserID string,
	req *brokerv1.SaveWorkflowRequest,
	store Store,
	reg *toolregistry.Registry,
	emitter AuditEmitter,
	logger *zap.Logger,
) (*brokerv1.SaveWorkflowResponse, error) {
	if req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "name required")
	}
	if req.DefinitionJson == "" {
		return nil, status.Error(codes.InvalidArgument, "definition_json required")
	}

	def, wfErr := workflow.FromJSON([]byte(req.DefinitionJson))
	if wfErr != nil {
		return nil, status.Errorf(codes.InvalidArgument, "definition_json invalid: %v", wfErr)
	}
	if err := validateStepSkills(def, reg); err != nil {
		return nil, err
	}

	if store == nil {
		return nil, status.Error(codes.FailedPrecondition, "workflow storage not configured")
	}

	tenantUUID, err := uuid.Parse(tenantID)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid tenant_id: %v", err)
	}

	// ── Edit vs. brand-new ────────────────────────────────────────────────────
	// When lineage_id is provided the caller is editing an existing workflow.
	// We inherit status + visibility_kind + visibility_groups from the current
	// approved version so that a published workflow stays published after an
	// owner edit (silent version advance), and a private workflow stays private.
	// Only the brand-new path (empty lineage_id) forces private.
	var (
		lineageID        uuid.UUID
		status_          string
		visibilityKind   string
		visibilityGroups json.RawMessage
		boundAgentID     *uuid.UUID // F9: nil = personal workflow
	)

	if req.LineageId != "" {
		// Edit path: parse + inherit from current version.
		lineageID, err = uuid.Parse(req.LineageId)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid lineage_id: %v", err)
		}
		current, curErr := store.GetCurrent(ctx, tenantID, lineageID)
		if curErr != nil {
			return nil, status.Errorf(codes.NotFound, "workflow lineage not found: %v", curErr)
		}
		status_ = current.Status
		visibilityKind = current.VisibilityKind
		visibilityGroups = current.VisibilityGroups
		// Agent binding is lineage-immutable: an edit inherits the current
		// version's binding and ignores req.AgentId entirely.
		boundAgentID = current.BoundAgentID
	} else {
		// Brand-new path: always private.
		lineageID = uuid.New()
		status_ = "private"
		visibilityKind = "private"
		emptyGroups, _ := json.Marshal([]string{})
		visibilityGroups = json.RawMessage(emptyGroups)
		// Only a brand-new lineage takes the binding from the request. A
		// malformed/empty agent_id is treated as unbound (NULL) rather than an
		// error — same posture as CreateGatewayTask's agent_id handling in
		// gateway_tasks.go; the plan-time skill-boundary check denies later if
		// the id is nonsense.
		if req.AgentId != "" {
			if parsed, parseErr := uuid.Parse(req.AgentId); parseErr == nil {
				boundAgentID = &parsed
			}
		}
	}

	row := db.WorkflowRow{
		LineageID:        lineageID,
		TenantID:         tenantUUID,
		OwnerUserID:      ownerUserID,
		Name:             req.Name,
		Description:      req.Description,
		Status:           status_,
		VisibilityKind:   visibilityKind,
		VisibilityGroups: visibilityGroups,
		Definition:       json.RawMessage(req.DefinitionJson),
		BoundAgentID:     boundAgentID,
	}

	created, err := store.CreateVersion(ctx, tenantID, row)
	if err != nil {
		logger.Error("SaveWorkflow: CreateVersion failed",
			zap.String("tenant_id", tenantID),
			zap.String("owner", ownerUserID),
			zap.Error(err),
		)
		return nil, status.Errorf(codes.Internal, "failed to save workflow")
	}

	if err := emitter.Emit(ctx, &auditv1.AuditEvent{
		EventId:     ids.EventID(),
		TenantId:    tenantID,
		OccurredAt:  timestamppb.New(time.Now().UTC()),
		ActorUserId: ownerUserID,
		EventType:   "aikonos.broker.workflow.save",
		ResourceRef: "aikonos:workflow:" + created.ID.String(),
		Decision:    auditv1.PolicyDecision_ALLOW,
	}); err != nil {
		audit.RecordEmitFailure(ctx, logger, err, "aikonos.broker.workflow.save")
	}

	return &brokerv1.SaveWorkflowResponse{
		WorkflowId: created.ID.String(),
		LineageId:  created.LineageID.String(),
		Version:    int32(created.Version),
	}, nil
}

// Get fetches the definition for a workflow lineage. Was getWorkflowCore.
func Get(
	ctx context.Context,
	tenantID, ownerUserID string,
	req *brokerv1.GetWorkflowRequest,
	store Store,
	logger *zap.Logger,
) (*brokerv1.GetWorkflowResponse, error) {
	if store == nil {
		return nil, status.Error(codes.FailedPrecondition, "workflow storage not configured")
	}

	lineageID, err := uuid.Parse(req.LineageId)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid lineage_id: %v", err)
	}

	// userId for pin resolution: prefer the explicit field, fall back to ownerUserID.
	resolveUser := req.UserId
	if resolveUser == "" {
		resolveUser = ownerUserID
	}

	version, err := store.ResolveVersionForUser(ctx, tenantID, resolveUser, lineageID)
	if err != nil {
		logger.Error("GetWorkflow: ResolveVersionForUser failed",
			zap.String("lineage_id", req.LineageId),
			zap.Error(err),
		)
		return nil, status.Errorf(codes.NotFound, "workflow not found: %v", err)
	}

	row, err := store.GetVersion(ctx, tenantID, lineageID, version)
	if err != nil {
		logger.Error("GetWorkflow: GetVersion failed",
			zap.String("lineage_id", req.LineageId),
			zap.Int("version", version),
			zap.Error(err),
		)
		return nil, status.Errorf(codes.NotFound, "workflow version not found: %v", err)
	}

	boundAgentID := ""
	if row.BoundAgentID != nil {
		boundAgentID = row.BoundAgentID.String()
	}
	return &brokerv1.GetWorkflowResponse{
		DefinitionJson: string(row.Definition),
		Version:        int32(row.Version),
		BoundAgentId:   boundAgentID,
	}, nil
}

// List returns own workflows plus group-visible shared workflows, with a
// per-workflow access state (runnable / greyed_out). Was listWorkflowsCore.
func List(
	ctx context.Context,
	tenantID, ownerUserID string,
	store Store,
	ap AccessPolicy,
	tenantObject string,
	logger *zap.Logger,
	limit int32,
	cursor string,
) (*brokerv1.ListWorkflowsResponse, error) {
	if store == nil {
		return nil, status.Error(codes.FailedPrecondition, "workflow storage not configured")
	}

	// Bound each per-source SQL fetch so a limited request no longer
	// materializes the whole table. Fetch one extra row per source (limit+1) so
	// the in-memory merge below can still tell whether more rows remain (for
	// next_cursor). fetchLimit==0 preserves the legacy unbounded fetch (limit=0).
	// Correctness: each source is ordered ascending by lineage_id, so the
	// smallest N distinct lineages of the own∪shared union after the cursor are
	// guaranteed to sit within each source's first N+1 rows after the cursor —
	// fetching limit+1 per source is provably sufficient for a page of `limit`.
	fetchLimit := 0
	if limit > 0 {
		fetchLimit = int(limit) + 1
	}

	// ── 1. Own workflows ──────────────────────────────────────────────────────
	ownRows, err := store.ListByOwner(ctx, tenantID, ownerUserID, cursor, fetchLimit)
	if err != nil {
		logger.Error("ListWorkflows: ListByOwner failed",
			zap.String("tenant_id", tenantID),
			zap.String("owner", ownerUserID),
			zap.Error(err),
		)
		return nil, status.Errorf(codes.Internal, "failed to list workflows")
	}

	// ── 2. Group-visible shared workflows ────────────────────────────────────
	// sharedUnavailable records that the shared half was requested but could not
	// be resolved (FGA outage on ListObjects, or a ListVisibleShared error): the
	// listing then degrades to own-only and the flag lets the client say so
	// rather than silently hiding shared workflows.
	var sharedRows []*db.WorkflowRow
	var sharedUnavailable bool
	if ap != nil && ap.FGAEnabled() {
		// Resolve the viewer's groups via the "member" relation (not delegatable_member).
		rawGroups, gErr := ap.ListObjects(ctx, "user:"+ownerUserID, "member", "group")
		if gErr != nil {
			// Non-fatal: FGA outage → viewer sees only own workflows (degraded UX,
			// not an error). Consistent with ListDelegatableUsers fail-open posture.
			sharedUnavailable = true
			logger.Warn("ListWorkflows: ListObjects(member,group) failed — shared workflows skipped",
				zap.String("user", ownerUserID),
				zap.Error(gErr),
			)
		} else if len(rawGroups) > 0 {
			// Strip the "group:" prefix that ListObjects returns.
			groups := make([]string, 0, len(rawGroups))
			for _, g := range rawGroups {
				if id, ok := strings.CutPrefix(g, "group:"); ok && id != "" {
					groups = append(groups, id)
				} else if g != "" {
					groups = append(groups, g) // already bare
				}
			}
			if len(groups) > 0 {
				sharedRows, err = store.ListVisibleShared(ctx, tenantID, groups, cursor, fetchLimit)
				if err != nil {
					sharedUnavailable = true
					logger.Warn("ListWorkflows: ListVisibleShared failed — shared workflows skipped",
						zap.String("user", ownerUserID),
						zap.Error(err),
					)
					sharedRows = nil
				}
			}
		}
	}

	// ── 3. Merge + dedup by lineage_id (own takes precedence) ────────────────
	// Track which lineage IDs the owner already owns so shared rows don't
	// duplicate them. Own rows are always included; shared rows are added only
	// when their lineage_id is not already present.
	type entry struct {
		row     *db.WorkflowRow
		isOwner bool
	}
	seen := make(map[string]struct{}, len(ownRows)+len(sharedRows))
	merged := make([]entry, 0, len(ownRows)+len(sharedRows))

	for _, r := range ownRows {
		lid := r.LineageID.String()
		seen[lid] = struct{}{}
		merged = append(merged, entry{row: r, isOwner: true})
	}
	for _, r := range sharedRows {
		lid := r.LineageID.String()
		if _, dup := seen[lid]; dup {
			continue // own row already present
		}
		seen[lid] = struct{}{}
		merged = append(merged, entry{row: r, isOwner: false})
	}

	// ── 3.5. Sort + paginate (F19) ────────────────────────────────────────────
	// The own+shared merge above interleaves two blocks that were each already
	// individually lineage_id-ordered, but not against each other, so a single
	// total order across both is established here:
	// ascending by lineage_id (stable, unique). Cursor is the raw lineage_id of
	// the last item returned — no encoding, mirroring the audit reader's
	// convention (proto/broker.proto QueryAuditRequest / audit/reader.go
	// filterAndPage): a plain domain-value string comparison, so a malformed
	// cursor never errors, it simply yields whatever falls after it lexically
	// (matches the audit reader's actual permissive behavior).
	//
	// The per-source fetches above are now keyset+LIMIT bounded (fetchLimit),
	// so a limit>0 request materializes at most 2·(limit+1) rows here, not the
	// whole table. The sort/cursor-filter/limit below run over that bounded
	// merged slice; the cursor filter is redundant with the SQL keyset but kept
	// as a cheap safety net and to preserve the legacy (limit=0) code path
	// exactly. A limit=0 request stays fully unbounded, matching legacy behavior.
	sort.Slice(merged, func(i, j int) bool {
		return merged[i].row.LineageID.String() < merged[j].row.LineageID.String()
	})
	if cursor != "" {
		var after []entry
		for _, e := range merged {
			if e.row.LineageID.String() > cursor {
				after = append(after, e)
			}
		}
		merged = after
	}
	var nextCursor string
	if limit > 0 && len(merged) > int(limit) {
		nextCursor = merged[limit-1].row.LineageID.String()
		merged = merged[:limit]
	}

	// ── 4. Per-workflow access state ─────────────────────────────────────────
	// Parse requires.skills; CheckFGA(viewer, can_invoke, skill:X) per skill.
	// When FGA is disabled, treat all as runnable (FGA-off allow-all posture).
	//
	// Two-pass to avoid N-rows × M-skills duplicate FGA calls: pass 1 parses
	// each row once and resolves every *distinct* skill across all rows into
	// skillGranted exactly once; pass 2 derives each row's access_state from
	// that map. A CheckFGA error keeps today's fail-closed semantics — the
	// skill is recorded as not-granted in skillGranted, identically to the
	// old per-row "ok = false" on error, so every row needing that skill sees
	// the same denial it would have gotten from its own (now-elided) call.
	requiredSkills := make([][]string, len(merged))
	skillGranted := make(map[string]bool)
	if ap != nil && ap.FGAEnabled() {
		for i, e := range merged {
			def, parseErr := workflow.FromJSON(e.row.Definition)
			if parseErr != nil {
				continue
			}
			requiredSkills[i] = def.Requires.Skills
			for _, skill := range def.Requires.Skills {
				if strings.HasPrefix(skill, "mcp:") {
					continue // mcp: skills are never per-user CheckFGA'd — see pass-2 guard
				}
				if _, resolved := skillGranted[skill]; resolved {
					continue
				}
				ok, checkErr := ap.CheckFGA(ctx, "user:"+ownerUserID, "can_invoke", "skill:"+skill)
				if checkErr != nil {
					// Fail closed: treat as lacking the skill.
					ok = false
				}
				skillGranted[skill] = ok
			}
		}
	}

	// F9: agent-bound workflows carry bound_agent_ok = "may the viewer operate
	// the bound agent" via MayOperateAgent, memoized per distinct agent id per
	// request (same shape as skillGranted above). Unbound (personal) rows are
	// always ok. When FGA is disabled MayOperateAgent returns true, so the memo
	// simply records true for every agent.
	agentOperable := make(map[string]bool)

	items := make([]*brokerv1.WorkflowSummary, 0, len(merged))
	for i, e := range merged {
		accessState := "runnable"
		var missing []string

		// Per-user skill grey-out mirrors the plan-time gate in
		// broker/internal/broker/service_plan.go ("if task.AgentID == nil &&
		// !strings.HasPrefix(step.ToolId, \"mcp:\")"). Two skips:
		//   - Agent-bound rows (BoundAgentID != nil) run under the agent's own
		//     authority (checkAgentSkills), so the viewer's personal can_invoke
		//     grants are irrelevant — runnability is governed by bound_agent_ok
		//     below, not this loop.
		//   - mcp: skills are authorized by the agent's connector can_access grant
		//     at InvokeTool, never a per-user can_invoke; their FGA object
		//     "skill:mcp:<conn>:<tool>" is invalid (id carries colons) and would
		//     fail closed. Skipped here (and in pass 1, which never issues the check).
		if ap != nil && ap.FGAEnabled() && e.row.BoundAgentID == nil {
			for _, skill := range requiredSkills[i] {
				if strings.HasPrefix(skill, "mcp:") {
					continue
				}
				if !skillGranted[skill] {
					missing = append(missing, skill)
				}
			}
			if len(missing) > 0 {
				accessState = "greyed_out"
			}
		}

		boundAgentID := ""
		boundAgentOk := true
		if e.row.BoundAgentID != nil {
			boundAgentID = e.row.BoundAgentID.String()
			ok, memoized := agentOperable[boundAgentID]
			if !memoized {
				ok = MayOperateAgent(ctx, ap, ownerUserID, boundAgentID, tenantObject)
				agentOperable[boundAgentID] = ok
			}
			boundAgentOk = ok
		}

		items = append(items, &brokerv1.WorkflowSummary{
			LineageId:           e.row.LineageID.String(),
			Name:                e.row.Name,
			Version:             int32(e.row.Version),
			VisibilityKind:      e.row.VisibilityKind,
			Status:              e.row.Status,
			AccessState:         accessState,
			MissingRequirements: missing,
			IsOwner:             e.isOwner,
			BoundAgentId:        boundAgentID,
			BoundAgentOk:        boundAgentOk,
		})
	}
	return &brokerv1.ListWorkflowsResponse{
		Items:             items,
		NextCursor:        nextCursor,
		SharedUnavailable: sharedUnavailable,
	}, nil
}

// MayOperateAgent reports whether user may operate agent agentID within the
// tenant. It is the F9 "may-operate-agent" gate shared by List's per-row
// bound_agent_ok and the BeginWorkflowRun RPC (added in a later slice): an
// agent-bound workflow is only runnable by someone who may drive its agent.
//
// The posture mirrors requireAgentOwnerOrAdmin (owner-or-admin) and
// ListMyAgents' FGA-off allow-all fallback:
//   - nil ap or FGA disabled → true (dev allow-all, no gate to enforce).
//   - can_use on agent:<id> → true; a CheckFGA transport error fails closed.
//   - else admin on the tenant → tenant admins may operate any agent; a
//     CheckFGA transport error fails closed.
func MayOperateAgent(ctx context.Context, ap AccessPolicy, user, agentID, tenantObject string) bool {
	if ap == nil || !ap.FGAEnabled() {
		return true
	}
	ok, err := ap.CheckFGA(ctx, "user:"+user, "can_use", "agent:"+agentID)
	if err != nil {
		return false
	}
	if ok {
		return true
	}
	admin, err := ap.CheckFGA(ctx, "user:"+user, "admin", tenantObject)
	if err != nil {
		return false
	}
	return admin
}

// MayAccessWorkflow reports whether user may access (read the definition of and
// run) the lineage represented by row. It is the workflow-dimension gate for
// BeginWorkflowRun. That RPC's MayOperateAgent check authorizes only the AGENT
// dimension — whether the caller may drive the bound agent — and says nothing
// about the caller's access to the workflow itself; without this gate a
// non-owner who may operate the bound agent could run (and thereby disclose)
// another user's private bound workflow. Both gates must pass.
//
// Semantics mirror what List exposes as runnable (own + visible-shared):
//   - own lineage (row.OwnerUserID == user) → true.
//   - a private (any non-"shared") lineage is never accessible to a non-owner,
//     EVEN with FGA disabled: visibility_kind is a stored property, not an FGA
//     relation, so we do not fail open here.
//   - a shared lineage with FGA disabled → false. List populates shared rows
//     only when FGA is enabled (it needs FGA to resolve the viewer's groups),
//     so with FGA off a non-owner never sees a shared workflow via List either;
//     mirroring List is the rule.
//   - a shared lineage with FGA enabled → membership in any visibility group
//     grants access; a CheckFGA transport error fails closed (not-a-member).
func MayAccessWorkflow(ctx context.Context, ap AccessPolicy, user string, row *db.WorkflowRow) bool {
	if row == nil {
		return false
	}
	if row.OwnerUserID == user {
		return true
	}
	// Non-owner: only shared lineages are reachable, and only via FGA group
	// membership. Private (and any non-"shared" kind) is denied outright.
	if row.VisibilityKind != "shared" {
		return false
	}
	if ap == nil || !ap.FGAEnabled() {
		return false
	}
	var groups []string
	if len(row.VisibilityGroups) > 0 {
		// VisibilityGroups is a JSONB array of bare group ids (Publish strips the
		// "group:" prefix before storing). A malformed blob → no groups → deny.
		_ = json.Unmarshal(row.VisibilityGroups, &groups)
	}
	for _, g := range groups {
		if g == "" {
			continue
		}
		ok, err := ap.CheckFGA(ctx, "user:"+user, "member", "group:"+g)
		if err != nil {
			continue // fail closed: treat a CheckFGA error as not-a-member
		}
		if ok {
			return true
		}
	}
	return false
}
