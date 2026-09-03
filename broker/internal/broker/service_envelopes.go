package broker

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/capability"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/ids"
	"github.com/adamdekan/aikonos/broker/internal/policy"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

func (s *BrokerService) SendEnvelope(ctx context.Context, req *brokerv1.SendEnvelopeRequest) (*brokerv1.EnvelopeHandle, error) {
	ctx, span := tracer.Start(ctx, "broker.envelope.route")
	defer span.End()
	traceID := span.SpanContext().TraceID().String()

	switch {
	case req.GetRecipient().GetUserId() != "":
		return s.sendEnvelopeToUser(ctx, req, req.GetRecipient().GetUserId(), traceID)
	case req.GetRecipient().GetGroupId() != "":
		return s.sendEnvelopeToGroup(ctx, req, req.GetRecipient().GetGroupId(), traceID)
	case req.GetRecipient().GetRole() != "":
		return nil, status.Error(codes.Unimplemented, "role recipient targeting is not supported")
	default:
		return nil, status.Error(codes.InvalidArgument, "recipient must specify user_id, group_id, or role")
	}
}

// sendEnvelopeToUser delivers one envelope to a single recipient user. It is
// the canonical per-user delivery path; both the user_id arm and the group
// fan-out loop call this helper so the logic lives in exactly one place.
func (s *BrokerService) sendEnvelopeToUser(ctx context.Context, req *brokerv1.SendEnvelopeRequest, recipient, traceID string) (*brokerv1.EnvelopeHandle, error) {
	// Self-delegation is denied by design. A member is in their own group's
	// delegatable_member set, so the delegation-group fallback below would
	// otherwise allow from==to — this guard is load-bearing, not incidental.
	// EqualFold: OIDC subjects are lowercased upstream, but defense-in-depth
	// against a mixed-case subject bypassing the byte-exact check.
	if strings.EqualFold(req.FromUserId, recipient) {
		s.emitEnvelopeAudit(ctx, traceID, req.TenantId, req.FromUserId, "aikonos.broker.envelope.denied", auditv1.PolicyDecision_DENY, "")
		return nil, status.Error(codes.PermissionDenied, "self-delegation not permitted")
	}
	tenantUUID, err := uuid.Parse(req.TenantId)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid tenant_id: %v", err)
	}

	deleg := req.GetDelegation()
	var attenuated []string
	var maxCost int64
	if deleg != nil {
		attenuated = deleg.AttenuatedScopes
		maxCost = deleg.MaxCostUnits
	}

	// Resolve the sender's real delegable scopes once — the result feeds both
	// the OPA attenuation check below and the Biscuit mint in
	// buildDelegationToken, so the authority checked and the authority minted
	// can never drift apart from two independent resolutions.
	senderScopes, scopeSource, err := s.senderDelegableScopes(ctx, req.FromUserId)
	if err != nil {
		s.deps.Logger.Error("SendEnvelope: delegation scope resolution failed", zap.Error(err))
		s.emitEnvelopeAudit(ctx, traceID, req.TenantId, req.FromUserId, "aikonos.broker.envelope.denied", auditv1.PolicyDecision_DENY, "")
		return nil, status.Error(codes.Internal, "delegation scope resolution failed")
	}

	// ReBAC: sender must be allowed to delegate to recipient (OpenFGA stub allows).
	fgaOK, _ := s.deps.Policy.CheckFGA(ctx, "user:"+req.FromUserId, "can_delegate_to_user", "user:"+recipient)
	// Delegation-group fallback: when no direct peer/manager edge grants the
	// send, allow it if sender and recipient share a delegatable group. Runs
	// only on the deny path, so the peer/manager case costs no extra FGA calls.
	if !fgaOK {
		fgaOK = s.sharesDelegatableGroup(ctx, req.FromUserId, recipient)
	}
	fgaDecision := "allow"
	if !fgaOK {
		fgaDecision = "deny"
	}

	dec, err := s.deps.Policy.DecideEnvelopeSend(ctx, maxCost, attenuated, policy.EnvelopeSendContext{
		FGASendDecision:     fgaDecision,
		Depth:               1,                // top-level user send; agent chains carry depth later
		EffectClass:         "write_external", // EnvelopeTask carries no effect_class yet — conservative default
		SenderScopes:        senderScopes,
		SenderRemainingCost: defaultDelegationBudget,
		Relationship:        "peer", // real relationship comes from OpenFGA (deferred)
	})
	if err != nil {
		s.deps.Logger.Error("SendEnvelope: policy error", zap.Error(err))
		return nil, status.Errorf(codes.Internal, "policy evaluation failed")
	}
	if !dec.Allow {
		reasons := append(append([]string{}, dec.ScopeViolations...), dec.DenyReasons...)
		s.emitEnvelopeAudit(ctx, traceID, req.TenantId, req.FromUserId, "aikonos.broker.envelope.denied", auditv1.PolicyDecision_DENY, "")
		return nil, status.Errorf(codes.PermissionDenied, "delegation denied: %s", joinReasons(reasons))
	}

	// Mint/attenuate the recipient's capability token. The OPA check above
	// already proved attenuated ⊆ sender scopes; the Biscuit makes that
	// attenuation cryptographically enforceable downstream (at InvokeTool).
	capToken, err := s.buildDelegationToken(recipient, deleg.GetCapabilityToken(), attenuated, senderScopes)
	if err != nil {
		s.deps.Logger.Error("SendEnvelope: capability token build failed", zap.Error(err))
		return nil, status.Errorf(codes.Internal, "failed to build capability token")
	}

	initial := db.EnvelopeDelivered
	if dec.AutoAccept {
		initial = db.EnvelopeAccepted
	}

	env := &db.Envelope{
		TenantID:   tenantUUID,
		FromUserID: req.FromUserId,
		ToTarget:   map[string]any{"type": "user", "id": recipient},
		TaskSpec: map[string]any{
			"intent":          req.GetTask().GetIntent(),
			"payload_ref":     req.GetTask().GetPayloadRef(),
			"required_skills": req.GetTask().GetRequiredSkills(),
			"priority":        req.GetTask().GetPriority(),
		},
		DelegationSpec: map[string]any{
			"attenuated_scopes": attenuated,
			"max_cost_units":    maxCost,
			"capability_token":  capToken, // base64 Biscuit, minted+attenuated by the broker
		},
		Depth:     1,
		TraceID:   traceID,
		ExpiresAt: time.Now().UTC().Add(72 * time.Hour),
	}
	envID, err := s.deps.Tasks.CreateEnvelope(ctx, env, initial)
	if err != nil {
		s.deps.Logger.Error("SendEnvelope: persist failed", zap.Error(err))
		return nil, status.Errorf(codes.Internal, "failed to create envelope")
	}

	// Auto-accepted delegations immediately spawn the recipient's task.
	if dec.AutoAccept {
		if _, err := s.spawnDelegatedTask(ctx, tenantUUID, recipient, req.GetTask().GetIntent(), maxCost, traceID); err != nil {
			s.deps.Logger.Error("SendEnvelope: auto-accept task spawn failed", zap.Error(err))
		}
	}

	s.deps.Logger.Info("SendEnvelope",
		zap.String("envelope_id", envID.String()),
		zap.String("from", req.FromUserId),
		zap.String("to", recipient),
		zap.String("state", string(initial)),
		zap.Bool("auto_accept", dec.AutoAccept),
	)
	s.emitEnvelopeAudit(ctx, traceID, req.TenantId, req.FromUserId, "aikonos.broker.envelope.sent", auditv1.PolicyDecision_ALLOW, scopeSource)

	publishInboxEvent(ctx, s.deps, req.TenantId, recipient, envID.String(), "ENVELOPE_DELIVERED", map[string]any{
		"from":        req.FromUserId,
		"intent":      req.GetTask().GetIntent(),
		"state":       string(initial),
		"auto_accept": dec.AutoAccept,
	})

	return &brokerv1.EnvelopeHandle{EnvelopeId: envID.String(), TraceId: traceID}, nil
}

