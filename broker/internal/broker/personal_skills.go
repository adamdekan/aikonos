package broker

// personal_skills.go — the north (OIDC) personal-skill management + transfer
// RPCs: ListPersonalSkills, DeletePersonalSkill, SendSkillTransfer,
// GetSkillTransferPreview, AcceptSkillTransfer. CP2's personal_skills_south.go covers the SPIFFE-gated
// session-build twins; this file is the OIDC-bearer management surface.
//
// Every handler chokepoints on personalSkillsCallerNorth: callerIdentity then
// CheckFGA(user, can_invoke, skill:personal-skills), PermissionDenied when
// ungranted — mirroring service_workflows.go's checkWorkflowSkillNorth
// exactly, including the FGA-disabled dev-mode allow-all posture.
//
// Transfer is a snapshot copy, never a live reference: SendSkillTransfer
// copies the sender's Skills/<name>/ into a shared xfer-<uuid> staging
// segment once per send (even for a group fan-out — every recipient's
// envelope points at the same snapshot); AcceptSkillTransfer copies staging
// into the recipient's own Skills/. The recipient's informed accept (full
// body + injection flags in the preview) is the sole authority gate — no
// capability is inherited from the sender.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/ids"
	"github.com/adamdekan/aikonos/broker/internal/personalskill"
	"github.com/adamdekan/aikonos/broker/internal/toolproxy"
	"github.com/adamdekan/aikonos/broker/internal/workspacefs"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

const (
	// skillTransferEnvelopeKind is the EnvelopeTask.kind value marking a
	// skill-transfer envelope. Shared with service_envelopes.go's
	// RespondToEnvelope guard and DismissEnvelope GC hook.
	skillTransferEnvelopeKind = "skill_transfer"

	skillTransferSentEvent     = "aikonos.skill.transfer.sent"
	skillTransferAcceptedEvent = "aikonos.skill.transfer.accepted"
	personalSkillDeletedEvent  = "aikonos.skill.deleted"

	// skillTransferExpiry mirrors SendEnvelope's ordinary delegation expiry —
	// no dedicated transfer TTL is called for in the spec.
	skillTransferExpiry = 72 * time.Hour
)

// ── FGA skill gate ────────────────────────────────────────────────────────────

// checkPersonalSkillsNorth enforces the skill:personal-skills FGA gate for the
// caller. Mirrors checkWorkflowSkillNorth exactly: FGA-disabled dev mode
// allows, a transport error fails closed, an ungranted user is denied.
func (s *BrokerService) checkPersonalSkillsNorth(ctx context.Context, user string) error {
	p := s.deps.Policy
	if p == nil || !p.FGAEnabled() {
		return nil
	}
	granted, err := p.CheckFGA(ctx, "user:"+user, "can_invoke", "skill:"+personalSkillsSkill)
	if err != nil {
		s.deps.Logger.Warn("checkPersonalSkillsNorth: CheckFGA error — failing closed",
			zap.String("user", user), zap.Error(err))
		return status.Error(codes.PermissionDenied, "skill:personal-skills check failed")
	}
	if !granted {
		return status.Error(codes.PermissionDenied, "skill:personal-skills not granted")
	}
	return nil
}

// personalSkillsCallerNorth is the shared preamble every handler in this file
// runs first: resolve+bind identity, gate the capability, and require a
// configured workspace (transfer/management have no meaning without one).
func (s *BrokerService) personalSkillsCallerNorth(ctx context.Context, tenantID, userID string) (tenant, user string, err error) {
	tenant, user, err = s.callerIdentity(ctx, tenantID, userID)
	if err != nil {
		return "", "", err
	}
	if err := s.checkPersonalSkillsNorth(ctx, user); err != nil {
		return "", "", err
	}
	if s.deps.Workspace.Local == nil {
		return "", "", status.Error(codes.FailedPrecondition, "workspace storage is not configured")
	}
	return tenant, user, nil
}

