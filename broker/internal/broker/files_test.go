package broker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/workspacefs"
	"github.com/adamdekan/aikonos/broker/internal/workspacepath"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

func filesSvc(t *testing.T, root string) *BrokerService {
	t.Helper()
	em, err := audit.NewEmitter(context.Background(), audit.Config{})
	if err != nil {
		t.Fatal(err)
	}
	return NewBrokerService(Deps{
		Logger:    zap.NewNop(),
		Audit:     em,
		TenantID:  "aikonos-dev",
		Workspace: WorkspaceDeps{Local: workspacefs.New(root)},
	})
}

// filesSvcSigned is filesSvc with a workspace session signing key installed
// (CP4.2) — used by tests exercising the .agent/Sessions/ integrity path.
func filesSvcSigned(t *testing.T, root string) *BrokerService {
	t.Helper()
	em, err := audit.NewEmitter(context.Background(), audit.Config{})
	if err != nil {
		t.Fatal(err)
	}
	ws := workspacefs.New(root)
	ws.SetSessionSigningKey([]byte("a-32-byte-test-key-aaaaaaaaaaaa!"))
	return NewBrokerService(Deps{
		Logger:    zap.NewNop(),
		Audit:     em,
		TenantID:  "aikonos-dev",
		Workspace: WorkspaceDeps{Local: ws},
	})
}

// TestFileError_BackendSentinels pins the three workspacefs sentinels a
// remote Backend (CP4's onedrivefs.Store, routed through CP3's Router) can
// return that a local-disk Store never produces: ErrUnavailable (backend
// unreachable), ErrReconnectRequired (stale OBO credential), and ErrForbidden
// (a remote 403) — table-tested together since none had fileError-level
// coverage before this fix.
func TestFileError_BackendSentinels(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want codes.Code
	}{
		{name: "unavailable", err: workspacefs.ErrUnavailable, want: codes.Unavailable},
		{name: "reconnect_required", err: workspacefs.ErrReconnectRequired, want: codes.FailedPrecondition},
		{name: "forbidden", err: workspacefs.ErrForbidden, want: codes.PermissionDenied},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := fileError("op", tc.err)
			if status.Code(err) != tc.want {
				t.Fatalf("fileError(%v) code = %v, want %v", tc.err, status.Code(err), tc.want)
			}
		})
	}
}

func TestUploadWorkspaceFile_RoundTrip(t *testing.T) {
	svc := filesSvc(t, t.TempDir())
	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")

	if _, err := svc.UploadWorkspaceFile(ctx, &brokerv1.UploadWorkspaceFileRequest{
		Path: "notes.txt", Content: []byte("hello"),
	}); err != nil {
		t.Fatalf("upload: %v", err)
	}
	list, err := svc.ListWorkspaceFiles(ctx, &brokerv1.ListWorkspaceFilesRequest{})
	if err != nil || len(list.Files) != 1 || list.Files[0].Path != "notes.txt" {
		t.Fatalf("list: files=%+v err=%v", list.GetFiles(), err)
	}
	read, err := svc.ReadWorkspaceFile(ctx, &brokerv1.ReadWorkspaceFileRequest{Path: "notes.txt"})
	if err != nil || string(read.Content) != "hello" {
		t.Fatalf("read: %q err=%v", read.GetContent(), err)
	}
	if _, err := svc.DeleteWorkspaceFile(ctx, &brokerv1.DeleteWorkspaceFileRequest{Path: "notes.txt"}); err != nil {
		t.Fatalf("delete: %v", err)
	}
}

func TestWorkspaceFile_TraversalRejected(t *testing.T) {
	svc := filesSvc(t, t.TempDir())
	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")
	_, err := svc.UploadWorkspaceFile(ctx, &brokerv1.UploadWorkspaceFileRequest{
		Path: "../../etc/passwd", Content: []byte("x"),
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("traversal should be InvalidArgument, got %v", err)
	}
}

func TestReadWorkspaceFile_NotFound(t *testing.T) {
	svc := filesSvc(t, t.TempDir())
	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")
	_, err := svc.ReadWorkspaceFile(ctx, &brokerv1.ReadWorkspaceFileRequest{Path: "nope.txt"})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("missing file should be NotFound, got %v", err)
	}
}