// sendEnvelopeToGroup fans the envelope out to every delegatable_member of
// groupID, excluding the sender. The sender must themselves be a
// delegatable_member of the target group (verified before any delivery).
// Per-member delivery uses sendEnvelopeToUser; a member that fails authz or
// persistence is logged and skipped (best-effort). Returns an EnvelopeHandle
// with an empty EnvelopeId if ≥1 member was delivered to, else PermissionDenied.
func (s *BrokerService) sendEnvelopeToGroup(ctx context.Context, req *brokerv1.SendEnvelopeRequest, groupID, traceID string) (*brokerv1.EnvelopeHandle, error) {
	// Verify the sender is a delegatable_member of the target group before fan-out.
	// ListObjects returns the groups the caller belongs to; the target group must be
	// in that set. Fails closed: FGA error → PermissionDenied.
	senderGroups, err := s.deps.Policy.ListObjects(ctx, "user:"+req.FromUserId, "delegatable_member", "group")
	if err != nil {
		s.deps.Logger.Warn("sendEnvelopeToGroup: ListObjects failed for sender", zap.String("from", req.FromUserId), zap.Error(err))
		return nil, status.Errorf(codes.PermissionDenied, "delegation denied: could not verify group membership")
	}
	senderInGroup := false
	for _, g := range senderGroups {
		if g == groupID {
			senderInGroup = true
			break
		}
	}
	if !senderInGroup {
		return nil, status.Errorf(codes.PermissionDenied, "delegation denied: sender is not a member of %s", groupID)
	}

	// Resolve the group's members, excluding the sender (self-exclusion invariant).
	members, err := s.groupMembersExcluding(ctx, groupID, req.FromUserId)
	if err != nil {
		s.deps.Logger.Warn("sendEnvelopeToGroup: groupMembersExcluding failed", zap.String("group", groupID), zap.Error(err))
		return nil, status.Errorf(codes.PermissionDenied, "delegation denied: could not resolve group members")
	}

	delivered := 0
	for _, member := range members {
		if _, err := s.sendEnvelopeToUser(ctx, req, member, traceID); err != nil {
			// Log the skip; do not abort the loop.
			s.deps.Logger.Info("sendEnvelopeToGroup: skipping member",
				zap.String("member", member),
				zap.String("group", groupID),
				zap.Error(err),
			)
			continue
		}
		delivered++
	}

	if delivered == 0 {
		return nil, status.Errorf(codes.PermissionDenied, "delegation denied: no deliverable group members")
	}
	return &brokerv1.EnvelopeHandle{EnvelopeId: "", TraceId: traceID}, nil
}