// personalSkillsGrantedOther checks a THIRD party's (a transfer recipient's,
// not the caller's) skill:personal-skills grant. Distinct from
// checkPersonalSkillsNorth because the two call sites need different failure
// handling: a direct-send failure is fail-loud (FailedPrecondition), while a
// group-send failure routes the member into skipped_user_ids instead of
// aborting the whole send. FGA-disabled dev mode allows, matching every other
// capability skill's posture.
func (s *BrokerService) personalSkillsGrantedOther(ctx context.Context, user string) (bool, error) {
	p := s.deps.Policy
	if p == nil {
		return false, fmt.Errorf("policy engine not configured")
	}
	if !p.FGAEnabled() {
		return true, nil
	}
	return p.CheckFGA(ctx, "user:"+user, "can_invoke", "skill:"+personalSkillsSkill)
}

// ── ListPersonalSkills / DeletePersonalSkill ─────────────────────────────────

// ListPersonalSkills returns the caller's own Skills/*/SKILL.md rows
// (backing the Skills view's "My skills" section), each carrying the folder's
// total content size in addition to the south catalog fields.
func (s *BrokerService) ListPersonalSkills(ctx context.Context, req *brokerv1.ListPersonalSkillsRequest) (*brokerv1.ListPersonalSkillsResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.personalskill.list")
	defer span.End()

	tenant, user, err := s.personalSkillsCallerNorth(ctx, req.GetTenantId(), req.GetUserId())
	if err != nil {
		return nil, err
	}

	be := s.deps.Workspace.Local
	entries, err := personalskill.Scan(ctx, be, tenant, user)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list personal skills: %v", err)
	}
	out := &brokerv1.ListPersonalSkillsResponse{}
	for _, e := range entries {
		out.Skills = append(out.Skills, &brokerv1.PersonalSkillListEntry{
			Name:                   e.Name,
			Description:            e.Description,
			Keywords:               e.Keywords,
			AllowedTools:           e.AllowedTools,
			DisableModelInvocation: e.DisableModelInvocation,
			Valid:                  e.Valid,
			Warning:                e.Warning,
			SizeBytes:              personalSkillFolderSize(ctx, be, tenant, user, e.Name),
		})
	}
	return out, nil
}

// personalSkillFolderSize sums one skill folder's file sizes. Never fails the
// listing: a per-skill listing error just yields a 0 size for that row.
func personalSkillFolderSize(ctx context.Context, be workspacefs.Backend, tenant, user, name string) int64 {
	infos, err := be.ListDir(ctx, tenant, user, personalskill.SkillDir(name), true)
	if err != nil {
		return 0
	}
	var total int64
	for _, fi := range infos {
		if !fi.IsDir {
			total += fi.Size
		}
	}
	return total
}

// DeletePersonalSkill recursively deletes the caller's own Skills/<name>/.
// Owner-only by construction: the delete runs against the caller's own
// workspace segment, never a request-supplied user.
func (s *BrokerService) DeletePersonalSkill(ctx context.Context, req *brokerv1.DeletePersonalSkillRequest) (*brokerv1.DeletePersonalSkillResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.personalskill.delete")
	defer span.End()

	tenant, user, err := s.personalSkillsCallerNorth(ctx, req.GetTenantId(), req.GetUserId())
	if err != nil {
		return nil, err
	}
	name := req.GetName()
	if !personalskill.ValidSkillName(name) {
		return nil, status.Errorf(codes.InvalidArgument, "invalid skill name %q", name)
	}
	if err := personalskill.DeleteSkill(ctx, s.deps.Workspace.Local, tenant, user, name); err != nil {
		return nil, fileError("delete personal skill", err)
	}
	s.emitPersonalSkillEvent(ctx, tenant, user, personalSkillDeletedEvent, name, nil)
	return &brokerv1.DeletePersonalSkillResponse{}, nil
}

// ── SendSkillTransfer ─────────────────────────────────────────────────────────

