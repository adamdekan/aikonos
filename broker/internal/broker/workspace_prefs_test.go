package broker

import (
	"context"
	"testing"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// workspacePrefsSvc builds a BrokerService for these tests. obo/graph are
// accepted as the narrow interfaces (not the concrete fake types) so a
// caller passing literal nil gets a genuinely nil interface value — passing
// a typed-nil *fakeOBOBroker/*fakeGraphStore through a concretely-typed
// parameter would instead produce a non-nil interface wrapping a nil
// pointer (the classic Go nil-interface trap main.go's oboBroker wiring
// comment documents), which would panic the first time a nil-check-then-call
// path (e.g. GetWorkspaceBackend's `OBOBroker != nil && OBOBroker.Configured`)
// dereferences it.
func workspacePrefsSvc(t *testing.T, obo oboBroker, graph OneDriveGraphStore, resolver *WorkspacePrefResolver) *BrokerService {
	t.Helper()
	em, err := audit.NewEmitter(context.Background(), audit.Config{})
	if err != nil {
		t.Fatal(err)
	}
	return NewBrokerService(Deps{
		Logger:    zap.NewNop(),
		Audit:     em,
		TenantID:  "aikonos-dev",
		OBOBroker: obo,
		Workspace: WorkspaceDeps{Graph: graph, Prefs: resolver},
	})
}

func TestGetWorkspaceBackend_UnconfiguredReportsAvailableFalse(t *testing.T) {
	repo := newFakeWorkspacePrefsRepo()
	resolver := NewWorkspacePrefResolver(repo, fakeConfiguredChecker{configured: false})
	svc := workspacePrefsSvc(t, &fakeOBOBroker{configured: false}, nil, resolver)
	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")

	resp, err := svc.GetWorkspaceBackend(ctx, &brokerv1.GetWorkspaceBackendRequest{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if resp.GetOnedriveAvailable() {
		t.Fatalf("expected onedrive_available=false when unconfigured, got %+v", resp)
	}
	if resp.GetPref().GetBackend() != "local" {
		t.Fatalf("expected local default when unconfigured, got %+v", resp.GetPref())
	}
}

func TestGetWorkspaceBackend_NilOBOBroker_AvailableFalseNoError(t *testing.T) {
	repo := newFakeWorkspacePrefsRepo()
	resolver := NewWorkspacePrefResolver(repo, fakeConfiguredChecker{configured: false})
	svc := workspacePrefsSvc(t, nil, nil, resolver)
	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")

	resp, err := svc.GetWorkspaceBackend(ctx, &brokerv1.GetWorkspaceBackendRequest{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if resp.GetOnedriveAvailable() {
		t.Fatalf("expected onedrive_available=false with a nil OBOBroker, got %+v", resp)
	}
}

func TestSetWorkspaceBackend_OneDrive_HappyPath(t *testing.T) {
	repo := newFakeWorkspacePrefsRepo()
	resolver := NewWorkspacePrefResolver(repo, fakeConfiguredChecker{configured: true})
	graph := &fakeGraphStore{root: &recordingRoot{}}
	svc := workspacePrefsSvc(t, &fakeOBOBroker{configured: true}, graph, resolver)
	ctx := ctxWithIdentityAndBearer(testTenantUUID, "alice@example.com", "tok")

	resp, err := svc.SetWorkspaceBackend(ctx, &brokerv1.SetWorkspaceBackendRequest{
		Backend: "onedrive", OnedriveFolderPath: "Reports/2026",
	})
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if resp.GetPref().GetBackend() != "onedrive" || resp.GetPref().GetOnedriveFolderPath() != "Reports/2026" {
		t.Fatalf("unexpected pref in response: %+v", resp.GetPref())
	}
	if graph.ensureCalls != 1 {
		t.Fatalf("expected EnsureFolder to be called once, got %d", graph.ensureCalls)
	}
	row, found, err := repo.Get(context.Background(), testTenantUUID, "alice@example.com")
	if err != nil || !found {
		t.Fatalf("expected the resolved ids to be persisted, found=%v err=%v", found, err)
	}
	if row.DriveID != "DRV1" || row.RootItemID != "ITEM1" {
		t.Fatalf("expected persisted ids (DRV1,ITEM1), got %+v", row)
	}

	// The cache must have been invalidated by Set — the very next Effective
	// call re-reads the repo instead of serving a stale pre-Set value.
	getCallsBefore := repo.getCalls
	if _, err := resolver.Effective(context.Background(), testTenantUUID, "alice@example.com"); err != nil {
		t.Fatalf("effective: %v", err)
	}
	if repo.getCalls != getCallsBefore+1 {
		t.Fatalf("expected Set to invalidate the cache (forcing a re-read), repo.getCalls before=%d after=%d", getCallsBefore, repo.getCalls)
	}
}

func TestSetWorkspaceBackend_RejectsBadBackendValue(t *testing.T) {
	repo := newFakeWorkspacePrefsRepo()
	resolver := NewWorkspacePrefResolver(repo, fakeConfiguredChecker{configured: true})
	svc := workspacePrefsSvc(t, &fakeOBOBroker{configured: true}, &fakeGraphStore{root: &recordingRoot{}}, resolver)
	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")

	_, err := svc.SetWorkspaceBackend(ctx, &brokerv1.SetWorkspaceBackendRequest{Backend: "dropbox"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad backend value should be InvalidArgument, got %v", err)
	}
}

func TestSetWorkspaceBackend_RejectsReservedFirstSegment(t *testing.T) {
	repo := newFakeWorkspacePrefsRepo()
	resolver := NewWorkspacePrefResolver(repo, fakeConfiguredChecker{configured: true})
	graph := &fakeGraphStore{root: &recordingRoot{}}
	svc := workspacePrefsSvc(t, &fakeOBOBroker{configured: true}, graph, resolver)
	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")

	_, err := svc.SetWorkspaceBackend(ctx, &brokerv1.SetWorkspaceBackendRequest{
		Backend: "onedrive", OnedriveFolderPath: ".agent/x",
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("reserved first segment should be InvalidArgument, got %v", err)
	}
	if graph.ensureCalls != 0 {
		t.Fatalf("EnsureFolder must not be called for a rejected path, got %d calls", graph.ensureCalls)
	}
}

func TestSetWorkspaceBackend_RejectsTraversalSpelling(t *testing.T) {
	repo := newFakeWorkspacePrefsRepo()
	resolver := NewWorkspacePrefResolver(repo, fakeConfiguredChecker{configured: true})
	graph := &fakeGraphStore{root: &recordingRoot{}}
	svc := workspacePrefsSvc(t, &fakeOBOBroker{configured: true}, graph, resolver)
	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")

	_, err := svc.SetWorkspaceBackend(ctx, &brokerv1.SetWorkspaceBackendRequest{
		Backend: "onedrive", OnedriveFolderPath: "a/../.agent",
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("traversal spelling into a reserved segment should be InvalidArgument, got %v", err)
	}
	if graph.ensureCalls != 0 {
		t.Fatalf("EnsureFolder must not be called for a rejected path, got %d calls", graph.ensureCalls)
	}
}

func TestSetWorkspaceBackend_OneDrive_Unconfigured_FailedPrecondition(t *testing.T) {
	repo := newFakeWorkspacePrefsRepo()
	resolver := NewWorkspacePrefResolver(repo, fakeConfiguredChecker{configured: false})
	svc := workspacePrefsSvc(t, &fakeOBOBroker{configured: false}, &fakeGraphStore{root: &recordingRoot{}}, resolver)
	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")

	_, err := svc.SetWorkspaceBackend(ctx, &brokerv1.SetWorkspaceBackendRequest{Backend: "onedrive"})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("onedrive request on an unconfigured tenant should be FailedPrecondition, got %v", err)
	}
}

func TestListOneDriveFolders_Unconfigured_FailedPrecondition(t *testing.T) {
	repo := newFakeWorkspacePrefsRepo()
	resolver := NewWorkspacePrefResolver(repo, fakeConfiguredChecker{configured: false})
	svc := workspacePrefsSvc(t, &fakeOBOBroker{configured: false}, nil, resolver)
	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")

	_, err := svc.ListOneDriveFolders(ctx, &brokerv1.ListOneDriveFoldersRequest{})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("listing folders on an unconfigured tenant should be FailedPrecondition, got %v", err)
	}
}