// ListInboxEnvelopes returns delegation envelopes addressed to the caller;
// pass include_accepted=true to include already-responded envelopes.
func (s *BrokerService) ListInboxEnvelopes(ctx context.Context, req *brokerv1.ListInboxEnvelopesRequest) (*brokerv1.ListInboxEnvelopesResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.envelope.inbox")
	defer span.End()

	envs, err := s.deps.Tasks.ListInbox(ctx, req.TenantId, req.UserId, req.IncludeAccepted)
	if err != nil {
		s.deps.Logger.Error("ListInboxEnvelopes: query failed", zap.Error(err))
		return nil, status.Errorf(codes.Internal, "failed to list inbox")
	}

	envs, nextCursor, err := paginateInbox(envs, req.Limit, req.Cursor)
	if err != nil {
		return nil, err
	}

	names := s.userDisplayNames(ctx, req.TenantId)
	out := make([]*brokerv1.InboxEnvelope, 0, len(envs))
	for _, e := range envs {
		out = append(out, &brokerv1.InboxEnvelope{
			EnvelopeId:      e.EnvelopeID.String(),
			FromUserId:      e.FromUserID,
			FromDisplayName: names[e.FromUserID],
			Task:            envelopeTaskFromSpec(e.TaskSpec),
			ReceivedAt:      timestamppb.New(e.CreatedAt),
			Status:          strings.ToLower(string(e.State)),
		})
	}
	return &brokerv1.ListInboxEnvelopesResponse{Envelopes: out, NextCursor: nextCursor}, nil
}