// SendSkillTransfer snapshots the caller's Skills/<name>/ once into a shared
// staging segment, then fans an envelope out to every eligible recipient
//.
func (s *BrokerService) SendSkillTransfer(ctx context.Context, req *brokerv1.SendSkillTransferRequest) (*brokerv1.SendSkillTransferResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.personalskill.transfer.send")
	defer span.End()
	traceID := span.SpanContext().TraceID().String()

	tenant, sender, err := s.personalSkillsCallerNorth(ctx, req.GetTenantId(), req.GetUserId())
	if err != nil {
		return nil, err
	}

	name := req.GetName()
	if !personalskill.ValidSkillName(name) {
		return nil, status.Errorf(codes.InvalidArgument, "invalid skill name %q", name)
	}
	be := s.deps.Workspace.Local
	if !senderOwnsValidPersonalSkill(ctx, be, tenant, sender, name) {
		return nil, status.Errorf(codes.FailedPrecondition, "no valid personal skill named %q", name)
	}

	recipientUser := req.GetRecipient().GetUserId()
	recipientGroup := req.GetRecipient().GetGroupId()
	switch {
	case req.GetRecipient().GetRole() != "":
		return nil, status.Error(codes.Unimplemented, "role recipient targeting is not supported")
	case recipientUser == "" && recipientGroup == "":
		return nil, status.Error(codes.InvalidArgument, "recipient must specify user_id or group_id")
	case recipientUser != "" && strings.EqualFold(recipientUser, sender):
		return nil, status.Error(codes.InvalidArgument, "cannot transfer a skill to yourself")
	}

	tenantUUID, err := uuid.Parse(tenant)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid tenant_id: %v", err)
	}

	// Snapshot once, shared by every fan-out envelope (spec step 2-3): a
	// group send must not re-read/re-scan the sender's folder per recipient.
	stagingSeg := "xfer-" + uuid.New().String()
	result, err := personalskill.Snapshot(ctx, be, tenant, sender, name, stagingSeg, toolproxy.ScanForInjection)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "snapshot personal skill: %v", err)
	}

	var recipients, skipped []string
	switch {
	case recipientUser != "":
		granted, gerr := s.personalSkillsGrantedOther(ctx, recipientUser)
		if gerr != nil || !granted {
			s.gcStagingSegment(ctx, be, tenant, stagingSeg)
			return nil, status.Errorf(codes.FailedPrecondition, "recipient %q does not hold skill:personal-skills", recipientUser)
		}
		recipients = []string{recipientUser}
	case recipientGroup != "":
		// Sender must be a delegatable_member of the target group before any
		// fan-out — mirrors sendEnvelopeToGroup's gate (service_envelopes.go)
		// exactly, so a capability holder can't snapshot-fan-out to a group
		// they don't belong to.
		senderGroups, serr := s.deps.Policy.ListObjects(ctx, "user:"+sender, "delegatable_member", "group")
		if serr != nil {
			s.deps.Logger.Warn("SendSkillTransfer: ListObjects failed for sender", zap.String("from", sender), zap.Error(serr))
			s.gcStagingSegment(ctx, be, tenant, stagingSeg)
			return nil, status.Error(codes.PermissionDenied, "could not verify group membership")
		}
		senderInGroup := false
		for _, g := range senderGroups {
			if g == recipientGroup {
				senderInGroup = true
				break
			}
		}
		if !senderInGroup {
			s.gcStagingSegment(ctx, be, tenant, stagingSeg)
			return nil, status.Errorf(codes.PermissionDenied, "sender is not a member of %s", recipientGroup)
		}

		members, merr := s.groupMembersExcluding(ctx, recipientGroup, sender)
		if merr != nil {
			s.gcStagingSegment(ctx, be, tenant, stagingSeg)
			return nil, status.Errorf(codes.FailedPrecondition, "could not resolve group members: %v", merr)
		}
		sort.Strings(members)
		for _, m := range members {
			granted, gerr := s.personalSkillsGrantedOther(ctx, m)
			if gerr == nil && granted {
				recipients = append(recipients, m)
			} else {
				skipped = append(skipped, m)
			}
		}
		if len(recipients) == 0 {
			s.gcStagingSegment(ctx, be, tenant, stagingSeg)
			return nil, status.Error(codes.FailedPrecondition, "no group members hold skill:personal-skills")
		}
	}

	intent := fmt.Sprintf("Skill transfer: %s", name)
	var envIDs []string
	for _, recipient := range recipients {
		env := &db.Envelope{
			TenantID:   tenantUUID,
			FromUserID: sender,
			ToTarget:   map[string]any{"type": "user", "id": recipient},
			TaskSpec: map[string]any{
				"intent":       intent,
				"payload_ref":  stagingSeg,
				"kind":         skillTransferEnvelopeKind,
				"skill_name":   name,
				"content_hash": result.ContentHash,
				"flags":        result.Flags,
				"file_count":   len(result.Manifest),
				"total_bytes":  manifestTotalBytes(result.Manifest),
			},
			Depth:     1,
			TraceID:   traceID,
			ExpiresAt: time.Now().UTC().Add(skillTransferExpiry),
		}
		id, cerr := s.deps.Tasks.CreateEnvelope(ctx, env, db.EnvelopeDelivered)
		if cerr != nil {
			s.deps.Logger.Error("SendSkillTransfer: create envelope failed",
				zap.String("recipient", recipient), zap.Error(cerr))
			continue
		}
		envIDs = append(envIDs, id.String())
		publishInboxEvent(ctx, s.deps, tenant, recipient, id.String(), "ENVELOPE_DELIVERED", map[string]any{
			"from": sender, "intent": intent, "state": string(db.EnvelopeDelivered),
		})
	}
	if len(envIDs) == 0 {
		s.gcStagingSegment(ctx, be, tenant, stagingSeg)
		return nil, status.Error(codes.Internal, "failed to create any transfer envelope")
	}

	// structpb.NewStruct rejects a bare []string value, so the slices are
	// joined rather than passed through — this is audit context, not a typed
	// wire field.
	s.emitPersonalSkillEvent(ctx, tenant, sender, skillTransferSentEvent, name, map[string]any{
		"content_hash": result.ContentHash,
		"recipients":   strings.Join(recipients, ","),
		"skipped":      strings.Join(skipped, ","),
		"envelope_ids": strings.Join(envIDs, ","),
	})

	return &brokerv1.SendSkillTransferResponse{EnvelopeIds: envIDs, SkippedUserIds: skipped}, nil
}

