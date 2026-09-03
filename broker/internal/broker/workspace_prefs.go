package broker

// workspace_prefs.go — CP5: GetWorkspaceBackend / SetWorkspaceBackend /
// ListOneDriveFolders. These are ordinary
// user RPCs — callerIdentity only, no requireTenantAdmin — since the working
// folder is a personal preference, not tenant configuration.

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/connector"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/workspacefs"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// oneDriveNotConfiguredMsg is the grep-stable FailedPrecondition reason every
// OneDrive-requiring RPC in this file returns when the tenant has no usable
// M365/OBO connection.
const oneDriveNotConfiguredMsg = "OneDrive is not configured for your organization"

func prefToProto(backend, folderPath string) *brokerv1.WorkspaceBackendPref {
	return &brokerv1.WorkspaceBackendPref{Backend: backend, OnedriveFolderPath: folderPath}
}

// oneDriveConnectorStatus reads the onedrive connectorstore entry's status
// for (tenant, user), if any — the same status ListConnectors reports.
// Absent/erroring reads report "" rather than failing the RPC.
func (s *BrokerService) oneDriveConnectorStatus(tenant, user string) string {
	if s.deps.Connectors == nil {
		return ""
	}
	entry, ok, err := s.deps.Connectors.Resolve(tenant, user, connector.ProviderOneDrive.ConnectorID())
	if err != nil || !ok {
		return ""
	}
	return entry.Status
}

func (s *BrokerService) GetWorkspaceBackend(ctx context.Context, req *brokerv1.GetWorkspaceBackendRequest) (*brokerv1.GetWorkspaceBackendResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.workspace.get_backend")
	defer span.End()

	tenant, user, err := s.callerIdentity(ctx, req.GetTenantId(), req.GetUserId())
	if err != nil {
		return nil, err
	}
	// Lazy OBO bootstrap: same fire-and-forget semantics as ListConnectors —
	// tolerant of failure, never blocks this read.
	s.ensureOneDriveOBO(ctx, tenant, user)

	available := s.deps.OBOBroker != nil && s.deps.OBOBroker.Configured(ctx, tenant)
	connStatus := s.oneDriveConnectorStatus(tenant, user)

	backend := string(workspacefs.KindLocal)
	folderPath := ""
	if s.deps.Workspace.Prefs != nil {
		eff, everr := s.deps.Workspace.Prefs.Effective(ctx, tenant, user)
		if everr != nil {
			return nil, status.Errorf(codes.Internal, "resolve workspace backend: %v", everr)
		}
		backend = string(eff.Backend)
		folderPath = eff.OneDriveFolderPath
	}

	return &brokerv1.GetWorkspaceBackendResponse{
		Pref:              prefToProto(backend, folderPath),
		OnedriveAvailable: available,
		OnedriveStatus:    connStatus,
	}, nil
}

func (s *BrokerService) SetWorkspaceBackend(ctx context.Context, req *brokerv1.SetWorkspaceBackendRequest) (*brokerv1.SetWorkspaceBackendResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.workspace.set_backend")
	defer span.End()

	tenant, user, err := s.callerIdentity(ctx, req.GetTenantId(), req.GetUserId())
	if err != nil {
		return nil, err
	}
	if s.deps.Workspace.Prefs == nil {
		return nil, status.Error(codes.FailedPrecondition, "workspace backend preferences not configured")
	}

	switch req.GetBackend() {
	case string(workspacefs.KindLocal):
		return s.setWorkspaceBackendLocal(ctx, tenant, user)
	case string(workspacefs.KindOneDrive):
		return s.setWorkspaceBackendOneDrive(ctx, tenant, user, req.GetOnedriveFolderPath())
	default:
		return nil, status.Errorf(codes.InvalidArgument, "backend must be %q or %q", workspacefs.KindLocal, workspacefs.KindOneDrive)
	}
}