// paginateInbox is the pure, unit-tested helper that pages an already-fetched
// inbox slice (F19). ListInbox is already ORDER BY created_at DESC LIMIT 100
// (unchanged — limit=0 reproduces today's 100-row cap exactly); this pages
// that already-fetched slice in memory, the same technique the audit reader's
// filterAndPage (broker/internal/audit/reader.go) uses on its own fetched
// events.
//
// The cursor is a composite keyset: "<created_at RFC3339Nano>|<envelope_id>".
// created_at alone is not a unique key — two envelopes can share an identical
// timestamp (unreachable with today's per-row inserts, but a latent gap
// otherwise) — so envelope_id breaks ties deterministically. The slice is
// re-sorted here (created_at DESC, then envelope_id DESC as tiebreak) before
// paging: the DB query only orders by created_at, so the secondary order
// exists solely at this layer. Continuation keeps rows strictly after the
// cursor in that same order: created_at < cursor_time, or created_at ==
// cursor_time and envelope_id < cursor_id. A typed value, so, deliberately
// deviating from the audit reader's laissez-faire acceptance of any string, a
// non-empty cursor that fails to parse (including the pre-composite bare
// timestamp) is rejected with InvalidArgument rather than silently
// mis-filtering.
func paginateInbox(envs []*db.Envelope, limit int32, cursor string) ([]*db.Envelope, string, error) {
	sorted := make([]*db.Envelope, len(envs))
	copy(sorted, envs)
	sort.SliceStable(sorted, func(i, j int) bool {
		if !sorted[i].CreatedAt.Equal(sorted[j].CreatedAt) {
			return sorted[i].CreatedAt.After(sorted[j].CreatedAt)
		}
		return sorted[i].EnvelopeID.String() > sorted[j].EnvelopeID.String()
	})
	envs = sorted

	if cursor != "" {
		cursorTime, cursorID, cerr := parseInboxCursor(cursor)
		if cerr != nil {
			return nil, "", status.Errorf(codes.InvalidArgument, "invalid cursor: %v", cerr)
		}
		var after []*db.Envelope
		for _, e := range envs {
			if e.CreatedAt.Before(cursorTime) || (e.CreatedAt.Equal(cursorTime) && e.EnvelopeID.String() < cursorID) {
				after = append(after, e)
			}
		}
		envs = after
	}

	var nextCursor string
	if limit > 0 && len(envs) > int(limit) {
		nextCursor = encodeInboxCursor(envs[limit-1])
		envs = envs[:limit]
	}
	return envs, nextCursor, nil
}

// encodeInboxCursor renders the composite keyset cursor for e.
func encodeInboxCursor(e *db.Envelope) string {
	return e.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + e.EnvelopeID.String()
}

// parseInboxCursor parses the "<created_at>|<envelope_id>" composite cursor.
// Any deviation from that shape — including the pre-composite bare-timestamp
// format — is an error; no production cursors predate this format.
func parseInboxCursor(cursor string) (time.Time, string, error) {
	ts, id, ok := strings.Cut(cursor, "|")
	if !ok || id == "" {
		return time.Time{}, "", fmt.Errorf("expected format <created_at>|<envelope_id>")
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return time.Time{}, "", err
	}
	return t, id, nil
}

func (s *BrokerService) RespondToEnvelope(ctx context.Context, req *brokerv1.RespondToEnvelopeRequest) (*brokerv1.RespondToEnvelopeResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.envelope.respond")
	defer span.End()
	traceID := span.SpanContext().TraceID().String()

	// F21 precedent (identity.go's GetTaskState/CancelTask): derive the acting
	// tenant/user from the authenticated identity rather than trusting the
	// request fields — a forged req.UserId must never let a caller respond on
	// another user's behalf.
	tenant, user, err := s.callerIdentity(ctx, req.TenantId, req.UserId)
	if err != nil {
		return nil, err
	}

	env, err := s.deps.Tasks.GetEnvelope(ctx, tenant, req.EnvelopeId)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, status.Errorf(codes.NotFound, "envelope %s not found", req.EnvelopeId)
		}
		return nil, status.Errorf(codes.Internal, "failed to load envelope")
	}
	// A skill_transfer envelope's only accept path is AcceptSkillTransfer,
	// which re-validates the capability and installs from staging — ordinary
	// RespondToEnvelope must never spawn a delegated task for one.
	if kind, _ := env.TaskSpec["kind"].(string); kind == skillTransferEnvelopeKind {
		return nil, status.Error(codes.FailedPrecondition, "a skill transfer envelope must be accepted via AcceptSkillTransfer")
	}

	newState, err := s.deps.Tasks.RespondEnvelope(ctx, tenant, req.EnvelopeId, user, req.Accepted)
	if err != nil {
		s.deps.Logger.Error("RespondToEnvelope: respond failed", zap.Error(err))
		return nil, status.Errorf(codes.FailedPrecondition, "%v", err)
	}

	eventType := "aikonos.broker.envelope.rejected"
	decision := auditv1.PolicyDecision_DENY
	if req.Accepted {
		eventType = "aikonos.broker.envelope.accepted"
		decision = auditv1.PolicyDecision_ALLOW
		// Accepting a delegation spawns the recipient's task to do the work.
		intent, _ := env.TaskSpec["intent"].(string)
		if _, err := s.spawnDelegatedTask(ctx, env.TenantID, user, intent, delegationMaxCost(env), traceID); err != nil {
			s.deps.Logger.Error("RespondToEnvelope: task spawn failed", zap.Error(err))
		}
	}

	s.deps.Logger.Info("RespondToEnvelope",
		zap.String("envelope_id", req.EnvelopeId),
		zap.String("user", user),
		zap.Bool("accepted", req.Accepted),
		zap.String("state", string(newState)),
	)
	s.emitEnvelopeAudit(ctx, traceID, tenant, user, eventType, decision, "")

	return &brokerv1.RespondToEnvelopeResponse{Success: true}, nil
}