// senderOwnsValidPersonalSkill reports whether user has a catalog-valid
// Skills/<name>/SKILL.md — the same validity rule GetPersonalSkillSouth
// applies (size cap, parseable frontmatter, non-empty description).
func senderOwnsValidPersonalSkill(ctx context.Context, be workspacefs.Backend, tenant, user, name string) bool {
	data, _, err := be.Read(ctx, tenant, user, personalskill.SkillMdPath(name))
	if err != nil || len(data) > personalskill.MaxSkillMdBytes {
		return false
	}
	fm, err := personalskill.ParseFrontmatter(data)
	return err == nil && fm.Description != ""
}

// manifestTotalBytes sums a snapshot manifest's file sizes.
func manifestTotalBytes(manifest []personalskill.ManifestEntry) int64 {
	var total int64
	for _, m := range manifest {
		total += m.Size
	}
	return total
}

// ── GetSkillTransferPreview ───────────────────────────────────────────────────

// GetSkillTransferPreview returns the full staged body, manifest, injection
// flags, and name-conflict status for a pending skill-transfer envelope.
// Recipient-only; an expired envelope GCs its staging and fails loud.
func (s *BrokerService) GetSkillTransferPreview(ctx context.Context, req *brokerv1.GetSkillTransferPreviewRequest) (*brokerv1.GetSkillTransferPreviewResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.personalskill.transfer.preview")
	defer span.End()

	tenant, user, err := s.personalSkillsCallerNorth(ctx, req.GetTenantId(), req.GetUserId())
	if err != nil {
		return nil, err
	}
	env, stagingSeg, skillName, contentHash, flags, err := s.loadSkillTransferEnvelope(ctx, tenant, user, req.GetEnvelopeId())
	if err != nil {
		return nil, err
	}

	// Serialize against a concurrent AcceptSkillTransfer/DismissEnvelope on
	// this envelope or a sibling sharing this staging segment: the reload
	// below and every staging read that follows run under the same lock
	// those paths hold for their own staging mutations.
	unlock := lockStagingSegment(tenant, stagingSeg)
	defer unlock()
	env, _, _, _, _, err = s.loadSkillTransferEnvelope(ctx, tenant, user, req.GetEnvelopeId())
	if err != nil {
		return nil, err
	}

	be := s.deps.Workspace.Local
	if env.ExpiresAt.Before(time.Now().UTC()) {
		s.settleExpiredTransfer(ctx, tenant, user, req.GetEnvelopeId(), stagingSeg)
		return nil, status.Error(codes.FailedPrecondition, "skill transfer envelope has expired")
	}

	data, _, rerr := be.Read(ctx, tenant, stagingSeg, personalskill.SkillMdName)
	if rerr != nil {
		return nil, status.Errorf(codes.Internal, "read staged SKILL.md: %v", rerr)
	}
	body, berr := personalskill.SplitBody(data)
	if berr != nil {
		return nil, status.Errorf(codes.Internal, "split staged SKILL.md body: %v", berr)
	}

	infos, lerr := be.ListDir(ctx, tenant, stagingSeg, "", true)
	if lerr != nil {
		return nil, status.Errorf(codes.Internal, "list staged manifest: %v", lerr)
	}
	var manifest []*brokerv1.SkillManifestEntry
	for _, fi := range infos {
		if fi.IsDir {
			continue
		}
		manifest = append(manifest, &brokerv1.SkillManifestEntry{Path: fi.Path, Size: fi.Size})
	}

	_, _, conflictErr := be.Read(ctx, tenant, user, personalskill.SkillMdPath(skillName))
	conflict := conflictErr == nil

	return &brokerv1.GetSkillTransferPreviewResponse{
		SkillName:   skillName,
		FromUserId:  env.FromUserID,
		Body:        body,
		Manifest:    manifest,
		Flags:       flags,
		ContentHash: contentHash,
		Conflict:    conflict,
	}, nil
}