// setWorkspaceBackendLocal preserves any previously-resolved OneDrive folder
// path/ids (so a later switch back to onedrive doesn't lose them) and just
// flips the active backend to local.
func (s *BrokerService) setWorkspaceBackendLocal(ctx context.Context, tenant, user string) (*brokerv1.SetWorkspaceBackendResponse, error) {
	existing, everr := s.deps.Workspace.Prefs.Effective(ctx, tenant, user)
	if everr != nil {
		return nil, status.Errorf(codes.Internal, "resolve existing workspace backend: %v", everr)
	}
	folderPath := existing.OneDriveFolderPath
	if folderPath == "" {
		folderPath = defaultOneDriveFolderPath
	}
	p := db.WorkspacePrefs{
		Backend:            string(workspacefs.KindLocal),
		OneDriveFolderPath: folderPath,
		DriveID:            existing.DriveID,
		RootItemID:         existing.RootItemID,
	}
	if err := s.deps.Workspace.Prefs.Set(ctx, tenant, user, p); err != nil {
		return nil, status.Errorf(codes.Internal, "set workspace backend: %v", err)
	}
	return &brokerv1.SetWorkspaceBackendResponse{Pref: prefToProto(p.Backend, p.OneDriveFolderPath)}, nil
}

// setWorkspaceBackendOneDrive validates the requested folder path, ensures
// the OBO connection + the folder itself, then persists the resolved ids.
func (s *BrokerService) setWorkspaceBackendOneDrive(ctx context.Context, tenant, user, rawPath string) (*brokerv1.SetWorkspaceBackendResponse, error) {
	if s.deps.OBOBroker == nil || !s.deps.OBOBroker.Configured(ctx, tenant) {
		return nil, status.Error(codes.FailedPrecondition, oneDriveNotConfiguredMsg)
	}
	if s.deps.Workspace.Graph == nil {
		return nil, status.Error(codes.FailedPrecondition, oneDriveNotConfiguredMsg)
	}

	path := rawPath
	if path == "" {
		path = defaultOneDriveFolderPath
	}
	clean, cerr := workspacefs.CleanRel(path)
	if cerr != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", cerr)
	}
	if workspacefs.IsReservedRelPath(clean) {
		return nil, status.Errorf(codes.InvalidArgument, "%q is a reserved path and cannot be used as a working folder", clean)
	}

	// Best-effort — same fire-and-forget semantics as GetWorkspaceBackend/
	// ListConnectors; a failure here still lets EnsureFolder attempt the call
	// (and fail loud on its own terms) rather than blocking this RPC early.
	s.ensureOneDriveOBO(ctx, tenant, user)

	driveID, itemID, ferr := s.deps.Workspace.Graph.EnsureFolder(ctx, tenant, user, clean)
	if ferr != nil {
		return nil, fileError("set workspace backend", ferr)
	}

	p := db.WorkspacePrefs{
		Backend:            string(workspacefs.KindOneDrive),
		OneDriveFolderPath: clean,
		DriveID:            driveID,
		RootItemID:         itemID,
	}
	// s.deps.Workspace.Prefs is guaranteed non-nil here — SetWorkspaceBackend
	// (the only caller) already returns FailedPrecondition when it's nil.
	if err := s.deps.Workspace.Prefs.Set(ctx, tenant, user, p); err != nil {
		return nil, status.Errorf(codes.Internal, "set workspace backend: %v", err)
	}
	return &brokerv1.SetWorkspaceBackendResponse{Pref: prefToProto(p.Backend, p.OneDriveFolderPath)}, nil
}

func (s *BrokerService) ListOneDriveFolders(ctx context.Context, req *brokerv1.ListOneDriveFoldersRequest) (*brokerv1.ListOneDriveFoldersResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.workspace.list_onedrive_folders")
	defer span.End()

	tenant, user, err := s.callerIdentity(ctx, req.GetTenantId(), req.GetUserId())
	if err != nil {
		return nil, err
	}
	if s.deps.OBOBroker == nil || !s.deps.OBOBroker.Configured(ctx, tenant) {
		return nil, status.Error(codes.FailedPrecondition, oneDriveNotConfiguredMsg)
	}
	if s.deps.Workspace.Graph == nil {
		return nil, status.Error(codes.FailedPrecondition, oneDriveNotConfiguredMsg)
	}
	s.ensureOneDriveOBO(ctx, tenant, user)

	folders, lerr := s.deps.Workspace.Graph.ListFolders(ctx, tenant, user, req.GetPath())
	if lerr != nil {
		return nil, fileError("list onedrive folders", lerr)
	}
	out := make([]*brokerv1.OneDriveFolder, 0, len(folders))
	for _, f := range folders {
		out = append(out, &brokerv1.OneDriveFolder{Name: f.Name, Path: f.Path})
	}
	return &brokerv1.ListOneDriveFoldersResponse{Folders: out}, nil
}
