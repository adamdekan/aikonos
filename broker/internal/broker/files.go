package broker

import (
	"context"
	"errors"
	"io/fs"
	"mime"
	"net/http"
	"path/filepath"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/adamdekan/aikonos/broker/internal/ids"
	"github.com/adamdekan/aikonos/broker/internal/workspacefs"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

func (s *BrokerService) workspaceEnabled() bool {
	return s.deps.Workspace.Local != nil && s.deps.Workspace.Local.Enabled()
}

// fileRPCRoutesOneDrive reports whether rel would actually route to Remote
// for (tenant, user) right now — delegating entirely to Router.ActiveKind,
// the single source of truth for the routing decision (reserved-first-
// segment-before-Prefs, INCLUDING the Remote == nil-with-an-onedrive-Prefs
// case, which ActiveKind surfaces as ErrUnavailable rather than a silent
// onedrive dispatch). false when s.deps.Workspace.Local isn't a *workspacefs.Router
// (a plain *workspacefs.Store never routes remote) or ActiveKind errors for
// any reason (unresolved path, Prefs lookup failure, or an unwired Remote).
func (s *BrokerService) fileRPCRoutesOneDrive(ctx context.Context, tenant, user, rel string) bool {
	router, ok := s.deps.Workspace.Local.(*workspacefs.Router)
	if !ok {
		return false
	}
	kind, err := router.ActiveKind(ctx, tenant, user, rel)
	return err == nil && kind == workspacefs.KindOneDrive
}

// ensureOneDriveOBOForFileRPC triggers the OBO bootstrap/refresh check before
// a single-path file RPC's workspace op runs, but only when rel actually
// routes to OneDrive right now (spec: "OneDrive-routed calls trigger
// ensureOneDriveOBO"). Best-effort and nil-safe throughout — see
// fileRPCRoutesOneDrive's doc comment.
func (s *BrokerService) ensureOneDriveOBOForFileRPC(ctx context.Context, tenant, user, rel string) {
	if s.fileRPCRoutesOneDrive(ctx, tenant, user, rel) {
		s.ensureOneDriveOBO(ctx, tenant, user)
	}
}

// ensureOneDriveOBOForMoveRPC is MoveWorkspaceFile's variant: Router.Move only
// ever dispatches to Remote when BOTH endpoints are non-reserved (a mixed
// reserved/non-reserved pair either stays local or is rejected — see
// Router.Move's doc comment), so this only fires the ensure check when both
// from and to independently route to onedrive.
func (s *BrokerService) ensureOneDriveOBOForMoveRPC(ctx context.Context, tenant, user, from, to string) {
	if s.fileRPCRoutesOneDrive(ctx, tenant, user, from) && s.fileRPCRoutesOneDrive(ctx, tenant, user, to) {
		s.ensureOneDriveOBO(ctx, tenant, user)
	}
}

// agentDirGuard rejects a mutating file-RPC target under .agent/ — the
// server-maintained tree. WHY at this layer: these four RPCs are the
// user-facing file boundary, and .agent/Memory/ holds memory bundles whose
// `generated`/`verified` frontmatter is stamped server-side by memory.write.
// A raw upload there would let any authenticated user forge human-reviewed
// provenance with no skill:memory.write grant, which auto-recall then surfaces
// as verified. The toolproxy seam has always refused this (workspace_seam.go's
// agentDirGuard, same workspacefs.UnderAgentDir predicate); these RPCs bypassed
// it because they call the backend directly.
//
// .agent/Sessions/ is carved out deliberately, not incidentally: the webui
// persists session records through UploadWorkspaceFile/MoveWorkspaceFile (see
// workspacefs.SessionsDirPrefix) and those writes are HMAC-signed by the store.
// Removing the carve-out breaks session persistence.
//
// Not routed through fileError — fileError maps workspacefs storage sentinels,
// and this is a policy denial with no storage call behind it.
func agentDirGuard(rel string) error {
	if !workspacefs.UnderAgentDir(rel) || workspacefs.UnderSessionsDir(rel) {
		return nil
	}
	return status.Errorf(codes.PermissionDenied,
		"%q is under .agent/: the server-maintained tree is not writable through the file API", rel)
}

// fileError maps a workspacefs error to a gRPC status: validation → InvalidArgument,
// missing file → NotFound, a remote-backend 403 → PermissionDenied, everything
// else → Internal.
func fileError(verb string, err error) error {
	switch {
	case errors.Is(err, workspacefs.ErrInvalidPath), errors.Is(err, workspacefs.ErrTooLarge):
		return status.Errorf(codes.InvalidArgument, "%v", err)
	case errors.Is(err, workspacefs.ErrExists):
		return status.Errorf(codes.AlreadyExists, "%v", err)
	case errors.Is(err, workspacefs.ErrNotEmpty):
		return status.Errorf(codes.FailedPrecondition, "%v", err)
	case errors.Is(err, workspacefs.ErrIntegrityMismatch):
		return status.Errorf(codes.FailedPrecondition, "%v", err)
	case errors.Is(err, workspacefs.ErrUnavailable):
		return status.Errorf(codes.Unavailable, "%v", err)
	case errors.Is(err, workspacefs.ErrReconnectRequired):
		return status.Errorf(codes.FailedPrecondition, "%v", err)
	case errors.Is(err, workspacefs.ErrForbidden):
		return status.Errorf(codes.PermissionDenied, "%v", err)
	case errors.Is(err, fs.ErrNotExist):
		return status.Error(codes.NotFound, "file not found")
	default:
		return status.Errorf(codes.Internal, "%s failed: %v", verb, err)
	}
}

func detectMime(path string, content []byte) string {
	if t := mime.TypeByExtension(filepath.Ext(path)); t != "" {
		return t
	}
	return http.DetectContentType(content)
}

func toProtoFile(f workspacefs.FileInfo) *brokerv1.WorkspaceFile {
	return &brokerv1.WorkspaceFile{
		Path:       f.Path,
		SizeBytes:  f.Size,
		ModifiedAt: timestamppb.New(f.ModTime),
		IsDir:      f.IsDir,
	}
}

func (s *BrokerService) ListWorkspaceFiles(ctx context.Context, req *brokerv1.ListWorkspaceFilesRequest) (*brokerv1.ListWorkspaceFilesResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.files.list")
	defer span.End()

	if !s.workspaceEnabled() {
		return nil, status.Error(codes.FailedPrecondition, "workspace files disabled (no workspace root configured)")
	}
	tenant, user, err := s.callerIdentity(ctx, req.TenantId, req.UserId)
	if err != nil {
		return nil, err
	}
	s.ensureOneDriveOBOForFileRPC(ctx, tenant, user, req.Path)
	var files []workspacefs.FileInfo
	if req.Path != "" {
		files, err = s.deps.Workspace.Local.ListDir(ctx, tenant, user, req.Path, req.Recursive)
	} else {
		files, err = s.deps.Workspace.Local.List(ctx, tenant, user)
	}
	if err != nil {
		return nil, fileError("list", err)
	}
	out := make([]*brokerv1.WorkspaceFile, 0, len(files))
	for _, f := range files {
		out = append(out, toProtoFile(f))
	}
	s.fileAudit(ctx, span.SpanContext().TraceID().String(), tenant, user,
		"aikonos.workspace.file.listed", "", auditv1.PolicyDecision_ALLOW)
	return &brokerv1.ListWorkspaceFilesResponse{Files: out}, nil
}

func (s *BrokerService) ReadWorkspaceFile(ctx context.Context, req *brokerv1.ReadWorkspaceFileRequest) (*brokerv1.ReadWorkspaceFileResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.files.read")
	defer span.End()

	if !s.workspaceEnabled() {
		return nil, status.Error(codes.FailedPrecondition, "workspace files disabled")
	}
	tenant, user, err := s.callerIdentity(ctx, req.TenantId, req.UserId)
	if err != nil {
		return nil, err
	}
	s.ensureOneDriveOBOForFileRPC(ctx, tenant, user, req.Path)
	// CP4.2 investigation finding: this RPC is the SOLE server-side read that
	// can seed downstream execution. The scheduler never resumes a run from a
	// stored session file (each scheduled run builds a fresh prompt — see
	// agent-gateway/src/scheduler/ticker.ts); the only path by which a session
	// record's bytes reach an LLM call is the webui downloading it here and
	// replaying it as /agui chat history into the gateway child (F62's
	// PromptMessage.history). That's why integrity verification lives on this
	// call and not, e.g., duplicated into some scheduler-side read.
	//
	// ReadVerified is called unconditionally (not just for paths that look
	// like session records) — it decides session-ness itself from the
	// RESOLVED path (workspacefs.Store.resolveSession) and is a no-op check
	// for every ordinary workspace file, so there is no raw-path branch here
	// to get wrong (see workspacefs.SessionsDirPrefix's doc comment for the
	// bug that pattern caused in an earlier cut of this feature).
	data, _, legacy, err := s.deps.Workspace.Local.ReadVerified(ctx, tenant, user, req.Path)
	if errors.Is(err, workspacefs.ErrIntegrityMismatch) {
		s.fileAudit(ctx, span.SpanContext().TraceID().String(), tenant, user,
			"aikonos.workspace.session.integrity_failed", req.Path, auditv1.PolicyDecision_DENY)
		return nil, fileError("read", err)
	}
	if err != nil {
		return nil, fileError("read", err)
	}
	if legacy {
		s.deps.Logger.Info("session file read without an integrity sidecar (legacy, pre-CP4.2)",
			zap.String("path", req.Path))
	}
	s.fileAudit(ctx, span.SpanContext().TraceID().String(), tenant, user,
		"aikonos.workspace.file.read", req.Path, auditv1.PolicyDecision_ALLOW)
	return &brokerv1.ReadWorkspaceFileResponse{
		Path:      req.Path,
		MimeType:  detectMime(req.Path, data),
		Content:   data,
		SizeBytes: int64(len(data)),
	}, nil
}

func (s *BrokerService) UploadWorkspaceFile(ctx context.Context, req *brokerv1.UploadWorkspaceFileRequest) (*brokerv1.UploadWorkspaceFileResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.files.upload")
	defer span.End()

	if !s.workspaceEnabled() {
		return nil, status.Error(codes.FailedPrecondition, "workspace files disabled")
	}
	tenant, user, err := s.callerIdentity(ctx, req.TenantId, req.UserId)
	if err != nil {
		return nil, err
	}
	if err := agentDirGuard(req.Path); err != nil {
		return nil, err
	}
	s.ensureOneDriveOBOForFileRPC(ctx, tenant, user, req.Path)
	// CP4.2: Write itself decides (from the RESOLVED path) whether this lands
	// under workspacefs.SessionsDirPrefix and signs it — no raw-path branch
	// needed here.
	info, err := s.deps.Workspace.Local.Write(ctx, tenant, user, req.Path, req.Content)
	if err != nil {
		return nil, fileError("upload", err)
	}
	s.fileAudit(ctx, span.SpanContext().TraceID().String(), tenant, user,
		"aikonos.workspace.file.uploaded", info.Path, auditv1.PolicyDecision_ALLOW)
	return &brokerv1.UploadWorkspaceFileResponse{File: toProtoFile(info)}, nil
}

func (s *BrokerService) DeleteWorkspaceFile(ctx context.Context, req *brokerv1.DeleteWorkspaceFileRequest) (*brokerv1.DeleteWorkspaceFileResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.files.delete")
	defer span.End()

	if !s.workspaceEnabled() {
		return nil, status.Error(codes.FailedPrecondition, "workspace files disabled")
	}
	tenant, user, err := s.callerIdentity(ctx, req.TenantId, req.UserId)
	if err != nil {
		return nil, err
	}
	if err := agentDirGuard(req.Path); err != nil {
		return nil, err
	}
	s.ensureOneDriveOBOForFileRPC(ctx, tenant, user, req.Path)
	if err := s.deps.Workspace.Local.Delete(ctx, tenant, user, req.Path); err != nil {
		return nil, fileError("delete", err)
	}
	s.fileAudit(ctx, span.SpanContext().TraceID().String(), tenant, user,
		"aikonos.workspace.file.deleted", req.Path, auditv1.PolicyDecision_ALLOW)
	return &brokerv1.DeleteWorkspaceFileResponse{Success: true}, nil
}

func (s *BrokerService) MoveWorkspaceFile(ctx context.Context, req *brokerv1.MoveWorkspaceFileRequest) (*brokerv1.MoveWorkspaceFileResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.files.move")
	defer span.End()

	if !s.workspaceEnabled() {
		return nil, status.Error(codes.FailedPrecondition, "workspace files disabled")
	}
	tenant, user, err := s.callerIdentity(ctx, req.TenantId, req.UserId)
	if err != nil {
		return nil, err
	}
	// Both endpoints: moving a file OUT of a memory bundle mutates it as much
	// as moving one in.
	if err := agentDirGuard(req.FromPath); err != nil {
		return nil, err
	}
	if err := agentDirGuard(req.ToPath); err != nil {
		return nil, err
	}
	s.ensureOneDriveOBOForMoveRPC(ctx, tenant, user, req.FromPath, req.ToPath)
	info, err := s.deps.Workspace.Local.Move(ctx, tenant, user, req.FromPath, req.ToPath)
	if err != nil {
		return nil, fileError("move", err)
	}
	s.fileAudit(ctx, span.SpanContext().TraceID().String(), tenant, user,
		"aikonos.workspace.file.moved", info.Path, auditv1.PolicyDecision_ALLOW)
	return &brokerv1.MoveWorkspaceFileResponse{File: toProtoFile(info)}, nil
}

func (s *BrokerService) CreateWorkspaceDir(ctx context.Context, req *brokerv1.CreateWorkspaceDirRequest) (*brokerv1.CreateWorkspaceDirResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.files.mkdir")
	defer span.End()

	if !s.workspaceEnabled() {
		return nil, status.Error(codes.FailedPrecondition, "workspace files disabled")
	}
	tenant, user, err := s.callerIdentity(ctx, req.TenantId, req.UserId)
	if err != nil {
		return nil, err
	}
	if err := agentDirGuard(req.Path); err != nil {
		return nil, err
	}
	s.ensureOneDriveOBOForFileRPC(ctx, tenant, user, req.Path)
	if err := s.deps.Workspace.Local.Mkdir(ctx, tenant, user, req.Path); err != nil {
		return nil, fileError("mkdir", err)
	}
	s.fileAudit(ctx, span.SpanContext().TraceID().String(), tenant, user,
		"aikonos.workspace.dir.created", req.Path, auditv1.PolicyDecision_ALLOW)
	return &brokerv1.CreateWorkspaceDirResponse{Success: true}, nil
}

func (s *BrokerService) fileAudit(ctx context.Context, traceID, tenant, actor, eventType, path string, decision auditv1.PolicyDecision) {
	ev := &auditv1.AuditEvent{
		EventId:     ids.EventID(),
		TraceId:     traceID,
		TenantId:    tenant,
		OccurredAt:  timestampNow(),
		ActorUserId: actor,
		EventType:   eventType,
		Decision:    decision,
	}
	if path != "" {
		ev.ResourceRef = "workspace_file:" + path
		if c, err := structpb.NewStruct(map[string]any{"path": path}); err == nil {
			ev.Context = c
		}
	}
	if err := s.deps.Audit.Emit(ctx, ev); err != nil {
		s.deps.Logger.Warn("file audit emit failed", zap.Error(err))
	}
}