// loadSkillTransferEnvelope fetches envelopeID and validates it is a
// skill_transfer envelope addressed to user and still PENDING/DELIVERED — the
// shared precondition GetSkillTransferPreview and AcceptSkillTransfer both
// re-check before touching staging.
func (s *BrokerService) loadSkillTransferEnvelope(ctx context.Context, tenant, user, envelopeID string) (env *db.Envelope, stagingSeg, skillName, contentHash string, flags []string, err error) {
	env, gerr := s.deps.Tasks.GetEnvelope(ctx, tenant, envelopeID)
	if gerr != nil {
		if errors.Is(gerr, db.ErrNotFound) {
			return nil, "", "", "", nil, status.Errorf(codes.NotFound, "envelope %s not found", envelopeID)
		}
		return nil, "", "", "", nil, status.Error(codes.Internal, "failed to load envelope")
	}
	toType, _ := env.ToTarget["type"].(string)
	toID, _ := env.ToTarget["id"].(string)
	if toType != "user" || !strings.EqualFold(toID, user) {
		return nil, "", "", "", nil, status.Error(codes.PermissionDenied, "envelope is not addressed to the caller")
	}
	kind, _ := env.TaskSpec["kind"].(string)
	if kind != skillTransferEnvelopeKind {
		return nil, "", "", "", nil, status.Error(codes.FailedPrecondition, "envelope is not a skill transfer")
	}
	if env.State != db.EnvelopePending && env.State != db.EnvelopeDelivered {
		return nil, "", "", "", nil, status.Errorf(codes.FailedPrecondition, "envelope is %s, not pending", strings.ToLower(string(env.State)))
	}
	stagingSeg, _ = env.TaskSpec["payload_ref"].(string)
	skillName, _ = env.TaskSpec["skill_name"].(string)
	contentHash, _ = env.TaskSpec["content_hash"].(string)
	flags = fmTexts(env.TaskSpec["flags"])
	return env, stagingSeg, skillName, contentHash, flags, nil
}