func TestWorkspaceFile_RejectsUserIdSpoofing(t *testing.T) {
	svc := filesSvc(t, t.TempDir())
	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")
	_, err := svc.ListWorkspaceFiles(ctx, &brokerv1.ListWorkspaceFilesRequest{UserId: "bob@example.com"})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("spoofed user_id should be PermissionDenied, got %v", err)
	}
}

func TestWorkspaceFile_DisabledFailsClosed(t *testing.T) {
	svc := filesSvc(t, "") // empty root → disabled
	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")
	_, err := svc.ListWorkspaceFiles(ctx, &brokerv1.ListWorkspaceFilesRequest{})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("disabled workspace should be FailedPrecondition, got %v", err)
	}
}

func TestWorkspaceFile_RejectsNonUUIDTenant(t *testing.T) {
	svc := filesSvc(t, t.TempDir())
	ctx := ctxWithIdentity("not-a-uuid", "alice@example.com")
	_, err := svc.ListWorkspaceFiles(ctx, &brokerv1.ListWorkspaceFilesRequest{})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("non-uuid tenant should be InvalidArgument, got %v", err)
	}
}

func TestMoveWorkspaceFile_RoundTrip(t *testing.T) {
	svc := filesSvc(t, t.TempDir())
	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")

	if _, err := svc.UploadWorkspaceFile(ctx, &brokerv1.UploadWorkspaceFileRequest{
		Path: "notes.txt", Content: []byte("hello"),
	}); err != nil {
		t.Fatalf("upload: %v", err)
	}
	resp, err := svc.MoveWorkspaceFile(ctx, &brokerv1.MoveWorkspaceFileRequest{
		FromPath: "notes.txt", ToPath: "archive/notes.txt",
	})
	if err != nil {
		t.Fatalf("move: %v", err)
	}
	if resp.GetFile().GetPath() != "archive/notes.txt" || resp.GetFile().GetIsDir() {
		t.Fatalf("move response: %+v", resp.GetFile())
	}
	list, err := svc.ListWorkspaceFiles(ctx, &brokerv1.ListWorkspaceFilesRequest{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var sawOld, sawNew bool
	for _, f := range list.Files {
		if f.Path == "notes.txt" {
			sawOld = true
		}
		if f.Path == "archive/notes.txt" {
			sawNew = true
			if f.IsDir {
				t.Fatalf("archive/notes.txt should not be IsDir")
			}
		}
	}
	if sawOld {
		t.Fatalf("old path notes.txt should be gone: %+v", list.Files)
	}
	if !sawNew {
		t.Fatalf("new path archive/notes.txt should be present: %+v", list.Files)
	}
}

func TestMoveWorkspaceFile_DestExists(t *testing.T) {
	svc := filesSvc(t, t.TempDir())
	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")

	if _, err := svc.UploadWorkspaceFile(ctx, &brokerv1.UploadWorkspaceFileRequest{
		Path: "a.txt", Content: []byte("a"),
	}); err != nil {
		t.Fatalf("upload a: %v", err)
	}
	if _, err := svc.UploadWorkspaceFile(ctx, &brokerv1.UploadWorkspaceFileRequest{
		Path: "b.txt", Content: []byte("b"),
	}); err != nil {
		t.Fatalf("upload b: %v", err)
	}
	_, err := svc.MoveWorkspaceFile(ctx, &brokerv1.MoveWorkspaceFileRequest{
		FromPath: "a.txt", ToPath: "b.txt",
	})
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("move onto existing dest should be AlreadyExists, got %v", err)
	}
}

func TestMoveWorkspaceFile_RejectsUserIdSpoofing(t *testing.T) {
	svc := filesSvc(t, t.TempDir())
	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")
	_, err := svc.MoveWorkspaceFile(ctx, &brokerv1.MoveWorkspaceFileRequest{
		UserId: "bob@example.com", FromPath: "a.txt", ToPath: "b.txt",
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("spoofed user_id should be PermissionDenied, got %v", err)
	}
}

func TestCreateWorkspaceDir_ThenListed(t *testing.T) {
	svc := filesSvc(t, t.TempDir())
	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")

	resp, err := svc.CreateWorkspaceDir(ctx, &brokerv1.CreateWorkspaceDirRequest{Path: "reports"})
	if err != nil || !resp.GetSuccess() {
		t.Fatalf("mkdir: resp=%+v err=%v", resp, err)
	}
	list, err := svc.ListWorkspaceFiles(ctx, &brokerv1.ListWorkspaceFilesRequest{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var found bool
	for _, f := range list.Files {
		if f.Path == "reports" {
			found = true
			if !f.IsDir {
				t.Fatalf("reports entry should be IsDir")
			}
		}
	}
	if !found {
		t.Fatalf("expected reports dir in list: %+v", list.Files)
	}
}

func TestCreateWorkspaceDir_Exists(t *testing.T) {
	svc := filesSvc(t, t.TempDir())
	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")

	if _, err := svc.CreateWorkspaceDir(ctx, &brokerv1.CreateWorkspaceDirRequest{Path: "reports"}); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	_, err := svc.CreateWorkspaceDir(ctx, &brokerv1.CreateWorkspaceDirRequest{Path: "reports"})
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("mkdir existing should be AlreadyExists, got %v", err)
	}
}

func TestCreateWorkspaceDir_RejectsUserIdSpoofing(t *testing.T) {
	svc := filesSvc(t, t.TempDir())
	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")
	_, err := svc.CreateWorkspaceDir(ctx, &brokerv1.CreateWorkspaceDirRequest{
		UserId: "bob@example.com", Path: "reports",
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("spoofed user_id should be PermissionDenied, got %v", err)
	}
}

func TestDeleteWorkspaceFile_EmptyDirSucceeds(t *testing.T) {
	svc := filesSvc(t, t.TempDir())
	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")

	if _, err := svc.CreateWorkspaceDir(ctx, &brokerv1.CreateWorkspaceDirRequest{Path: "empty"}); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if _, err := svc.DeleteWorkspaceFile(ctx, &brokerv1.DeleteWorkspaceFileRequest{Path: "empty"}); err != nil {
		t.Fatalf("delete empty dir: %v", err)
	}
}

func TestListWorkspaceFiles_EmptyPathUsesLegacyFullList(t *testing.T) {
	svc := filesSvc(t, t.TempDir())
	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")

	if _, err := svc.UploadWorkspaceFile(ctx, &brokerv1.UploadWorkspaceFileRequest{
		Path: "reports/q1.csv", Content: []byte("a,b,c"),
	}); err != nil {
		t.Fatalf("upload: %v", err)
	}
	// Legacy call (path unset): full recursive listing, byte-identical to today.
	list, err := svc.ListWorkspaceFiles(ctx, &brokerv1.ListWorkspaceFilesRequest{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	set := map[string]bool{}
	for _, f := range list.Files {
		set[f.Path] = true
	}
	if !set["reports/q1.csv"] || !set["reports"] {
		t.Fatalf("legacy listing should include the full subtree: %+v", list.Files)
	}
}

func TestListWorkspaceFiles_ScopedPathRoutesToListDir(t *testing.T) {
	svc := filesSvc(t, t.TempDir())
	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")

	if _, err := svc.UploadWorkspaceFile(ctx, &brokerv1.UploadWorkspaceFileRequest{
		Path: "reports/q1.csv", Content: []byte("a,b,c"),
	}); err != nil {
		t.Fatalf("upload: %v", err)
	}
	if _, err := svc.UploadWorkspaceFile(ctx, &brokerv1.UploadWorkspaceFileRequest{
		Path: "reports/archive/old.csv", Content: []byte("x"),
	}); err != nil {
		t.Fatalf("upload: %v", err)
	}
	if _, err := svc.UploadWorkspaceFile(ctx, &brokerv1.UploadWorkspaceFileRequest{
		Path: "notes.txt", Content: []byte("hi"),
	}); err != nil {
		t.Fatalf("upload: %v", err)
	}

	// Shallow scoped listing of "reports": immediate children only, and
	// nothing outside the scope.
	list, err := svc.ListWorkspaceFiles(ctx, &brokerv1.ListWorkspaceFilesRequest{Path: "reports"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	set := map[string]bool{}
	for _, f := range list.Files {
		set[f.Path] = true
	}
	if !set["reports/q1.csv"] || !set["reports/archive"] {
		t.Fatalf("scoped shallow listing missing immediate children: %+v", list.Files)
	}
	if set["reports/archive/old.csv"] || set["notes.txt"] {
		t.Fatalf("scoped shallow listing must not include nested or out-of-scope entries: %+v", list.Files)
	}

	// Same scope, recursive=true: widens to the subtree beneath it.
	list, err = svc.ListWorkspaceFiles(ctx, &brokerv1.ListWorkspaceFilesRequest{Path: "reports", Recursive: true})
	if err != nil {
		t.Fatalf("list recursive: %v", err)
	}
	set = map[string]bool{}
	for _, f := range list.Files {
		set[f.Path] = true
	}
	if !set["reports/archive/old.csv"] {
		t.Fatalf("recursive scoped listing should include the subtree: %+v", list.Files)
	}
	if set["notes.txt"] {
		t.Fatalf("recursive scoped listing must stay within scope: %+v", list.Files)
	}
}

func TestDeleteWorkspaceFile_NonEmptyDirRejected(t *testing.T) {
	svc := filesSvc(t, t.TempDir())
	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")

	if _, err := svc.UploadWorkspaceFile(ctx, &brokerv1.UploadWorkspaceFileRequest{
		Path: "reports/q1.csv", Content: []byte("x"),
	}); err != nil {
		t.Fatalf("upload: %v", err)
	}
	_, err := svc.DeleteWorkspaceFile(ctx, &brokerv1.DeleteWorkspaceFileRequest{Path: "reports"})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("delete non-empty dir should be FailedPrecondition, got %v", err)
	}
}

// ── CP4.2: session file integrity sidecars, exercised through the RPC layer ─

func TestSessionFile_RoundTripVerifiesClean(t *testing.T) {
	svc := filesSvcSigned(t, t.TempDir())
	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")

	rel := ".agent/Sessions/abc.json"
	if _, err := svc.UploadWorkspaceFile(ctx, &brokerv1.UploadWorkspaceFileRequest{
		Path: rel, Content: []byte(`{"id":"abc"}`),
	}); err != nil {
		t.Fatalf("upload: %v", err)
	}
	read, err := svc.ReadWorkspaceFile(ctx, &brokerv1.ReadWorkspaceFileRequest{Path: rel})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(read.Content) != `{"id":"abc"}` {
		t.Fatalf("content mismatch: %q", read.Content)
	}
}

func TestSessionFile_TamperedContentRefusedLoudly(t *testing.T) {
	root := t.TempDir()
	svc := filesSvcSigned(t, root)
	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")

	rel := ".agent/Sessions/abc.json"
	if _, err := svc.UploadWorkspaceFile(ctx, &brokerv1.UploadWorkspaceFileRequest{
		Path: rel, Content: []byte(`{"id":"abc"}`),
	}); err != nil {
		t.Fatalf("upload: %v", err)
	}
	// Tamper on disk directly, bypassing the RPC layer entirely.
	full := filepath.Join(root, workspacepath.SanitizeSeg(testTenantUUID), workspacepath.SanitizeSeg("alice@example.com"), rel)
	if err := os.WriteFile(full, []byte(`{"id":"abc","injected":true}`), 0o640); err != nil {
		t.Fatalf("tamper: %v", err)
	}

	_, err := svc.ReadWorkspaceFile(ctx, &brokerv1.ReadWorkspaceFileRequest{Path: rel})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("tampered session file should be FailedPrecondition, got %v", err)
	}
	if !strings.Contains(err.Error(), "session integrity check failed") {
		t.Fatalf("error must carry the grep-stable message, got %q", err.Error())
	}
}

func TestSessionFile_LegacyNoSidecarAllowed(t *testing.T) {
	root := t.TempDir()
	// Written with signing disabled (pre-CP4.2 posture), then read back with
	// signing enabled — simulates a file persisted before the key existed.
	unsignedSvc := filesSvc(t, root)
	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")
	rel := ".agent/Sessions/abc.json"
	if _, err := unsignedSvc.UploadWorkspaceFile(ctx, &brokerv1.UploadWorkspaceFileRequest{
		Path: rel, Content: []byte(`{"id":"abc"}`),
	}); err != nil {
		t.Fatalf("upload: %v", err)
	}

	signedSvc := filesSvcSigned(t, root)
	read, err := signedSvc.ReadWorkspaceFile(ctx, &brokerv1.ReadWorkspaceFileRequest{Path: rel})
	if err != nil {
		t.Fatalf("legacy (no sidecar) read should be allowed, got %v", err)
	}
	if string(read.Content) != `{"id":"abc"}` {
		t.Fatalf("content mismatch: %q", read.Content)
	}
}

func TestSessionFile_SigNotListed(t *testing.T) {
	svc := filesSvcSigned(t, t.TempDir())
	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")

	rel := ".agent/Sessions/abc.json"
	if _, err := svc.UploadWorkspaceFile(ctx, &brokerv1.UploadWorkspaceFileRequest{
		Path: rel, Content: []byte(`{"id":"abc"}`),
	}); err != nil {
		t.Fatalf("upload: %v", err)
	}
	list, err := svc.ListWorkspaceFiles(ctx, &brokerv1.ListWorkspaceFilesRequest{Path: ".agent/Sessions", Recursive: true})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, f := range list.Files {
		if strings.HasSuffix(f.Path, ".sig") {
			t.Fatalf("sig sidecar must not appear in listings: %+v", list.Files)
		}
	}
}

// TestSessionFile_MovedIntoSessionsDirVerifies is the RPC-level proof of
// review fix #2: MoveWorkspaceFile is a third writer that the initial CP4.2
// investigation missed. Moving an ordinary file onto a path under
// .agent/Sessions/ must produce a signed record, not a legacy/unsigned one.
func TestSessionFile_MovedIntoSessionsDirVerifies(t *testing.T) {
	root := t.TempDir()
	svc := filesSvcSigned(t, root)
	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")

	content := []byte(`{"id":"moved"}`)
	if _, err := svc.UploadWorkspaceFile(ctx, &brokerv1.UploadWorkspaceFileRequest{
		Path: "draft.json", Content: content,
	}); err != nil {
		t.Fatalf("upload: %v", err)
	}

	dest := ".agent/Sessions/moved.json"
	if _, err := svc.MoveWorkspaceFile(ctx, &brokerv1.MoveWorkspaceFileRequest{
		FromPath: "draft.json", ToPath: dest,
	}); err != nil {
		t.Fatalf("move: %v", err)
	}

	read, err := svc.ReadWorkspaceFile(ctx, &brokerv1.ReadWorkspaceFileRequest{Path: dest})
	if err != nil {
		t.Fatalf("read after move: %v", err)
	}
	if string(read.Content) != string(content) {
		t.Fatalf("content mismatch after move: got %q want %q", read.Content, content)
	}

	// Tamper on disk directly after the move — a genuinely-signed record must
	// refuse, proving Move did not leave the file unsigned/legacy.
	full := filepath.Join(root, workspacepath.SanitizeSeg(testTenantUUID), workspacepath.SanitizeSeg("alice@example.com"), dest)
	if err := os.WriteFile(full, []byte(`{"id":"moved","injected":true}`), 0o640); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	_, err = svc.ReadWorkspaceFile(ctx, &brokerv1.ReadWorkspaceFileRequest{Path: dest})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("tampered moved-in session file should be FailedPrecondition, got %v", err)
	}
	if !strings.Contains(err.Error(), "session integrity check failed") {
		t.Fatalf("error must carry the grep-stable message, got %q", err.Error())
	}
}

// ── .agent/ write guard at the user-facing file RPCs (ZT audit 2026-08 CP1) ─

// TestFileRPCs_RejectAgentDirWrites pins the authorization fix: the four
// mutating file RPCs are a user-facing boundary and must refuse the
// server-maintained .agent/ tree. Without this, any authenticated user could
// POST a concept file into their own .agent/Memory/ bundle carrying forged
// `verified:` frontmatter — provenance forgery with no skill:memory.write grant
// (the toolproxy seam has always refused this; these RPCs bypassed it).
func TestFileRPCs_RejectAgentDirWrites(t *testing.T) {
	// Traversal spellings are included deliberately: filepath.Join collapses
	// them into the real .agent/ tree, so a raw prefix test would let them past.
	blocked := []string{
		".agent/Memory/x.md",
		".agent/Memory",
		".agent/x",
		".agent",
		"./.agent/Memory/x.md",
		"a/../.agent/Memory/x.md",
		// The Sessions carve-out is the one hole in this guard: a path spelled
		// through it must be cleaned before the carve-out is tested, or it
		// lands in Memory/ with the exception's blessing.
		".agent/Sessions/../Memory/x.md",
		// workspacefs.CleanRel trims leading whitespace before resolving, so a
		// guard that normalizes differently reports "not .agent/" for a path
		// the store still writes into the real bundle.
		" .agent/Memory/x.md",
		"\t.agent/Sessions/../Memory/x.md",
	}
	for _, rel := range blocked {
		t.Run(rel, func(t *testing.T) {
			root := t.TempDir()
			svc := filesSvc(t, root)
			ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")

			_, err := svc.UploadWorkspaceFile(ctx, &brokerv1.UploadWorkspaceFileRequest{
				Path: rel, Content: []byte("---\nverified: true\n---\nforged\n"),
			})
			if status.Code(err) != codes.PermissionDenied {
				t.Errorf("upload %q: want PermissionDenied, got %v", rel, err)
			}
			if _, err := svc.CreateWorkspaceDir(ctx, &brokerv1.CreateWorkspaceDirRequest{Path: rel}); status.Code(err) != codes.PermissionDenied {
				t.Errorf("mkdir %q: want PermissionDenied, got %v", rel, err)
			}
			if _, err := svc.DeleteWorkspaceFile(ctx, &brokerv1.DeleteWorkspaceFileRequest{Path: rel}); status.Code(err) != codes.PermissionDenied {
				t.Errorf("delete %q: want PermissionDenied, got %v", rel, err)
			}
			// Both Move endpoints: moving a file OUT of a memory bundle mutates
			// it just as much as moving one in.
			if _, err := svc.MoveWorkspaceFile(ctx, &brokerv1.MoveWorkspaceFileRequest{
				FromPath: "notes.txt", ToPath: rel,
			}); status.Code(err) != codes.PermissionDenied {
				t.Errorf("move to %q: want PermissionDenied, got %v", rel, err)
			}
			if _, err := svc.MoveWorkspaceFile(ctx, &brokerv1.MoveWorkspaceFileRequest{
				FromPath: rel, ToPath: "notes.txt",
			}); status.Code(err) != codes.PermissionDenied {
				t.Errorf("move from %q: want PermissionDenied, got %v", rel, err)
			}

			// The status codes alone would stay green if the guard denied
			// *after* performing the write — prove nothing landed on disk.
			if err := filepath.Walk(root, func(p string, _ os.FileInfo, err error) error {
				if err != nil {
					return err
				}
				if filepath.Base(p) == ".agent" {
					t.Errorf("%q denied but %s exists on disk", rel, p)
				}
				return nil
			}); err != nil {
				t.Fatalf("walk %s: %v", root, err)
			}
		})
	}
}

// TestFileRPCs_SessionsDirCarveOutHolds pins the required exception: the webui
// persists session records through these same RPCs (see
// workspacefs.SessionsDirPrefix) and those writes are HMAC-signed. Removing the
// carve-out would break session persistence outright.
func TestFileRPCs_SessionsDirCarveOutHolds(t *testing.T) {
	svc := filesSvcSigned(t, t.TempDir())
	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")

	rel := ".agent/Sessions/x.json"
	if _, err := svc.UploadWorkspaceFile(ctx, &brokerv1.UploadWorkspaceFileRequest{
		Path: rel, Content: []byte(`{"id":"x"}`),
	}); err != nil {
		t.Fatalf("upload %q: %v", rel, err)
	}
	if _, err := svc.MoveWorkspaceFile(ctx, &brokerv1.MoveWorkspaceFileRequest{
		FromPath: rel, ToPath: ".agent/Sessions/y.json",
	}); err != nil {
		t.Fatalf("move within Sessions: %v", err)
	}
	if _, err := svc.DeleteWorkspaceFile(ctx, &brokerv1.DeleteWorkspaceFileRequest{Path: ".agent/Sessions/y.json"}); err != nil {
		t.Fatalf("delete session record: %v", err)
	}
	if _, err := svc.CreateWorkspaceDir(ctx, &brokerv1.CreateWorkspaceDirRequest{Path: ".agent/Sessions/archive"}); err != nil {
		t.Fatalf("mkdir under Sessions: %v", err)
	}
}

// ── CP5: file RPCs route by pref + trigger ensureOneDriveOBO ────────────────

// fakeRemoteBackend is a minimal recording workspacefs.Backend fake used as
// Router.Remote — proves a non-reserved, onedrive-routed call reaches Remote
// rather than Local (spec success criterion "file RPCs route by pref").
type fakeRemoteBackend struct {
	calls []string
}

func (f *fakeRemoteBackend) Enabled() bool { return true }

func (f *fakeRemoteBackend) List(ctx context.Context, tenant, user string) ([]workspacefs.FileInfo, error) {
	f.calls = append(f.calls, "List")
	return nil, nil
}

func (f *fakeRemoteBackend) ListDir(ctx context.Context, tenant, user, dir string, recursive bool) ([]workspacefs.FileInfo, error) {
	f.calls = append(f.calls, "ListDir:"+dir)
	return nil, nil
}

func (f *fakeRemoteBackend) Read(ctx context.Context, tenant, user, rel string) ([]byte, time.Time, error) {
	f.calls = append(f.calls, "Read:"+rel)
	return []byte("remote-data"), time.Time{}, nil
}

func (f *fakeRemoteBackend) Write(ctx context.Context, tenant, user, rel string, data []byte) (workspacefs.FileInfo, error) {
	f.calls = append(f.calls, "Write:"+rel)
	return workspacefs.FileInfo{Path: rel, Size: int64(len(data))}, nil
}

func (f *fakeRemoteBackend) Delete(ctx context.Context, tenant, user, rel string) error {
	f.calls = append(f.calls, "Delete:"+rel)
	return nil
}

func (f *fakeRemoteBackend) Move(ctx context.Context, tenant, user, from, to string) (workspacefs.FileInfo, error) {
	f.calls = append(f.calls, "Move:"+from+"->"+to)
	return workspacefs.FileInfo{Path: to}, nil
}

func (f *fakeRemoteBackend) Mkdir(ctx context.Context, tenant, user, rel string) error {
	f.calls = append(f.calls, "Mkdir:"+rel)
	return nil
}

// fakeFilesPrefs is a fake workspacefs.PrefResolver reporting a fixed kind,
// never touching a database.
type fakeFilesPrefs struct{ kind workspacefs.BackendKind }

func (p fakeFilesPrefs) WorkspaceBackend(ctx context.Context, tenant, user string) (workspacefs.BackendKind, error) {
	return p.kind, nil
}

// ctxWithIdentityAndBearer layers a bearer token (ensureOneDriveOBO needs one
// to attempt Bootstrap) on top of ctxWithIdentity's authenticated identity.
func ctxWithIdentityAndBearer(tenant, user, token string) context.Context {
	return metadata.NewIncomingContext(ctxWithIdentity(tenant, user), metadata.Pairs("authorization", "Bearer "+token))
}

func TestFiles_OneDriveRoutedRead_HitsRemoteAndTriggersEnsureOBO(t *testing.T) {
	em, err := audit.NewEmitter(context.Background(), audit.Config{})
	if err != nil {
		t.Fatal(err)
	}
	remote := &fakeRemoteBackend{}
	obo := &fakeOBOBroker{configured: true}
	svc := NewBrokerService(Deps{
		Logger:    zap.NewNop(),
		Audit:     em,
		TenantID:  "aikonos-dev",
		Workspace: WorkspaceDeps{Local: &workspacefs.Router{Local: workspacefs.New(t.TempDir()), Remote: remote, Prefs: fakeFilesPrefs{kind: workspacefs.KindOneDrive}}},
		OBOBroker: obo,
	})
	ctx := ctxWithIdentityAndBearer(testTenantUUID, "alice@example.com", "tok")

	if _, err := svc.ReadWorkspaceFile(ctx, &brokerv1.ReadWorkspaceFileRequest{Path: "notes.txt"}); err != nil {
		t.Fatalf("read: %v", err)
	}
	var sawRead bool
	for _, c := range remote.calls {
		if c == "Read:notes.txt" {
			sawRead = true
		}
	}
	if !sawRead {
		t.Fatalf("expected the onedrive-routed read to hit Remote, got %v", remote.calls)
	}
	if len(obo.bootstrapCalls) != 1 {
		t.Fatalf("expected ensureOneDriveOBO to fire exactly once for the onedrive-routed call, got %v", obo.bootstrapCalls)
	}
}

func TestFiles_ReservedPath_StaysLocal_NeverEnsuresOBO(t *testing.T) {
	em, err := audit.NewEmitter(context.Background(), audit.Config{})
	if err != nil {
		t.Fatal(err)
	}
	local := workspacefs.New(t.TempDir())
	remote := &fakeRemoteBackend{}
	obo := &fakeOBOBroker{configured: true}
	svc := NewBrokerService(Deps{
		Logger:    zap.NewNop(),
		Audit:     em,
		TenantID:  "aikonos-dev",
		Workspace: WorkspaceDeps{Local: &workspacefs.Router{Local: local, Remote: remote, Prefs: fakeFilesPrefs{kind: workspacefs.KindOneDrive}}},
		OBOBroker: obo,
	})
	ctx := ctxWithIdentityAndBearer(testTenantUUID, "alice@example.com", "tok")

	if _, err := svc.UploadWorkspaceFile(ctx, &brokerv1.UploadWorkspaceFileRequest{
		Path: ".agent/Sessions/x.json", Content: []byte("{}"),
	}); err != nil {
		t.Fatalf("upload: %v", err)
	}
	if len(remote.calls) != 0 {
		t.Fatalf("a reserved path must never reach Remote: %v", remote.calls)
	}
	if len(obo.bootstrapCalls) != 0 {
		t.Fatalf("a reserved path must never trigger ensureOneDriveOBO: %v", obo.bootstrapCalls)
	}
	data, _, err := local.Read(context.Background(), testTenantUUID, "alice@example.com", ".agent/Sessions/x.json")
	if err != nil || string(data) != "{}" {
		t.Fatalf("expected the reserved-path write to have landed on Local: data=%q err=%v", data, err)
	}
}

// TestFiles_PrefsOneDriveButRemoteNil_NeverEnsuresOBO pins F-4's fix: Router
// is the single source of truth for the routing decision (Router.ActiveKind),
// including the Remote == nil case activeBackend already handles (surfaced
// as ErrUnavailable, never a silent onedrive dispatch). Before the fix,
// fileRPCRoutesOneDrive queried router.Prefs directly and never noticed
// Remote was nil, so it fired ensureOneDriveOBO even though the RPC's own
// actual write is about to fail with the very same ErrUnavailable.
func TestFiles_PrefsOneDriveButRemoteNil_NeverEnsuresOBO(t *testing.T) {
	em, err := audit.NewEmitter(context.Background(), audit.Config{})
	if err != nil {
		t.Fatal(err)
	}
	local := workspacefs.New(t.TempDir())
	obo := &fakeOBOBroker{configured: true}
	svc := NewBrokerService(Deps{
		Logger:   zap.NewNop(),
		Audit:    em,
		TenantID: "aikonos-dev",
		// Remote intentionally nil: Prefs reports onedrive but there is no
		// remote backend wired to actually serve it.
		Workspace: WorkspaceDeps{Local: &workspacefs.Router{Local: local, Prefs: fakeFilesPrefs{kind: workspacefs.KindOneDrive}}},
		OBOBroker: obo,
	})
	ctx := ctxWithIdentityAndBearer(testTenantUUID, "alice@example.com", "tok")

	_, err = svc.UploadWorkspaceFile(ctx, &brokerv1.UploadWorkspaceFileRequest{
		Path: "notes.txt", Content: []byte("hi"),
	})
	if err == nil {
		t.Fatalf("expected the upload itself to fail with ErrUnavailable (Prefs=onedrive, Remote=nil)")
	}
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("expected codes.Unavailable, got %v", err)
	}
	if len(obo.bootstrapCalls) != 0 {
		t.Fatalf("Prefs=onedrive with Remote==nil must never trigger ensureOneDriveOBO, got %v", obo.bootstrapCalls)
	}
}