// buildDelegationToken produces the base64 Biscuit capability stored on an
// envelope. When the caller forwards a parent token (an agent re-delegating its
// own capability) it is attenuated in place; otherwise the broker mints a fresh
// root token over senderScopes (the caller's already-resolved delegable
// scopes — SendEnvelope resolves once via senderDelegableScopes and threads
// the result here so the mint and the OPA attenuation check share one
// resolution) and attenuates it to the delegated subset. Returns "" when
// there is no capability minter or nothing to delegate.
func (s *BrokerService) buildDelegationToken(recipient, parentB64 string, attenuated, senderScopes []string) (string, error) {
	if s.deps.Capability == nil || len(attenuated) == 0 {
		return "", nil
	}

	var raw []byte
	var err error
	if parentB64 != "" {
		parent, derr := base64.StdEncoding.DecodeString(parentB64)
		if derr != nil {
			return "", fmt.Errorf("decode parent capability token: %w", derr)
		}
		raw, err = capability.Attenuate(parent, attenuated)
	} else {
		var minted []byte
		if minted, err = s.deps.Capability.Mint(recipient, "", senderScopes); err == nil {
			raw, err = capability.Attenuate(minted, attenuated)
		}
	}
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}

// spawnDelegatedTask creates a task owned by the recipient for an accepted
// delegation, using the envelope intent as the prompt.
func (s *BrokerService) spawnDelegatedTask(ctx context.Context, tenantID uuid.UUID, owner, intent string, costBudget int64, traceID string) (uuid.UUID, error) {
	if costBudget <= 0 {
		costBudget = 1000
	}
	t := &db.Task{
		TaskID:      uuid.New(),
		TenantID:    tenantID,
		OwnerUserID: owner,
		Prompt:      intent,
		CostBudget:  costBudget,
		TraceID:     traceID,
	}
	if err := s.deps.Tasks.Create(ctx, t); err != nil {
		return uuid.Nil, err
	}
	s.writeTaskRelations(ctx, owner, t.TaskID.String())
	s.deps.Logger.Info("delegated task spawned",
		zap.String("task_id", t.TaskID.String()),
		zap.String("owner", owner),
	)
	return t.TaskID, nil
}