// ── AcceptSkillTransfer ────────────────────────────────────────────────────────

// AcceptSkillTransfer installs a pending skill-transfer envelope's staged
// snapshot into the recipient's own Skills/, then settles the envelope and
// GCs staging if no other envelope still references it. Recipient-only,
// capability re-checked via personalSkillsCallerNorth.
func (s *BrokerService) AcceptSkillTransfer(ctx context.Context, req *brokerv1.AcceptSkillTransferRequest) (*brokerv1.AcceptSkillTransferResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.personalskill.transfer.accept")
	defer span.End()

	tenant, user, err := s.personalSkillsCallerNorth(ctx, req.GetTenantId(), req.GetUserId())
	if err != nil {
		return nil, err
	}
	envelopeID := req.GetEnvelopeId()
	env, stagingSeg, skillName, _, _, err := s.loadSkillTransferEnvelope(ctx, tenant, user, envelopeID)
	if err != nil {
		return nil, err
	}

	// Serialize the whole reload→Install→settle→refcount-GC sequence: a
	// racing duplicate accept of this same envelope, or a dismiss/accept of a
	// sibling envelope sharing this staging segment, must never interleave
	// with this one's Install or GC.
	unlock := lockStagingSegment(tenant, stagingSeg)
	defer unlock()

	// Re-load inside the critical section: a racing duplicate accept must see
	// the now-non-PENDING/DELIVERED state here and fail before Install ever
	// runs a second time.
	env, _, _, _, _, err = s.loadSkillTransferEnvelope(ctx, tenant, user, envelopeID)
	if err != nil {
		return nil, err
	}

	be := s.deps.Workspace.Local
	if env.ExpiresAt.Before(time.Now().UTC()) {
		s.settleExpiredTransfer(ctx, tenant, user, envelopeID, stagingSeg)
		return nil, status.Error(codes.FailedPrecondition, "skill transfer envelope has expired")
	}

	mode := req.GetMode()
	if mode == "" {
		mode = "rename"
	}
	var targetName string
	var replace bool
	switch mode {
	case "rename":
		targetName = req.GetNameOverride()
		switch {
		case targetName == "":
			targetName, err = personalskill.FreeName(ctx, be, tenant, user, skillName)
			if err != nil {
				return nil, status.Errorf(codes.Internal, "resolve free skill name: %v", err)
			}
		case !personalskill.ValidSkillName(targetName):
			return nil, status.Errorf(codes.InvalidArgument, "invalid name_override %q", targetName)
		}
	case "replace":
		targetName = skillName
		replace = true
	default:
		return nil, status.Errorf(codes.InvalidArgument, "mode must be %q or %q, got %q", "rename", "replace", mode)
	}

	if err := personalskill.Install(ctx, be, tenant, stagingSeg, user, targetName, replace); err != nil {
		if errors.Is(err, personalskill.ErrExists) {
			return nil, status.Errorf(codes.AlreadyExists, "skill %q already exists", targetName)
		}
		return nil, fileError("install personal skill", err)
	}

	// RespondEnvelope (not RespondToEnvelope) is reused purely for the
	// PENDING/DELIVERED → ACCEPTED transition already scoped to this
	// recipient — it spawns no task, which is exactly what a transfer accept
	// needs (the ordinary RespondToEnvelope RPC path is guarded off entirely
	// for skill_transfer envelopes; see service_envelopes.go).
	if _, rerr := s.deps.Tasks.RespondEnvelope(ctx, tenant, envelopeID, user, true); rerr != nil {
		s.deps.Logger.Error("AcceptSkillTransfer: settle envelope failed", zap.Error(rerr))
		return nil, status.Errorf(codes.Internal, "failed to settle envelope: %v", rerr)
	}
	s.gcStagingIfUnreferenced(ctx, tenant, stagingSeg)

	s.emitPersonalSkillEvent(ctx, tenant, user, skillTransferAcceptedEvent, targetName, map[string]any{
		"mode":        mode,
		"envelope_id": envelopeID,
	})

	return &brokerv1.AcceptSkillTransferResponse{InstalledName: targetName}, nil
}

// ── staging lock ───────────────────────────────────────────────────────────────

// stagingLocksMu/stagingLocks hold one mutex per (tenant, staging segment),
// serializing every operation that reads from, installs from, or GCs a shared
// xfer-<uuid> segment: AcceptSkillTransfer's reload→Install→settle→refcount-GC
// sequence, GetSkillTransferPreview's staging reads (+ its expiry GC), and
// DismissEnvelope's GC hook (service_envelopes.go). Mirrors
// memorybundle.LockBundle's pattern exactly — process-local locking is sound
// because deployment enforces exactly one broker process
// (broker/cmd/broker/singleton.go's advisory lock).
//
// Callers acquire this once per RPC and hold it across their whole critical
// section; gcStagingIfUnreferenced/gcStagingSegment/settleExpiredTransfer
// never lock internally — they assume the caller already holds it — so
// nesting two acquisitions on the same goroutine (self-deadlock) never
// happens.
var (
	stagingLocksMu sync.Mutex
	stagingLocks   = map[string]*sync.Mutex{}
)

// lockStagingSegment acquires (tenant, stagingSeg)'s mutex and returns its
// unlock.
func lockStagingSegment(tenant, stagingSeg string) func() {
	key := tenant + "/" + stagingSeg
	stagingLocksMu.Lock()
	l, ok := stagingLocks[key]
	if !ok {
		l = &sync.Mutex{}
		stagingLocks[key] = l
	}
	stagingLocksMu.Unlock()
	l.Lock()
	return l.Unlock
}

// ── staging GC ─────────────────────────────────────────────────────────────────

// settleExpiredTransfer reacts to an expired-but-still-PENDING/DELIVERED
// skill-transfer envelope: it reuses the existing DismissEnvelope repo method
// to move the envelope out of the active states (no interface addition
// needed — DismissEnvelope's own precondition, PENDING/DELIVERED addressed to
// this user, is exactly what an already-validated transfer envelope
// satisfies), then runs the refcount GC. Without this step the envelope would
// still count as an active reference to its own staging segment and the GC
// would never fire — expiry must retire the envelope before counting.
//
// Callers must already hold stagingSeg's lockStagingSegment — this never
// locks itself.
func (s *BrokerService) settleExpiredTransfer(ctx context.Context, tenant, user, envelopeID, stagingSeg string) {
	if _, err := s.deps.Tasks.DismissEnvelope(ctx, tenant, envelopeID, user); err != nil {
		s.deps.Logger.Warn("settleExpiredTransfer: auto-dismiss failed",
			zap.String("envelope_id", envelopeID), zap.Error(err))
	}
	s.gcStagingIfUnreferenced(ctx, tenant, stagingSeg)
}