// DismissEnvelope transitions a PENDING/DELIVERED envelope to DISMISSED.
// No task is spawned and no rejection is written — the item clears from the
// inbox with a neutral audit event.
func (s *BrokerService) DismissEnvelope(ctx context.Context, req *brokerv1.DismissEnvelopeRequest) (*brokerv1.DismissEnvelopeResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.envelope.dismiss")
	defer span.End()
	traceID := span.SpanContext().TraceID().String()

	// F21 precedent (identity.go's GetTaskState/CancelTask): derive the acting
	// tenant/user from the authenticated identity rather than trusting the
	// request fields — a forged req.UserId must never let a caller dismiss
	// another user's envelope.
	tenant, user, err := s.callerIdentity(ctx, req.TenantId, req.UserId)
	if err != nil {
		return nil, err
	}

	store := s.deps.Tasks
	env, err := store.GetEnvelope(ctx, tenant, req.EnvelopeId)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, status.Errorf(codes.NotFound, "envelope %s not found", req.EnvelopeId)
		}
		return nil, status.Errorf(codes.Internal, "failed to load envelope")
	}

	if _, err := store.DismissEnvelope(ctx, tenant, req.EnvelopeId, user); err != nil {
		s.deps.Logger.Error("DismissEnvelope: dismiss failed", zap.Error(err))
		return nil, status.Errorf(codes.FailedPrecondition, "%v", err)
	}
	// A dismissed skill_transfer envelope's staging snapshot is GC'd once no
	// other envelope still references it. Locked: a concurrent AcceptSkillTransfer on a
	// sibling envelope sharing this staging segment must never interleave its
	// Install with this GC.
	if kind, _ := env.TaskSpec["kind"].(string); kind == skillTransferEnvelopeKind {
		if payloadRef, _ := env.TaskSpec["payload_ref"].(string); payloadRef != "" {
			unlock := lockStagingSegment(tenant, payloadRef)
			defer unlock()
			s.gcStagingIfUnreferenced(ctx, tenant, payloadRef)
		}
	}

	s.deps.Logger.Info("DismissEnvelope",
		zap.String("envelope_id", req.EnvelopeId),
		zap.String("user", user),
	)
	s.emitEnvelopeAudit(ctx, traceID, tenant, user, "aikonos.broker.envelope.dismissed", auditv1.PolicyDecision_POLICY_DECISION_UNSPECIFIED, "")

	return &brokerv1.DismissEnvelopeResponse{Success: true}, nil
}

// sharesDelegatableGroup reports whether sender and recipient are both members
// of at least one delegatable group (the DL1 delegation-cohort path). Fails
// closed: any FGA error denies (returns false).
func (s *BrokerService) sharesDelegatableGroup(ctx context.Context, from, to string) bool {
	fromGroups, err := s.deps.Policy.ListObjects(ctx, "user:"+from, "delegatable_member", "group")
	if err != nil {
		return false
	}
	toGroups, err := s.deps.Policy.ListObjects(ctx, "user:"+to, "delegatable_member", "group")
	if err != nil {
		return false
	}
	return sharedAny(fromGroups, toGroups)
}

// sharedAny reports whether the two slices share at least one element.
func sharedAny(a, b []string) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	seen := make(map[string]struct{}, len(a))
	for _, v := range a {
		seen[v] = struct{}{}
	}
	for _, v := range b {
		if _, ok := seen[v]; ok {
			return true
		}
	}
	return false
}

// emitEnvelopeAudit emits the envelope-lifecycle audit event. scopeSource is
// only meaningful for the "sent" event: it carries senderDelegableScopes'
// source marker ("fga-derived" or "dev-stub") so an operator reading the
// audit trail can tell whether the delegated scopes were grounded in a real
// OpenFGA grant or the dev-stub fallback. Other event types pass "".
func (s *BrokerService) emitEnvelopeAudit(ctx context.Context, traceID, tenantID, actor, eventType string, decision auditv1.PolicyDecision, scopeSource string) {
	ev := &auditv1.AuditEvent{
		EventId:     ids.EventID(),
		TraceId:     traceID,
		TenantId:    tenantID,
		OccurredAt:  timestampNow(),
		ActorUserId: actor,
		EventType:   eventType,
		Decision:    decision,
	}
	// F24: AuditEvent has no dedicated field for the scope-derivation source;
	// Decision is a closed PolicyDecision enum (not a string) and its exact
	// value is filtered on elsewhere (broker/internal/audit/reader.go), so the
	// free-form Context Struct — already used this way by other emit sites in
	// this package (agents.go, network.go, files.go, mcp_south.go) — is the
	// only field that fits without a proto shape change.
	if eventType == "aikonos.broker.envelope.sent" && scopeSource != "" {
		if c, err := structpb.NewStruct(map[string]any{"delegation_scopes": scopeSource}); err == nil {
			ev.Context = c
		}
	}
	if err := s.deps.Audit.Emit(ctx, ev); err != nil {
		audit.RecordEmitFailure(ctx, s.deps.Logger, err, eventType)
	}
}