// gcStagingIfUnreferenced deletes stagingSeg once no PENDING/DELIVERED
// envelope still points at it (the GC rule: ,
// "Transfer"). A count-query failure leaves staging in place rather than
// risk deleting a segment another envelope still needs.
//
// Callers must already hold stagingSeg's lockStagingSegment — this never
// locks itself.
func (s *BrokerService) gcStagingIfUnreferenced(ctx context.Context, tenant, stagingSeg string) {
	if stagingSeg == "" {
		return
	}
	n, err := s.deps.Tasks.CountEnvelopesByPayloadRef(ctx, tenant, stagingSeg)
	if err != nil {
		s.deps.Logger.Warn("gcStagingIfUnreferenced: count failed, leaving staging in place",
			zap.String("segment", stagingSeg), zap.Error(err))
		return
	}
	if n == 0 {
		s.gcStagingSegment(ctx, s.deps.Workspace.Local, tenant, stagingSeg)
	}
}

// gcStagingSegment recursively deletes an xfer-<uuid> staging segment,
// including its now-empty root. Best-effort throughout: a failure is logged
// and swallowed, leaving an orphaned-but-harmless tenant-scoped directory
// rather than surfacing a GC failure to the RPC caller (spec risk table:
// "acceptable dev-ops cost").
//
// The final Delete call passes "." rather than "" for the root itself:
// CleanRel rejects an empty relative path (ErrInvalidPath) but accepts "."
// (it denotes the segment root the same way it does for ListDir), so "."
// is the only rel value that lets Delete remove an already-emptied segment.
func (s *BrokerService) gcStagingSegment(ctx context.Context, be workspacefs.Backend, tenant, stagingSeg string) {
	infos, err := be.ListDir(ctx, tenant, stagingSeg, "", true)
	if err != nil {
		s.deps.Logger.Warn("gcStagingSegment: list failed", zap.String("segment", stagingSeg), zap.Error(err))
		return
	}
	var dirs []string
	for _, fi := range infos {
		if fi.IsDir {
			dirs = append(dirs, fi.Path)
			continue
		}
		if derr := be.Delete(ctx, tenant, stagingSeg, fi.Path); derr != nil {
			s.deps.Logger.Warn("gcStagingSegment: delete file failed",
				zap.String("segment", stagingSeg), zap.String("path", fi.Path), zap.Error(derr))
		}
	}
	// Deepest (longest path) first so each directory is already empty by the
	// time Delete rejects a non-empty one.
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	for _, d := range dirs {
		if derr := be.Delete(ctx, tenant, stagingSeg, d); derr != nil {
			s.deps.Logger.Warn("gcStagingSegment: delete dir failed",
				zap.String("segment", stagingSeg), zap.String("path", d), zap.Error(derr))
		}
	}
	if derr := be.Delete(ctx, tenant, stagingSeg, "."); derr != nil && !errors.Is(derr, fs.ErrNotExist) {
		s.deps.Logger.Warn("gcStagingSegment: delete root failed",
			zap.String("segment", stagingSeg), zap.Error(derr))
	}
}

// ── audit ──────────────────────────────────────────────────────────────────────

// emitPersonalSkillEvent records a personal-skill management/transfer
// mutation. Fire-and-forget: the mutation already landed, so an audit
// failure is recorded and never returned (workflowsvc.Publish precedent).
func (s *BrokerService) emitPersonalSkillEvent(ctx context.Context, tenant, user, eventType, name string, extra map[string]any) {
	if s.deps.Audit == nil {
		return
	}
	fields := map[string]any{"skill_name": name}
	for k, v := range extra {
		fields[k] = v
	}
	ctxStruct, _ := structpb.NewStruct(fields)
	if err := s.deps.Audit.Emit(ctx, &auditv1.AuditEvent{
		EventId:     ids.EventID(),
		TenantId:    tenant,
		OccurredAt:  timestampNow(),
		ActorUserId: user,
		EventType:   eventType,
		ResourceRef: "aikonos:personal-skill:" + user + "/" + name,
		Decision:    auditv1.PolicyDecision_ALLOW,
		Context:     ctxStruct,
	}); err != nil {
		audit.RecordEmitFailure(ctx, s.deps.Logger, err, eventType)
	}
}
